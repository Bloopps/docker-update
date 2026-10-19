package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/Bloopps/docker-update/deploy"
)

var unitMarker = regexp.MustCompile(`(?m)^# docker-update-unit-version: (\d+)[ \t]*\n?`)

// unitChange est une unité installée dont la version est plus ancienne que celle fournie par ce binaire.
type unitChange struct {
	name, path string
	old, new   string
}

func withoutMarker(s string) string { return unitMarker.ReplaceAllString(s, "") }

func unitVersion(s string) int {
	m := unitMarker.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// outdatedUnits liste les unités installées qui ne sont pas à la version fournie. Une unité absente n'est pas listée (c'est
// `init` qui la pose). Une unité sans marqueur mais identique à la fournie (hors marqueur) est à jour ; une unité qui porte déjà
// le marqueur courant est à jour même si l'utilisateur l'a adaptée.
func outdatedUnits(env *Env, cfgPath string) []unitChange {
	var out []unitChange
	for _, u := range []struct{ name, body string }{{"docker-update.service", deploy.Service}, {"docker-update.timer", deploy.Timer}} {
		path := filepath.Join(env.UnitDir, u.name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		cur := string(b)
		want := renderUnit(env, cfgPath, u.body)
		if unitVersion(cur) >= deploy.UnitVersion || withoutMarker(cur) == withoutMarker(want) {
			continue
		}
		out = append(out, unitChange{name: u.name, path: path, old: cur, new: want})
	}
	return out
}

// migrateUnits propose, unité par unité, de remplacer l'unité installée par la version fournie : aperçu des différences,
// confirmation, sauvegarde de l'ancienne, écriture atomique, puis `systemctl daemon-reload`. Renvoie le code de sortie.
func migrateUnits(env *Env, g globals, p *prompter, yes bool, changes []unitChange, log func(string)) (int, error) {
	changed := false
	for _, c := range changes {
		log(fmt.Sprintf("unité systemd %s : version plus ancienne que celle de cet outil (%d)", c.path, deploy.UnitVersion))
		fmt.Fprint(env.Stdout, unifiedDiff(c.path, c.old, c.new))
		if g.dryRun {
			log("[simulation] rien n'est écrit")
			continue
		}
		if !yes && !p.yes("Remplacer cette unité (vos adaptations seraient perdues ; une sauvegarde est faite) ?") {
			log("unité laissée telle quelle : " + c.path)
			continue
		}
		info, err := os.Stat(c.path)
		if err != nil {
			return 0, err
		}
		backup := c.path + ".bak-" + env.Now().Format("20060102-150405")
		for i := 2; ; i++ {
			if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
				break
			}
			backup = fmt.Sprintf("%s.bak-%s-%d", c.path, env.Now().Format("20060102-150405"), i)
		}
		if err := writeFileAtomic(backup, []byte(c.old), info.Mode().Perm()); err != nil {
			return 0, fmt.Errorf("sauvegarde impossible, rien n'est écrit : %w", err)
		}
		if err := writeFileAtomic(c.path, []byte(c.new), info.Mode().Perm()); err != nil {
			return 0, fmt.Errorf("écriture impossible (l'ancienne unité est intacte) : %w", err)
		}
		log(fmt.Sprintf("✓ %s remplacée (sauvegarde : %s)", c.path, backup))
		changed = true
	}
	if changed {
		run := env.NewRunner(false, log)
		if r := run.Run([]string{"systemctl", "daemon-reload"}, true, 60*time.Second); r.RC != 0 {
			log("systemctl daemon-reload a échoué (" + shortErr(r) + ") : lancer `sudo systemctl daemon-reload`")
			return 1, nil
		}
		log("✓ systemctl daemon-reload (le minuteur garde son état ; rien d'autre n'est redémarré)")
	}
	return 0, nil
}
