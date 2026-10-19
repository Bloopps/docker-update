// Package e2e lance le vrai binaire en sous-processus, avec un faux `docker` dans le PATH (testdata/fakedocker) et des
// serveurs HTTP locaux pour GitHub et le webhook. Rien ne touche un vrai Docker ni un vrai service distant.
package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var binDir string // contient `docker-update` et `docker` (le faux)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "du-e2e-")
	if err != nil {
		panic(err)
	}
	code := 1
	func() {
		defer os.RemoveAll(dir)
		for _, b := range []struct{ out, pkg string }{{"docker-update", "../cmd/docker-update"}, {"docker", "../testdata/fakedocker"}} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(dir, b.out), b.pkg)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "compilation de %s : %v\n%s", b.pkg, err, out)
				return
			}
		}
		binDir = dir
		code = m.Run()
	}()
	os.Exit(code)
}

func man(n int) string { return fmt.Sprintf("{\"schemaVersion\":2,\"n\":%d}\n", n) }

func dig(n int) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(man(n)))) }

func idOf(n int) string { return "id-" + dig(n)[7:19] }

type container struct {
	ID      string `json:"id"`
	ImageID string `json:"image_id"`
	Status  string `json:"status"`
}

type world struct {
	Registry       map[string]string            `json:"registry"`
	Images         map[string][]string          `json:"images"`
	Tags           map[string]string            `json:"tags"`
	By             map[string]string            `json:"by"`
	Containers     map[string]*container        `json:"containers"`
	Compose        map[string]map[string]string `json:"compose"`
	FailUp         []string                     `json:"fail_up"`
	Labels         map[string]map[string]string `json:"labels"`
	DigestVersions map[string]string            `json:"digest_versions"`
	Log            [][]string                   `json:"log"`
	Removed        []string                     `json:"removed,omitempty"`
}

type rig struct {
	t         *testing.T
	tmp, dir  string
	shimState string
	state     string
	cfg       string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	tmp := t.TempDir()
	r := &rig{t: t, tmp: tmp, dir: filepath.Join(tmp, "traefik"), shimState: filepath.Join(tmp, "world.json"),
		state: filepath.Join(tmp, "state.json"), cfg: filepath.Join(tmp, "updates.toml")}
	if err := os.Mkdir(r.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.writeWorld(2, nil, "", "")
	r.writeCfg("0", "")
	return r
}

func (r *rig) writeFile(path, content string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) writeCfg(cooldown, extra string) {
	r.writeFile(r.cfg, fmt.Sprintf(`[settings]
state_file = %q
lock_file = %q
settle_seconds = 0
[[service]]
name = "traefik"
compose_dir = %q
service = "traefik"
mode = "auto"
cooldown = %q
%s
`, r.state, r.tmp+"/lock", r.dir, cooldown, extra))
}

// writeWorld remet le monde dans l'état de départ : traefik tourne sur id-old, le registre est à la révision `remote`.
func (r *rig) writeWorld(remote int, failUp []string, oldVersion, newVersion string) {
	w := world{
		Registry:   map[string]string{"traefik:v3.6": man(remote)},
		Images:     map[string][]string{"id-old": {"traefik@" + dig(1)}},
		Tags:       map[string]string{"traefik:v3.6": "id-old"},
		By:         map[string]string{},
		Containers: map[string]*container{r.dir + "|traefik": {ID: "cid1", ImageID: "id-old", Status: "running"}},
		Compose:    map[string]map[string]string{r.dir: {"traefik": "traefik:v3.6"}},
		FailUp:     failUp,
		Labels:     map[string]map[string]string{},
	}
	if oldVersion != "" {
		w.Labels["id-old"] = map[string]string{"org.opencontainers.image.version": oldVersion}
	}
	if newVersion != "" {
		w.DigestVersions = map[string]string{dig(remote): newVersion}
	}
	r.saveWorld(&w)
}

func (r *rig) saveWorld(w *world) {
	b, err := json.Marshal(w)
	if err != nil {
		r.t.Fatal(err)
	}
	r.writeFile(r.shimState, string(b))
}

func (r *rig) world() *world {
	r.t.Helper()
	b, err := os.ReadFile(r.shimState)
	if err != nil {
		r.t.Fatal(err)
	}
	var w world
	if err := json.Unmarshal(b, &w); err != nil {
		r.t.Fatal(err)
	}
	return &w
}

func (r *rig) image() string { return r.world().Containers[r.dir+"|traefik"].ImageID }

// du lance le programme ; sortie standard et erreur sont réunies (comme stdout + stderr dans les messages d'échec).
func (r *rig) du(args ...string) (int, string) { return r.duIn("", args...) }

func (r *rig) duIn(stdin string, args ...string) (int, string) {
	r.t.Helper()
	cmd := exec.Command(filepath.Join(binDir, "docker-update"), append([]string{"--config", r.cfg, "--allow-untrusted-config"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "SHIM_STATE="+r.shimState)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			r.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return code, string(out)
}

func (r *rig) ok(args ...string) string {
	r.t.Helper()
	code, out := r.du(args...)
	if code != 0 {
		r.t.Fatalf("%v : code %d\n%s", args, code, out)
	}
	return out
}

func (r *rig) wantImage(want string, why string) {
	r.t.Helper()
	if got := r.image(); got != want {
		r.t.Fatalf("image = %s, attendu %s (%s)", got, want, why)
	}
}

func (r *rig) pulls() [][]string { return r.logWhere(func(c []string) bool { return c[0] == "pull" }) }

func (r *rig) logWhere(f func([]string) bool) [][]string {
	var out [][]string
	for _, c := range r.world().Log {
		if len(c) > 0 && f(c) {
			out = append(out, c)
		}
	}
	return out
}

func mustContain(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Fatalf("%q absent de :\n%s", w, text)
		}
	}
}

func mustNotContain(t *testing.T, text string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(text, n) {
			t.Fatalf("%q ne doit pas apparaître dans :\n%s", n, text)
		}
	}
}

