package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/notify"
	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/tomlmini"
)

type rig struct {
	t      *testing.T
	dir    string
	cfg    string
	out    *strings.Builder
	errOut *strings.Builder
	env    *Env
}

func newRig(t *testing.T, extra string) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, dir: dir, cfg: filepath.Join(dir, "updates.toml"), out: &strings.Builder{}, errOut: &strings.Builder{}}
	r.writeCfg(extra)
	r.env = &Env{Stdin: strings.NewReader(""), Stdout: r.out, Stderr: r.errOut, Version: "test"}
	return r
}

func (r *rig) writeCfg(extra string) {
	body := fmt.Sprintf("[settings]\nstate_file = %q\nlock_file = %q\n[[service]]\nname = \"a\"\ncompose_dir = %q\nservice = \"a\"\n%s\n",
		filepath.Join(r.dir, "state", "state.json"), filepath.Join(r.dir, "lock"), r.dir, extra)
	if err := os.WriteFile(r.cfg, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// run lance la commande avec la configuration du banc (en plus des options données avant la commande).
func (r *rig) run(args ...string) int {
	r.t.Helper()
	return Main(append([]string{"--config", r.cfg, "--allow-untrusted-config"}, args...), r.env)
}

type call struct {
	kind  string
	apply bool
	yes   bool
	on    bool
	names []string
	note  bool
}

type recorder struct{ calls *[]call }

func (e recorder) Cycle(apply bool, names []string, notify bool) (int, error) {
	*e.calls = append(*e.calls, call{kind: "cycle", apply: apply, names: names, note: notify})
	return 0, nil
}
func (e recorder) NowCmd(names []string, yes bool) (int, error) {
	*e.calls = append(*e.calls, call{kind: "now", names: names, yes: yes})
	return 0, nil
}
func (e recorder) Hold(names []string, on bool) (int, error) {
	*e.calls = append(*e.calls, call{kind: "hold", names: names, on: on})
	return 0, nil
}
func (e recorder) Status() { *e.calls = append(*e.calls, call{kind: "status"}) }

func (r *rig) record() *[]call {
	var calls []call
	r.env.NewEngine = func(*config.Config, dockerx.Runner, *notify.Notifier, *state.State, func(string), bool) Engine {
		return recorder{&calls}
	}
	return &calls
}

func TestCheckAndRunPassTheirNamesAndOptionsOn(t *testing.T) {
	r := newRig(t, "")
	calls := r.record()
	for _, c := range []struct {
		args []string
		want call
	}{
		{[]string{"run", "a"}, call{kind: "cycle", apply: true, names: []string{"a"}}},
		{[]string{"run"}, call{kind: "cycle", apply: true, names: []string{}}},
		{[]string{"check", "a", "b", "--notify"}, call{kind: "cycle", names: []string{"a", "b"}, note: true}},
		{[]string{"check"}, call{kind: "cycle", names: []string{}}},
		{[]string{"now", "a", "--yes"}, call{kind: "now", names: []string{"a"}, yes: true}},
		{[]string{"now", "a"}, call{kind: "now", names: []string{"a"}}},
		{[]string{"hold", "a", "b"}, call{kind: "hold", names: []string{"a", "b"}, on: true}},
		{[]string{"unhold", "a"}, call{kind: "hold", names: []string{"a"}}},
		{[]string{"status"}, call{kind: "status"}},
	} {
		*calls = nil
		if rc := r.run(c.args...); rc != 0 {
			t.Fatalf("%v : code %d (%s)", c.args, rc, r.out.String()+r.errOut.String())
		}
		if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], c.want) {
			t.Errorf("%v : appel %+v, attendu %+v", c.args, *calls, c.want)
		}
	}
}

func TestValidateAndStatusAndHold(t *testing.T) {
	r := newRig(t, "")
	if rc := r.run("hold", "a"); rc != 0 {
		t.Fatalf("hold : %d", rc)
	}
	if rc := r.run("status"); rc != 0 {
		t.Fatalf("status : %d", rc)
	}
	if !strings.Contains(r.out.String(), "en pause : a") {
		t.Errorf("sortie sans « en pause : a » :\n%s", r.out.String())
	}
}

func TestNotifyTestCommand(t *testing.T) {
	r := newRig(t, "")
	if rc := r.run("notify-test"); rc != 1 {
		t.Errorf("code %d, attendu 1", rc)
	}
	if !strings.Contains(r.out.String(), "aucune URL") {
		t.Errorf("sortie : %s", r.out.String())
	}
}

