package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bloopps/docker-update/internal/tomlmini"
)

const day = 86400

func parse(t *testing.T, src string) (*Config, error) {
	t.Helper()
	doc, err := tomlmini.Parse([]byte(src))
	if err != nil {
		t.Fatalf("TOML de test invalide : %v\n%s", err, src)
	}
	return Parse(doc)
}

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := parse(t, src)
	if err != nil {
		t.Fatalf("Parse : %v\n%s", err, src)
	}
	return cfg
}

// one construit un [[service]] minimal, avec des lignes en plus.
func one(extra ...string) string {
	return "[[service]]\nname = \"a\"\ncompose_dir = \"/x\"\nservice = \"a\"\n" + strings.Join(extra, "\n") + "\n"
}

func TestValidAndDefaults(t *testing.T) {
	cfg := mustParse(t, "[settings]\ndefault_cooldown = \"2d\"\n[[service]]\nname = \"a\"\ncompose_dir = \"/x\"\nservice = \"web\"\ncompose_files = [\"c.yml\", \"/abs/o.yml\"]\n")
	s := cfg.Services[0]
	if s.Cooldown != 2*day {
		t.Errorf("cooldown = %d", s.Cooldown)
	}
	if s.Mode != "notify" {
		t.Errorf("le plus prudent par défaut : %q", s.Mode)
	}
	if !slices.Equal(s.ComposeFiles, []string{"/x/c.yml", "/abs/o.yml"}) {
		t.Errorf("compose_files = %v", s.ComposeFiles)
	}
	d := DefaultSettings()
	if empty := mustParse(t, "").Settings; empty != d {
		t.Errorf("réglages par défaut modifiés : %+v", empty)
	}
	if d.StateFile != "/var/lib/docker-update/state.json" || d.LockFile != "/run/docker-update.lock" || d.SettleSeconds != 15 ||
		d.HealthTimeout != 180 || d.AdvisoryPoll != 3600 || d.AdvisoryLookback != 30*day || !d.Cleanup || d.DefaultCooldown != 3*day {
		t.Errorf("valeurs par défaut : %+v", d)
	}
	if s.HealthTimeout != 180 || s.SecurityCooldown != 6*3600 || s.SecurityPolicy != "fast" || s.MinSeverity != "high" || !s.Cleanup {
		t.Errorf("service par défaut : %+v", s)
	}
	if empty := mustParse(t, ""); len(empty.Services) != 0 {
		t.Errorf("configuration vide : %+v", empty)
	}
}

func TestUnitAndRelativeFilesJoin(t *testing.T) {
	cfg := mustParse(t, "[[service]]\nname = \"a\"\ncompose_dir = \"/x/\"\nservice = \"a\"\ncompose_files = [\"c.yml\", \"\"]\ngroup = \"g\"\n")
	s := cfg.Services[0]
	if s.Unit() != "g" || !slices.Equal(s.ComposeFiles, []string{"/x/c.yml", "/x/"}) {
		t.Errorf("%q %v", s.Unit(), s.ComposeFiles)
	}
	if u := mustParse(t, one()).Services[0].Unit(); u != "a" {
		t.Errorf("unité sans groupe : %q", u)
	}
}

func TestRejections(t *testing.T) {
	bad := []struct{ src, word string }{
		{"[settings]\nnope = 1\n", "inconnue"},
		{one("mode = \"fast\""), "mode"},
		{"[[service]]\nname = \"a\"\ncompose_dir = \"x\"\nservice = \"a\"\n", "absolu"},
		{"[[service]]\nname = \"a b\"\ncompose_dir = \"/x\"\nservice = \"a\"\n", "nom invalide"},
		{"[[service]]\nname = \"a\"\ncompose_dir = \"/x\"\nservice = \"x;rm\"\n", "invalide"},
		{one("typo = 1"), "inconnue"},
		{one("cooldown = \"3\""), "durée"},
		{one("pre_hook = [[\"/usr/local/sbin/avant\"]]"), "n'existe plus"}, // refusé avant même de lire la valeur
		{one("rollback = true"), "n'existe plus"},
		{one("rollback = false"), "n'existe plus"},
		{one("window = \"nuit\""), "fenêtre"},
		{one("smoke_url = \"ftp://x\""), "http"},
		{one("smoke_url = \"https://x\""), "smoke_expect"},
		{one("smoke_expect = [200]"), "sans smoke_url"},
		{one("health_timeout = 0"), "health_timeout"},
		{"[settings]\nregistry_tool = \"curl\"\n", "registry_tool"},
		{"[settings]\nnotify_format = \"sms\"\n", "notify_format"},
	}
	for _, c := range bad {
		if c.word == "n'existe plus" && strings.Contains(c.src, "[[\"") {
			continue // tableau imbriqué : non pris en charge par le lecteur TOML ; voir TestRemovedKeys
		}
		_, err := parse(t, c.src)
		if err == nil {
			t.Errorf("devrait échouer : %s", c.src)
		} else if !strings.Contains(err.Error(), c.word) {
			t.Errorf("%q : %v", c.word, err)
		}
	}
}

