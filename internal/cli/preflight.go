package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// Gravité d'un contrôle d'installation.
const (
	blocking = "bloquant"
	advisory = "avertissement"
)

// check est le résultat d'un contrôle d'installation (en lecture seule).
type check struct {
	label  string
	level  string // blocking ou advisory : ce que vaut un échec
	ok     bool
	detail string
	fix    string // commande qui corrige, si elle existe
}

// checker regroupe ce que les contrôles lisent : tout peut être remplacé dans les tests.
type checker struct {
	run      dockerx.Runner
	euid     int
	cfgPath  string
	settings config.Settings
	unitDir  string
	binary   string
	getenv   func(string) string
	dial     func(addr string) error
	trusted  func(path string) error
}

func exists(path string) (os.FileInfo, bool) {
	fi, err := os.Stat(path)
	return fi, err == nil
}

// writableDir dit si on peut créer des fichiers dans dir, ou dans son plus proche ancêtre existant si dir n'existe pas.
func writableDir(dir string) (writable, present bool) {
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil {
			return fi.IsDir() && syscall.Access(d, 2) == nil, d == dir
		}
		if filepath.Dir(d) == d {
			return false, false
		}
	}
}

func shortErr(r dockerx.Result) string { return textutil.Truncate(textutil.Strip(r.Err), 120) }

var execLine = regexp.MustCompile(`(?m)^\s*ExecStart=\S*docker-update\b[^\n]*$`)

