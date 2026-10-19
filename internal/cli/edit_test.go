package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/state"
)

const editBase = `# configuration de test
[settings]
state_file = %q
lock_file = %q

# ── web ──
[[service]]
name        = "web"
compose_dir = "/srv/web"
service     = "web"
mode        = "notify"

# ── le groupe app ──
[[service]]
name = "app-db"
group = "app"
compose_dir = "/srv/app"
service = "db"
mode = "notify"
cooldown = "1d"
cleanup = false

[[service]]
name = "app-web"
group = "app"
compose_dir = "/srv/app"
service = "web"
mode = "notify"
cooldown = "1d"
cleanup = false

[[service]]
name = "tail"
compose_dir = "/srv/tail"
service = "tail"
`

type editRig struct {
	*rig
	calls *[]call
}

func newEditRig(t *testing.T) *editRig {
	t.Helper()
	r := newRig(t, "")
	r.writeEdit(editBase)
	r.env.Now = func() time.Time { return time.Date(2026, 10, 19, 15, 4, 5, 0, time.Local) }
	return &editRig{rig: r, calls: r.record()}
}

func (r *rig) writeEdit(tpl string) {
	r.t.Helper()
	body := strings.Replace(strings.Replace(tpl, "%q", `"`+filepath.Join(r.dir, "state", "state.json")+`"`, 1), "%q", `"`+filepath.Join(r.dir, "lock")+`"`, 1)
	if err := os.WriteFile(r.cfg, []byte(body), 0o640); err != nil {
		r.t.Fatal(err)
	}
	if err := os.Chmod(r.cfg, 0o640); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read() string {
	r.t.Helper()
	b, err := os.ReadFile(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *rig) names() []string {
	r.t.Helper()
	cfg, err := config.Load(r.cfg)
	if err != nil {
		r.t.Fatalf("configuration invalide : %v\n%s", err, r.read())
	}
	var out []string
	for _, s := range cfg.Services {
		out = append(out, s.Name)
	}
	return out
}

func (r *rig) backups() []string {
	m, _ := filepath.Glob(r.cfg + ".bak-*")
	return m
}

func (r *rig) fresh() {
	r.out.Reset()
	r.errOut.Reset()
}

func join(l []string) string { return strings.Join(l, ",") }

func (r *rig) must(want string) {
	r.t.Helper()
	if !strings.Contains(r.out.String()+r.errOut.String(), want) {
		r.t.Fatalf("%q absent de la sortie :\n%s%s", want, r.out.String(), r.errOut.String())
	}
}

func TestList(t *testing.T) {
	r := newEditRig(t)
	st, _ := state.Load(filepath.Join(r.dir, "state", "state.json"))
	st.Holds = []string{"app"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if code := r.run("list"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	out := r.out.String()
	for _, want := range []string{"SERVICE", "FENÊTRE", "4 service(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q absent :\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "app-db"), strings.HasPrefix(line, "app-web"):
			if !strings.Contains(line, "app") || !strings.Contains(line, "1 j") || !strings.HasSuffix(line, "pause") {
				t.Fatalf("ligne de groupe en pause : %q", line)
			}
		case strings.HasPrefix(line, "web "):
			if !strings.HasSuffix(line, "-") || !strings.Contains(line, "toujours") {
				t.Fatalf("ligne : %q", line)
			}
		}
	}
	if len(*r.calls) != 0 {
		t.Fatalf("list ne lance aucune commande docker : %v", *r.calls)
	}
}

func TestAddInsertsACanonicalBlockAndKeepsTheRest(t *testing.T) {
	r := newEditRig(t)
	code := r.run("add", "metrics", "--compose-dir", "/srv/m", "--service=metrics", "--compose-file", "compose.yml", "--compose-file", "o.yml",
		"--mode", "notify", "--cooldown", "2d", "--window", "02:00-05:00", "--cleanup=false", "--advisories", "a/b, c/d",
		"--release-notes", "--release-notes-repos", "a/b", "--smoke-url", "https://x.example/", "--smoke-expect", "200,302", "--yes")
	if code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	got := r.read()
	want := strings.TrimSuffix(editBase, "\n")
	want = strings.Replace(want, "%q", `"`+filepath.Join(r.dir, "state", "state.json")+`"`, 1)
	want = strings.Replace(want, "%q", `"`+filepath.Join(r.dir, "lock")+`"`, 1)
	block := `[[service]]
name                = "metrics"
compose_dir         = "/srv/m"
service             = "metrics"
compose_files       = ["compose.yml", "o.yml"]
mode                = "notify"
cooldown            = "2d"
window              = "02:00-05:00"
cleanup             = false
advisories          = ["a/b", "c/d"]
release_notes       = true
release_notes_repos = ["a/b"]
smoke_url           = "https://x.example/"
smoke_expect        = [200, 302]`
	if got != want+"\n\n"+block+"\n" {
		t.Fatalf("fichier obtenu :\n%s", got)
	}
	if join(r.names()) != "web,app-db,app-web,tail,metrics" {
		t.Fatalf("noms = %v", r.names())
	}
	b := r.backups()
	if len(b) != 1 || !strings.HasSuffix(b[0], ".bak-20261019-150405") {
		t.Fatalf("sauvegardes = %v", b)
	}
	old, _ := os.ReadFile(b[0])
	if !strings.Contains(string(old), `name = "tail"`) || strings.Contains(string(old), "metrics") {
		t.Fatal("la sauvegarde doit être l'ancien fichier")
	}
	for _, f := range append(b, r.cfg) {
		if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o640 {
			t.Fatalf("%s : droits %v", f, fi.Mode().Perm())
		}
	}
	r.must("--- ")
	r.must("+name                = \"metrics\"")
	r.must("docker-update check metrics")
	if len(*r.calls) != 0 {
		t.Fatalf("add ne lance rien : %v", *r.calls)
	}
}

func TestAddDefaultsToNotifyAndNeverToAuto(t *testing.T) {
	r := newEditRig(t)
	if code := r.run("add", "x", "--compose-dir", "/x", "--service", "x", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	cfg, _ := config.Load(r.cfg)
	if got := cfg.Services[len(cfg.Services)-1].Mode; got != "notify" {
		t.Fatalf("mode = %s", got)
	}
}

func TestAddToAnExistingGroupInheritsItsSettingsAndGoesAfterTheLastMember(t *testing.T) {
	r := newEditRig(t)
	if code := r.run("add", "app-worker", "--group", "app", "--compose-dir", "/srv/app", "--service", "worker", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	if join(r.names()) != "web,app-db,app-web,app-worker,tail" {
		t.Fatalf("ordre = %v : après le dernier membre du groupe", r.names())
	}
	cfg, _ := config.Load(r.cfg)
	w := cfg.Services[3]
	if w.Group != "app" || w.Cooldown != 86400 || w.Cleanup || w.Mode != "notify" {
		t.Fatalf("réglages hérités : %+v", w)
	}
	// --after et --first
	r.fresh()
	if code := r.run("add", "app-first", "--group", "app", "--compose-dir", "/srv/app", "--service", "f", "--after", "app-db", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	if join(r.names()) != "web,app-db,app-first,app-web,app-worker,tail" {
		t.Fatalf("--after : %v", r.names())
	}
	if code := r.run("add", "top", "--compose-dir", "/srv/t", "--service", "t", "--first", "--yes"); code != 0 {
		t.Fatalf("code %d", code)
	}
	if r.names()[0] != "top" {
		t.Fatalf("--first : %v", r.names())
	}
}

func TestAddRefusals(t *testing.T) {
	r := newEditRig(t)
	before := r.read()
	for _, tc := range []struct {
		label string
		args  []string
		want  string
	}{
		{"nom pris", []string{"add", "web", "--compose-dir", "/x", "--service", "x", "--yes"}, "déjà pris"},
		{"groupe = nom de service", []string{"add", "web2", "--group", "web", "--compose-dir", "/x", "--service", "x", "--yes"}, "porte le nom d'un service"},
		{"nom = groupe", []string{"add", "app", "--compose-dir", "/x", "--service", "x", "--yes"}, "déjà un nom de groupe"},
		{"groupe = son propre nom", []string{"add", "g1", "--group", "g1", "--compose-dir", "/x", "--service", "x", "--yes"}, "ne peut pas être celui du service"},
		{"réglages incompatibles", []string{"add", "app-x", "--group", "app", "--mode", "auto", "--compose-dir", "/x", "--service", "x", "--yes"}, "identiques"},
		{"délai invalide", []string{"add", "n1", "--cooldown", "banana", "--compose-dir", "/x", "--service", "x", "--yes"}, "refusé"},
		{"nom invalide", []string{"add", "bad name", "--compose-dir", "/x", "--service", "x", "--yes"}, "refusé"},
		{"après un inconnu", []string{"add", "n2", "--after", "nope", "--compose-dir", "/x", "--service", "x", "--yes"}, "service inconnu"},
		{"compose_dir manquant", []string{"add", "n3", "--service", "x", "--yes"}, "--compose-dir est obligatoire"},
		{"code de fumée invalide", []string{"add", "n4", "--smoke-expect", "oui", "--compose-dir", "/x", "--service", "x", "--yes"}, "n'est pas un code HTTP"},
		{"service manquant", []string{"add", "n5", "--compose-dir", "/x", "--yes"}, "--service est obligatoire"},
	} {
		r.fresh()
		if code := r.run(tc.args...); code != 1 {
			t.Fatalf("%s : code %d\n%s%s", tc.label, code, r.out, r.errOut)
		}
		r.must(tc.want)
		if r.read() != before || len(r.backups()) != 0 {
			t.Fatalf("%s : le fichier ou les sauvegardes ont changé", tc.label)
		}
	}
}

func TestAddAsksForWhatIsMissingAndForConfirmation(t *testing.T) {
	r := newEditRig(t)
	before := r.read()
	r.env.Stdin = strings.NewReader("/srv/q\nq-svc\nn\n") // dossier, service, puis « non »
	if code := r.run("add", "q"); code != 1 {
		t.Fatalf("refus : code %d\n%s", code, r.out)
	}
	r.must("Dossier compose")
	r.must("annulé")
	if r.read() != before || len(r.backups()) != 0 {
		t.Fatal("« non » : rien n'est écrit")
	}
	r.fresh()
	r.env.Stdin = strings.NewReader("/srv/q\nq-svc\noui\n")
	if code := r.run("add", "q"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	if join(r.names()) != "web,app-db,app-web,tail,q" {
		t.Fatalf("noms = %v", r.names())
	}
	// sans aucune réponse (entrée vide) : refus, jamais d'écriture
	r.fresh()
	r.env.Stdin = strings.NewReader("")
	if code := r.run("add", "q2", "--compose-dir", "/x", "--service", "x"); code != 1 {
		t.Fatalf("sans réponse : code %d", code)
	}
	if strings.Contains(r.read(), "q2") {
		t.Fatal("écrit sans confirmation")
	}
}

func TestAddAndDeleteDryRunWriteNothing(t *testing.T) {
	r := newEditRig(t)
	before := r.read()
	if code := r.run("--dry-run", "add", "x", "--compose-dir", "/x", "--service", "x", "--yes"); code != 0 {
		t.Fatalf("code %d", code)
	}
	r.must("[simulation]")
	r.must("+name                = \"x\"")
	r.fresh()
	if code := r.run("--dry-run", "delete", "tail", "--yes"); code != 0 {
		t.Fatalf("code %d", code)
	}
	r.must("[simulation]")
	if r.read() != before || len(r.backups()) != 0 {
		t.Fatal("la simulation a écrit")
	}
}

func TestAddThenDeleteRestoresTheFile(t *testing.T) {
	r := newEditRig(t)
	before := r.read()
	if code := r.run("add", "zz", "--compose-dir", "/x", "--service", "x", "--after", "web", "--yes"); code != 0 {
		t.Fatalf("code %d", code)
	}
	if code := r.run("delete", "zz", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	if r.read() != before {
		t.Fatalf("fichier différent :\n%s", r.read())
	}
	if len(r.backups()) != 2 { // les deux écritures tombent dans la même seconde : « -2 » évite d'écraser la première
		t.Fatalf("deux écritures, deux sauvegardes distinctes : %v", r.backups())
	}
}

func TestDeleteRemovesBlocksAndCleansTheState(t *testing.T) {
	r := newEditRig(t)
	statePath := filepath.Join(r.dir, "state", "state.json")
	st, _ := state.Load(statePath)
	st.Holds = []string{"app", "web", "tail"}
	st.Errors["web"] = "boom"
	st.Errors["tail"] = "other"
	st.Alerts["release:web:v1"] = 1
	st.Alerts["correctif:web:sha256:abc"] = 1
	st.Alerts["release:tail:v1"] = 1
	st.Alerts["release:webby:v1"] = 1
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if code := r.run("delete", "web", "--group", "app", "--yes"); code != 0 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	if join(r.names()) != "tail" {
		t.Fatalf("noms = %v\n%s", r.names(), r.read())
	}
	got := r.read()
	if strings.Contains(got, "── web ──") || strings.Contains(got, "le groupe app") {
		t.Fatalf("les commentaires collés doivent partir avec leur bloc :\n%s", got)
	}
	st, _ = state.Load(statePath)
	if join(st.Holds) != "tail" || len(st.Errors) != 1 || st.Errors["tail"] == "" {
		t.Fatalf("pauses = %v, erreurs = %v", st.Holds, st.Errors)
	}
	for _, gone := range []string{"release:web:v1", "correctif:web:sha256:abc"} {
		if _, ok := st.Alerts[gone]; ok {
			t.Fatalf("alerte %s non retirée", gone)
		}
	}
	for _, kept := range []string{"release:tail:v1", "release:webby:v1"} {
		if _, ok := st.Alerts[kept]; !ok {
			t.Fatalf("alerte %s retirée à tort", kept)
		}
	}
	r.must("continuent de tourner")
	if len(*r.calls) != 0 {
		t.Fatalf("delete ne lance aucune commande docker : %v", *r.calls)
	}
}

func TestDeleteKeepStateAndRefusals(t *testing.T) {
	r := newEditRig(t)
	statePath := filepath.Join(r.dir, "state", "state.json")
	st, _ := state.Load(statePath)
	st.Holds = []string{"tail"}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if code := r.run("delete", "tail", "--keep-state", "--yes"); code != 0 {
		t.Fatalf("code %d", code)
	}
	st, _ = state.Load(statePath)
	if join(st.Holds) != "tail" {
		t.Fatalf("--keep-state : l'état ne bouge pas, pauses = %v", st.Holds)
	}
	before := r.read()
	nBackups := len(r.backups())
	for _, tc := range [][]string{{"delete", "nope", "--yes"}, {"delete", "web", "nope", "--yes"}, {"delete", "--group", "nope", "--yes"}} {
		r.fresh()
		if code := r.run(tc...); code != 1 {
			t.Fatalf("%v : code %d", tc, code)
		}
		r.must("inconnu")
		if r.read() != before || len(r.backups()) != nBackups {
			t.Fatalf("%v : fichier modifié", tc)
		}
	}
	r.fresh()
	if code := r.run("delete"); code != 2 {
		t.Fatalf("sans argument : code %d", code)
	}
	// sans --yes et sans réponse : rien n'est retiré
	r.env.Stdin = strings.NewReader("")
	if code := r.run("delete", "web"); code != 1 || r.read() != before {
		t.Fatalf("sans confirmation : code %d", code)
	}
}

func TestEditCommandsRefuseWhileAPassIsRunning(t *testing.T) {
	r := newEditRig(t)
	before := r.read()
	f, err := os.OpenFile(filepath.Join(r.dir, "lock"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "x", "--compose-dir", "/x", "--service", "x", "--yes"}, {"delete", "tail", "--yes"}} {
		r.fresh()
		if code := r.run(args...); code != 1 {
			t.Fatalf("%v : code %d", args, code)
		}
		r.must("un passage est en cours")
	}
	if r.read() != before {
		t.Fatal("modifié pendant un passage")
	}
	// `list` est en lecture seule : il ne prend pas le verrou
	if code := r.run("list"); code != 0 {
		t.Fatalf("list : code %d", code)
	}
}

func TestEditRefusesAnInvalidOrWindowsFile(t *testing.T) {
	r := newEditRig(t)
	if err := os.WriteFile(r.cfg, []byte("[settings]\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := r.run("add", "x", "--compose-dir", "/x", "--service", "x", "--yes"); code != 2 {
		t.Fatalf("code %d\n%s", code, r.out)
	}
	r.must("fins de ligne")
}

func TestEditUsageErrors(t *testing.T) {
	r := newEditRig(t)
	for _, args := range [][]string{{"add"}, {"add", "a", "b"}, {"add", "x", "--mode"}, {"add", "x", "--cleanup=maybe"}, {"add", "x", "--nope"}, {"list", "extra"}} {
		if code := r.run(args...); code != 2 {
			t.Fatalf("%v : code %d", args, code)
		}
	}
	// abréviations d'options uniques
	if code := r.run("add", "ab", "--compose-d", "/x", "--serv", "x", "--ye"); code != 0 {
		t.Fatalf("abréviations : code %d\n%s%s", code, r.out, r.errOut)
	}
}

// Si la relecture du fichier écrit échoue, l'ancien fichier est remis en place.
func TestAFileThatFailsToReadBackIsRolledBack(t *testing.T) {
	r := newEditRig(t)
	before := r.read()
	r.env.Load = func(path string) (*config.Config, error) {
		if r.read() != before { // le nouveau fichier est en place : la relecture « échoue »
			return nil, &config.Error{Msg: "relecture impossible"}
		}
		return config.Load(path)
	}
	if code := r.run("add", "x", "--compose-dir", "/x", "--service", "x", "--yes"); code != 1 {
		t.Fatalf("code %d\n%s%s", code, r.out, r.errOut)
	}
	r.must("l'ancien est remis en place")
	if r.read() != before {
		t.Fatalf("le fichier n'a pas été restauré :\n%s", r.read())
	}
	if fi, _ := os.Stat(r.cfg); fi.Mode().Perm() != 0o640 {
		t.Fatalf("droits = %v", fi.Mode().Perm())
	}
	if len(r.backups()) != 1 {
		t.Fatalf("la sauvegarde reste : %v", r.backups())
	}
}