func TestRemovedKeys(t *testing.T) {
	// Les clés supprimées restent refusées, avec leur explication, dans [settings] comme dans [[service]].
	for _, c := range []struct{ src, want string }{
		{one("rollback = true"), "[[service]] n°1 : « rollback » n'existe plus (en cas d'échec l'outil alerte et ne touche à rien : aucun retour arrière automatique)"},
		{one("pre_hook = [\"x\"]"), "« pre_hook » n'existe plus (la sauvegarde se gère en dehors de l'outil)"},
		{"[settings]\nheartbeat_url_file = \"/x\"\n", "[settings] : « heartbeat_url_file » n'existe plus (seules les notifications Discord existent)"},
		{"[settings]\nrollback = true\n", "[settings] : « rollback » n'existe plus"},
	} {
		_, err := parse(t, c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s : %v ; attendu %q", c.src, err, c.want)
		}
	}
}

func TestMessagesAreThoseOfThePython(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"[settings]\nnope = 1\nzut = 2\n", "[settings] : clé(s) inconnue(s) ['nope', 'zut'] (faute de frappe ?)"},
		{one("mode = \"fast\""), "a : mode doit valoir ('auto', 'notify', 'manual')"},
		{"[settings]\nregistry_tool = \"curl\"\n", "registry_tool doit valoir ('buildx', 'regctl')"},
		{"[settings]\nnotify_format = \"sms\"\n", "notify_format doit valoir ('discord', 'ntfy', 'json')"},
		{one("min_severity = \"énorme\""), "a : min_severity doit valoir ('low', 'medium', 'high', 'critical')"},
		{one("security_policy = \"yolo\""), "a : security_policy doit valoir ('fast', 'alert')"},
		{one("cooldown = 3"), "durée invalide : 3 (exemples : 30m, 6h, 3d, 1w, 0)"},
		{one("cooldown = \"3\""), "durée invalide : '3' (exemples : 30m, 6h, 3d, 1w, 0)"},
		{"[[service]]\ncompose_dir = \"/x\"\nservice = \"a\"\n", "[[service]] n°1 : « name » est obligatoire"},
		{"[[service]]\nname = \"a b\"\ncompose_dir = \"/x\"\nservice = \"a\"\n", "[[service]] n°1 : nom invalide 'a b'"},
		{one() + one(), "[[service]] n°2 : nom en double 'a'"},
		{"[settings]\nadvisory_poll = \"1m\"\n", "advisory_poll : 5 minutes au minimum (la limite de GitHub est de 60 requêtes/h sans jeton)"},
		{"[settings]\ngithub_api = \"https://api.github.com/chemin\"\n", "github_api doit être une adresse http(s) sans chemin (ex. https://api.github.com)"},
		{"[settings]\ncleanup = \"true\"\n", "cleanup doit valoir true ou false"},
		{"[settings]\nsettle_seconds = -1\n", "settle_seconds doit être un entier >= 0"},
		{"[settings]\nwindow = 5\n", "fenêtre invalide : '5' (exemple : 02:00-05:00)"},
		{one("window = 5"), "fenêtre invalide : 5 (exemple : 02:00-05:00)"},
		{"[settings]\nhealth_timeout = 0\n" + one(), "a : health_timeout doit être un entier >= 1"},
	} {
		_, err := parse(t, c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s\n  obtenu  : %v\n  attendu : %s", c.src, err, c.want)
		}
	}
}

