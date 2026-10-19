package updater

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/fakeworld"
	"github.com/Bloopps/docker-update/internal/notify"
	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/tomlmini"
)

const day = 86400

// M : un dictionnaire de configuration (service ou réglages).
type M = map[string]any

func manifest(n int) []byte { return fakeworld.Manifest(n) }
func dg(n int) string       { return fakeworld.DG(n) }

// id : identifiant de l'image que le faux « docker pull » donne au digest n.
func id(n int) string { return "id-" + dg(n)[7:19] }

type clock struct{ t int64 }

func (c *clock) now() int64 { return c.t }
func (c *clock) days(n float64) {
	c.t += int64(math.Round(n * day))
}

type post struct {
	url     string
	body    []byte
	headers map[string]string
}

// content : champ « content » d'un message Discord.
func (p post) content() string {
	var m map[string]any
	if err := json.Unmarshal(p.body, &m); err != nil {
		return string(p.body)
	}
	s, _ := m["content"].(string)
	return s
}

type ghReply struct {
	code int
	body any
	etag string
}

type ghCall struct{ url, token, etag string }

// env : l'équivalent de la classe Base des tests Python.
type env struct {
	t         *testing.T
	tmp       string
	logs      []string
	posts     []post
	clock     *clock
	sleeps    []time.Duration
	world     *fakeworld.World
	minutes   int
	httpCodes map[string]int
	urlFile   string
	statePath string
	gh, ghRel map[string]ghReply
	ghCalls   []ghCall
}

func newBase(t *testing.T) *env {
	t.Helper()
	b := &env{t: t, tmp: t.TempDir(), clock: &clock{t: 1_000_000}, world: fakeworld.New(), minutes: 180,
		httpCodes: map[string]int{}, gh: map[string]ghReply{}, ghRel: map[string]ghReply{}}
	b.urlFile = filepath.Join(b.tmp, "notify_url")
	if err := os.WriteFile(b.urlFile, []byte("https://discord.test/api/webhooks/1/SECRET-TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.statePath = filepath.Join(b.tmp, "state", "state.json")
	return b
}

func (b *env) log(m string) { b.logs = append(b.logs, m) }

func (b *env) logtext() string { return strings.Join(b.logs, "\n") }

func (b *env) ghGet(url, token, etag string) (int, any, string) {
	b.ghCalls = append(b.ghCalls, ghCall{url, token, etag})
	var rep ghReply
	var ok bool
	if strings.Contains(url, "/releases?") {
		repo := strings.SplitN(strings.SplitN(url, "/repos/", 2)[1], "/releases", 2)[0]
		rep, ok = b.ghRel[repo]
	} else {
		repo := strings.SplitN(strings.SplitN(url, "/repos/", 2)[1], "/security-advisories", 2)[0]
		rep, ok = b.gh[repo]
	}
	if !ok {
		rep = ghReply{code: 404}
	}
	if rep.code == 200 && etag != "" && etag == rep.etag {
		return 304, nil, rep.etag
	}
	return rep.code, rep.body, rep.etag
}

// mk : options de make.
type mk struct {
	Settings M
	Tool     string
	Dry      bool
	NoSave   bool
	Fmt      string
}

func tomlValue(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []int:
		parts := make([]string, len(x))
		for i, n := range x {
			parts[i] = strconv.Itoa(n)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	panic(fmt.Sprintf("type TOML non géré : %T", v))
}

func tomlTable(m M) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k + " = " + tomlValue(m[k]) + "\n")
	}
	return sb.String()
}

// tryConfig construit une configuration comme parse_config(dict) en Python.
func tryConfig(settings M, services []M) (*config.Config, error) {
	var sb strings.Builder
	sb.WriteString("[settings]\n" + tomlTable(settings))
	for _, s := range services {
		sb.WriteString("\n[[service]]\n" + tomlTable(s))
	}
	doc, err := tomlmini.Parse([]byte(sb.String()))
	if err != nil {
		return nil, fmt.Errorf("TOML de test invalide : %w\n%s", err, sb.String())
	}
	return config.Parse(doc)
}

func parseConfig(t *testing.T, settings M, services []M) *config.Config {
	t.Helper()
	cfg, err := tryConfig(settings, services)
	if err != nil {
		t.Fatalf("configuration de test invalide : %v", err)
	}
	return cfg
}