func readState(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// ── Cycle de vie ──

func TestFullLife(t *testing.T) {
	r := newRig(t)
	mustContain(t, r.ok("validate"), "OK")

	out := r.ok("--dry-run", "run") // simulation : rien ne bouge
	mustContain(t, out, "[simulation]")
	r.wantImage("id-old", "simulation")
	if exists(r.state) {
		t.Fatal("la simulation a écrit l'état")
	}

	r.writeCfg("1d", "")
	mustContain(t, r.ok("check"), "différée") // observe seulement
	r.wantImage("id-old", "check")
	if _, ok := readState(t, r.state)["images"].(map[string]any)["traefik:v3.6"]; !ok {
		t.Fatal("check n'a pas mémorisé l'image")
	}

	r.ok("run") // délai pas écoulé : rien
	r.wantImage("id-old", "délai")

	_, out = r.du("now", "traefik") // sans --yes : simulation
	r.wantImage("id-old", "now sans --yes")
	mustContain(t, out, "--yes")

	r.ok("now", "traefik", "--yes") // voie rapide
	r.wantImage(idOf(2), "now --yes")
	if got := r.pulls(); len(got) != 1 || !slices.Equal(got[0], []string{"pull", "traefik@" + dig(2)}) {
		t.Fatalf("pulls = %v", got)
	}

	out = r.ok("run") // tout est à jour
	mustContain(t, out, "à jour")
	mustNotContain(t, out, "hunter2") // le contenu de compose config ne sort jamais
}

func TestOldImageIsRemovedAfterASuccessfulUpdateOnly(t *testing.T) {
	r := newRig(t)
	out := r.ok("run")
	if got := r.world().Removed; !slices.Equal(got, []string{"id-old"}) {
		t.Fatalf("supprimées = %v", got)
	}
	mustContain(t, out, "nettoyage : image précédente")
	r.writeWorld(3, []string{idOf(3)}, "", "")
	if code, _ := r.du("run"); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if got := r.world().Removed; len(got) != 0 {
		t.Fatalf("échec : rien n'est supprimé, supprimées = %v", got)
	}
}

func TestCleanupCanBeTurnedOff(t *testing.T) {
	r := newRig(t)
	r.writeCfg("0", "cleanup = false")
	r.ok("run")
	r.wantImage(idOf(2), "mise à jour")
	if got := r.world().Removed; len(got) != 0 {
		t.Fatalf("supprimées = %v", got)
	}
}

func TestCheckAndRunAcceptNames(t *testing.T) {
	r := newRig(t)
	mustContain(t, r.ok("check", "traefik"), "scan limité à : traefik")
	code, out := r.du("check", "inconnu")
	if code != 1 {
		t.Fatalf("code = %d", code)
	}
	mustContain(t, out, "inconnu : inconnu")
	r.wantImage("id-old", "check n'applique rien")
	r.ok("run", "traefik")
	r.wantImage(idOf(2), "run NOM")
}

func TestStoppedContainerIsLeftAlone(t *testing.T) {
	r := newRig(t)
	w := r.world()
	w.Containers[r.dir+"|traefik"].Status = "exited"
	r.saveWorld(w)
	for _, args := range [][]string{{"run"}, {"check"}, {"now", "traefik", "--yes"}} {
		r.ok(args...)
	}
	mustContain(t, r.ok("run"), "conteneur arrêté (exited), non touché")
	r.wantImage("id-old", "conteneur arrêté")
	w = r.world()
	if s := w.Containers[r.dir+"|traefik"].Status; s != "exited" {
		t.Fatalf("statut = %s", s)
	}
	touched := r.logWhere(func(c []string) bool {
		return c[0] == "pull" || c[0] == "tag" || (len(c) > 1 && c[0] == "image" && c[1] == "rm") || slices.Contains(c, "up")
	})
	if len(touched) != 0 {
		t.Fatalf("commandes qui modifient : %v", touched)
	}
	if reg := r.logWhere(func(c []string) bool { return c[0] == "buildx" }); len(reg) != 0 {
		t.Fatalf("le registre n'est même pas interrogé : %v", reg)
	}
}

func TestFailureAlertsWithoutRollbackAndExitsWith1(t *testing.T) {
	r := newRig(t)
	r.writeWorld(2, []string{idOf(2)}, "", "")
	code, out := r.du("run")
	if code != 1 {
		t.Fatalf("code = %d\n%s", code, out)
	}
	mustContain(t, out, "Aucun retour arrière automatique")
	ups := r.logWhere(func(c []string) bool { return c[0] == "compose" && slices.Contains(c, "up") })
	if len(ups) != 1 {
		t.Fatalf("un seul `up` : pas de second pour revenir en arrière, obtenu %d", len(ups))
	}
	if got := r.world().Tags["traefik:v3.6"]; got != idOf(2) {
		t.Fatalf("l'étiquette est revenue en arrière : %s", got)
	}
	code, out = r.du("run") // pas de nouvelle tentative sur le même digest
	mustContain(t, out, "déjà tentée")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
}

func TestHoldThenStatus(t *testing.T) {
	r := newRig(t)
	r.ok("hold", "traefik")
	mustContain(t, r.ok("run"), "en pause")
	r.wantImage("id-old", "pause")
	mustContain(t, r.ok("status"), "traefik")
	r.ok("unhold", "traefik")
	r.ok("run")
	r.wantImage(idOf(2), "après unhold")
}

func TestBadConfigAndUnknownName(t *testing.T) {
	r := newRig(t)
	r.writeCfg("0", "oups = 1")
	if code, _ := r.du("validate"); code != 2 {
		t.Fatalf("code = %d", code)
	}
	r.writeCfg("0", "")
	if code, _ := r.du("now", "inconnu", "--yes"); code != 1 {
		t.Fatalf("code = %d", code)
	}
}

// ── Serveur HTTP local (GitHub et webhook) ──

type server struct {
	*httptest.Server
	mu    sync.Mutex
	gets  []string
	posts []map[string]any
}

func (s *server) Gets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.gets...)
}

