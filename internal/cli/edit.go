package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/textutil"
	"github.com/Bloopps/docker-update/internal/tomledit"
	"github.com/Bloopps/docker-update/internal/tomlmini"
)

// prompter lit les réponses de l'utilisateur sur l'entrée standard (un tube dans les tests).
type prompter struct {
	r   *bufio.Reader
	out io.Writer
}

func newPrompter(env *Env) *prompter {
	return &prompter{r: bufio.NewReader(env.Stdin), out: env.Stdout}
}

var errNoAnswer = errors.New("pas de réponse sur l'entrée standard")

func (p *prompter) ask(question string) (string, error) {
	fmt.Fprint(p.out, question)
	line, err := p.r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		fmt.Fprintln(p.out)
		return "", errNoAnswer
	}
	return strings.TrimSpace(line), nil
}

func (p *prompter) yes(question string) bool {
	a, err := p.ask(question + " [o/N] ")
	if err != nil {
		return false
	}
	switch strings.ToLower(a) {
	case "o", "oui", "y", "yes":
		return true
	}
	return false
}

// ── list ──

func pad(s string, n int) string {
	if c := utf8.RuneCountInString(s); c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return s
}

func windowText(w textutil.Window) string {
	if !w.Set {
		return "toujours"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
}

// list affiche les services configurés (lecture seule).
func list(cfg *config.Config, st *state.State, log func(string)) {
	rows := [][]string{{"SERVICE", "GROUPE", "MODE", "DÉLAI", "FENÊTRE", "VEILLE", "PAUSE"}}
	for _, s := range cfg.Services {
		var watch []string
		if len(s.Advisories) > 0 {
			watch = append(watch, "avis")
		}
		if s.ReleaseNotes {
			watch = append(watch, "notes")
		}
		w := "-"
		if len(watch) > 0 {
			w = strings.Join(watch, "+")
		}
		held := "-"
		if st != nil && (st.HasHold(s.Name) || (s.Group != "" && st.HasHold(s.Group))) {
			held = "pause"
		}
		group := s.Group
		if group == "" {
			group = "-"
		}
		rows = append(rows, []string{s.Name, group, s.Mode, textutil.FmtDuration(s.Cooldown), windowText(s.Window), w, held})
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = pad(c, widths[i])
		}
		log(strings.TrimRight(strings.Join(cells, "  "), " "))
	}
	log(fmt.Sprintf("%d service(s)", len(cfg.Services)))
}

// ── écriture sûre du fichier de configuration ──

// unifiedDiff compare deux textes qui ne diffèrent que par un bloc contigu de lignes (ajout ou retrait d'un bloc).
func unifiedDiff(name, a, b string) string {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	x, y := split(a), split(b)
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	if pre == len(x) && pre == len(y) {
		return ""
	}
	const ctx = 3
	from := max(0, pre-ctx)
	endX, endY := len(x)-suf+min(ctx, suf), len(y)-suf+min(ctx, suf)
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s (modifié)\n@@ -%d,%d +%d,%d @@\n", name, name, from+1, endX-from, from+1, endY-from)
	for _, l := range x[from:pre] {
		sb.WriteString(" " + l + "\n")
	}
	for _, l := range x[pre : len(x)-suf] {
		sb.WriteString("-" + l + "\n")
	}
	for _, l := range y[pre : len(y)-suf] {
		sb.WriteString("+" + l + "\n")
	}
	for _, l := range x[len(x)-suf : endX] {
		sb.WriteString(" " + l + "\n")
	}
	return sb.String()
}

// writeLike écrit un fichier de façon atomique avec les droits et le propriétaire d'un fichier de référence.
func writeLike(path string, data []byte, ref os.FileInfo) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".edit-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, ref.Mode().Perm()); err != nil {
		return err
	}
	if st, ok := ref.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(tmp, int(st.Uid), int(st.Gid)); err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// checkResult relit un texte de configuration avec le même code que `validate` : erreur de lecture ou de validation.
func checkResult(data []byte) (*config.Config, error) {
	doc, err := tomlmini.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("configuration illisible : %v", err)
	}
	cfg, err := config.Parse(doc)
	if err != nil {
		var ce *config.Error
		if errors.As(err, &ce) {
			return nil, errors.New(ce.Msg)
		}
		return nil, err
	}
	return cfg, nil
}

