package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/discover"
	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/textutil"
	"github.com/Bloopps/docker-update/internal/tomledit"
)

// initDefaults : les réglages écrits par `init`, d'accord avec les unités systemd fournies (deploy/).
func initDefaults() config.Settings {
	s := config.DefaultSettings()
	s.StateFile = "/var/lib/docker-update/state.json"
	s.LockFile = "/run/docker-update/lock"
	s.NotifyURLFile = "/etc/docker-update/discord_webhook"
	s.SettleSeconds = 20
	return s
}

// unit : un service Compose qui tourne, avec ce que l'utilisateur a décidé pour lui.
type unit struct {
	name, project, service, image, dir string
	files                              []string
	follow                             bool
	why                                string // pourquoi il est écarté par défaut
	group                              string
	mode                               string
	cleanup                            bool
	repo                               string // dépôt GitHub des avis ("" : aucun)
	notes                              bool   // release_notes
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

var nameCharsRE = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeName(s string) string {
	s = strings.Trim(nameCharsRE.ReplaceAllString(s, "-"), "-._")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func matches(u *unit, list []string) bool {
	return slices.ContainsFunc(list, func(w string) bool { return w == u.name || w == u.service || w == u.project })
}

// initCmd vérifie l'installation, puis crée une configuration neuve à partir des conteneurs Compose qui tournent.
// Il n'exécute aucune commande qui modifie quoi que ce soit côté Docker, et ne lance aucune mise à jour.
func initCmd(env *Env, g globals, sub subArgs, log func(string)) (int, error) {
	yes := sub.flags["--yes"]
	mode, _ := lastValue(sub, "--mode")
	if mode != "" && !slices.Contains(config.Modes, mode) {
		return 0, usagef("init : --mode doit être l'un de : %s", strings.Join(config.Modes, ", "))
	}
	settings := env.Defaults()
	_, fileThere := exists(g.config)
	if fileThere {
		if cfg, err := env.Load(g.config); err == nil {
			settings = cfg.Settings
		}
	}
	run := env.NewRunner(g.dryRun, log)
	ck := &checker{run: run, euid: env.Euid(), cfgPath: g.config, settings: settings, unitDir: env.UnitDir, binary: env.InstallBin,
		getenv: env.Getenv, dial: env.Dial, trusted: env.CheckTrusted}
	log("Vérification de l'installation (lecture seule) :")
	blocked, warned := printChecks(ck.all(sub.flags["--skip-checks"]), log)
	if sub.flags["--check"] {
		if blocked+warned == 0 {
			log("tout est en ordre")
			return 0, nil
		}
		log(fmt.Sprintf("%d point(s) bloquant(s), %d avertissement(s)", blocked, warned))
		return 1, nil
	}
	if blocked > 0 {
		log("init s'arrête : corrigez les points ✗ ci-dessus puis relancez (init --check rejoue ces contrôles).")
		return 1, nil
	}
	if warned > 0 {
		log("Les points ⚠ n'empêchent pas init ; ils se corrigent ensuite (init --check les rejoue).")
	}
	p := newPrompter(env)
	if !sub.flags["--no-install"] {
		steps, reload := installPlan(env, settings, g.config)
		if len(steps) > 0 {
			log("À installer :")
			for _, s := range steps {
				log("  - " + s.what)
			}
			switch {
			case g.dryRun:
				log("[simulation] rien n'est installé")
			case env.Euid() != 0:
				log("init s'arrête : l'installation demande root. Relancez avec sudo : sudo docker-update init")
				return 1, nil
			case !yes && !p.yes("Installer maintenant ?"):
				log("installation ignorée (relancez init pour la refaire)")
			default:
				if err := runInstall(run, steps, reload, log); err != nil {
					log("installation interrompue : " + err.Error())
					return 1, nil
				}
				log("Vérification après installation :")
				if blocked, _ := printChecks(ck.all(sub.flags["--skip-checks"]), log); blocked > 0 {
					log("init s'arrête : corrigez les points ✗ ci-dessus puis relancez.")
					return 1, nil
				}
			}
		}
	}
	if fileThere && !sub.flags["--force"] {
		log(fmt.Sprintf("%s existe déjà : `docker-update add` ajoute un service ; `init --force` repart de zéro (l'ancien fichier est sauvegardé).", g.config))
		return 1, nil
	}

	containers, err := discover.Running(run)
	if err != nil {
		return 0, err
	}
	var units []*unit
	seen := map[string]bool{}
	for _, c := range containers {
		key := c.Project + "|" + c.Service
		if seen[key] {
			continue
		}
		seen[key] = true
		if why, ok := discover.Verify(run, c); !ok {
			log(fmt.Sprintf("AVERTISSEMENT : %s/%s écarté : %s", textutil.CleanText(c.Project, 64), textutil.CleanText(c.Service, 64), why))
			continue
		}
		u := &unit{project: c.Project, service: c.Service, image: textutil.CleanText(c.Image, 200), dir: c.WorkingDir,
			files: discover.ComposeFiles(c), cleanup: true}
		u.why, _ = discover.Never(c.Image)
		u.follow = u.why == ""
		units = append(units, u)
	}
	if len(units) == 0 {
		log("aucun conteneur Compose utilisable en marche : rien à proposer (un fichier se rédige à la main à partir de examples/).")
		return 1, nil
	}
	names := map[string]int{}
	for _, u := range units {
		names[sanitizeName(u.service)]++
	}
	for _, u := range units {
		u.name = sanitizeName(u.service)
		if names[u.name] > 1 {
			u.name = sanitizeName(u.project + "-" + u.service)
		}
		if u.name == "" {
			u.name = "service"
		}
	}

	var ierr error
	if yes {
		selectAuto(units, sub, mode, log)
	} else {
		ierr = selectInteractive(env, p, run, units, settings, mode, log)
	}
	if ierr != nil {
		log("init interrompu : " + ierr.Error() + " ; rien n'est écrit.")
		return 1, nil
	}
	var chosen []*unit
	for _, u := range units {
		if u.follow {
			chosen = append(chosen, u)
		}
	}
	if len(chosen) == 0 {
		log("aucun service retenu : rien n'est écrit.")
		return 1, nil
	}
	text, err := buildConfig(settings, chosen, env.Now())
	if err != nil {
		return 0, err
	}
	if _, err := checkResult([]byte(text)); err != nil {
		log("erreur interne : la configuration produite est invalide, rien n'est écrit : " + err.Error())
		return 1, nil
	}

	if fileThere { // --force : même chemin que add / delete (aperçu, sauvegarde, validation)
		old, err := os.ReadFile(g.config)
		if err != nil {
			return 0, err
		}
		code, err := applyEdit(env, g, p, yes, old, []byte(text), log)
		if err != nil || code != 0 || g.dryRun {
			return code, err
		}
		return finishInit(env, g, p, yes, chosen, log)
	}
	fmt.Fprintf(env.Stdout, "\n----- %s -----\n%s----- fin -----\n", g.config, text)
	if g.dryRun {
		log("[simulation] rien n'est écrit")
		return 0, nil
	}
	if !yes && !p.yes("Écrire ce fichier ?") {
		log("annulé, rien n'est écrit")
		return 1, nil
	}
	if err := writeNew(g.config, []byte(text)); err != nil {
		return 0, fmt.Errorf("écriture impossible : %w", err)
	}
	if _, err := env.Load(g.config); err != nil {
		os.Remove(g.config)
		return 0, fmt.Errorf("le fichier écrit est invalide, il a été retiré : %w", err)
	}
	log("écrit : " + g.config)
	return finishInit(env, g, p, yes, chosen, log)
}

func finishInit(env *Env, g globals, p *prompter, yes bool, chosen []*unit, log func(string)) (int, error) {
	cfg, err := env.Load(g.config)
	if err != nil {
		return 0, err
	}
	if nu := cfg.Settings.NotifyURLFile; nu != "" && !yes {
		if _, there := exists(nu); !there && p.yes("Enregistrer maintenant l'URL du webhook de notification ?") {
			if err := setWebhook(cfg, false, env, log); err != nil {
				log("webhook non enregistré : " + err.Error() + " (`docker-update set-webhook` le fait plus tard)")
			}
		}
	}
	enableTimer(env, env.NewRunner(g.dryRun, log), p, yes, log)
	auto := 0
	for _, u := range chosen {
		if u.mode == "auto" {
			auto++
		}
	}
	log(fmt.Sprintf("%d service(s) écrit(s), dont %d en auto. init n'a lancé aucune mise à jour et n'a touché à aucun conteneur.", len(chosen), auto))
	log("Suite : docker-update validate, puis docker-update check (observation seule) ; passer un service en auto se fait dans le fichier ou avec delete puis add.")
	return 0, nil
}

// writeNew écrit un fichier neuf (droits 644) de façon atomique.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".init-")
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
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// selectAuto : version non interactive (--yes). Aucun regroupement ; seuls les dépôts d'avis certains sont repris.
func selectAuto(units []*unit, sub subArgs, mode string, log func(string)) {
	include := splitList(strings.Join(sub.values["--include"], ","))
	exclude := splitList(strings.Join(sub.values["--exclude"], ","))
	if mode == "" {
		mode = "notify"
	}
	for _, u := range units {
		if len(include) > 0 {
			u.follow = matches(u, include)
		}
		if matches(u, exclude) {
			u.follow = false
		}
		if !u.follow && u.why != "" && len(include) == 0 {
			log(fmt.Sprintf("écarté : %s (%s)", u.name, u.why))
		}
		u.mode = mode
		if repo, sure := discover.SuggestRepo(u.image); sure {
			u.repo = repo
		}
	}
	if mode == "auto" {
		log("Rappel : --mode auto fait appliquer les mises à jour par l'outil lui-même (de nuit, après le délai) pour tous les services retenus.")
	}
}

func parseYN(a string, def bool) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "":
		return def, true
	case "o", "oui", "y", "yes":
		return true, true
	case "n", "non", "no":
		return false, true
	}
	return false, false
}