func (s *server) Posts() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.posts...)
}

// serve répond `body` (code 200) ou `{}` (autre code) à tout GET dont le chemin contient `only` ; 404 sinon.
func serve(t *testing.T, only string, body any, code int) *server {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			s.mu.Lock()
			s.gets = append(s.gets, req.URL.RequestURI())
			s.mu.Unlock()
			if !strings.Contains(req.URL.Path, only) {
				http.Error(w, "{}", http.StatusNotFound)
				return
			}
			if code != 200 {
				w.WriteHeader(code)
				io.WriteString(w, "{}")
				return
			}
			w.Write(data)
		case http.MethodPost:
			var m map[string]any
			b, _ := io.ReadAll(req.Body)
			json.Unmarshal(b, &m)
			s.mu.Lock()
			s.posts = append(s.posts, m)
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func threeHoursAgo() string { return time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339) }

// ── Avis de sécurité ──

func advisoryServer(t *testing.T) *server {
	return serve(t, "/security-advisories", []any{map[string]any{
		"ghsa_id": "GHSA-2222-3333-4444", "cve_id": "CVE-2026-12345", "severity": "high",
		"published_at": threeHoursAgo(), "summary": "Contournement d'authentification",
		"vulnerabilities": []any{map[string]any{"vulnerable_version_range": "< 3.6.3"}},
	}}, 200)
}

