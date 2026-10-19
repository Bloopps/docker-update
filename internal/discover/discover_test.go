package discover

import (
	"strings"
	"testing"
	"time"

	"github.com/Bloopps/docker-update/internal/dockerx"
)

// stub répond aux commandes par leur ligne ; toute autre commande est un échec du test (jamais de vrai docker).
type stub struct {
	t     *testing.T
	reply map[string]dockerx.Result
	seen  []string
}

func (s *stub) Run(argv []string, mutating bool, _ time.Duration) dockerx.Result {
	line := strings.Join(argv, " ")
	s.seen = append(s.seen, line)
	if mutating {
		s.t.Fatalf("commande qui modifie, interdite en lecture seule : %s", line)
	}
	if r, ok := s.reply[line]; ok {
		return r
	}
	for k, r := range s.reply {
		if strings.HasSuffix(k, "*") && strings.HasPrefix(line, strings.TrimSuffix(k, "*")) {
			return r
		}
	}
	s.t.Fatalf("commande inattendue : %s", line)
	return dockerx.Result{}
}
func (s *stub) RunBytes([]string, time.Duration) (int, []byte, string) {
	s.t.Fatal("RunBytes")
	return 0, nil, ""
}
func (s *stub) DryRun() bool { return false }

func row(id, name, image, project, service, dir, files string) string {
	return strings.Join([]string{id, "/" + name, image, project, service, dir, files}, "\x1f")
}

func TestRunningKeepsOnlyComposeContainersSortedAndReadsLabelsOnly(t *testing.T) {
	out := strings.Join([]string{
		row("c2", "web-1", "traefik:v3.6", "stack", "web", "/srv/stack", "/srv/stack/compose.yml,/srv/stack/o.yml"),
		row("c3", "manual", "alpine", "", "", "", ""), // hors compose : ignoré
		row("c1", "db-1", "postgres:17", "stack", "db", "/srv/stack", "/srv/stack/compose.yml,/srv/stack/o.yml"),
		row("c4", "a-1", "x/y", "alpha", "a", "/srv/a", "/srv/a/compose.yaml"),
	}, "\n") + "\n"
	run := &stub{t: t, reply: map[string]dockerx.Result{
		"docker ps -q":              {Out: "c1\nc2\nc3\nc4\n"},
		"docker inspect --format *": {Out: out},
	}}
	got, err := Running(run)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Project+"/"+c.Service)
	}
	if strings.Join(names, " ") != "alpha/a stack/db stack/web" {
		t.Fatalf("conteneurs = %v", names)
	}
	if got[1].Name != "db-1" || got[2].Image != "traefik:v3.6" || len(got[2].ConfigFiles) != 2 {
		t.Fatalf("champs : %+v", got)
	}
	inspect := run.seen[1]
	if strings.Contains(inspect, "Config.Env") || strings.Contains(inspect, " inspect c1") || !strings.Contains(inspect, "--format") {
		t.Fatalf("docker inspect ne doit lire que les champs utiles (jamais l'environnement) : %s", inspect)
	}
}

func TestRunningWithNothingOrErrors(t *testing.T) {
	run := &stub{t: t, reply: map[string]dockerx.Result{"docker ps -q": {}}}
	if got, err := Running(run); err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	run = &stub{t: t, reply: map[string]dockerx.Result{"docker ps -q": {RC: 1, Err: "Cannot connect to the Docker daemon\n"}}}
	if _, err := Running(run); err == nil || !strings.Contains(err.Error(), "docker ps a échoué") {
		t.Fatalf("%v", err)
	}
}

func TestComposeFilesAreOmittedOnlyForASingleDefaultFile(t *testing.T) {
	cases := []struct {
		files []string
		want  string
	}{
		{[]string{"/srv/a/compose.yml"}, ""},
		{[]string{"/srv/a/docker-compose.yaml"}, ""},
		{[]string{"/srv/a/compose.yml", "/srv/a/compose.traefik.yml"}, "/srv/a/compose.yml,/srv/a/compose.traefik.yml"},
		{[]string{"/srv/a/custom.yml"}, "/srv/a/custom.yml"},
		{[]string{"/elsewhere/compose.yml"}, "/elsewhere/compose.yml"},
	}
	for _, c := range cases {
		if got := strings.Join(ComposeFiles(Container{WorkingDir: "/srv/a", ConfigFiles: c.files}), ","); got != c.want {
			t.Fatalf("%v : %q, attendu %q", c.files, got, c.want)
		}
	}
}

