// Package cli : sous-commandes, verrou, contrôle de la configuration et codes de sortie.
//
// Codes de sortie : 0 réussi, 1 échec (mise à jour, erreur d'exécution), 2 erreur de configuration ou de ligne de commande.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/notify"
	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/updater"
)

// Engine est ce que la ligne de commande demande au cœur (l'Updater, ou un double dans les tests).
type Engine interface {
	Cycle(apply bool, names []string, notify bool) (int, error)
	NowCmd(names []string, yes bool) (int, error)
	Hold(names []string, on bool) (int, error)
	Status()
}

// Env regroupe ce qui touche au monde extérieur : les tests le remplacent.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Version est la version affichée par `version`.
	Version string
	// CheckTrusted vérifie le fichier de configuration (défaut : config.CheckTrustedFile).
	CheckTrusted func(path string) error
	// IsTerminal dit si l'entrée standard est un terminal (défaut : ioctl TCGETS).
	IsTerminal func(io.Reader) bool
	// ReadHidden lit une ligne sans l'afficher (défaut : terminal sans écho).
	ReadHidden func(prompt string) (string, error)
	// Load relit la configuration écrite par add / delete (défaut : config.Load).
	Load func(path string) (*config.Config, error)
	// Now donne l'heure (nom des sauvegardes) ; défaut : time.Now.
	Now func() time.Time
	// NewEngine construit le cœur (défaut : updater.New).
	NewEngine func(cfg *config.Config, run dockerx.Runner, n *notify.Notifier, st *state.State, log func(string), noSave bool) Engine
}

// DefaultEnv est l'environnement réel.
func DefaultEnv(version string) *Env {
	e := &Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Version: version}
	e.fill()
	return e
}

func (e *Env) fill() {
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.CheckTrusted == nil {
		e.CheckTrusted = config.CheckTrustedFile
	}
	if e.IsTerminal == nil {
		e.IsTerminal = isTerminal
	}
	if e.ReadHidden == nil {
		e.ReadHidden = func(prompt string) (string, error) {
			f, ok := e.Stdin.(*os.File)
			if !ok {
				return "", errors.New("pas de terminal")
			}
			return readHidden(f, e.Stderr, prompt)
		}
	}
	if e.Load == nil {
		e.Load = config.Load
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.NewEngine == nil {
		e.NewEngine = func(cfg *config.Config, run dockerx.Runner, n *notify.Notifier, st *state.State, log func(string), noSave bool) Engine {
			return updater.New(updater.Deps{Cfg: cfg, Run: run, Notifier: n, State: st, Log: log, NoSave: noSave})
		}
	}
}

// Main exécute une commande et renvoie le code de sortie.
func Main(args []string, env *Env) int {
	env.fill()
	log := func(msg string) { fmt.Fprintln(env.Stdout, msg) }

	g, cmd, rest, err := parseGlobals(args)
	if err == nil && !g.help && !g.version && cmd == "" {
		err = usagef("une commande est requise")
	}
	if err == nil && !g.help && !g.version {
		if _, ok := subcommands[cmd]; !ok {
			err = usagef("commande inconnue : %s (commandes : %s)", cmd, strings.Join(commandOrder, ", "))
		}
	}
	var sub subArgs
	if err == nil && !g.help && !g.version {
		sub, err = parseSub(cmd, rest)
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "docker-update : %v\n(docker-update -h pour l'aide)\n", err)
		return 2
	}
	if g.help {
		fmt.Fprint(env.Stdout, helpText)
		return 0
	}
	if g.version || cmd == "version" {
		fmt.Fprintf(env.Stdout, "docker-update %s\n", env.Version)
		return 0
	}

	code, err := execute(g, cmd, sub, env, log)
	switch e := err.(type) {
	case nil:
		return code
	case *usageError:
		fmt.Fprintf(env.Stderr, "docker-update : %v\n(docker-update -h pour l'aide)\n", e)
		return 2
	case *config.Error:
		log("configuration : " + e.Msg)
		return 2
	case *state.Error:
		log("erreur : " + e.Msg)
		return 1
	default:
		log("erreur : " + err.Error())
		return 1
	}
}