func (r *rig) cfgAdvisories(api string) {
	r.writeFile(r.cfg, fmt.Sprintf(`[settings]
state_file = %q
lock_file = %q
settle_seconds = 0
github_api = %q
[[service]]
name = "traefik"
compose_dir = %q
service = "traefik"
mode = "auto"
cooldown = "3d"
advisories = ["acme/proxy"]
security_cooldown = "0"
`, r.state, r.tmp+"/lock", api, r.dir))
}

func TestAdvisoryConfirmedFixIsAppliedDespiteTheLongCooldown(t *testing.T) {
	r := newRig(t)
	srv := advisoryServer(t)
	r.writeWorld(2, nil, "3.6.2", "3.6.3")
	r.cfgAdvisories(srv.URL)
	mustContain(t, r.ok("run"), "CORRECTIF DE SÉCURITÉ")
	r.wantImage(idOf(2), "appliqué alors que le délai normal est de 3 jours")
	if got := srv.Gets(); !slices.Equal(got, []string{"/repos/acme/proxy/security-advisories?per_page=50"}) {
		t.Fatalf("requêtes = %v", got)
	}
}

func TestAdvisoryCandidateStillVulnerableWaitsTheNormalDelay(t *testing.T) {
	r := newRig(t)
	srv := advisoryServer(t)
	r.writeWorld(2, nil, "3.6.2", "3.6.2")
	r.cfgAdvisories(srv.URL)
	mustContain(t, r.ok("run"), "pas de correctif dans la nouvelle image")
	r.wantImage("id-old", "candidate encore touchée")
}

func TestAdvisoryCheckDoesNotPullEvenWithAnAdvisory(t *testing.T) {
	r := newRig(t)
	srv := advisoryServer(t)
	r.writeWorld(2, nil, "3.6.2", "3.6.3")
	r.cfgAdvisories(srv.URL)
	out := r.ok("check")
	if got := r.pulls(); len(got) != 0 {
		t.Fatalf("pulls = %v", got)
	}
	mustContain(t, out, "prochain passage")
}

func TestAdvisoryUnreachableAPIIsNotFatal(t *testing.T) {
	r := newRig(t)
	r.writeWorld(2, nil, "3.6.2", "3.6.3")
	r.cfgAdvisories("http://127.0.0.1:9")
	mustContain(t, r.ok("run"), "injoignable")
	r.wantImage("id-old", "sans avis, le délai normal s'applique")
}