func TestVerify(t *testing.T) {
	c := Container{ID: "abcdef123456", Project: "stack", Service: "web", WorkingDir: "/srv/stack", ConfigFiles: []string{"/srv/stack/a.yml", "/srv/stack/b.yml"}}
	base := "docker compose --project-directory /srv/stack -f /srv/stack/a.yml -f /srv/stack/b.yml"
	secret := `{"name":"stack","services":{"web":{"environment":{"TOKEN":"hunter2"}}}}`
	ok := map[string]dockerx.Result{base + " config --format json": {Out: secret}, base + " ps -q --all web": {Out: "abcdef123456\n"}}
	if why, good := Verify(&stub{t: t, reply: ok}, c); !good {
		t.Fatalf("devrait passer : %s", why)
	}
	bad := []struct {
		label string
		reply map[string]dockerx.Result
		want  string
	}{
		{"autre projet", map[string]dockerx.Result{base + " config --format json": {Out: `{"name":"autre","environment":{"T":"hunter2"}}`}}, "n'est pas celui du conteneur"},
		{"config en échec", map[string]dockerx.Result{base + " config --format json": {RC: 1, Err: "boom"}}, "config a échoué"},
		{"config illisible", map[string]dockerx.Result{base + " config --format json": {Out: "pas du json hunter2"}}, "pas de nom de projet"},
		{"autre conteneur", map[string]dockerx.Result{base + " config --format json": {Out: secret}, base + " ps -q --all web": {Out: "ffffff\n"}}, "ne désigne pas ce conteneur"},
		{"aucun conteneur", map[string]dockerx.Result{base + " config --format json": {Out: secret}, base + " ps -q --all web": {}}, "ne désigne pas ce conteneur"},
	}
	for _, b := range bad {
		why, good := Verify(&stub{t: t, reply: b.reply}, c)
		if good || !strings.Contains(why, b.want) {
			t.Fatalf("%s : %v %q", b.label, good, why)
		}
		if strings.Contains(why, "hunter2") {
			t.Fatalf("%s : la sortie de config ne doit jamais apparaître : %q", b.label, why)
		}
	}
}

func TestVersionAndRepoAndSuggestions(t *testing.T) {
	run := &stub{t: t, reply: map[string]dockerx.Result{
		"docker image inspect --format {{json .Config.Labels}} traefik:v3.6": {Out: `{"org.opencontainers.image.version":"3.6.2"}` + "\n"},
		"docker image inspect --format {{json .Config.Labels}} nolabel":      {Out: "null\n"},
		"docker image inspect --format {{json .Config.Labels}} gone":         {RC: 1},
	}}
	if v := Version(run, "traefik:v3.6", "org.opencontainers.image.version"); v != "3.6.2" {
		t.Fatalf("version = %q", v)
	}
	if v := Version(run, "nolabel", "org.opencontainers.image.version"); v != "" {
		t.Fatalf("version = %q", v)
	}
	if v := Version(run, "gone", "x"); v != "" {
		t.Fatalf("version = %q", v)
	}
	for in, want := range map[string][2]string{
		"traefik:latest":                      {"", "traefik"},
		"ghcr.io/immich-app/immich-server:v3": {"ghcr.io", "immich-app/immich-server"},
		"localhost:5000/x/y@sha256:abc":       {"localhost:5000", "x/y"},
		"portainer/portainer-ce":              {"", "portainer/portainer-ce"},
		"registry.example.com:443/a/b:1":      {"registry.example.com:443", "a/b"},
	} {
		if r, p := Repo(in); r != want[0] || p != want[1] {
			t.Fatalf("Repo(%q) = %q %q", in, r, p)
		}
	}
	for in, want := range map[string]struct {
		repo string
		sure bool
	}{
		"traefik:v3.6":                         {"traefik/traefik", true},
		"ghcr.io/immich-app/immich-server:v3":  {"immich-app/immich", true},
		"ghcr.io/theduffman85/crowdsec-web-ui": {"theduffman85/crowdsec-web-ui", false},
		"portainer/portainer-ce:latest":        {"portainer/portainer", true},
		"docker.io/unknown/thing":              {"", false},
		"ghcr.io/a/b/c":                        {"", false},
	} {
		if r, sure := SuggestRepo(in); r != want.repo || sure != want.sure {
			t.Fatalf("SuggestRepo(%q) = %q %v", in, r, sure)
		}
	}
}

func TestNeverFollowByDefault(t *testing.T) {
	for _, image := range []string{"containrrr/watchtower", "ghcr.io/containrrr/watchtower:latest", "tecnativa/docker-socket-proxy", "postgres:17-alpine",
		"ghcr.io/immich-app/postgres:18-vectorchord0.5.3", "valkey/valkey:8", "mariadb:11", "redis:7", "mongo", "bitnami/postgresql:16", "mysql:8"} {
		if why, never := Never(image); !never || why == "" {
			t.Fatalf("%s devrait être écarté par défaut", image)
		}
	}
	for _, image := range []string{"traefik:v3", "ghcr.io/gethomepage/homepage", "henrygd/beszel", "nginx", "prometheuscommunity/postgres-exporter", "redisinsight/redisinsight"} {
		if why, never := Never(image); never {
			t.Fatalf("%s ne devrait pas être écarté (%s)", image, why)
		}
	}
}
