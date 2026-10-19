// Package config lit et valide le fichier de configuration (TOML) : réglages généraux, services, groupes.
//
// Les messages d'erreur sont ceux du script Python d'origine (docker-update.py, fonction parse_config), qui fait foi.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/textutil"
	"github.com/Bloopps/docker-update/internal/tomlmini"
)

// Valeurs autorisées (les tuples sont repris dans les messages d'erreur, comme en Python).
var (
	Modes         = []string{"auto", "notify", "manual"}
	RegistryTools = []string{"buildx", "regctl"}
	NotifyFormats = []string{"discord", "ntfy", "json"}
	Policies      = []string{"fast", "alert"}
)

var (
	nameRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	githubAPIRE = regexp.MustCompile(`^https?://[^/\s]+$`)
	smokeURLRE  = regexp.MustCompile(`^https?://`)
)

// Error est une erreur de configuration (code de sortie 2).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Settings : réglages généraux ([settings]).
type Settings struct {
	StateFile        string
	LockFile         string
	RegistryTool     string
	NotifyURLFile    string
	NotifyFormat     string
	DefaultCooldown  int64
	SettleSeconds    int
	HealthTimeout    int
	Window           string
	GithubAPI        string
	GithubTokenFile  string
	AdvisoryPoll     int64
	AdvisoryLookback int64
	VersionLabel     string
	Cleanup          bool
}

// Service : un service suivi ([[service]]).
type Service struct {
	Name               string
	ComposeDir         string
	Service            string
	ComposeFiles       []string
	Mode               string
	Cooldown           int64
	Group              string
	HealthTimeout      int
	Window             textutil.Window
	SmokeURL           string
	SmokeExpect        []int
	Advisories         []string
	MinSeverity        string
	SecurityCooldown   int64
	SecurityPolicy     string
	Cleanup            bool
	ReleaseNotes       bool
	ReleaseNotesRepos  []string
	ReleaseNotesIgnore []string
	// IgnoreRE : les expressions de ReleaseNotesIgnore, compilées (insensibles à la casse).
	IgnoreRE []*regexp.Regexp
}

// Unit est l'unité de mise à jour : le groupe, ou à défaut le nom du service.
func (s *Service) Unit() string {
	if s.Group != "" {
		return s.Group
	}
	return s.Name
}

// Config est une configuration validée.
type Config struct {
	Settings Settings
	Services []*Service
}

// DefaultSettings : valeurs par défaut de [settings].
func DefaultSettings() Settings {
	return Settings{
		StateFile: "/var/lib/docker-update/state.json", LockFile: "/run/docker-update.lock", RegistryTool: "buildx",
		NotifyFormat: "discord", DefaultCooldown: 3 * 86400, SettleSeconds: 15, HealthTimeout: 180,
		GithubAPI: "https://api.github.com", AdvisoryPoll: 3600, AdvisoryLookback: 30 * 86400,
		VersionLabel: "org.opencontainers.image.version", Cleanup: true,
	}
}

var settingsKeys = []string{"state_file", "lock_file", "registry_tool", "notify_url_file", "notify_format",
	"default_cooldown", "settle_seconds", "health_timeout", "window",
	"github_api", "github_token_file", "advisory_poll", "advisory_lookback", "version_label", "cleanup"}

var serviceKeys = []string{"name", "compose_dir", "service", "compose_files", "mode", "cooldown", "group",
	"health_timeout", "window", "smoke_url", "smoke_expect",
	"advisories", "min_severity", "security_cooldown", "security_policy", "cleanup",
	"release_notes", "release_notes_repos", "release_notes_ignore"}

// removedKeys : fonctionnalités supprimées volontairement, dans l'ordre de contrôle du Python.
var removedKeys = []struct{ key, why string }{
	{"rollback", "en cas d'échec l'outil alerte et ne touche à rien : aucun retour arrière automatique"},
	{"pre_hook", "la sauvegarde se gère en dehors de l'outil"},
	{"heartbeat_url_file", "seules les notifications Discord existent"},
}