func (c *checker) all(onlyBlocking bool) []check {
	var out []check
	add := func(k check) { out = append(out, k) }
	rt := func(argv ...string) dockerx.Result { return c.run.Run(argv, false, 30*time.Second) }

	// Bloquants : sans eux, rien ne peut tourner.
	if r := rt("docker", "version", "--format", "{{.Server.Version}}"); r.RC == 0 {
		add(check{"Docker", blocking, true, "démon joignable, version " + textutil.CleanText(strings.TrimSpace(r.Out), 30), ""})
	} else {
		add(check{"Docker", blocking, false, "docker absent ou démon injoignable : " + shortErr(r),
			"installer Docker, démarrer le démon (systemctl start docker), lancer en root ou dans le groupe docker"})
	}
	if r := rt("docker", "compose", "version", "--short"); r.RC == 0 {
		add(check{"Docker Compose (plugin)", blocking, true, "version " + textutil.CleanText(strings.TrimSpace(r.Out), 30), ""})
	} else {
		add(check{"Docker Compose (plugin)", blocking, false, "« docker compose » indisponible : " + shortErr(r),
			"installer le plugin docker-compose-plugin"})
	}
	if c.settings.RegistryTool == "regctl" {
		if r := rt("regctl", "version"); r.RC == 0 {
			add(check{"regctl", blocking, true, "présent (registry_tool = regctl)", ""})
		} else {
			add(check{"regctl", blocking, false, "regctl introuvable alors que registry_tool = regctl", "installer regctl, ou choisir registry_tool = \"buildx\""})
		}
	} else if r := rt("docker", "buildx", "version"); r.RC == 0 {
		add(check{"docker buildx", blocking, true, "présent (registry_tool = buildx)", ""})
	} else {
		add(check{"docker buildx", blocking, false, "« docker buildx » indisponible : " + shortErr(r), "installer docker-buildx-plugin, ou utiliser regctl"})
	}
	cfgDir := filepath.Dir(c.cfgPath)
	if ok, there := writableDir(cfgDir); ok {
		d := cfgDir
		if !there {
			d += " (sera créé)"
		}
		add(check{"Dossier de configuration", blocking, true, d, ""})
	} else {
		add(check{"Dossier de configuration", blocking, false, cfgDir + " n'est pas accessible en écriture", "sudo install -d -m 755 -o root -g root " + cfgDir})
	}
	if onlyBlocking {
		return out
	}

	// Avertissements : l'outil peut être utilisé, mais l'installation n'est pas terminée ou pas sûre.
	if c.euid == 0 {
		add(check{"Compte", advisory, true, "exécuté en root", ""})
	} else {
		add(check{"Compte", advisory, false, "pas root : l'outil pilote Docker et lit /etc, il doit tourner en root (comme l'unité systemd)", "sudo docker-update …"})
	}
	if fi, ok := exists(c.binary); !ok || !fi.Mode().IsRegular() {
		add(check{"Binaire", advisory, false, "introuvable : " + c.binary, "sudo install -m 755 -o root -g root docker-update /usr/local/sbin/docker-update"})
	} else {
		bad := fi.Mode().Perm()&0o022 != 0 || fi.Mode().Perm()&0o111 == 0
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && os.Geteuid() == 0 {
			bad = true
		}
		if bad {
			add(check{"Binaire", advisory, false, c.binary + " : droits ou propriétaire à corriger (root, exécutable, non modifiable par d'autres)", "sudo chown root:root " + c.binary + " && sudo chmod 755 " + c.binary})
		} else {
			add(check{"Binaire", advisory, true, c.binary, ""})
		}
	}
	if _, ok := exists(c.cfgPath); ok && c.trusted != nil {
		if err := c.trusted(c.cfgPath); err != nil {
			add(check{"Fichier de configuration", advisory, false, err.Error(), "sudo chown root:root " + c.cfgPath + " && sudo chmod 644 " + c.cfgPath})
		} else {
			add(check{"Fichier de configuration", advisory, true, c.cfgPath + " (propriétaire et droits sûrs)", ""})
		}
	}
	{
		dir := filepath.Dir(c.settings.StateFile)
		if ok, there := writableDir(dir); ok {
			d := dir
			if !there {
				d += " (sera créé)"
			}
			add(check{"Dossier d'état", advisory, true, d, ""})
		} else {
			add(check{"Dossier d'état", advisory, false, dir + " n'est pas accessible en écriture", "sudo install -d -m 700 -o root -g root " + dir})
		}
	}
	{
		dir := filepath.Dir(c.settings.LockFile)
		if fi, ok := exists(dir); ok && fi.IsDir() && syscall.Access(dir, 2) == nil {
			add(check{"Dossier du verrou", advisory, true, dir, ""})
		} else {
			add(check{"Dossier du verrou", advisory, false, dir + " n'existe pas ou n'est pas accessible en écriture (créé par systemd avec RuntimeDirectory ; à la main : à créer)",
				"sudo install -d -m 755 -o root -g root " + dir})
		}
	}
	if dc := c.getenv("DOCKER_CONFIG"); dc != "" {
		f := filepath.Join(dc, "config.json")
		switch fi, ok := exists(f); {
		case !ok:
			add(check{"Identifiants du registre", advisory, false, f + " absent (utile seulement pour un registre privé)", "DOCKER_CONFIG=" + dc + " docker login <registre> --password-stdin"})
		case fi.Mode().Perm()&0o077 != 0:
			add(check{"Identifiants du registre", advisory, false, f + " lisible par d'autres comptes", "sudo chmod 600 " + f})
		default:
			add(check{"Identifiants du registre", advisory, true, f + " (droits 600)", ""})
		}
	}
	secret := func(label, path, fix string) {
		if path == "" {
			return
		}
		fi, ok := exists(path)
		switch {
		case !ok:
			add(check{label, advisory, false, path + " absent", fix})
		case fi.Mode().Perm()&0o077 != 0:
			add(check{label, advisory, false, path + " lisible par d'autres comptes", "sudo chmod 600 " + path})
		default:
			add(check{label, advisory, true, path + " (droits 600)", ""})
		}
	}
	if c.settings.NotifyURLFile == "" {
		add(check{"Webhook de notification", advisory, false, "notify_url_file non défini : aucune notification", "ajouter notify_url_file puis docker-update set-webhook"})
	} else {
		secret("Webhook de notification", c.settings.NotifyURLFile, "docker-update --config "+c.cfgPath+" set-webhook")
	}
	secret("Jeton GitHub", c.settings.GithubTokenFile, "créer le fichier (jeton sans aucun droit), chmod 600")

	svc, tim := filepath.Join(c.unitDir, "docker-update.service"), filepath.Join(c.unitDir, "docker-update.timer")
	_, hasSvc := exists(svc)
	_, hasTim := exists(tim)
	if !hasSvc || !hasTim {
		add(check{"Unités systemd", advisory, false, "docker-update.service ou .timer absent de " + c.unitDir,
			"sudo install -m 644 -o root -g root deploy/docker-update.service deploy/docker-update.timer " + c.unitDir + "/ && sudo systemctl daemon-reload"})
	} else {
		mode := "inconnu"
		if b, err := os.ReadFile(svc); err == nil {
			if line := execLine.FindString(string(b)); line != "" {
				fields := strings.Fields(line)
				mode = fields[len(fields)-1]
			}
		}
		add(check{"Unités systemd", advisory, true, "service et minuteur installés ; le service lance : " + textutil.CleanText(mode, 30) +
			" (check n'applique rien ; run applique le mode auto)", ""})
		en := rt("systemctl", "is-enabled", "docker-update.timer")
		ac := rt("systemctl", "is-active", "docker-update.timer")
		if en.RC == 0 && ac.RC == 0 {
			add(check{"Minuteur", advisory, true, "activé et en marche", ""})
		} else {
			add(check{"Minuteur", advisory, false, "docker-update.timer n'est pas activé ou pas démarré", "sudo systemctl enable --now docker-update.timer"})
		}
	}
	for _, host := range []string{"api.github.com:443", "registry-1.docker.io:443"} {
		if err := c.dial(host); err != nil {
			add(check{"Réseau " + host, advisory, false, "injoignable : " + textutil.CleanText(err.Error(), 100), ""})
		} else {
			add(check{"Réseau " + host, advisory, true, "joignable", ""})
		}
	}
	return out
}

// printChecks écrit les résultats ; renvoie (bloquants en échec, avertissements en échec).
func printChecks(checks []check, log func(string)) (int, int) {
	var blocked, warned int
	for _, k := range checks {
		mark := "✓"
		if !k.ok {
			if k.level == blocking {
				mark = "✗"
				blocked++
			} else {
				mark = "⚠"
				warned++
			}
		}
		log(fmt.Sprintf("%s %s — %s", mark, k.label, k.detail))
		if !k.ok && k.fix != "" {
			log("    → correction : " + k.fix)
		}
	}
	return blocked, warned
}