// applyEdit montre l'aperçu, demande confirmation, sauvegarde l'ancien fichier, écrit le nouveau, le relit et le valide ;
// si la relecture échoue, l'ancien fichier est remis en place. Renvoie le code de sortie.
func applyEdit(env *Env, g globals, p *prompter, yes bool, oldData, newData []byte, log func(string)) (int, error) {
	if _, err := checkResult(newData); err != nil {
		log("refusé : le fichier obtenu serait invalide, rien n'est écrit : " + err.Error())
		return 1, nil
	}
	diff := unifiedDiff(g.config, string(oldData), string(newData))
	if diff == "" {
		log("aucun changement")
		return 0, nil
	}
	fmt.Fprint(env.Stdout, diff)
	if g.dryRun {
		log("[simulation] rien n'est écrit")
		return 0, nil
	}
	if !yes && !p.yes("Écrire ces changements ?") {
		log("annulé, rien n'est écrit")
		return 1, nil
	}
	info, err := os.Stat(g.config)
	if err != nil {
		return 0, err
	}
	stamp := env.Now().Format("20060102-150405")
	backup := g.config + ".bak-" + stamp
	for i := 2; ; i++ {
		if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
			break
		}
		backup = fmt.Sprintf("%s.bak-%s-%d", g.config, stamp, i)
	}
	if err := writeLike(backup, oldData, info); err != nil {
		return 0, fmt.Errorf("sauvegarde impossible, rien n'est écrit : %v", err)
	}
	if err := writeLike(g.config, newData, info); err != nil {
		return 0, fmt.Errorf("écriture impossible (l'ancien fichier est intact) : %v", err)
	}
	if _, err := env.Load(g.config); err != nil {
		if rerr := writeLike(g.config, oldData, info); rerr != nil {
			return 0, fmt.Errorf("fichier écrit invalide et ancien fichier non remis : %v ; copie : %s", rerr, backup)
		}
		log("annulé : le fichier relu est invalide, l'ancien est remis en place : " + err.Error())
		return 1, nil
	}
	log(fmt.Sprintf("écrit : %s (sauvegarde : %s)", g.config, backup))
	return 0, nil
}

// withConfigLock prend le verrou de passage pendant l'édition, puis relit le fichier sous le verrou.
func withConfigLock(cfg *config.Config, g globals, log func(string), f func(data []byte) (int, error)) (int, error) {
	unlock, err := lock(cfg.Settings.LockFile)
	if err != nil {
		if errors.Is(err, errBusy) {
			log("un passage est en cours : réessayez dans un instant, rien n'est modifié")
			return 1, nil
		}
		return 0, err
	}
	defer unlock()
	data, err := os.ReadFile(g.config)
	if err != nil {
		return 0, err
	}
	return f(data)
}

func editErr(err error) error {
	var te *tomledit.Error
	if errors.As(err, &te) {
		return &config.Error{Msg: te.Msg}
	}
	return err
}

// ── delete ──