func TestNotifyTestSendsAMessageAndNeverPrintsTheURL(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(req.Body)
		json.Unmarshal(b, &m)
		got = append(got, m)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	r := newRig(t, "")
	hook := filepath.Join(r.dir, "hook")
	os.WriteFile(hook, []byte(srv.URL+"/hook/SECRET-TOKEN\n"), 0o600)
	r.writeCfg("")
	body, _ := os.ReadFile(r.cfg)
	os.WriteFile(r.cfg, []byte(strings.Replace(string(body), "[[service]]", fmt.Sprintf("notify_url_file = %q\nnotify_format = \"json\"\n[[service]]", hook), 1)), 0o600)
	if rc := r.run("notify-test"); rc != 0 {
		t.Fatalf("code %d : %s", rc, r.out.String())
	}
	if len(got) != 1 || got[0]["title"] != "docker-update : test" {
		t.Errorf("message reçu : %v", got)
	}
	if strings.Contains(r.out.String()+r.errOut.String(), "SECRET-TOKEN") {
		t.Error("l'URL du webhook est apparue dans la sortie")
	}
}

func TestBadConfigExitCode2(t *testing.T) {
	r := newRig(t, `mode = "turbo"`)
	if rc := r.run("validate"); rc != 2 {
		t.Errorf("code %d, attendu 2", rc)
	}
	if !strings.Contains(r.out.String(), "configuration : ") {
		t.Errorf("sortie : %s", r.out.String())
	}
}

func TestLockPreventsConcurrentRuns(t *testing.T) {
	r := newRig(t, "")
	held, err := os.OpenFile(filepath.Join(r.dir, "lock"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	calls := r.record()
	if rc := r.run("run"); rc != 0 {
		t.Errorf("code %d, attendu 0", rc)
	}
	if !strings.Contains(r.out.String(), "autre passage") {
		t.Errorf("sortie : %s", r.out.String())
	}
	if len(*calls) != 0 {
		t.Errorf("le cœur ne doit pas tourner quand le verrou est pris : %+v", *calls)
	}
	// relâché : le passage suivant a lieu
	held.Close()
	if rc := r.run("run"); rc != 0 || len(*calls) != 1 {
		t.Errorf("après libération : code %d, appels %v", rc, *calls)
	}
}

func TestValidateAndSetWebhookDoNotTakeTheLock(t *testing.T) {
	r := newRig(t, "")
	held, _ := os.OpenFile(filepath.Join(r.dir, "lock"), os.O_WRONLY|os.O_CREATE, 0o600)
	defer held.Close()
	syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	r.run("validate")
	if strings.Contains(r.out.String(), "autre passage") {
		t.Error("validate ne prend pas le verrou")
	}
}

func TestUntrustedConfigRefusedAsRoot(t *testing.T) {
	r := newRig(t, "")
	r.env.CheckTrusted = func(path string) error { return config.CheckTrusted(path, 0, 1000, 0o644) }
	if rc := Main([]string{"--config", r.cfg, "validate"}, r.env); rc != 2 {
		t.Errorf("code %d, attendu 2", rc)
	}
	if !strings.Contains(r.out.String(), "doit appartenir à root") {
		t.Errorf("sortie : %s", r.out.String())
	}
	// avec l'option réservée aux tests, la vérification est sautée
	r.out.Reset()
	if rc := r.run("validate"); rc == 2 {
		t.Errorf("--allow-untrusted-config doit sauter la vérification : %s", r.out.String())
	}
}

func TestValidate(t *testing.T) {
	d := t.TempDir()
	var cfg *config.Config
	toml := fmt.Sprintf("[settings]\nnotify_url_file = %q\n[[service]]\nname = \"a\"\ncompose_dir = %q\nservice = \"a\"\ncompose_files = [\"absent.yml\"]\n"+
		"[[service]]\nname = \"b\"\ncompose_dir = \"/nonexistent/dir\"\nservice = \"b\"\n", filepath.Join(d, "absent"), d)
	doc, err := tomlmini.Parse([]byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err = config.Parse(doc); err != nil {
		t.Fatal(err)
	}
	var lines []string
	rc := validate(cfg, func(m string) { lines = append(lines, m) })
	text := strings.Join(lines, "\n")
	if rc != 1 {
		t.Errorf("code %d, attendu 1", rc)
	}
	for _, want := range []string{"introuvable", "absent.yml", "AVERTISSEMENT", "2 service(s), 2 unité(s) : "} {
		if !strings.Contains(text, want) {
			t.Errorf("sortie sans %q :\n%s", want, text)
		}
	}
}

// ── set-webhook ─────────────────────────────────────────────────────────────

const hookURL = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcDEF"

func (r *rig) webhookCfg() string {
	target := filepath.Join(r.dir, "etc", "discord_webhook")
	body, _ := os.ReadFile(r.cfg)
	os.WriteFile(r.cfg, []byte(strings.Replace(string(body), "[[service]]", fmt.Sprintf("notify_url_file = %q\n[[service]]", target), 1)), 0o600)
	return target
}

type neverRead struct{ t *testing.T }

func (n neverRead) Read([]byte) (int, error) {
	n.t.Error("l'entrée standard ne doit pas être lue en mode interactif")
	return 0, io.EOF
}

func TestInteractiveInputIsHidden(t *testing.T) {
	r := newRig(t, "")
	target := r.webhookCfg()
	r.env.Stdin = neverRead{t}
	r.env.IsTerminal = func(io.Reader) bool { return true }
	prompt := ""
	r.env.ReadHidden = func(p string) (string, error) { prompt = p; return hookURL, nil }
	if rc := r.run("set-webhook"); rc != 0 {
		t.Fatalf("code %d : %s", rc, r.out.String())
	}
	if !strings.Contains(prompt, "invisible") {
		t.Errorf("invite %q sans « invisible »", prompt)
	}
	got, _ := os.ReadFile(target)
	if string(got) != hookURL+"\n" {
		t.Errorf("fichier : %q", got)
	}
	if strings.Contains(r.out.String()+r.errOut.String(), "AbCdEfGh") {
		t.Error("l'URL est apparue dans la sortie")
	}
}

func TestSetWebhookReadsOnlyTheFirstLineOfStdin(t *testing.T) {
	r := newRig(t, "")
	target := r.webhookCfg()
	r.env.Stdin = strings.NewReader(hookURL + "\nautre chose\n")
	if rc := r.run("set-webhook"); rc != 0 {
		t.Fatalf("code %d : %s", rc, r.out.String())
	}
	got, _ := os.ReadFile(target)
	if string(got) != hookURL+"\n" {
		t.Errorf("fichier : %q", got)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("droits %o", info.Mode().Perm())
	}
}

func TestTheURLIsNeverACommandLineArgument(t *testing.T) {
	r := newRig(t, "")
	target := r.webhookCfg()
	r.env.Stdin = strings.NewReader("")
	if rc := r.run("set-webhook", hookURL); rc != 2 {
		t.Errorf("code %d, attendu 2", rc)
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("rien ne doit être écrit")
	}
	if strings.Contains(r.out.String()+r.errOut.String(), "AbCdEfGh") {
		t.Error("l'URL est répétée dans le message d'erreur")
	}
}

// ── ligne de commande ───────────────────────────────────────────────────────

func TestUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"sans commande":                        {},
		"commande inconnue":                    {"explose"},
		"option globale après la commande":     {"check", "--dry-run"},
		"option inconnue":                      {"--nope", "check"},
		"now sans nom":                         {"now", "--yes"},
		"hold sans nom":                        {"hold"},
		"argument en trop":                     {"status", "x"},
		"--config sans valeur":                 {"--config"},
		"option de sous-commande inconnue":     {"run", "--notify"},
		"valeur donnée à une option booléenne": {"--dry-run=1", "status"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "")
			if rc := Main(args, r.env); rc != 2 {
				t.Errorf("code %d, attendu 2", rc)
			}
			if !strings.Contains(r.errOut.String(), "docker-update") {
				t.Errorf("message d'usage absent : %q", r.errOut.String())
			}
		})
	}
}