// pyTuple écrit une liste comme un tuple Python : ('a', 'b').
func pyTuple(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = textutil.Repr(s)
	}
	if len(parts) == 1 {
		return "(" + parts[0] + ",)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func pyList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = textutil.Repr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func checkKeys(where string, t *tomlmini.Table, allowed []string) error {
	for _, r := range removedKeys {
		if _, ok := t.Get(r.key); ok {
			return errf("%s : « %s » n'existe plus (%s)", where, r.key, r.why)
		}
	}
	var unknown []string
	for _, k := range t.Keys() {
		if !slices.Contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return errf("%s : clé(s) inconnue(s) %s (faute de frappe ?)", where, pyList(unknown))
	}
	return nil
}

func isInt(v tomlmini.Value) bool { return v.Kind == tomlmini.Int }

func isBool(v tomlmini.Value) bool { return v.Kind == tomlmini.Bool }

// strList lit une liste de chaînes ; ok est faux si la valeur n'est pas une liste ou contient autre chose.
func strList(v tomlmini.Value) ([]string, bool) {
	if v.Kind != tomlmini.Array {
		return nil, false
	}
	out := make([]string, 0, len(v.Items))
	for _, it := range v.Items {
		if it.Kind != tomlmini.String {
			return nil, false
		}
		out = append(out, it.Str)
	}
	return out, true
}

func repoList(v tomlmini.Value) ([]string, bool) {
	l, ok := strList(v)
	if !ok {
		return nil, false
	}
	for _, r := range l {
		if !repoRE.MatchString(r) {
			return nil, false
		}
	}
	return l, true
}

func duration(v tomlmini.Value) (int64, error) {
	n, err := textutil.ParseDurationRepr(v.PyStr(), v.Repr())
	if err != nil {
		return 0, &Error{Msg: err.Error()}
	}
	return n, nil
}

func window(v tomlmini.Value) (textutil.Window, error) {
	w, err := textutil.ParseWindowRepr(v.PyStr(), v.Repr())
	if err != nil {
		return w, &Error{Msg: err.Error()}
	}
	return w, nil
}

func containsStr(list []string, s string) bool { return slices.Contains(list, s) }

// Parse valide un document TOML déjà lu.
func Parse(doc *tomlmini.Document) (*Config, error) {
	for _, name := range []string{"settings", "service"} {
		if _, ok := doc.Root.Get(name); ok {
			return nil, errf("« %s » doit être %s", name, expected(name))
		}
	}
	if _, ok := doc.Arrays["settings"]; ok {
		return nil, errf("« settings » doit être une table [settings]")
	}
	if _, ok := doc.Tables["service"]; ok {
		return nil, errf("« service » doit être un tableau de tables [[service]]")
	}
	st, err := parseSettings(doc.Tables["settings"])
	if err != nil {
		return nil, err
	}
	cfg := &Config{Settings: st}
	seen := map[string]bool{}
	for i, t := range doc.Arrays["service"] {
		svc, err := parseService(i+1, t, st, seen)
		if err != nil {
			return nil, err
		}
		cfg.Services = append(cfg.Services, svc)
	}
	if err := checkGroups(cfg.Services, seen); err != nil {
		return nil, err
	}
	return cfg, nil
}

func expected(name string) string {
	if name == "settings" {
		return "une table [settings]"
	}
	return "un tableau de tables [[service]]"
}