// ── Notes de release ──

func rel(tag, body string, pre bool) map[string]any {
	return map[string]any{"tag_name": tag, "body": body, "published_at": threeHoursAgo(), "prerelease": pre, "draft": false}
}

func (r *rig) cfgReleases(api, cooldown, extra string) {
	hook := filepath.Join(r.tmp, "webhook")
	r.writeFile(hook, api+"/hook/SECRET-TOKEN\n")
	r.writeFile(r.cfg, fmt.Sprintf(`[settings]
state_file = %q
lock_file = %q
settle_seconds = 0
github_api = %q
notify_url_file = %q
[[service]]
name = "traefik"
compose_dir = %q
service = "traefik"
mode = "auto"
cooldown = %q
release_notes = true
release_notes_repos = ["acme/proxy"]
%s
`, r.state, r.tmp+"/lock", api, hook, r.dir, cooldown, extra))
}

func content(t *testing.T, p map[string]any) string {
	t.Helper()
	s, ok := p["content"].(string)
	if !ok {
		t.Fatalf("corps sans « content » : %v", p)
	}
	return s
}

func TestReleaseAlertIsSentOnceAndNothingIsApplied(t *testing.T) {
	r := newRig(t)
	srv := serve(t, "/releases", []any{
		rel("v3.6.3", "Fixes CVE-2026-12345 and a path traversal", false),
		rel("v3.6.1", "CVE-2020-0001 (older than the running version)", false),
		rel("v3.7.0-rc1", "CVE-2026-9999", true),
	}, 200)
	r.writeWorld(2, nil, "3.6.2", "3.6.3")
	r.cfgReleases(srv.URL, "3d", "")
	out := r.ok("run")
	if got := srv.Gets(); !slices.Equal(got, []string{"/repos/acme/proxy/releases?per_page=30"}) {
		t.Fatalf("requêtes = %v", got)
	}
	posts := srv.Posts()
	if len(posts) != 1 {
		t.Fatalf("%d messages, attendu 1\n%s", len(posts), out)
	}
	c := content(t, posts[0])
	mustContain(t, c, "3.6.2 → v3.6.3", "CVE-2026-12345", "path traversal", "https://github.com/acme/proxy/releases/tag/v3.6.3")
	mustNotContain(t, c, "CVE-2020-0001")
	if am, _ := json.Marshal(posts[0]["allowed_mentions"]); string(am) != `{"parse":[]}` {
		t.Fatalf("allowed_mentions = %s", am)
	}
	r.wantImage("id-old", "une note de release n'accélère rien : délai de 3 jours en cours")
	if got := r.pulls(); len(got) != 0 {
		t.Fatalf("pulls = %v", got)
	}
	mustNotContain(t, out, "SECRET-TOKEN")
	r.ok("run") // nouveau processus : l'état disque dédoublonne
	if n := len(srv.Posts()); n != 1 {
		t.Fatalf("%d messages après le second passage", n)
	}
	st := readState(t, r.state)
	if _, ok := st["alerts"].(map[string]any)["release:traefik:v3.6.3"]; !ok {
		t.Fatalf("alertes = %v", st["alerts"])
	}
	if n := len(st["releases"].(map[string]any)["acme/proxy"].(map[string]any)["items"].([]any)); n != 3 {
		t.Fatalf("%d releases mémorisées", n)
	}
}

func TestReleaseCheckReportsButSendsNothing(t *testing.T) {
	r := newRig(t)
	srv := serve(t, "/releases", []any{rel("v3.6.3", "Fixes CVE-2026-12345", false)}, 200)
	r.writeWorld(2, nil, "3.6.2", "")
	r.cfgReleases(srv.URL, "3d", "")
	mustContain(t, r.ok("check"), "notes de release traefik: version détectée 3.6.2", "ALERTE notes de release")
	if n := len(srv.Posts()); n != 0 {
		t.Fatalf("%d messages", n)
	}
}