func (b *env) make(services []M, o ...mk) *Updater {
	b.t.Helper()
	var opt mk
	if len(o) > 0 {
		opt = o[0]
	}
	if opt.Tool == "" {
		opt.Tool = "buildx"
	}
	if opt.Fmt == "" {
		opt.Fmt = "discord"
	}
	st := M{"state_file": b.statePath, "lock_file": filepath.Join(b.tmp, "lock"), "notify_url_file": b.urlFile,
		"notify_format": opt.Fmt, "registry_tool": opt.Tool, "settle_seconds": 7}
	for k, v := range opt.Settings {
		st[k] = v
	}
	cfg := parseConfig(b.t, st, services)
	b.world.Dry = opt.Dry
	notifier := notify.New(b.urlFile, opt.Fmt, b.log, opt.Dry)
	notifier.Post = func(url string, body []byte, headers map[string]string) error {
		b.posts = append(b.posts, post{url, body, headers})
		return nil
	}
	s, err := state.Load(b.statePath)
	if err != nil {
		b.t.Fatal(err)
	}
	return New(Deps{Cfg: cfg, Run: b.world, Notifier: notifier, State: s, Now: b.clock.now,
		Sleep: func(d time.Duration) { b.sleeps = append(b.sleeps, d) }, Log: b.log,
		HTTP: func(url string) int { return b.httpCodes[url] }, Minutes: func() int { return b.minutes },
		NoSave: opt.NoSave, GHGet: b.ghGet})
}

// svc : un service « traefik » par défaut, complété par kw.
func svc(kw M) M { return svcN("traefik", "/stack/traefik", "", kw) }

func svcN(name, dir, service string, kw M) M {
	if service == "" {
		service = name
	}
	d := M{"name": name, "compose_dir": dir, "service": service, "mode": "auto"}
	for k, v := range kw {
		d[k] = v
	}
	return d
}

func (b *env) traefik() string {
	return b.world.Add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1)
}

func (b *env) newRemote(n int) { b.world.Registry["traefik:v3.6"] = manifest(n) }

func (b *env) cmds(prefix ...string) [][]string { return b.world.Cmds(prefix...) }

// ── assertions ──────────────────────────────────────────────────────────────

func (b *env) eq(got, want any, msg ...string) {
	b.t.Helper()
	if !reflect.DeepEqual(got, want) {
		b.t.Errorf("obtenu %#v, attendu %#v %s", got, want, strings.Join(msg, " "))
	}
}

func (b *env) in(needle, hay string, msg ...string) {
	b.t.Helper()
	if !strings.Contains(hay, needle) {
		b.t.Errorf("%q absent de :\n%s\n%s", needle, hay, strings.Join(msg, " "))
	}
}

func (b *env) notIn(needle, hay string, msg ...string) {
	b.t.Helper()
	if strings.Contains(hay, needle) {
		b.t.Errorf("%q présent dans :\n%s\n%s", needle, hay, strings.Join(msg, " "))
	}
}

func (b *env) regex(pattern, hay string) {
	b.t.Helper()
	if !regexp.MustCompile(pattern).MatchString(hay) {
		b.t.Errorf("%q ne correspond pas à :\n%s", pattern, hay)
	}
}

func (b *env) ok(cond bool, msg ...string) {
	b.t.Helper()
	if !cond {
		b.t.Errorf("condition fausse : %s", strings.Join(msg, " "))
	}
}

// cycle lance un passage et échoue le test sur une erreur inattendue.
func (b *env) cycle(u *Updater, apply bool, names ...string) int {
	b.t.Helper()
	rc, err := u.Cycle(apply, names, false)
	if err != nil {
		b.t.Fatalf("Cycle : %v", err)
	}
	return rc
}

func (b *env) nowCmd(u *Updater, yes bool, names ...string) int {
	b.t.Helper()
	rc, err := u.NowCmd(names, yes)
	if err != nil {
		b.t.Fatalf("NowCmd : %v", err)
	}
	return rc
}

func (b *env) hold(u *Updater, on bool, names ...string) {
	b.t.Helper()
	if _, err := u.Hold(names, on); err != nil {
		b.t.Fatalf("Hold : %v", err)
	}
}

func (b *env) allPosts() string {
	var parts []string
	for _, p := range b.posts {
		parts = append(parts, p.content())
	}
	return strings.Join(parts, " ")
}

func ups(w *fakeworld.World) []string {
	out := []string{}
	for _, u := range w.Ups {
		out = append(out, u.Service)
	}
	return out
}

func (b *env) containerImage(dir, service string) string {
	return b.world.Containers[fakeworld.Key{Dir: dir, Service: service}].ImageID
}

// wrapRunner remplace Run d'un monde simulé (les autres méthodes restent celles du monde).
type wrapRunner struct {
	*fakeworld.World
	run func(argv []string, mutating bool, timeout time.Duration) dockerx.Result
}

func (w wrapRunner) Run(argv []string, mutating bool, timeout time.Duration) dockerx.Result {
	return w.run(argv, mutating, timeout)
}

func regexpMatch(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) }