func deleteCmd(env *Env, g globals, sub subArgs, cfg *config.Config, log func(string)) (int, error) {
	groups := sub.values["--group"]
	if len(sub.names) == 0 && len(groups) == 0 {
		return 0, usagef("delete : au moins un nom ou --group est requis")
	}
	return withConfigLock(cfg, g, log, func(data []byte) (int, error) {
		f, err := tomledit.Parse(data)
		if err != nil {
			return 0, editErr(err)
		}
		cur, err := checkResult(data)
		if err != nil {
			return 0, &config.Error{Msg: err.Error()}
		}
		var names []string
		add := func(n string) {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		for _, n := range sub.names {
			if f.Index(n) < 0 {
				log(fmt.Sprintf("inconnu : %s (services : %s)", n, strings.Join(f.Names(), ", ")))
				return 1, nil
			}
			add(n)
		}
		for _, gr := range groups {
			found := false
			for _, s := range cur.Services {
				if s.Group == gr {
					add(s.Name)
					found = true
				}
			}
			if !found {
				log(fmt.Sprintf("groupe inconnu : %s", gr))
				return 1, nil
			}
		}
		for _, n := range names {
			text, _ := f.Block(n)
			fmt.Fprintf(env.Stdout, "À retirer : %s\n%s\n\n", n, text)
		}
		for _, n := range names {
			if err := f.Delete(n); err != nil {
				return 0, editErr(err)
			}
		}
		p := newPrompter(env)
		code, err := applyEdit(env, g, p, sub.flags["--yes"], data, f.Bytes(), log)
		if err != nil || code != 0 || g.dryRun {
			return code, err
		}
		if !sub.flags["--keep-state"] {
			if err := purgeState(cur, names, log); err != nil {
				return 0, err
			}
		}
		log("Les conteneurs de ces services continuent de tourner, simplement hors du suivi : rien n'a été arrêté ni supprimé.")
		return 0, nil
	})
}

// purgeState retire du fichier d'état ce qui ne concerne plus que les services supprimés : pauses, erreurs, alertes.
// Les compteurs d'images se purgent au prochain passage complet.
func purgeState(cfg *config.Config, removed []string, log func(string)) error {
	st, err := state.Load(cfg.Settings.StateFile)
	if err != nil {
		return err
	}
	gone := map[string]bool{}
	for _, n := range removed {
		gone[n] = true
	}
	remaining := map[string]bool{}
	for _, s := range cfg.Services {
		if !gone[s.Name] && s.Group != "" {
			remaining[s.Group] = true
		}
	}
	changed := 0
	var holds []string
	for _, h := range st.Holds {
		isGroup := false
		for _, s := range cfg.Services {
			if s.Group == h {
				isGroup = true
			}
		}
		if gone[h] || (isGroup && !remaining[h]) {
			changed++
			continue
		}
		holds = append(holds, h)
	}
	if holds == nil {
		holds = []string{}
	}
	st.Holds = holds
	for n := range gone {
		if _, ok := st.Errors[n]; ok {
			delete(st.Errors, n)
			changed++
		}
	}
	for k := range st.Alerts {
		if parts := strings.SplitN(k, ":", 3); len(parts) >= 2 && gone[parts[1]] {
			delete(st.Alerts, k)
			changed++
		}
	}
	if changed == 0 {
		return nil
	}
	if err := st.Save(); err != nil {
		return err
	}
	log(fmt.Sprintf("état : %d entrée(s) retirée(s) (pauses, erreurs, alertes)", changed))
	return nil
}

// ── add ──

var groupSettingKeys = []string{"mode", "cooldown", "window", "cleanup", "advisories", "min_severity", "security_cooldown", "security_policy"}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func lastValue(sub subArgs, flag string) (string, bool) {
	v := sub.values[flag]
	if len(v) == 0 {
		return "", false
	}
	return v[len(v)-1], true
}

func addCmd(env *Env, g globals, sub subArgs, cfg *config.Config, log func(string)) (int, error) {
	name := sub.names[0]
	return withConfigLock(cfg, g, log, func(data []byte) (int, error) {
		f, err := tomledit.Parse(data)
		if err != nil {
			return 0, editErr(err)
		}
		if _, err := checkResult(data); err != nil {
			return 0, &config.Error{Msg: err.Error()}
		}
		doc, _ := tomlmini.Parse(data)
		p := newPrompter(env)
		fail := func(format string, a ...any) (int, error) {
			log("refusé : " + fmt.Sprintf(format, a...))
			return 1, nil
		}
		if f.Index(name) >= 0 {
			return fail("le nom %s est déjà pris", name)
		}
		group, _ := lastValue(sub, "--group")
		if group != "" && group == name {
			return fail("le nom de groupe ne peut pas être celui du service (%s)", name)
		}
		if f.Index(group) >= 0 && group != "" {
			return fail("le groupe %s porte le nom d'un service existant", group)
		}
		for _, t := range doc.Arrays["service"] {
			if gv, ok := t.Get("group"); ok && gv.PyStr() == name {
				return fail("%s est déjà un nom de groupe", name)
			}
		}
		need := func(flag, question string) (string, bool) {
			if v, ok := lastValue(sub, flag); ok {
				return v, true
			}
			a, err := p.ask(question)
			if err != nil || a == "" {
				return "", false
			}
			return a, true
		}
		dir, ok := need("--compose-dir", "Dossier compose (compose_dir) : ")
		if !ok {
			return fail("--compose-dir est obligatoire")
		}
		svc, ok := need("--service", "Nom du service dans le fichier compose : ")
		if !ok {
			return fail("--service est obligatoire")
		}

		entries := []tomledit.Entry{{Key: "name", Val: tomledit.String(name)}, {Key: "compose_dir", Val: tomledit.String(dir)},
			{Key: "service", Val: tomledit.String(svc)}}
		have := map[string]bool{}
		set := func(key string, v tomlmini.Value) {
			entries = append(entries, tomledit.Entry{Key: key, Val: v})
			have[key] = true
		}
		if files := sub.values["--compose-file"]; len(files) > 0 {
			set("compose_files", tomledit.Strings(files))
		}
		if v, ok := lastValue(sub, "--mode"); ok {
			set("mode", tomledit.String(v))
		}
		if v, ok := lastValue(sub, "--cooldown"); ok {
			set("cooldown", tomledit.String(v))
		}
		if group != "" {
			set("group", tomledit.String(group))
		}
		if v, ok := lastValue(sub, "--window"); ok {
			set("window", tomledit.String(v))
		}
		if v, ok := lastValue(sub, "--cleanup"); ok {
			set("cleanup", tomledit.Bool(v == "true"))
		}
		if v, ok := lastValue(sub, "--advisories"); ok {
			set("advisories", tomledit.Strings(splitList(v)))
		}
		if v, ok := lastValue(sub, "--min-severity"); ok {
			set("min_severity", tomledit.String(v))
		}
		if v, ok := lastValue(sub, "--security-cooldown"); ok {
			set("security_cooldown", tomledit.String(v))
		}
		if v, ok := lastValue(sub, "--security-policy"); ok {
			set("security_policy", tomledit.String(v))
		}
		if sub.flags["--release-notes"] {
			set("release_notes", tomledit.Bool(true))
		}
		if v, ok := lastValue(sub, "--release-notes-repos"); ok {
			set("release_notes_repos", tomledit.Strings(splitList(v)))
		}
		if v, ok := lastValue(sub, "--release-notes-ignore"); ok {
			set("release_notes_ignore", tomledit.Strings(splitList(v)))
		}
		if v, ok := lastValue(sub, "--smoke-url"); ok {
			set("smoke_url", tomledit.String(v))
		}
		if v, ok := lastValue(sub, "--smoke-expect"); ok {
			var codes []int
			for _, c := range splitList(v) {
				n, err := strconv.Atoi(c)
				if err != nil {
					return fail("--smoke-expect : %q n'est pas un code HTTP", c)
				}
				codes = append(codes, n)
			}
			set("smoke_expect", tomledit.Ints(codes))
		}
		if v, ok := lastValue(sub, "--health-timeout"); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail("--health-timeout : %q n'est pas un nombre de secondes", v)
			}
			set("health_timeout", tomledit.Int(int64(n)))
		}

		// Un groupe existant : le nouveau membre reprend les réglages du groupe qu'il ne précise pas.
		after, first := "", sub.flags["--first"]
		if group != "" {
			var lastTable *tomlmini.Table
			for _, t := range doc.Arrays["service"] {
				if gv, ok := t.Get("group"); ok && gv.PyStr() == group {
					lastTable = t
				}
			}
			if lastTable != nil {
				for _, k := range groupSettingKeys {
					if v, ok := lastTable.Get(k); ok && !have[k] {
						set(k, v)
					}
				}
				if mn, ok := lastTable.Get("name"); ok {
					after = mn.PyStr()
				}
			}
		}
		if !have["mode"] {
			set("mode", tomledit.String("notify"))
		}
		if v, ok := lastValue(sub, "--after"); ok {
			if f.Index(v) < 0 {
				return fail("--after : service inconnu : %s", v)
			}
			after, first = v, false
		}
		if first {
			after = ""
		}
		text, err := tomledit.Render(entries)
		if err != nil {
			return 0, editErr(err)
		}
		if err := f.Insert(text, after, first); err != nil {
			return 0, editErr(err)
		}
		code, err := applyEdit(env, g, p, sub.flags["--yes"], data, f.Bytes(), log)
		if err == nil && code == 0 && !g.dryRun {
			log(fmt.Sprintf("Suite : docker-update validate, puis docker-update check %s (aucune mise à jour n'est lancée par `add`).", name))
		}
		return code, err
	})
}
