package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/deploy"
	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
)

// installStep est une étape d'installation que `init` sait faire lui-même.
type installStep struct {
	what string
	do   func() error
}

func sameContent(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	ia, _ := fa.Stat()
	ib, _ := fb.Stat()
	if ia.Size() != ib.Size() {
		return false
	}
	x, _ := io.ReadAll(fa)
	y, _ := io.ReadAll(fb)
	return bytes.Equal(x, y)
}

// installPlan liste ce qui manque : dossiers, binaire à sa place, unités systemd. Rien n'est modifié ici.
func installPlan(env *Env, settings config.Settings, cfgPath string) (steps []installStep, reload bool) {
	stateDir := filepath.Dir(settings.StateFile)
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Dir(cfgPath), 0o755}, {stateDir, 0o700}, {filepath.Join(stateDir, "docker"), 0o700}, {filepath.Dir(settings.LockFile), 0o755},
	} {
		d := d
		if fi, ok := exists(d.path); ok && fi.IsDir() {
			continue
		}
		steps = append(steps, installStep{fmt.Sprintf("créer le dossier %s (droits %04o)", d.path, d.mode), func() error {
			if err := os.MkdirAll(d.path, d.mode); err != nil {
				return err
			}
			return os.Chmod(d.path, d.mode)
		}})
	}
	if self, target := env.Binary, env.InstallBin; self != "" {
		a, errA := os.Stat(self)
		b, errB := os.Stat(target)
		if errA == nil && (errB != nil || (!os.SameFile(a, b) && !sameContent(self, target))) {
			steps = append(steps, installStep{fmt.Sprintf("copier le binaire en %s (droits 755)", target), func() error { return copyExecutable(self, target) }})
		}
	}
	for _, u := range []struct{ name, body string }{{"docker-update.service", deploy.Service}, {"docker-update.timer", deploy.Timer}} {
		u := u
		path := filepath.Join(env.UnitDir, u.name)
		if _, ok := exists(path); ok {
			continue // une unité déjà là peut avoir été adaptée : jamais écrasée
		}
		reload = true
		steps = append(steps, installStep{fmt.Sprintf("écrire l'unité systemd %s", path), func() error {
			if err := os.MkdirAll(env.UnitDir, 0o755); err != nil {
				return err
			}
			// l'unité fournie vise les chemins par défaut : on y met ceux de cette installation
			body := strings.NewReplacer("/usr/local/sbin/docker-update", env.InstallBin, "/etc/docker-update/updates.toml", cfgPath).Replace(u.body)
			return writeFileAtomic(path, []byte(body), 0o644)
		}})
	}
	return steps, reload
}

// writeFileAtomic écrit un fichier de façon atomique avec les droits donnés.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".install-")
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
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func copyExecutable(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(to, data, 0o755)
}

// runInstall exécute les étapes, puis recharge systemd si des unités ont été écrites. Renvoie la première erreur.
func runInstall(run dockerx.Runner, steps []installStep, reload bool, log func(string)) error {
	for _, s := range steps {
		if err := s.do(); err != nil {
			return fmt.Errorf("%s : %v", s.what, err)
		}
		log("✓ " + s.what)
	}
	if reload {
		if r := run.Run([]string{"systemctl", "daemon-reload"}, true, 60*time.Second); r.RC != 0 {
			return fmt.Errorf("systemctl daemon-reload a échoué : %s", shortErr(r))
		}
		log("✓ systemctl daemon-reload")
	}
	return nil
}

// enableTimer active le minuteur s'il ne l'est pas déjà (le service fourni ne fait que `check` : aucune mise à jour).
func enableTimer(env *Env, run dockerx.Runner, p *prompter, yes bool, log func(string)) {
	if _, ok := exists(filepath.Join(env.UnitDir, "docker-update.timer")); !ok {
		return
	}
	en := run.Run([]string{"systemctl", "is-enabled", "docker-update.timer"}, false, 30*time.Second)
	ac := run.Run([]string{"systemctl", "is-active", "docker-update.timer"}, false, 30*time.Second)
	if en.RC == 0 && ac.RC == 0 {
		return
	}
	if !yes && !p.yes("Activer le minuteur (un passage par heure, observation seule : rien n'est appliqué) ?") {
		log("minuteur non activé : sudo systemctl enable --now docker-update.timer")
		return
	}
	if r := run.Run([]string{"systemctl", "enable", "--now", "docker-update.timer"}, true, 60*time.Second); r.RC != 0 {
		log("minuteur non activé (" + shortErr(r) + ") : sudo systemctl enable --now docker-update.timer")
		return
	}
	log("✓ minuteur activé : un passage par heure, en observation seule (voir docs/AGENT.md pour passer à « run »)")
}
