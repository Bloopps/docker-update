package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// runEditor ouvre un fichier dans l'éditeur de l'utilisateur ($VISUAL, puis $EDITOR, puis vi), branché sur son terminal.
// La variable est découpée en mots (« code -w ») ; aucun shell n'est lancé.
func (e *Env) runEditor(path string) error {
	spec := e.Getenv("VISUAL")
	if strings.TrimSpace(spec) == "" {
		spec = e.Getenv("EDITOR")
	}
	argv := strings.Fields(spec)
	if len(argv) == 0 {
		argv = []string{"vi"}
	}
	cmd := exec.Command(argv[0], append(argv[1:], path)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, e.Stdout, e.Stderr
	return cmd.Run()
}

// editCmd ouvre une copie de la configuration dans l'éditeur, puis applique le résultat comme add / delete : validation,
// aperçu, confirmation, sauvegarde, écriture atomique, relecture. Si le fichier a changé pendant l'édition, rien n'est écrit.
func editCmd(env *Env, g globals, sub subArgs, cfg *config.Config, log func(string)) (int, error) {
	orig, err := os.ReadFile(g.config)
	if err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp("", "docker-update-edit-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "updates.toml")
	if err := os.WriteFile(tmp, orig, 0o600); err != nil {
		return 0, err
	}
	p := newPrompter(env)
	var edited []byte
	for {
		if err := env.Edit(tmp); err != nil {
			log("éditeur terminé en erreur, rien n'est écrit : " + err.Error())
			return 1, nil
		}
		if edited, err = os.ReadFile(tmp); err != nil {
			return 0, err
		}
		if _, cerr := checkResult(edited); cerr == nil {
			break
		} else {
			log("le fichier édité est invalide : " + cerr.Error())
			if !p.yes("Rouvrir l'éditeur ?") {
				log("annulé, rien n'est écrit")
				return 1, nil
			}
		}
	}
	return withConfigLock(cfg, g, log, func(now []byte) (int, error) {
		if string(now) != string(orig) {
			log("refusé : la configuration a changé pendant l'édition, rien n'est écrit (relancez edit)")
			return 1, nil
		}
		return applyEdit(env, g, p, sub.flags["--yes"], now, edited, log)
	})
}

type exportSettings struct {
	StateFile        string `json:"state_file"`
	LockFile         string `json:"lock_file"`
	RegistryTool     string `json:"registry_tool"`
	NotifyURLFile    string `json:"notify_url_file"`
	NotifyFormat     string `json:"notify_format"`
	DefaultCooldown  int64  `json:"default_cooldown_seconds"`
	SettleSeconds    int    `json:"settle_seconds"`
	HealthTimeout    int    `json:"health_timeout_seconds"`
	Window           string `json:"window"`
	GithubAPI        string `json:"github_api"`
	GithubTokenFile  string `json:"github_token_file"`
	AdvisoryPoll     int64  `json:"advisory_poll_seconds"`
	AdvisoryLookback int64  `json:"advisory_lookback_seconds"`
	VersionLabel     string `json:"version_label"`
	Cleanup          bool   `json:"cleanup"`
}

type exportService struct {
	Name               string   `json:"name"`
	ComposeDir         string   `json:"compose_dir"`
	Service            string   `json:"service"`
	ComposeFiles       []string `json:"compose_files"`
	Mode               string   `json:"mode"`
	CooldownSeconds    int64    `json:"cooldown_seconds"`
	Group              string   `json:"group"`
	Unit               string   `json:"unit"`
	HealthTimeout      int      `json:"health_timeout_seconds"`
	Window             string   `json:"window"`
	SmokeURL           string   `json:"smoke_url"`
	SmokeExpect        []int    `json:"smoke_expect"`
	Advisories         []string `json:"advisories"`
	MinSeverity        string   `json:"min_severity"`
	SecurityCooldown   int64    `json:"security_cooldown_seconds"`
	SecurityPolicy     string   `json:"security_policy"`
	Cleanup            bool     `json:"cleanup"`
	ReleaseNotes       bool     `json:"release_notes"`
	ReleaseNotesRepos  []string `json:"release_notes_repos"`
	ReleaseNotesIgnore []string `json:"release_notes_ignore"`
}

// exportCmd écrit sur la sortie standard la configuration effective (défauts appliqués) en JSON, pour des scripts.
// Lecture seule ; ne contient que des chemins de fichiers de secrets, jamais leur contenu.
func exportCmd(env *Env, cfg *config.Config, log func(string)) (int, error) {
	s := cfg.Settings
	out := struct {
		Settings exportSettings  `json:"settings"`
		Services []exportService `json:"services"`
	}{
		Settings: exportSettings{s.StateFile, s.LockFile, s.RegistryTool, s.NotifyURLFile, s.NotifyFormat, s.DefaultCooldown,
			s.SettleSeconds, s.HealthTimeout, s.Window, s.GithubAPI, s.GithubTokenFile, s.AdvisoryPoll, s.AdvisoryLookback,
			s.VersionLabel, s.Cleanup},
		Services: []exportService{},
	}
	for _, v := range cfg.Services {
		out.Services = append(out.Services, exportService{v.Name, v.ComposeDir, v.Service, nz(v.ComposeFiles), v.Mode, v.Cooldown,
			v.Group, v.Unit(), v.HealthTimeout, winSpec(v.Window), v.SmokeURL, nzi(v.SmokeExpect), nz(v.Advisories), v.MinSeverity,
			v.SecurityCooldown, v.SecurityPolicy, v.Cleanup, v.ReleaseNotes, nz(v.ReleaseNotesRepos), nz(v.ReleaseNotesIgnore)})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(env.Stdout, string(b))
	return 0, nil
}

func winSpec(w textutil.Window) string {
	if !w.Set {
		return ""
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
}

// nz et nzi donnent « [] » plutôt que « null » dans le JSON.
func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func nzi(l []int) []int {
	if l == nil {
		return []int{}
	}
	return l
}