func TestOptionPrefixesAndEqualsForm(t *testing.T) {
	r := newRig(t, "")
	calls := r.record()
	if rc := Main([]string{"--conf=" + r.cfg, "--allow", "check", "a", "--not"}, r.env); rc != 0 {
		t.Fatalf("code %d : %s%s", rc, r.out.String(), r.errOut.String())
	}
	if len(*calls) != 1 || !(*calls)[0].note {
		t.Errorf("appels : %+v", *calls)
	}
}

func TestHelpAndVersion(t *testing.T) {
	r := newRig(t, "")
	if rc := Main([]string{"-h"}, r.env); rc != 0 || !strings.Contains(r.out.String(), "Commandes :") {
		t.Errorf("-h : code %d, sortie %q", rc, r.out.String())
	}
	r.out.Reset()
	if rc := Main([]string{"version"}, r.env); rc != 0 || !strings.Contains(r.out.String(), "docker-update test") {
		t.Errorf("version : code %d, sortie %q", rc, r.out.String())
	}
}

func TestStateErrorExitsWith1(t *testing.T) {
	r := newRig(t, "")
	if err := os.MkdirAll(filepath.Join(r.dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.dir, "state", "state.json"), []byte("{pas du json"), 0o600)
	if rc := r.run("status"); rc != 1 {
		t.Errorf("code %d, attendu 1", rc)
	}
	if !strings.Contains(r.out.String(), "erreur : ") {
		t.Errorf("sortie : %s", r.out.String())
	}
}

func TestUnknownNameExitsWith1(t *testing.T) {
	r := newRig(t, "")
	if rc := r.run("hold", "inconnu"); rc != 1 {
		t.Errorf("code %d, attendu 1", rc)
	}
	if !strings.Contains(r.out.String(), "erreur : inconnu : inconnu") {
		t.Errorf("sortie : %s", r.out.String())
	}
}