func askYN(p *prompter, question string, def bool) (bool, error) {
	hint := " [O/n] "
	if !def {
		hint = " [o/N] "
	}
	for try := 0; try < 3; try++ {
		a, err := p.ask(question + hint)
		if err != nil {
			return false, err
		}
		if v, ok := parseYN(a, def); ok {
			return v, nil
		}
		fmt.Fprintln(p.out, "  réponse attendue : o ou n")
	}
	return def, nil
}

// selectInteractive pose les questions : quels services, quels groupes, puis les réglages de chaque unité.
func selectInteractive(env *Env, p *prompter, run dockerx.Runner, units []*unit, settings config.Settings, modeFlag string, log func(string)) error {
	fmt.Fprintln(p.out, "\nConteneurs Compose qui tournent. Réponse par défaut entre crochets (Entrée).")
	for _, u := range units {
		fmt.Fprintf(p.out, "\n%s  (projet %s, image %s)\n", u.name, u.project, u.image)
		if u.why != "" {
			fmt.Fprintf(p.out, "  Non suivi par défaut : %s.\n", u.why)
		}
		v, err := askYN(p, "  Suivre ce service ?", u.follow)
		if err != nil {
			return err
		}
		u.follow = v
	}
	var chosen []*unit
	for _, u := range units {
		if u.follow {
			chosen = append(chosen, u)
		}
	}
	// Regroupements : services d'un même projet qui partagent la même image (mêmes réglages, mise à jour ensemble).
	byKey := map[string][]*unit{}
	var keys []string
	for _, u := range chosen {
		k := u.project + "\x00" + u.image
		if _, ok := byKey[k]; !ok {
			keys = append(keys, k)
		}
		byKey[k] = append(byKey[k], u)
	}
	taken := map[string]bool{}
	for _, u := range units {
		taken[u.name] = true
	}
	for _, k := range keys {
		members := byKey[k]
		if len(members) < 2 {
			continue
		}
		var list []string
		for _, m := range members {
			list = append(list, m.name)
		}
		fmt.Fprintf(p.out, "\n%s utilisent la même image (%s).\n", strings.Join(list, ", "), members[0].image)
		for try := 0; try < 3; try++ {
			a, err := p.ask("Les mettre à jour ensemble ? Nom du groupe (Entrée : non) : ")
			if err != nil {
				return err
			}
			if a == "" {
				break
			}
			if !nameRE.MatchString(a) || taken[a] {
				fmt.Fprintln(p.out, "  nom invalide ou déjà pris (lettres, chiffres, _ . -)")
				continue
			}
			a2, err := p.ask(fmt.Sprintf("Ordre de mise à jour (noms séparés par des virgules ; Entrée : %s) : ", strings.Join(list, ", ")))
			if err != nil {
				return err
			}
			order := members
			if a2 != "" {
				var re []*unit
				for _, n := range splitList(a2) {
					for _, m := range members {
						if m.name == n && !slices.Contains(re, m) {
							re = append(re, m)
						}
					}
				}
				if len(re) == len(members) {
					order = re
				} else {
					fmt.Fprintln(p.out, "  ordre non reconnu : ordre proposé conservé")
				}
			}
			taken[a] = true
			for _, m := range order {
				m.group = a
			}
			reorder(units, order) // l'ordre du fichier est l'ordre de mise à jour
			break
		}
	}
	// Réglages par unité (un groupe se règle une fois, pour tous ses membres).
	done := map[string]bool{}
	for _, u := range units {
		if !u.follow {
			continue
		}
		label := u.name
		members := []*unit{u}
		if u.group != "" {
			if done[u.group] {
				continue
			}
			done[u.group] = true
			label = "groupe " + u.group
			members = nil
			for _, m := range units {
				if m.follow && m.group == u.group {
					members = append(members, m)
				}
			}
		}
		fmt.Fprintf(p.out, "\n── %s ──\n", label)
		mode := modeFlag
		if mode == "" {
			for try := 0; try < 3; try++ {
				a, err := p.ask("  Mode : notify (prévient seulement), manual, auto (applique seul, de nuit) [notify] : ")
				if err != nil {
					return err
				}
				if a == "" {
					a = "notify"
				}
				if slices.Contains(config.Modes, a) {
					mode = a
					break
				}
				fmt.Fprintln(p.out, "  mode inconnu")
			}
			if mode == "" {
				mode = "notify"
			}
		}
		if mode == "auto" {
			a, err := p.ask("  « auto » : l'outil appliquera lui-même les mises à jour après le délai. Tapez auto pour confirmer : ")
			if err != nil {
				return err
			}
			if a != "auto" {
				fmt.Fprintln(p.out, "  non confirmé : notify")
				mode = "notify"
			}
		}
		cleanup, err := askYN(p, "  Supprimer l'image précédente après une mise à jour réussie ?", true)
		if err != nil {
			return err
		}
		repo := ""
		suggestion, sure := discover.SuggestRepo(u.image)
		hint := "Entrée : aucun"
		if suggestion != "" {
			hint = "Entrée : " + suggestion
			if !sure {
				hint += " (à confirmer)"
			}
		}
		a, err := p.ask(fmt.Sprintf("  Dépôt GitHub des avis de sécurité (propriétaire/nom ; %s ; - : aucun) : ", hint))
		if err != nil {
			return err
		}
		switch {
		case a == "-":
		case a == "":
			repo = suggestion
		default:
			repo = a
		}
		notes := false
		if repo != "" {
			if v := discover.Version(run, u.image, settings.VersionLabel); v != "" {
				notes, err = askYN(p, fmt.Sprintf("  Surveiller les notes de release de %s (version lue : %s ; alertes seulement) ?", repo, v), false)
				if err != nil {
					return err
				}
			} else {
				fmt.Fprintln(p.out, "  (notes de release non proposées : l'image ne porte pas de version lisible)")
			}
		}
		for i, m := range members {
			m.mode, m.cleanup, m.repo = mode, cleanup, repo
			m.notes = notes && i == 0
		}
	}
	return nil
}