func parseSettings(t *tomlmini.Table) (Settings, error) {
	st := DefaultSettings()
	if t == nil {
		t = &tomlmini.Table{}
	}
	if err := checkKeys("[settings]", t, settingsKeys); err != nil {
		return st, err
	}
	get := func(k string) (tomlmini.Value, bool) { return t.Get(k) }
	for _, k := range []struct {
		key string
		dst *string
	}{{"state_file", &st.StateFile}, {"lock_file", &st.LockFile}, {"notify_url_file", &st.NotifyURLFile}, {"window", &st.Window},
		{"github_api", &st.GithubAPI}, {"github_token_file", &st.GithubTokenFile}, {"version_label", &st.VersionLabel}} {
		if v, ok := get(k.key); ok {
			*k.dst = v.PyStr()
		}
	}
	if v, ok := get("registry_tool"); ok {
		st.RegistryTool = v.PyStr()
	}
	if !containsStr(RegistryTools, st.RegistryTool) {
		return st, errf("registry_tool doit valoir %s", pyTuple(RegistryTools))
	}
	if v, ok := get("notify_format"); ok {
		st.NotifyFormat = v.PyStr()
	}
	if !containsStr(NotifyFormats, st.NotifyFormat) {
		return st, errf("notify_format doit valoir %s", pyTuple(NotifyFormats))
	}
	if _, err := textutil.ParseWindow(st.Window); err != nil {
		return st, &Error{Msg: err.Error()}
	}
	if !githubAPIRE.MatchString(st.GithubAPI) {
		return st, errf("github_api doit être une adresse http(s) sans chemin (ex. https://api.github.com)")
	}
	var err error
	if v, ok := get("advisory_poll"); ok {
		if st.AdvisoryPoll, err = duration(v); err != nil {
			return st, err
		}
		if st.AdvisoryPoll < 300 {
			return st, errf("advisory_poll : 5 minutes au minimum (la limite de GitHub est de 60 requêtes/h sans jeton)")
		}
	}
	if v, ok := get("advisory_lookback"); ok {
		if st.AdvisoryLookback, err = duration(v); err != nil {
			return st, err
		}
	}
	if v, ok := get("default_cooldown"); ok {
		if st.DefaultCooldown, err = duration(v); err != nil {
			return st, err
		}
	}
	if v, ok := get("cleanup"); ok {
		if !isBool(v) {
			return st, errf("cleanup doit valoir true ou false")
		}
		st.Cleanup = v.Bool
	}
	for _, k := range []struct {
		key string
		dst *int
	}{{"settle_seconds", &st.SettleSeconds}, {"health_timeout", &st.HealthTimeout}} {
		if v, ok := get(k.key); ok {
			if !isInt(v) || v.Int < 0 {
				return st, errf("%s doit être un entier >= 0", k.key)
			}
			*k.dst = int(v.Int)
		}
	}
	return st, nil
}