func TestReleaseMissingRepositoryAndUnreachableAPIAreNotErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		api  string
	}{{"404", 404, ""}, {"500", 500, ""}, {"injoignable", 200, "http://127.0.0.1:9"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			api := tc.api
			if api == "" {
				api = serve(t, "/releases", []any{}, tc.code).URL
			}
			r.writeWorld(2, nil, "3.6.2", "")
			r.cfgReleases(api, "0", "")
			mustNotContain(t, r.ok("run"), "ERREUR")
			r.wantImage(idOf(2), "la mise à jour normale n'est jamais empêchée")
		})
	}
}

func TestReleaseUnreadableVersionGivesOneAlertAndNoRequest(t *testing.T) {
	r := newRig(t)
	srv := serve(t, "/releases", []any{rel("v3.6.3", "Fixes CVE-2026-12345", false)}, 200)
	r.writeWorld(2, nil, "", "") // image sans étiquette de version
	r.cfgReleases(srv.URL, "3d", "")
	r.ok("run")
	r.ok("run")
	posts := srv.Posts()
	if len(posts) != 1 {
		t.Fatalf("%d messages, attendu 1", len(posts))
	}
	mustContain(t, content(t, posts[0]), "Impossible de comparer")
	if got := srv.Gets(); len(got) != 0 {
		t.Fatalf("requêtes = %v", got)
	}
}

func TestReleaseValidateRefusesUnknownAndIncompleteKeys(t *testing.T) {
	r := newRig(t)
	r.writeCfg("0", "release_notes = true") // aucun dépôt à lire
	code, out := r.du("validate")
	if code != 2 {
		t.Fatalf("code = %d\n%s", code, out)
	}
	mustContain(t, out, "release_notes")
	r.writeCfg("0", "release_note = true") // faute de frappe
	if code, _ := r.du("validate"); code != 2 {
		t.Fatalf("code = %d", code)
	}
}

func TestOneReportUpdateAndReleaseAlertArriveInASinglePost(t *testing.T) {
	r := newRig(t)
	srv := serve(t, "/releases", []any{rel("v3.6.3", "Fixes CVE-2026-12345", false)}, 200)
	r.writeWorld(2, nil, "3.6.2", "3.6.3")
	r.cfgReleases(srv.URL, "0", "")
	out := r.ok("run")
	posts := srv.Posts()
	if len(posts) != 1 {
		t.Fatalf("UN seul message pour le passage, obtenu %d", len(posts))
	}
	mustContain(t, content(t, posts[0]), "1 mise à jour", "Mises à jour réussies", "Notes de release à lire", "CVE-2026-12345")
	mustNotContain(t, out, "SECRET-TOKEN")
	r.ok("run")
	if n := len(srv.Posts()); n != 1 {
		t.Fatalf("rien de nouveau : aucun message, obtenu %d", n)
	}
}

// ── set-webhook ──

func TestSetWebhookThroughStdin(t *testing.T) {
	r := newRig(t)
	const url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	target := filepath.Join(r.tmp, "conf", "discord_webhook")
	r.writeCfg("0", "")
	b, _ := os.ReadFile(r.cfg)
	r.writeFile(r.cfg, strings.Replace(string(b), "settle_seconds = 0", fmt.Sprintf("settle_seconds = 0\nnotify_url_file = %q", target), 1))
	code, out := r.duIn(url+"\n", "set-webhook")
	if code != 0 {
		t.Fatalf("code = %d\n%s", code, out)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != url+"\n" {
		t.Fatalf("fichier = %q (%v)", got, err)
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o600 {
		t.Fatalf("droits = %v", fi.Mode().Perm())
	}
	mustNotContain(t, out, "AbCdEfGh")
	if log := r.world().Log; len(log) != 0 {
		t.Fatalf("aucune commande docker lancée, obtenu %v", log)
	}
}