// reorder place les membres d'un groupe, dans l'ordre voulu, à la position du premier d'entre eux.
func reorder(units []*unit, order []*unit) {
	var slots []int
	for i, u := range units {
		if slices.Contains(order, u) {
			slots = append(slots, i)
		}
	}
	for i, s := range slots {
		units[s] = order[i]
	}
}

// buildConfig écrit le fichier : [settings], puis un bloc par service ; les membres d'un groupe se suivent.
func buildConfig(s config.Settings, chosen []*unit, now time.Time) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, `# Configuration de docker-update, écrite par `+"`docker-update init`"+` le %s.
# Propriétaire root:root, mode 644 : l'outil refuse un fichier modifiable par d'autres comptes.
# À éditer à la main, ou avec `+"`docker-update add` / `delete` / `list`"+`.

[settings]
state_file        = %s
lock_file         = %s
registry_tool     = %s
notify_url_file   = %s   # à remplir avec `+"`docker-update set-webhook`"+` (saisie masquée)
notify_format     = %s
default_cooldown  = "3d"          # une image doit rester identique ce temps avant d'être appliquée
window            = "02:00-05:00"  # on n'applique que la nuit ; "" = à toute heure
cleanup           = true
settle_seconds    = %d
health_timeout    = %d
`, now.Format("2006-01-02"), q(s.StateFile), q(s.LockFile), q(s.RegistryTool), q(s.NotifyURLFile), q(s.NotifyFormat), s.SettleSeconds, s.HealthTimeout)

	emitted := map[*unit]bool{}
	var order []*unit
	for _, u := range chosen {
		if emitted[u] {
			continue
		}
		for _, m := range chosen {
			if !emitted[m] && (m == u || (u.group != "" && m.group == u.group)) {
				emitted[m] = true
				order = append(order, m)
			}
		}
	}
	for _, u := range order {
		entries := []tomledit.Entry{{Key: "name", Val: tomledit.String(u.name)}, {Key: "compose_dir", Val: tomledit.String(u.dir)},
			{Key: "service", Val: tomledit.String(u.service)}}
		if len(u.files) > 0 {
			entries = append(entries, tomledit.Entry{Key: "compose_files", Val: tomledit.Strings(u.files)})
		}
		entries = append(entries, tomledit.Entry{Key: "mode", Val: tomledit.String(u.mode)})
		if u.group != "" {
			entries = append(entries, tomledit.Entry{Key: "group", Val: tomledit.String(u.group)})
		}
		if !u.cleanup {
			entries = append(entries, tomledit.Entry{Key: "cleanup", Val: tomledit.Bool(false)})
		}
		if u.repo != "" {
			entries = append(entries, tomledit.Entry{Key: "advisories", Val: tomledit.Strings([]string{u.repo})})
		}
		if u.notes {
			entries = append(entries, tomledit.Entry{Key: "release_notes", Val: tomledit.Bool(true)})
		}
		block, err := tomledit.Render(entries)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n# image : %s\n%s\n", strings.ReplaceAll(strings.ReplaceAll(u.image, "\n", " "), "\r", " "), block)
	}
	return b.String(), nil
}

func q(s string) string { return tomledit.Quote(s) }