func parseService(n int, t *tomlmini.Table, st Settings, seen map[string]bool) (*Service, error) {
	where := fmt.Sprintf("[[service]] n°%d", n)
	if err := checkKeys(where, t, serviceKeys); err != nil {
		return nil, err
	}
	for _, req := range []string{"name", "compose_dir", "service"} {
		if _, ok := t.Get(req); !ok {
			return nil, errf("%s : « %s » est obligatoire", where, req)
		}
	}
	val := func(k string) (tomlmini.Value, bool) { return t.Get(k) }
	str := func(k, def string) string {
		if v, ok := val(k); ok {
			return v.PyStr()
		}
		return def
	}
	nameV, _ := val("name")
	name := nameV.PyStr()
	if !nameRE.MatchString(name) {
		return nil, errf("%s : nom invalide %s", where, textutil.Repr(name))
	}
	if seen[name] {
		return nil, errf("%s : nom en double %s", where, textutil.Repr(name))
	}
	seen[name] = true
	cdir := str("compose_dir", "")
	if !strings.HasPrefix(cdir, "/") {
		return nil, errf("%s : compose_dir doit être un chemin absolu", name)
	}
	svcName := str("service", "")
	if !nameRE.MatchString(svcName) {
		return nil, errf("%s : nom de service compose invalide %s", name, textutil.Repr(svcName))
	}
	var files []string
	if v, ok := val("compose_files"); ok {
		l, ok := strList(v)
		if !ok {
			return nil, errf("%s : compose_files doit être une liste de chaînes", name)
		}
		for _, f := range l {
			if !strings.HasPrefix(f, "/") {
				if strings.HasSuffix(cdir, "/") {
					f = cdir + f
				} else {
					f = cdir + "/" + f
				}
			}
			files = append(files, f)
		}
	}
	mode := str("mode", "notify")
	if !containsStr(Modes, mode) {
		return nil, errf("%s : mode doit valoir %s", name, pyTuple(Modes))
	}
	group := str("group", "")
	if group != "" && !nameRE.MatchString(group) {
		return nil, errf("%s : nom de groupe invalide %s", name, textutil.Repr(group))
	}
	ht := st.HealthTimeout
	if v, ok := val("health_timeout"); ok {
		if !isInt(v) || v.Int < 1 {
			return nil, errf("%s : health_timeout doit être un entier >= 1", name)
		}
		ht = int(v.Int)
	} else if ht < 1 {
		return nil, errf("%s : health_timeout doit être un entier >= 1", name)
	}
	smokeURL := str("smoke_url", "")
	var smokeExpect []int
	expectV, hasExpect := val("smoke_expect")
	if smokeURL != "" {
		if !smokeURLRE.MatchString(smokeURL) {
			return nil, errf("%s : smoke_url doit commencer par http:// ou https://", name)
		}
		ok := hasExpect && expectV.Kind == tomlmini.Array && len(expectV.Items) > 0
		if ok {
			for _, c := range expectV.Items {
				if !isInt(c) || c.Int < 100 || c.Int > 599 {
					ok = false
					break
				}
				smokeExpect = append(smokeExpect, int(c.Int))
			}
		}
		if !ok {
			return nil, errf("%s : smoke_expect doit lister les codes HTTP acceptés (ex. [302, 401])", name)
		}
	} else if hasExpect && expectV.Truthy() {
		return nil, errf("%s : smoke_expect sans smoke_url", name)
	}
	var advisories []string
	if v, ok := val("advisories"); ok {
		l, ok := repoList(v)
		if !ok {
			return nil, errf("%s : advisories doit être une liste de « propriétaire/dépôt » GitHub", name)
		}
		advisories = l
	}
	minSev := strings.ToLower(str("min_severity", "high"))
	if !containsStr(textutil.Severities, minSev) {
		return nil, errf("%s : min_severity doit valoir %s", name, pyTuple(textutil.Severities))
	}
	policy := str("security_policy", "fast")
	if !containsStr(Policies, policy) {
		return nil, errf("%s : security_policy doit valoir %s", name, pyTuple(Policies))
	}
	cleanup := st.Cleanup
	if v, ok := val("cleanup"); ok {
		if !isBool(v) {
			return nil, errf("%s : cleanup doit valoir true ou false", name)
		}
		cleanup = v.Bool
	}
	rn := false
	if v, ok := val("release_notes"); ok {
		if !isBool(v) {
			return nil, errf("%s : release_notes doit valoir true ou false", name)
		}
		rn = v.Bool
	}
	var rnRepos []string
	if v, ok := val("release_notes_repos"); ok {
		l, ok := repoList(v)
		if !ok {
			return nil, errf("%s : release_notes_repos doit être une liste de « propriétaire/dépôt » GitHub", name)
		}
		rnRepos = l
	}
	var rnIgnore []string
	if v, ok := val("release_notes_ignore"); ok {
		l, ok := strList(v)
		if ok {
			for _, p := range l {
				if c := utf8.RuneCountInString(p); c == 0 || c > 200 {
					ok = false
				}
			}
		}
		if !ok {
			return nil, errf("%s : release_notes_ignore doit être une liste d'expressions (200 caractères au plus)", name)
		}
		rnIgnore = l
	}
	var ignoreRE []*regexp.Regexp
	for _, p := range rnIgnore {
		re, err := compileIgnore(p)
		if err != nil {
			return nil, errf("%s : release_notes_ignore : expression invalide %s (%s)", name, textutil.Repr(p), err)
		}
		ignoreRE = append(ignoreRE, re)
	}
	if !rn && (len(rnRepos) > 0 || len(rnIgnore) > 0) {
		return nil, errf("%s : release_notes_repos / release_notes_ignore sans release_notes = true", name)
	}
	if rn && len(rnRepos) == 0 && len(advisories) == 0 {
		return nil, errf("%s : release_notes = true demande release_notes_repos (ou advisories) : le dépôt GitHub à lire", name)
	}
	// Comme en Python, les durées et la fenêtre se lisent après tous les autres contrôles.
	cooldown := st.DefaultCooldown
	if v, ok := val("cooldown"); ok {
		var err error
		if cooldown, err = duration(v); err != nil {
			return nil, err
		}
	}
	var w textutil.Window
	var err error
	if v, ok := val("window"); ok {
		w, err = window(v)
	} else {
		w, err = textutil.ParseWindow(st.Window)
		if err != nil {
			err = &Error{Msg: err.Error()}
		}
	}
	if err != nil {
		return nil, err
	}
	secCooldown := int64(6 * 3600)
	if v, ok := val("security_cooldown"); ok {
		if secCooldown, err = duration(v); err != nil {
			return nil, err
		}
	}
	return &Service{
		Name: name, ComposeDir: cdir, Service: svcName, ComposeFiles: files, Mode: mode, Cooldown: cooldown,
		Group: group, HealthTimeout: ht, Window: w, SmokeURL: smokeURL, SmokeExpect: smokeExpect, Advisories: advisories,
		MinSeverity: minSev, SecurityCooldown: secCooldown, SecurityPolicy: policy, Cleanup: cleanup, ReleaseNotes: rn,
		ReleaseNotesRepos: rnRepos, ReleaseNotesIgnore: rnIgnore, IgnoreRE: ignoreRE,
	}, nil
}

