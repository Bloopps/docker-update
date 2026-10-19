package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
)

// fakeDocker répond aux commandes de lecture par leur ligne exacte ; toute commande inconnue, ou qui modifie, fait échouer le test.
type fakeDocker struct {
	t     *testing.T
	reply map[string]dockerx.Result
	seen  []string
}

func (f *fakeDocker) Run(argv []string, mutating bool, _ time.Duration) dockerx.Result {
	line := strings.Join(argv, " ")
	f.seen = append(f.seen, line)
	if mutating {
		f.t.Fatalf("init ne modifie rien côté docker : %s", line)
	}
	if r, ok := f.reply[line]; ok {
		return r
	}
	if argv[0] == "systemctl" {
		return dockerx.Result{RC: 1}
	}
	f.t.Fatalf("commande inattendue : %s", line)
	return dockerx.Result{}
}
func (f *fakeDocker) RunBytes([]string, time.Duration) (int, []byte, string) {
	f.t.Fatal("RunBytes")
	return 0, nil, ""
}
func (f *fakeDocker) DryRun() bool { return false }

func (f *fakeDocker) ran(prefix string) bool {
	for _, l := range f.seen {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

const us = "\x1f"

func inspectRow(id, name, image, project, service, dir, files string) string {
	return strings.Join([]string{id, "/" + name, image, project, service, dir, files}, us)
}

const secretConfig = `{"name":"%s","services":{"x":{"environment":{"TOKEN":"hunter2"}}}}`

// world : une pile « stack » (web, deux services de même image, une base), Watchtower, et un conteneur dont le projet ne se retrouve pas.
func newWorld(t *testing.T) *fakeDocker {
	f := &fakeDocker{t: t, reply: map[string]dockerx.Result{
		"docker version --format {{.Server.Version}}": {Out: "27.0.1\n"},
		"docker compose version --short":              {Out: "2.29.0\n"},
		"docker buildx version":                       {Out: "github.com/docker/buildx v0.16\n"},
		"docker ps -q":                                {Out: "c1\nc2\nc3\nc4\nc5\nc6\n"},
	}}
	rows := []string{
		inspectRow("c1web000000", "stack-web-1", "traefik:v3.6", "stack", "web", "/srv/stack", "/srv/stack/compose.yml"),
		inspectRow("c2api000000", "stack-api-1", "ghcr.io/acme/api:1", "stack", "api", "/srv/stack", "/srv/stack/compose.yml"),
		inspectRow("c3wrk000000", "stack-worker-1", "ghcr.io/acme/api:1", "stack", "worker", "/srv/stack", "/srv/stack/compose.yml"),
		inspectRow("c4db0000000", "stack-db-1", "postgres:17", "stack", "db", "/srv/stack", "/srv/stack/compose.yml"),
		inspectRow("c5wt0000000", "watchtower", "containrrr/watchtower", "wt", "watchtower", "/srv/wt", "/srv/wt/compose.yml"),
		inspectRow("c6ghost0000", "ghost-1", "nginx", "ghost", "web", "/srv/ghost", "/srv/ghost/compose.yml"),
	}
	inspectOut := strings.Join(rows, "\n") + "\n"
	f.reply["INSPECT"] = dockerx.Result{Out: inspectOut}
	for dir, name := range map[string]string{"/srv/stack": "stack", "/srv/wt": "wt", "/srv/ghost": "autre-nom"} {
		f.reply["docker compose --project-directory "+dir+" config --format json"] = dockerx.Result{Out: strings.Replace(secretConfig, "%s", name, 1)}
	}
	for svc, id := range map[string]string{"web": "c1web000000", "api": "c2api000000", "worker": "c3wrk000000", "db": "c4db0000000"} {
		f.reply["docker compose --project-directory /srv/stack ps -q --all "+svc] = dockerx.Result{Out: id + "\n"}
	}
	f.reply["docker compose --project-directory /srv/wt ps -q --all watchtower"] = dockerx.Result{Out: "c5wt0000000\n"}
	f.reply["docker compose --project-directory /srv/ghost ps -q --all web"] = dockerx.Result{Out: "c6ghost0000\n"}
	for image, v := range map[string]string{"traefik:v3.6": "3.6.2", "ghcr.io/acme/api:1": "1.4.0", "postgres:17": "", "containrrr/watchtower": "", "nginx": ""} {
		out := "null\n"
		if v != "" {
			out = `{"org.opencontainers.image.version":"` + v + `"}` + "\n"
		}
		f.reply["docker image inspect --format {{json .Config.Labels}} "+image] = dockerx.Result{Out: out}
	}
	return f
}

// Run complète : l'appel `docker inspect --format … ids` a un gabarit long ; on le reconnaît par son préfixe.
func (f *fakeDocker) withInspect() *fakeDocker {
	inner := f.reply["INSPECT"]
	f.reply["docker inspect --format "+inspectFormatForTests()+" c1 c2 c3 c4 c5 c6"] = inner
	return f
}

func inspectFormatForTests() string {
	return "{{.ID}}" + us + "{{.Name}}" + us + "{{.Config.Image}}" + us + `{{index .Config.Labels "com.docker.compose.project"}}` + us +
		`{{index .Config.Labels "com.docker.compose.service"}}` + us + `{{index .Config.Labels "com.docker.compose.project.working_dir"}}` + us +
		`{{index .Config.Labels "com.docker.compose.project.config_files"}}`
}

type initRig struct {
	*rig
	docker *fakeDocker
	paths  config.Settings
}

func newInitRig(t *testing.T) *initRig {
	t.Helper()
	r := newRig(t, "")
	os.Remove(r.cfg) // init part d'un dossier sans configuration
	cfgDir := filepath.Join(r.dir, "etc")
	r.cfg = filepath.Join(cfgDir, "updates.toml")
	d := newWorld(t).withInspect()
	st := config.DefaultSettings()
	st.StateFile = filepath.Join(r.dir, "var", "state.json")
	st.LockFile = filepath.Join(r.dir, "run", "lock")
	st.NotifyURLFile = filepath.Join(cfgDir, "discord_webhook")
	st.SettleSeconds = 20
	bin := filepath.Join(r.dir, "bin-docker-update")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.env.NewRunner = func(bool, func(string)) dockerx.Runner { return d }
	r.env.Defaults = func() config.Settings { return st }
	r.env.Euid = func() int { return 0 }
	r.env.UnitDir = filepath.Join(r.dir, "systemd")
	r.env.Binary = bin
	r.env.Getenv = func(string) string { return "" }
	r.env.Dial = func(string) error { return nil }
	r.env.Now = func() time.Time { return time.Date(2026, 10, 19, 15, 4, 5, 0, time.Local) }
	return &initRig{rig: r, docker: d, paths: st}
}

func (r *initRig) installEverything() {
	r.t.Helper()
	for _, d := range []string{r.env.UnitDir, filepath.Dir(r.paths.LockFile), filepath.Dir(r.paths.NotifyURLFile)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	svc := "[Service]\nExecStart=/usr/local/sbin/docker-update --config /etc/docker-update/updates.toml check\n"
	for f, body := range map[string]string{"docker-update.service": svc, "docker-update.timer": "[Timer]\n"} {
		if err := os.WriteFile(filepath.Join(r.env.UnitDir, f), []byte(body), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := os.WriteFile(r.paths.NotifyURLFile, []byte("https://discord.com/api/webhooks/1/x\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.docker.reply["systemctl is-enabled docker-update.timer"] = dockerx.Result{Out: "enabled\n"}
	r.docker.reply["systemctl is-active docker-update.timer"] = dockerx.Result{Out: "active\n"}
}

func (r *initRig) cfgText() string {
	r.t.Helper()
	b, err := os.ReadFile(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func TestInitCheckAllGreenAndEachFailure(t *testing.T) {
	r := newInitRig(t)
	r.installEverything()
	if code := r.run("init", "--check"); code != 0 {
		t.Fatalf("installation complète : code %d\n%s", code, r.out)
	}
	r.must("tout est en ordre")
	if r.docker.ran("docker ps") || r.docker.ran("docker inspect") {
		t.Fatal("init --check ne regarde pas les conteneurs")
	}
	if _, there := exists(r.cfg); there {
		t.Fatal("init --check n'écrit rien")
	}
	mark := func(sub string) { // chaque défaut, un par un, est vu et sa correction est donnée
		t.Helper()
		if !strings.Contains(r.out.String(), sub) {
			t.Fatalf("%q absent :\n%s", sub, r.out)
		}
	}
	type breakIt struct {
		label, want string
		apply       func()
		restore     func()
	}
	reply := func(k string, v dockerx.Result) func() {
		old, had := r.docker.reply[k]
		r.docker.reply[k] = v
		return func() {
			if had {
				r.docker.reply[k] = old
			} else {
				delete(r.docker.reply, k)
			}
		}
	}
	for _, tc := range []struct {
		label  string
		want   []string
		break_ func() func()
	}{
		{"docker", []string{"✗ Docker", "démarrer le démon"}, func() func() {
			return reply("docker version --format {{.Server.Version}}", dockerx.Result{RC: 1, Err: "Cannot connect"})
		}},
		{"compose", []string{"✗ Docker Compose", "docker-compose-plugin"}, func() func() { return reply("docker compose version --short", dockerx.Result{RC: 1}) }},
		{"buildx", []string{"✗ docker buildx", "docker-buildx-plugin"}, func() func() { return reply("docker buildx version", dockerx.Result{RC: 1}) }},
		{"minuteur", []string{"⚠ Minuteur", "systemctl enable --now docker-update.timer"}, func() func() { return reply("systemctl is-active docker-update.timer", dockerx.Result{RC: 3}) }},
		{"non root", []string{"⚠ Compte"}, func() func() {
			old := r.env.Euid
			r.env.Euid = func() int { return 1000 }
			return func() { r.env.Euid = old }
		}},
		{"réseau", []string{"⚠ Réseau api.github.com:443", "injoignable"}, func() func() {
			old := r.env.Dial
			r.env.Dial = func(string) error { return os.ErrDeadlineExceeded }
			return func() { r.env.Dial = old }
		}},
		{"binaire absent", []string{"⚠ Binaire", "introuvable"}, func() func() {
			old := r.env.Binary
			r.env.Binary = filepath.Join(r.dir, "nope")
			return func() { r.env.Binary = old }
		}},
		{"webhook absent", []string{"⚠ Webhook", "set-webhook"}, func() func() {
			os.Rename(r.paths.NotifyURLFile, r.paths.NotifyURLFile+".x")
			return func() { os.Rename(r.paths.NotifyURLFile+".x", r.paths.NotifyURLFile) }
		}},
		{"webhook lisible", []string{"⚠ Webhook", "chmod 600"}, func() func() {
			os.Chmod(r.paths.NotifyURLFile, 0o644)
			return func() { os.Chmod(r.paths.NotifyURLFile, 0o600) }
		}},
		{"dossier du verrou", []string{"⚠ Dossier du verrou", "install -d -m 755"}, func() func() {
			os.Rename(filepath.Dir(r.paths.LockFile), filepath.Dir(r.paths.LockFile)+".x")
			return func() { os.Rename(filepath.Dir(r.paths.LockFile)+".x", filepath.Dir(r.paths.LockFile)) }
		}},
		{"unités", []string{"⚠ Unités systemd", "daemon-reload"}, func() func() {
			os.Rename(filepath.Join(r.env.UnitDir, "docker-update.timer"), filepath.Join(r.dir, "t.x"))
			return func() { os.Rename(filepath.Join(r.dir, "t.x"), filepath.Join(r.env.UnitDir, "docker-update.timer")) }
		}},
		{"DOCKER_CONFIG illisible", []string{"⚠ Identifiants du registre", "chmod 600"}, func() func() {
			dc := filepath.Join(r.dir, "dockercfg")
			os.MkdirAll(dc, 0o755)
			os.WriteFile(filepath.Join(dc, "config.json"), []byte("{}"), 0o644)
			old := r.env.Getenv
			r.env.Getenv = func(k string) string {
				if k == "DOCKER_CONFIG" {
					return dc
				}
				return ""
			}
			return func() { r.env.Getenv = old }
		}},
	} {
		undo := tc.break_()
		r.fresh()
		if code := r.run("init", "--check"); code != 1 {
			t.Fatalf("%s : code %d\n%s", tc.label, code, r.out)
		}
		for _, w := range tc.want {
			mark(w)
		}
		undo()
	}
	r.fresh()
	if code := r.run("init", "--check"); code != 0 {
		t.Fatalf("après restauration : code %d\n%s", code, r.out)
	}
}

func TestInitStopsOnABlockingFailureBeforeLookingAtContainers(t *testing.T) {
	r := newInitRig(t)
	r.docker.reply["docker version --format {{.Server.Version}}"] = dockerx.Result{RC: 1, Err: "Cannot connect to the Docker daemon"}
	if code := r.run("init", "--yes"); code != 1 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	r.must("✗ Docker")
	r.must("init s'arrête")
	if r.docker.ran("docker ps") {
		t.Fatal("aucune découverte tant qu'un point bloquant échoue")
	}
	if _, there := exists(r.cfg); there {
		t.Fatal("rien n'est écrit")
	}
}

func TestInitNonInteractiveWritesANotifyOnlyFile(t *testing.T) {
	r := newInitRig(t)
	if code := r.run("init", "--yes", "--skip-checks"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	got := r.cfgText()
	want := `# Configuration de docker-update, écrite par ` + "`docker-update init`" + ` le 2026-10-19.
# Propriétaire root:root, mode 644 : l'outil refuse un fichier modifiable par d'autres comptes.
# À éditer à la main, ou avec ` + "`docker-update add` / `delete` / `list`" + `.

[settings]
state_file        = "` + r.paths.StateFile + `"
lock_file         = "` + r.paths.LockFile + `"
registry_tool     = "buildx"
notify_url_file   = "` + r.paths.NotifyURLFile + `"   # à remplir avec ` + "`docker-update set-webhook`" + ` (saisie masquée)
notify_format     = "discord"
default_cooldown  = "3d"          # une image doit rester identique ce temps avant d'être appliquée
window            = "02:00-05:00"  # on n'applique que la nuit ; "" = à toute heure
cleanup           = true
settle_seconds    = 20
health_timeout    = 180

# image : ghcr.io/acme/api:1
[[service]]
name                = "api"
compose_dir         = "/srv/stack"
service             = "api"
mode                = "notify"

# image : traefik:v3.6
[[service]]
name                = "web"
compose_dir         = "/srv/stack"
service             = "web"
mode                = "notify"
advisories          = ["traefik/traefik"]

# image : ghcr.io/acme/api:1
[[service]]
name                = "worker"
compose_dir         = "/srv/stack"
service             = "worker"
mode                = "notify"
`
	if got != want {
		t.Fatalf("fichier obtenu :\n%s\nattendu :\n%s", got, want)
	}
	if fi, _ := os.Stat(r.cfg); fi.Mode().Perm() != 0o644 {
		t.Fatalf("droits = %v", fi.Mode().Perm())
	}
	if _, err := config.Load(r.cfg); err != nil {
		t.Fatalf("validate : %v", err)
	}
	r.must("écarté : db (base de données")
	r.must("écarté : watchtower")
	r.must("AVERTISSEMENT : ghost/web écarté")
	if strings.Contains(r.out.String(), "hunter2") || strings.Contains(r.errOut.String(), "hunter2") {
		t.Fatal("la sortie de `docker compose config` ne doit jamais apparaître")
	}
	if strings.Contains(got, "auto") && strings.Contains(got, `mode                = "auto"`) {
		t.Fatal("jamais auto sans l'accord explicite")
	}
	r.must("aucune mise à jour")
	for _, l := range r.docker.seen {
		if strings.HasPrefix(l, "docker pull") || strings.Contains(l, " up ") || strings.Contains(l, "image rm") {
			t.Fatalf("init ne doit rien lancer : %s", l)
		}
	}
}

func TestInitIncludeExcludeAndModeFlags(t *testing.T) {
	r := newInitRig(t)
	if code := r.run("init", "--yes", "--skip-checks", "--mode", "manual", "--include", "web,db", "--exclude", "worker"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	cfg, err := config.Load(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cfg.Services {
		names = append(names, s.Name+":"+s.Mode)
	}
	if strings.Join(names, " ") != "db:manual web:manual" {
		t.Fatalf("services = %v (--include nomme explicitement la base, donc elle est suivie)", names)
	}
	r2 := newInitRig(t)
	if code := r2.run("init", "--yes", "--skip-checks", "--mode", "auto", "--include", "web"); code != 0 {
		t.Fatalf("code %d", code)
	}
	r2.must("--mode auto fait appliquer")
	if !strings.Contains(r2.cfgText(), `mode                = "auto"`) {
		t.Fatal("auto demandé explicitement : auto écrit")
	}
	if code := r2.run("init", "--yes", "--mode", "oups"); code != 2 {
		t.Fatalf("mode inconnu : code %d", code)
	}
}

func TestInitRefusesAnExistingFileUnlessForced(t *testing.T) {
	r := newInitRig(t)
	if err := os.MkdirAll(filepath.Dir(r.cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	old := "[settings]\n# à garder en sauvegarde\n"
	if err := os.WriteFile(r.cfg, []byte(old), 0o640); err != nil {
		t.Fatal(err)
	}
	if code := r.run("init", "--yes", "--skip-checks"); code != 1 {
		t.Fatalf("code %d", code)
	}
	r.must("existe déjà")
	r.must("docker-update add")
	if r.cfgText() != old || len(r.backups()) != 0 {
		t.Fatal("le fichier existant ne doit pas bouger")
	}
	if r.docker.ran("docker ps") {
		t.Fatal("pas de découverte quand le fichier existe")
	}
	r.fresh()
	if code := r.run("init", "--yes", "--skip-checks", "--force"); code != 0 {
		t.Fatalf("--force : code %d\n%s", code, r.out)
	}
	if b := r.backups(); len(b) != 1 {
		t.Fatalf("sauvegardes = %v", b)
	} else if saved, _ := os.ReadFile(b[0]); string(saved) != old {
		t.Fatalf("sauvegarde = %q", saved)
	}
	if fi, _ := os.Stat(r.cfg); fi.Mode().Perm() != 0o640 {
		t.Fatalf("--force garde les droits du fichier : %v", fi.Mode().Perm())
	}
	if len(r.names()) != 3 {
		t.Fatalf("services = %v", r.names())
	}
}

func TestInitInteractiveSession(t *testing.T) {
	r := newInitRig(t)
	r.installEverything()
	// ordre des conteneurs : stack/api, stack/db, stack/web, stack/worker, wt/watchtower
	answers := []string{
		"o",          // api : suivre
		"",           // db : défaut (non)
		"",           // web : défaut (oui)
		"o",          // worker : suivre
		"",           // watchtower : défaut (non)
		"apiw",       // groupe pour api + worker
		"worker,api", // ordre de mise à jour
		"",           // groupe apiw : mode (notify)
		"n",          // cleanup
		"",           // dépôt des avis : suggestion acme/api (à confirmer)
		"o",          // notes de release
		"auto",       // web : mode
		"auto",       // confirmation de auto
		"",           // cleanup (oui)
		"",           // dépôt : traefik/traefik
		"n",          // notes de release
		"o",          // écrire le fichier
		"o",          // enregistrer le webhook maintenant ?
		"https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
	}
	os.Remove(r.paths.NotifyURLFile)
	r.env.Stdin = strings.NewReader(strings.Join(answers, "\n") + "\n")
	if code := r.run("init"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	cfg, err := config.Load(r.cfg)
	if err != nil {
		t.Fatalf("%v\n%s", err, r.cfgText())
	}
	var rows []string
	for _, s := range cfg.Services {
		rows = append(rows, strings.Join([]string{s.Name, s.Group, s.Mode, join(s.Advisories), map[bool]string{true: "notes", false: "-"}[s.ReleaseNotes], map[bool]string{true: "ménage", false: "garde"}[s.Cleanup]}, "/"))
	}
	if got := strings.Join(rows, " "); got != "worker/apiw/notify/acme/api/notes/garde api/apiw/notify/acme/api/-/garde web//auto/traefik/traefik/-/ménage" {
		t.Fatalf("services = %s\n%s", got, r.cfgText())
	}
	hook, err := os.ReadFile(r.paths.NotifyURLFile)
	if err != nil {
		t.Logf("SORTIE :\n%s%s", r.out, r.errOut)
	}
	if err != nil || !strings.HasPrefix(string(hook), "https://discord.com/api/webhooks/123456789012345678/") {
		t.Fatalf("webhook = %q (%v)", hook, err)
	}
	if fi, _ := os.Stat(r.paths.NotifyURLFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("droits du webhook = %v", fi.Mode().Perm())
	}
	for _, leak := range []string{"AbCdEfGh", "hunter2"} {
		if strings.Contains(r.out.String()+r.errOut.String(), leak) {
			t.Fatalf("%q ne doit jamais être affiché", leak)
		}
	}
	r.must("Tapez auto pour confirmer")
	r.must("Ordre de mise à jour")
	r.must("3 service(s) écrit(s), dont 1 en auto")
}

func TestInitInteractiveStopsWithoutWritingWhenTheAnswersRunOut(t *testing.T) {
	r := newInitRig(t)
	r.env.Stdin = strings.NewReader("o\n\n") // deux réponses puis plus rien
	if code := r.run("init", "--skip-checks"); code != 1 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	r.must("init interrompu")
	if _, there := exists(r.cfg); there {
		t.Fatal("rien ne doit être écrit")
	}
}

func lines(l ...string) *strings.Reader { return strings.NewReader(strings.Join(l, "\n") + "\n") }

func TestInitInteractiveDeclineAndDryRunWriteNothing(t *testing.T) {
	r := newInitRig(t)
	r.env.Stdin = lines("", "", "", "", "", // suivre : api, web, worker (défauts) ; pas db ni watchtower
		"",                                    // pas de groupe
		"", "", "-", "", "", "-", "", "", "-", // mode, ménage, dépôt pour api, web, worker
		"n") // « Écrire ce fichier ? » : non
	if code := r.run("init", "--skip-checks"); code != 1 {
		t.Fatalf("« non » : code %d\n%s", code, r.out)
	}
	r.must("annulé, rien n'est écrit")
	if _, there := exists(r.cfg); there {
		t.Fatal("écrit malgré le refus")
	}
	r.fresh()
	r.env.Stdin = strings.NewReader("")
	if code := r.run("--dry-run", "init", "--yes", "--skip-checks"); code != 0 {
		t.Fatalf("dry-run : code %d", code)
	}
	r.must("[simulation] rien n'est écrit")
	r.must("[[service]]")
	if _, there := exists(r.cfg); there {
		t.Fatal("la simulation a écrit")
	}
}

func TestInitAutoNeedsAnExplicitConfirmation(t *testing.T) {
	r := newInitRig(t)
	r.env.Stdin = lines("", "", "", "n", "", // api oui, db non, web oui, worker non (explicite), watchtower non
		"auto", "oui", "", "-", // api : auto demandé, « oui » n'est pas « auto » : retombe sur notify
		"auto", "auto", "", "-", // web : auto confirmé
		"o", "n") // écrire ; pas de webhook
	if code := r.run("init", "--skip-checks"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	r.must("non confirmé : notify")
	cfg, err := config.Load(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range cfg.Services {
		got[s.Name] = s.Mode
	}
	if got["api"] != "notify" || got["web"] != "auto" || len(got) != 2 {
		t.Fatalf("modes = %v\n%s", got, r.cfgText())
	}
}

func TestInitGroupNameMustBeUniqueAndValid(t *testing.T) {
	r := newInitRig(t)
	r.env.Stdin = lines("o", "", "n", "o", "", // api et worker suivis (même image)
		"web", "bad name", "", // noms refusés (déjà pris, invalide), puis « non »
		"", "-", "", "-", // api, worker : ménage, dépôt (le mode vient de --mode)
		"o", "n") // écrire ; pas de webhook
	if code := r.run("init", "--skip-checks", "--mode", "notify"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	r.must("nom invalide ou déjà pris")
	cfg, err := config.Load(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range cfg.Services {
		if s.Group != "" {
			t.Fatalf("aucun groupe attendu : %+v", s)
		}
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("services = %d", len(cfg.Services))
	}
}

func TestAddFromARunningContainer(t *testing.T) {
	r := newInitRig(t)
	if err := os.MkdirAll(filepath.Dir(r.cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	base := `[settings]
state_file = "` + r.paths.StateFile + `"
lock_file = "` + r.paths.LockFile + `"
`
	if err := os.WriteFile(r.cfg, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.paths.LockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := r.run("add", "front", "--container", "stack-web-1", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	r.must("Dépôt suggéré pour les avis : traefik/traefik")
	cfg, err := config.Load(r.cfg)
	if err != nil {
		t.Fatalf("%v\n%s", err, r.cfgText())
	}
	s := cfg.Services[0]
	if s.Name != "front" || s.ComposeDir != "/srv/stack" || s.Service != "web" || s.Mode != "notify" || len(s.Advisories) != 0 {
		t.Fatalf("service = %+v", s)
	}
	// un préfixe d'identifiant suffit, une option explicite l'emporte, un conteneur inconnu ou invérifiable est refusé
	if code := r.run("add", "front2", "--container", "c1web", "--service", "autre", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	if cfg, _ := config.Load(r.cfg); cfg.Services[1].Service != "autre" {
		t.Fatalf("l'option explicite doit l'emporter : %+v", cfg.Services[1])
	}
	before := r.cfgText()
	for _, tc := range []struct{ ref, want string }{{"nope", "aucun conteneur"}, {"ghost-1", "n'est pas celui du conteneur"}, {"stack-", "aucun conteneur"}, {"c1", "aucun conteneur"}} {
		r.fresh()
		if code := r.run("add", "x", "--container", tc.ref, "--yes"); code != 1 {
			t.Fatalf("%s : code %d\n%s", tc.ref, code, r.out)
		}
		r.must(tc.want)
	}
	if r.cfgText() != before {
		t.Fatal("un refus ne modifie rien")
	}
	for _, l := range r.docker.seen {
		if strings.Contains(l, "Config.Env") {
			t.Fatalf("jamais l'environnement : %s", l)
		}
	}
}

func TestInitBlocksWhenTheConfigurationFolderCannotBeUsed(t *testing.T) {
	r := newInitRig(t)
	notADir := filepath.Join(r.dir, "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.cfg = filepath.Join(notADir, "etc", "updates.toml")
	if code := r.run("init", "--yes", "--skip-checks"); code != 1 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	r.must("✗ Dossier de configuration")
	r.must("install -d -m 755 -o root -g root")
	if r.docker.ran("docker ps") {
		t.Fatal("pas de découverte")
	}
}
