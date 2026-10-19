// Package updater est le cœur : évaluation des images, délais de sécurité, application, nettoyage, passage.
//
// Le comportement est celui de la classe Updater du script Python d'origine (docker-update.py), qui fait foi. Toutes
// les dépendances (commandes, horloge, sommeil, HTTP, GitHub) sont injectées, comme les paramètres du constructeur
// Python : les tests rejouent des scénarios complets sans jamais toucher à un vrai Docker, à GitHub ou à Discord.
package updater

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/github"
	"github.com/Bloopps/docker-update/internal/notify"
	"github.com/Bloopps/docker-update/internal/smoke"
	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// Error est une erreur d'exécution (code de sortie 1), comme UpdateError en Python.
type Error = state.Error

func errf(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

const (
	maxManifest    = 4 << 20
	releasesFirst  = 30
	releasesNext   = 10
	releaseLabel   = "ALERTE notes de release"
	releasesResync = 7 * 86400
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// États de conteneur qu'on accepte de toucher ; les autres (exited, created, paused, dead…) sont laissés tels quels.
var runningStates = []string{"running", "restarting"}

// Étiquettes flottantes : un service en `auto` qui les suit reçoit un avertissement.
var floatingTags = []string{"latest", "main", "master", "dev", "edge", "nightly", "stable"}

func contains[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Deps regroupe les dépendances injectées. Les valeurs nulles prennent le comportement réel.
type Deps struct {
	Cfg      *config.Config
	Run      dockerx.Runner
	Notifier *notify.Notifier
	State    *state.State
	Now      func() int64         // secondes depuis l'epoch
	Sleep    func(time.Duration)  // attente
	Log      func(string)         // journal
	HTTP     func(url string) int // test de fumée : code HTTP (0 : injoignable)
	Minutes  func() int           // heure locale, en minutes depuis minuit
	NoSave   bool                 // vrai : l'état n'est jamais écrit
	GHGet    github.GetFunc       // lecture de l'API GitHub
}

// Eval est le verdict d'évaluation d'un service.
type Eval struct {
	Svc          *config.Service
	Status       string // uptodate | pending | skipped | error
	Detail       string
	Ref          string
	Remote       string
	OldImageID   string
	FirstSeen    int64
	Eligible     bool
	FailedBefore bool
	Fast         bool // raccourci par un avis de sécurité confirmé
	Urgent       bool // avis critique : ignore aussi la fenêtre horaire
	SecIDs       []string
	SecVersion   string
	AdvPub       int64 // publication (epoch) de l'avis le plus récent qui justifie l'accélération
}

// Updater fait un passage.
type Updater struct {
	cfg      *config.Config
	run      dockerx.Runner
	notifier *notify.Notifier
	state    *state.State
	now      func() int64
	sleep    func(time.Duration)
	log      func(string)
	http     func(string) int
	minutes  func() int
	persist  bool
	ghGet    github.GetFunc

	alertsOn bool
	report   *notify.Report
	// notes : service -> {avis cité (en minuscules) -> (release, lien)}
	notes map[string]map[string][2]string
}

func localMinutes() int {
	t := time.Now()
	return t.Hour()*60 + t.Minute()
}

// New crée un Updater.
func New(d Deps) *Updater {
	u := &Updater{cfg: d.Cfg, run: d.Run, notifier: d.Notifier, state: d.State, now: d.Now, sleep: d.Sleep, log: d.Log,
		http: d.HTTP, minutes: d.Minutes, persist: !d.NoSave, ghGet: d.GHGet, report: notify.NewReport()}
	if u.now == nil {
		u.now = func() int64 { return time.Now().Unix() }
	}
	if u.sleep == nil {
		u.sleep = time.Sleep
	}
	if u.log == nil {
		u.log = func(m string) { fmt.Println(m) }
	}
	if u.http == nil {
		u.http = smoke.Default
	}
	if u.minutes == nil {
		u.minutes = localMinutes
	}
	if u.ghGet == nil {
		u.ghGet = github.Get
	}
	return u
}

func (u *Updater) held(svc *config.Service) bool {
	return u.state.HasHold(svc.Name) || (svc.Group != "" && u.state.HasHold(svc.Group))
}

func (u *Updater) smoke(svc *config.Service) bool {
	if svc.SmokeURL == "" {
		return true
	}
	for attempt := 0; attempt < 3; attempt++ {
		if contains(svc.SmokeExpect, u.http(svc.SmokeURL)) {
			return true
		}
		if attempt < 2 {
			u.sleep(5 * time.Second)
		}
	}
	return false
}

func (u *Updater) saveState() error {
	if u.persist {
		return u.state.Save()
	}
	return nil
}

// ── commandes docker ────────────────────────────────────────────────────────

func (u *Updater) sh(argv ...string) dockerx.Result {
	return u.run.Run(argv, false, dockerx.DefaultRunTimeout)
}

func (u *Updater) mutate(timeout time.Duration, argv ...string) dockerx.Result {
	return u.run.Run(argv, true, timeout)
}

func base(svc *config.Service) []string {
	argv := []string{"docker", "compose", "--project-directory", svc.ComposeDir}
	for _, f := range svc.ComposeFiles {
		argv = append(argv, "-f", f)
	}
	return argv
}

func with(prefix []string, more ...string) []string {
	return append(append([]string{}, prefix...), more...)
}

func (u *Updater) containerID(svc *config.Service) (string, error) {
	r := u.sh(with(base(svc), "ps", "-q", "--all", svc.Service)...)
	if r.RC != 0 {
		return "", errf("compose ps a échoué : %s", textutil.Truncate(textutil.Strip(r.Err), 200))
	}
	if f := textutil.Fields(r.Out); len(f) > 0 {
		return f[0], nil
	}
	return "", nil
}

func (u *Updater) containerStatus(cid string) (string, error) {
	r := u.sh("docker", "inspect", "--format", "{{.State.Status}}", cid)
	first, _, _ := strings.Cut(r.Out, "|")
	status := textutil.CleanText(first, 30)
	if r.RC != 0 || status == "" {
		return "", errf("docker inspect du conteneur a échoué")
	}
	return status, nil
}

func (u *Updater) imageRef(svc *config.Service) (string, error) {
	r := u.sh(with(base(svc), "config", "--format", "json")...)
	if r.RC != 0 {
		return "", errf("compose config a échoué : %s", textutil.Truncate(textutil.Strip(r.Err), 200))
	}
	// Jamais la sortie de `compose config` dans un message : elle contient l'environnement du service.
	notFound := errf("service %s introuvable ou sans « image: » dans le compose", svc.Service)
	var doc map[string]any
	if err := json.Unmarshal([]byte(r.Out), &doc); err != nil {
		return "", notFound
	}
	services, ok := doc["services"].(map[string]any)
	if !ok {
		return "", notFound
	}
	entry, ok := services[svc.Service].(map[string]any)
	if !ok {
		return "", notFound
	}
	image, ok := entry["image"]
	if !ok {
		return "", notFound
	}
	if s, ok := image.(string); ok {
		return s, nil
	}
	return fmt.Sprint(image), nil
}

// remoteDigest : digest de niveau supérieur de l'étiquette (celui qu'enregistre `docker pull`).
//
// buildx : on lit le manifeste brut (`--raw`, sans télécharger les attestations) et on le hache ; c'est exactement le
// digest que le registre annonce. regctl : `regctl image digest` (requête HEAD).
func (u *Updater) remoteDigest(ref string) (string, error) {
	repo, tag, _ := textutil.SplitRef(ref)
	full := repo + ":" + tag
	if u.cfg.Settings.RegistryTool == "buildx" {
		rc, raw, errText := u.run.RunBytes([]string{"docker", "buildx", "imagetools", "inspect", full, "--raw"}, dockerx.DefaultBytesTimeout)
		if rc != 0 || !bytes.HasPrefix(bytes.TrimLeft(raw, " \t\n\r\v\f"), []byte("{")) || len(raw) > maxManifest {
			why := textutil.Truncate(textutil.Strip(errText), 150)
			if why == "" {
				why = "sortie inattendue"
			}
			return "", errf("manifeste distant illisible pour %s (%s)", full, why)
		}
		sum := sha256.Sum256(raw)
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
	r := u.run.Run([]string{"regctl", "image", "digest", full}, false, dockerx.DefaultBytesTimeout)
	out := textutil.Strip(r.Out)
	if r.RC != 0 || !digestRE.MatchString(out) {
		why := textutil.Truncate(textutil.Strip(r.Err), 150)
		if why == "" {
			why = "sortie inattendue"
		}
		return "", errf("digest distant introuvable pour %s (%s)", full, why)
	}
	return out, nil
}

func (u *Updater) localState(cid string) (string, []string, error) {
	r := u.sh("docker", "inspect", "--format", "{{.Image}}", cid)
	imageID := textutil.Strip(r.Out)
	if r.RC != 0 || imageID == "" {
		return "", nil, errf("docker inspect du conteneur a échoué")
	}
	r = u.sh("docker", "image", "inspect", "--format", "{{json .RepoDigests}}", imageID)
	var digests []string
	var raw any
	if json.Unmarshal([]byte(r.Out), &raw) == nil {
		if list, ok := raw.([]any); ok {
			for _, d := range list {
				if s, ok := d.(string); ok {
					if _, after, found := strings.Cut(s, "@"); found {
						digests = append(digests, after)
					}
				}
			}
		}
	}
	return imageID, digests, nil
}

// ── évaluation ──────────────────────────────────────────────────────────────

func (u *Updater) evaluate(svc *config.Service) *Eval {
	e, err := u.evaluateErr(svc)
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) {
			return &Eval{Svc: svc, Status: "error", Detail: ue.Msg}
		}
		return &Eval{Svc: svc, Status: "error", Detail: err.Error()}
	}
	return e
}

func (u *Updater) evaluateErr(svc *config.Service) (*Eval, error) {
	if u.held(svc) {
		return &Eval{Svc: svc, Status: "skipped", Detail: "en pause (hold)"}, nil
	}
	cid, err := u.containerID(svc)
	if err != nil {
		return nil, err
	}
	if cid == "" {
		return &Eval{Svc: svc, Status: "skipped", Detail: "conteneur absent"}, nil
	}
	status, err := u.containerStatus(cid)
	if err != nil {
		return nil, err
	}
	if !contains(runningStates, status) {
		// un service arrêté (volontairement) n'est touché en rien : ni registre, ni mise à jour, ni veille, ni nettoyage
		return &Eval{Svc: svc, Status: "skipped", Detail: fmt.Sprintf("conteneur arrêté (%s), non touché", status)}, nil
	}
	ref, err := u.imageRef(svc)
	if err != nil {
		return nil, err
	}
	_, tag, pinned := textutil.SplitRef(ref)
	if pinned != "" {
		return &Eval{Svc: svc, Status: "skipped", Detail: "image épinglée par digest dans le compose (gérée à la main)", Ref: ref}, nil
	}
	if contains(floatingTags, tag) && svc.Mode == "auto" {
		u.log(fmt.Sprintf("  avertissement : %s suit l'étiquette flottante « %s » ; préférez une étiquette de version mineure", svc.Name, tag))
	}
	remote, err := u.remoteDigest(ref)
	if err != nil {
		return nil, err
	}
	imageID, local, err := u.localState(cid)
	if err != nil {
		return nil, err
	}
	if len(local) == 0 {
		return &Eval{Svc: svc, Status: "skipped", Detail: "image locale sans digest (construite ici ?)", Ref: ref}, nil
	}
	if contains(local, remote) {
		return &Eval{Svc: svc, Status: "uptodate", Ref: ref, Remote: remote, OldImageID: imageID}, nil
	}
	now := u.now()
	rec := u.state.Get(ref)
	if rec == nil || rec.Digest != remote {
		rec = &state.Image{Digest: remote, FirstSeen: now}
		u.state.Put(ref, rec)
	}
	first := rec.FirstSeen
	return &Eval{Svc: svc, Status: "pending", Ref: ref, Remote: remote, OldImageID: imageID, FirstSeen: first,
		Eligible: now-first >= svc.Cooldown, FailedBefore: rec.Failed}, nil
}

func short(digest string) string { return textutil.Slice(digest, 7, 19) }

func (u *Updater) describe(e *Eval) string {
	n := e.Svc.Name
	switch e.Status {
	case "uptodate":
		return n + ": à jour"
	case "skipped", "error":
		word := "ignoré"
		if e.Status == "error" {
			word = "ERREUR"
		}
		return fmt.Sprintf("%s: %s — %s", n, word, e.Detail)
	}
	age := u.now() - e.FirstSeen
	if e.FailedBefore {
		return fmt.Sprintf("%s: nouvelle image %s déjà tentée sans succès — attend une autre version ou `now`", n, short(e.Remote))
	}
	sec := ""
	if e.Fast {
		sec = fmt.Sprintf(" [CORRECTIF DE SÉCURITÉ %s → %s]", strings.Join(e.SecIDs, ", "), e.SecVersion)
	}
	if e.Eligible {
		return fmt.Sprintf("%s: nouvelle image %s prête (vue depuis %s), mode %s%s", n, short(e.Remote), textutil.FmtDuration(age), e.Svc.Mode, sec)
	}
	if e.Fast {
		left := max(0, u.readyAt(e)-u.now())
		return fmt.Sprintf("%s: nouvelle image %s différée%s — encore %s (délai de sécurité)", n, short(e.Remote), sec, textutil.FmtDuration(left))
	}
	left := e.Svc.Cooldown - age
	return fmt.Sprintf("%s: nouvelle image %s différée — encore %s sur %s", n, short(e.Remote), textutil.FmtDuration(left), textutil.FmtDuration(e.Svc.Cooldown))
}

func (u *Updater) cleanupState(evals []*Eval) {
	keep := map[string]bool{}
	for _, e := range evals {
		if e.Status == "pending" || ((e.Status == "error" || e.Status == "skipped") && e.Ref != "") {
			keep[e.Ref] = true
		}
	}
	for ref := range u.state.Images {
		if !keep[ref] {
			u.state.Drop(ref)
		}
	}
}

// readyAt : un correctif de sécurité attend `security_cooldown` après la plus tardive de deux dates : première
// observation de l'image et publication de l'avis. Un avis tout neuf ne déclenche donc rien d'instantané.
func (u *Updater) readyAt(e *Eval) int64 {
	return max(e.FirstSeen, e.AdvPub) + e.Svc.SecurityCooldown
}

func itoa(n int) string { return strconv.Itoa(n) }

func readTokenFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return textutil.Strip(string(b))
}

const dockerxDefault = dockerx.DefaultRunTimeout

func sortStrings(s []string) { sort.Strings(s) }