func TestStructureErrors(t *testing.T) {
	for _, src := range []string{"settings = 1\n", "service = [\"a\"]\n", "[[settings]]\nx = 1\n", "[service]\nname = \"a\"\n"} {
		if _, err := parse(t, src); err == nil {
			t.Errorf("%q devrait échouer", src)
		}
	}
	// Comme en Python, les autres tables et clés racine sont ignorées (voir PORTAGE.md, proposition 2).
	if cfg := mustParse(t, "autre = 1\n[inconnue]\nx = 2\n"); len(cfg.Services) != 0 {
		t.Errorf("%+v", cfg)
	}
}

func TestDuplicatesAndGroups(t *testing.T) {
	if _, err := parse(t, one()+one()); err == nil {
		t.Error("nom en double")
	}
	two := func(a, b string) string {
		return "[[service]]\nname = \"a\"\ncompose_dir = \"/x\"\nservice = \"a\"\ngroup = \"g\"\n" + a +
			"\n[[service]]\nname = \"b\"\ncompose_dir = \"/x\"\nservice = \"a\"\ngroup = \"g\"\n" + b + "\n"
	}
	if _, err := parse(t, two("mode = \"auto\"", "mode = \"notify\"")); err == nil || !strings.Contains(err.Error(), "groupe g : mode, cooldown, window, cleanup et réglages de sécurité doivent être identiques") {
		t.Errorf("groupe incohérent : %v", err)
	}
	if _, err := parse(t, "[[service]]\nname = \"g\"\ncompose_dir = \"/x\"\nservice = \"a\"\n[[service]]\nname = \"b\"\ncompose_dir = \"/x\"\nservice = \"a\"\ngroup = \"g\"\n"); err == nil || !strings.Contains(err.Error(), "groupe g : même nom qu'un service") {
		t.Errorf("groupe = nom d'un service : %v", err)
	}
	// Chaque réglage partagé compte.
	for _, c := range [][2]string{{"cooldown = \"1d\"", ""}, {"window = \"01:00-02:00\"", ""}, {"cleanup = false", ""}, {"min_severity = \"low\"", ""},
		{"security_cooldown = \"1h\"", ""}, {"security_policy = \"alert\"", ""}, {"advisories = [\"a/b\"]", ""}} {
		if _, err := parse(t, two(c[0], c[1])); err == nil {
			t.Errorf("%s devrait être refusé dans un groupe", c[0])
		}
	}
	// release_notes ne fait pas partie des réglages partagés : une alerte par release, sur un seul membre.
	if _, err := parse(t, two("release_notes = true\nrelease_notes_repos = [\"a/b\"]", "")); err != nil {
		t.Errorf("release_notes sur un seul membre : %v", err)
	}
	ok := mustParse(t, two("mode = \"auto\"", "mode = \"auto\""))
	if len(ok.Services) != 2 || ok.Services[0].Name != "a" || ok.Services[1].Name != "b" {
		t.Errorf("l'ordre du fichier est conservé : %+v", ok.Services)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	if _, err := Load("/nonexistent/x.toml"); err == nil || !strings.Contains(err.Error(), "configuration introuvable : /nonexistent/x.toml") {
		t.Errorf("fichier absent : %v", err)
	}
	p := filepath.Join(t.TempDir(), "x.toml")
	if err := os.WriteFile(p, []byte("ceci = n'est pas du toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.HasPrefix(err.Error(), "configuration illisible : ") {
		t.Errorf("TOML invalide : %v", err)
	}
	if err := os.WriteFile(p, []byte("a = 1.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "TOML non pris en charge") {
		t.Errorf("TOML hors sous-ensemble : %v", err)
	}
	if err := os.WriteFile(p, []byte(one()), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(p); err != nil || len(cfg.Services) != 1 {
		t.Errorf("fichier valide : %v", err)
	}
}

func TestTrustedFile(t *testing.T) {
	const p = "/etc/docker-update/updates.toml"
	if err := CheckTrusted(p, 0, 1000, fs.FileMode(0o644)); err == nil || !strings.Contains(err.Error(), "doit appartenir à root") {
		t.Errorf("pas à root : %v", err)
	}
	if err := CheckTrusted(p, 0, 0, fs.FileMode(0o666)); err == nil {
		t.Error("modifiable par tous")
	}
	if err := CheckTrusted(p, 0, 0, fs.FileMode(0o664)); err == nil {
		t.Error("modifiable par le groupe")
	}
	if err := CheckTrusted(p, 0, 0, fs.FileMode(0o602)); err == nil {
		t.Error("modifiable par les autres")
	}
	for _, mode := range []fs.FileMode{0o644, 0o600, 0o640, 0o444} {
		if err := CheckTrusted(p, 0, 0, mode); err != nil {
			t.Errorf("%o : %v", mode, err)
		}
	}
	if err := CheckTrusted(p, 1000, 1000, fs.FileMode(0o666)); err != nil {
		t.Errorf("pas root : pas de contrainte : %v", err)
	}
	if err := CheckTrustedFile(filepath.Join(t.TempDir(), "x")); err != nil && os.Geteuid() != 0 {
		t.Errorf("hors root : %v", err)
	}
}

// ── TestVersions.test_config_security_keys ──────────────────────────────────

func TestConfigSecurityKeys(t *testing.T) {
	cfg := mustParse(t, one("advisories = [\"traefik/traefik\"]", "min_severity = \"Critical\"", "security_cooldown = \"1h\"", "security_policy = \"alert\""))
	s := cfg.Services[0]
	if !slices.Equal(s.Advisories, []string{"traefik/traefik"}) || s.MinSeverity != "critical" || s.SecurityCooldown != 3600 || s.SecurityPolicy != "alert" {
		t.Errorf("%+v", s)
	}
	d := mustParse(t, one()).Services[0]
	if len(d.Advisories) != 0 || d.MinSeverity != "high" || d.SecurityCooldown != 6*3600 || d.SecurityPolicy != "fast" {
		t.Errorf("défauts : %+v", d)
	}
	for _, b := range []string{one("advisories = [\"pas un depot\"]"), one("advisories = \"traefik/traefik\""), one("advisories = [\"../../etc/passwd\"]"),
		one("min_severity = \"énorme\""), one("security_policy = \"yolo\""), one("security_cooldown = \"vite\"")} {
		if _, err := parse(t, b); err == nil {
			t.Errorf("devrait être refusé : %s", b)
		}
	}
	for _, st := range []string{"github_api = \"ftp://x\"", "github_api = \"https://api.github.com/chemin\"", "advisory_poll = \"1m\""} {
		if _, err := parse(t, "[settings]\n"+st+"\n"+one()); err == nil {
			t.Errorf("devrait être refusé : %s", st)
		}
	}
	if _, err := parse(t, "[[service]]\nname = \"a\"\ncompose_dir = \"/x\"\nservice = \"a\"\ngroup = \"g\"\nadvisories = [\"a/b\"]\n[[service]]\nname = \"b\"\ncompose_dir = \"/x\"\nservice = \"a\"\ngroup = \"g\"\n"); err == nil {
		t.Error("un groupe doit avoir la même politique de sécurité")
	}
}

// ── TestReleaseScan.test_config_release_keys ────────────────────────────────

func TestConfigReleaseKeys(t *testing.T) {
	d := mustParse(t, one()).Services[0]
	if d.ReleaseNotes || len(d.ReleaseNotesRepos) != 0 || len(d.ReleaseNotesIgnore) != 0 {
		t.Errorf("défauts : %+v", d)
	}
	s := mustParse(t, one("release_notes = true", "release_notes_repos = [\"a/b\"]", "release_notes_ignore = ['npm audit', 'CVE-2025-\\d+']")).Services[0]
	if !s.ReleaseNotes || !slices.Equal(s.ReleaseNotesRepos, []string{"a/b"}) || !slices.Equal(s.ReleaseNotesIgnore, []string{"npm audit", `CVE-2025-\d+`}) {
		t.Errorf("%+v", s)
	}
	if len(s.IgnoreRE) != 2 || !s.IgnoreRE[0].MatchString("Run NPM AUDIT now") || !s.IgnoreRE[1].MatchString("cve-2025-1234") || s.IgnoreRE[1].MatchString("CVE-2026-1234") {
		t.Errorf("expressions compilées insensibles à la casse : %v", s.IgnoreRE)
	}
	if !mustParse(t, one("release_notes = true", "advisories = [\"a/b\"]")).Services[0].ReleaseNotes {
		t.Error("advisories suffit comme dépôt à lire")
	}
	// Un « false » écrit en chaîne serait vrai : seuls les vrais booléens passent.
	for _, notbool := range []string{"\"true\"", "\"false\"", "\"\"", "1", "0", "[\"x\"]"} {
		if _, err := parse(t, one("release_notes = "+notbool, "release_notes_repos = [\"a/b\"]")); err == nil {
			t.Errorf("release_notes = %s devrait être refusé", notbool)
		}
	}
	bad := []string{
		one("release_notes = \"true\""), one("release_notes = 1"), one("release_notes = true"),
		one("release_notes = true", "release_notes_repos = [\"pas un depot\"]"),
		one("release_notes = true", "release_notes_repos = \"a/b\""),
		one("release_notes = true", "release_notes_repos = [\"../../x\"]", "advisories = [\"a/b\"]"),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = \"npm\""),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [\"(\"]"),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [\"\"]"),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [\""+strings.Repeat("x", 201)+"\"]"),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [3]"),
		one("release_notes_ignore = [\"x\"]"), one("release_notes_repos = [\"a/b\"]"),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ingore = [\"x\"]"),
		one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [\"(?=x)\"]"), // lookahead : hors RE2
	}
	for _, b := range bad {
		if _, err := parse(t, b); err == nil {
			t.Errorf("devrait être refusé : %s", b)
		}
	}
	if _, err := parse(t, one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [\"(?=x)\"]")); err == nil ||
		!strings.Contains(err.Error(), "release_notes_ignore : expression invalide '(?=x)'") || !strings.Contains(err.Error(), "RE2") {
		t.Errorf("message : %v", err)
	}
	if _, err := parse(t, one("release_notes = true", "advisories = [\"a/b\"]", "release_notes_ignore = [\""+strings.Repeat("é", 200)+"\"]")); err != nil {
		t.Errorf("200 caractères (et non octets) : %v", err)
	}
}

// ── Exemples ────────────────────────────────────────────────────────────────

func examples(t *testing.T, name string) *Config {
	t.Helper()
	cfg, err := Load(filepath.Join("..", "..", "examples", name))
	if err != nil {
		t.Fatalf("%s : %v", name, err)
	}
	return cfg
}

func TestExampleConfigIsValid(t *testing.T) {
	cfg := examples(t, "updates.example.toml")
	if len(cfg.Services) < 10 {
		t.Errorf("%d services", len(cfg.Services))
	}
	groups := map[string]bool{}
	for _, s := range cfg.Services {
		if s.Group != "" {
			groups[s.Group] = true
		}
		if s.Mode != "auto" && s.Mode != "notify" {
			t.Errorf("%s : mode %q", s.Name, s.Mode)
		}
		if s.Group == "tirelire" && s.Cleanup {
			t.Errorf("%s : on garde l'image précédente d'une base migrée", s.Name)
		}
	}
	if len(groups) != 1 || !groups["tirelire"] {
		t.Errorf("groupes : %v", groups)
	}
	rn, ign := false, false
	for _, s := range cfg.Services {
		rn = rn || s.ReleaseNotes
		ign = ign || len(s.ReleaseNotesIgnore) > 0
	}
	if !rn || !ign {
		t.Error("l'exemple montre la veille des notes de release")
	}
}

func TestWatchtowerMigrationExample(t *testing.T) {
	cfg := examples(t, "updates.watchtower.toml")
	by := map[string]*Service{}
	for _, s := range cfg.Services {
		by[s.Name] = s
		if s.Mode != "notify" {
			t.Errorf("%s : aucun service en auto sans accord explicite (%s)", s.Name, s.Mode)
		}
	}
	if _, ok := by["watchtower"]; ok {
		t.Error("watchtower ne se suit pas lui-même")
	}
	tirelire, immichRN := 0, 0
	immich := map[string]bool{}
	for _, s := range cfg.Services {
		switch s.Group {
		case "tirelire":
			tirelire++
			if s.Cleanup || s.ReleaseNotes {
				t.Errorf("%s : cleanup = false, pas de notes (dépôt privé)", s.Name)
			}
		case "immich":
			immich[s.Name] = true
			if s.ReleaseNotes {
				immichRN++
			}
		}
	}
	if tirelire != 5 {
		t.Errorf("tirelire : %d", tirelire)
	}
	if len(immich) != 2 || !immich["immich_server"] || !immich["immich_machine_learning"] || immichRN != 1 {
		t.Errorf("immich : %v, %d alerte(s)", immich, immichRN)
	}
	for _, n := range []string{"traefik", "homepage", "wg-easy", "pocket-id", "crowdsec", "beszel"} {
		if by[n] == nil || !by[n].ReleaseNotes {
			t.Errorf("%s : release_notes attendu", n)
		}
	}
}