func execute(g globals, cmd string, sub subArgs, env *Env, log func(string)) (int, error) {
	if !g.allowUntrusted {
		if err := env.CheckTrusted(g.config); err != nil {
			return 0, err
		}
	}
	cfg, err := config.Load(g.config)
	if err != nil {
		return 0, err
	}
	switch cmd {
	case "validate":
		return validate(cfg, log), nil
	case "set-webhook":
		return 0, setWebhook(cfg, g.dryRun, env, log)
	case "list":
		st, err := state.Load(cfg.Settings.StateFile)
		if err != nil {
			return 0, err
		}
		list(cfg, st, log)
		return 0, nil
	case "add":
		return addCmd(env, g, sub, cfg, log)
	case "delete":
		return deleteCmd(env, g, sub, cfg, log)
	}
	unlock, err := lock(cfg.Settings.LockFile)
	if err != nil {
		if errors.Is(err, errBusy) {
			log("un autre passage est en cours, rien à faire")
			return 0, nil
		}
		return 0, err
	}
	defer unlock()

	runner := &dockerx.Exec{Dry: g.dryRun, Log: log}
	notifier := notify.New(cfg.Settings.NotifyURLFile, cfg.Settings.NotifyFormat, log, g.dryRun)
	st, err := state.Load(cfg.Settings.StateFile)
	if err != nil {
		return 0, err
	}
	engine := env.NewEngine(cfg, runner, notifier, st, log, g.dryRun)
	switch cmd {
	case "check":
		return engine.Cycle(false, sub.names, sub.flags["--notify"])
	case "run":
		return engine.Cycle(true, sub.names, false)
	case "now":
		return engine.NowCmd(sub.names, sub.flags["--yes"])
	case "notify-test":
		if notifier.URL() == "" {
			log("aucune URL de notification lisible (notify_url_file)")
			return 1, nil
		}
		notifier.Send("docker-update : test", "Si vous lisez ceci, les notifications fonctionnent.", "info")
		log("message de test envoyé (ou signalé ci-dessus en cas d'échec)")
		return 0, nil
	case "hold", "unhold":
		return engine.Hold(sub.names, cmd == "hold")
	}
	engine.Status()
	return 0, nil
}

var errBusy = errors.New("verrou pris")

// lock prend le verrou exclusif (non bloquant) : un seul passage à la fois. Le verrou est relâché à la fin du processus.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, fmt.Errorf("verrou illisible : %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errBusy
	}
	return func() { f.Close() }, nil
}

// validate vérifie la configuration et les outils, sans rien toucher.
func validate(cfg *config.Config, log func(string)) int {
	problems := 0
	tools := []string{"docker"}
	if cfg.Settings.RegistryTool == "regctl" {
		tools = append(tools, "regctl")
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t); err != nil {
			log(fmt.Sprintf("ERREUR : %s introuvable", t))
			problems++
		}
	}
	units := map[string]bool{}
	for _, s := range cfg.Services {
		units[s.Unit()] = true
		if info, err := os.Stat(s.ComposeDir); err != nil || !info.IsDir() {
			log(fmt.Sprintf("ERREUR : %s: dossier compose introuvable : %s", s.Name, s.ComposeDir))
			problems++
		}
		for _, f := range s.ComposeFiles {
			if info, err := os.Stat(f); err != nil || !info.Mode().IsRegular() {
				log(fmt.Sprintf("ERREUR : %s: fichier compose introuvable : %s", s.Name, f))
				problems++
			}
		}
	}
	if nu := cfg.Settings.NotifyURLFile; nu != "" {
		info, err := os.Stat(nu)
		switch {
		case err != nil || !info.Mode().IsRegular():
			log(fmt.Sprintf("AVERTISSEMENT : fichier d'URL de notification absent : %s (aucune notification)", nu))
		case info.Mode().Perm()&0o077 != 0:
			log(fmt.Sprintf("AVERTISSEMENT : %s est lisible par d'autres comptes (chmod 600)", nu))
		}
	}
	verdict := "OK"
	if problems > 0 {
		verdict = fmt.Sprintf("%d problème(s)", problems)
	}
	log(fmt.Sprintf("%d service(s), %d unité(s) : %s", len(cfg.Services), len(units), verdict))
	if problems > 0 {
		return 1
	}
	return 0
}

// setWebhook enregistre l'URL du webhook : saisie masquée au clavier, ou première ligne de l'entrée standard.
func setWebhook(cfg *config.Config, dryRun bool, env *Env, log func(string)) error {
	read := func() (string, error) {
		if env.IsTerminal(env.Stdin) {
			return env.ReadHidden("URL du webhook (la saisie reste invisible) : ")
		}
		line, err := bufio.NewReader(env.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return line, nil
	}
	return notify.SetWebhook(cfg.Settings.NotifyURLFile, cfg.Settings.NotifyFormat, dryRun, read, log)
}