// compileIgnore compile une expression de release_notes_ignore (insensible à la casse). Go utilise RE2 : ni
// lookahead ni référence arrière (écart documenté avec le module re de Python).
func compileIgnore(pat string) (*regexp.Regexp, error) {
	re, err := regexp.Compile("(?i)" + pat)
	if err == nil {
		return re, nil
	}
	var se *syntax.Error
	if errors.As(err, &se) {
		return nil, fmt.Errorf("%s ; syntaxe RE2 : ni lookahead ni référence arrière", se.Code)
	}
	return nil, err
}

type groupKey struct {
	mode             string
	cooldown         int64
	window           textutil.Window
	advisories       string
	minSeverity      string
	securityCooldown int64
	securityPolicy   string
	cleanup          bool
}

func checkGroups(services []*Service, names map[string]bool) error {
	var order []string
	groups := map[string][]*Service{}
	for _, s := range services {
		if s.Group == "" {
			continue
		}
		if _, ok := groups[s.Group]; !ok {
			order = append(order, s.Group)
		}
		groups[s.Group] = append(groups[s.Group], s)
	}
	for _, g := range order {
		keys := map[groupKey]bool{}
		for _, m := range groups[g] {
			keys[groupKey{m.Mode, m.Cooldown, m.Window, strings.Join(m.Advisories, "\x00"), m.MinSeverity,
				m.SecurityCooldown, m.SecurityPolicy, m.Cleanup}] = true
		}
		if len(keys) > 1 {
			return errf("groupe %s : mode, cooldown, window, cleanup et réglages de sécurité doivent être identiques", g)
		}
		if names[g] {
			return errf("groupe %s : même nom qu'un service", g)
		}
	}
	return nil
}

// Load lit et valide un fichier de configuration.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errf("configuration introuvable : %s", path)
		}
		return nil, errf("configuration illisible : %v", err)
	}
	doc, err := tomlmini.Parse(data)
	if err != nil {
		return nil, errf("configuration illisible : %v", err)
	}
	return Parse(doc)
}

// CheckTrusted : quand l'outil tourne en root (euid 0), la configuration (qui désigne les piles compose pilotées en
// root) doit appartenir à root et ne pas être modifiable par un autre compte.
func CheckTrusted(path string, euid int, ownerUID uint32, perm fs.FileMode) error {
	if euid != 0 {
		return nil
	}
	if ownerUID != 0 || perm&0o022 != 0 {
		return errf("%s doit appartenir à root et ne pas être modifiable par le groupe ou les autres (chown root:root, chmod 644 ou 600)", path)
	}
	return nil
}

// CheckTrustedFile applique CheckTrusted au fichier réel.
func CheckTrustedFile(path string) error {
	euid := os.Geteuid()
	if euid != 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return errf("configuration illisible : %v", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errf("%s : propriétaire illisible", path)
	}
	return CheckTrusted(path, euid, st.Uid, info.Mode().Perm())
}
