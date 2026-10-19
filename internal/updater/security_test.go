package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/fakeworld"
	"github.com/Bloopps/docker-update/internal/notify"
)

// ── TestSecurityProperties ──────────────────────────────────────────────────

func TestSecurityProperties_ComposeConfigContentIsNeverLogged(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}})
	b.cycle(u, true)
	b.cycle(u, false)
	blob := b.logtext()
	for _, p := range b.posts {
		blob += string(p.body)
	}
	b.notIn(b.world.SecretInConfig, blob)
	b.notIn("SECRET-TOKEN", blob)
}

func TestSecurityProperties_CommandsNeverUseAShell(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(M{"compose_files": []string{"a; rm -rf /.yml"}})}, mk{Settings: M{"default_cooldown": "0"}})
	b.cycle(u, true)
	found := false
	for _, c := range b.world.Calls {
		if contains(c, "/stack/traefik/a; rm -rf /.yml") {
			found = true
		}
	}
	b.ok(found, "un seul argument, jamais interprété")
}

func TestSecurityProperties_PullIsByDigestOfTheEvaluatedImage(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}})
	orig := b.world.Run
	u.run = wrapRunner{b.world, func(argv []string, mutating bool, timeout time.Duration) dockerx.Result {
		if argv[0] == "docker" && argv[1] == "pull" {
			b.world.Registry["traefik:v3.6"] = manifest(99) // l'étiquette bouge juste avant le tirage
		}
		return orig(argv, mutating, timeout)
	}}
	b.cycle(u, true)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2), "on applique le digest évalué, pas celui qui vient de bouger")
}

// ── TestSecurity ────────────────────────────────────────────────────────────

type advOpts struct {
	ghsa      string
	sev       string
	hoursAgo  float64
	rng       string
	cve       string
	summary   string
	withdrawn bool
}

type advOpt func(*advOpts)

func advGHSA(v string) advOpt         { return func(o *advOpts) { o.ghsa = v } }
func advSev(v string) advOpt          { return func(o *advOpts) { o.sev = v } }
func advHours(v float64) advOpt       { return func(o *advOpts) { o.hoursAgo = v } }
func advRange(v string) advOpt        { return func(o *advOpts) { o.rng = v } }
func advSummary(v string) advOpt      { return func(o *advOpts) { o.summary = v } }
func advWithdrawn() advOpt            { return func(o *advOpts) { o.withdrawn = true } }
func advCVE(v string) advOpt          { return func(o *advOpts) { o.cve = v } }
func isoAt(unix int64) string         { return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05-07:00") }
func ghsaDefault() string             { return "GHSA-2222-3333-4444" }
func (b *env) adv(o ...advOpt) M      { return advisory(b.clock, o...) }
func (b *env) advs(o ...advOpt) []any { return []any{b.adv(o...)} }

// advisory : un avis GitHub tel que l'API le renvoie (JSON décodé).
func advisory(c *clock, opts ...advOpt) M {
	o := advOpts{ghsa: ghsaDefault(), sev: "high", hoursAgo: 1.0, rng: "< 3.6.3", cve: "CVE-2026-12345",
		summary: "Contournement d'authentification"}
	for _, f := range opts {
		f(&o)
	}
	var withdrawn any
	if o.withdrawn {
		withdrawn = "2026-01-01T00:00:00Z"
	}
	return M{"ghsa_id": o.ghsa, "cve_id": o.cve, "severity": o.sev, "published_at": isoAt(c.t - int64(o.hoursAgo*3600)),
		"summary": o.summary, "withdrawn_at": withdrawn,
		"vulnerabilities": []any{M{"package": M{"ecosystem": "go", "name": "x"}, "vulnerable_version_range": o.rng,
			"patched_versions": "3.6.3"}}}
}

func newSec(t *testing.T) *env {
	b := newBase(t)
	b.world.Add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1, fakeworld.WithVersion("3.6.2"))
	b.newRemote(2)
	b.world.DigestVersions[dg(2)] = "3.6.3"
	b.gh["traefik/traefik"] = ghReply{200, b.advs(), "etag-1"}
	return b
}

func (b *env) mkSec(svcKw M, o ...mk) *Updater {
	kw := M{"advisories": []string{"traefik/traefik"}}
	for k, v := range svcKw {
		kw[k] = v
	}
	return b.make([]M{svc(kw)}, o...)
}

func (b *env) posted() string {
	var parts []string
	for _, p := range b.posts {
		parts = append(parts, p.content())
	}
	return strings.Join(parts, "\n")
}

func TestSecurity_FastTrackWaitsSecurityCooldownThenApplies(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("Correctif de sécurité détecté", b.posted())
	b.in("GHSA-2222-3333-4444", b.posted())
	b.in("CORRECTIF DE SÉCURITÉ", b.logtext())
	b.clock.days(5.0 / 24) // +5 h : le délai de sécurité (6 h) n'est pas écoulé
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.clock.days(2.0 / 24) // +7 h : 3 jours n'ont PAS passé, mais le correctif est confirmé
	b.eq(b.cycle(u, true), 0)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2))
	b.in("Correctif de sécurité appliqué : GHSA-2222-3333-4444", b.posted())
	b.eq(strings.Count(b.posted(), "Correctif de sécurité détecté"), 1, "alerte non répétée")
}

func TestSecurity_ClockStartsAtTheLatestOfFirstSeenAndPublication(t *testing.T) {
	b := newSec(t)
	// avis publié il y a 2 j, image vue pour la première fois maintenant : on attend 6 h APRÈS la première observation
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advHours(48)), "e"}
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1)
}

func TestSecurity_ABrandNewAdvisoryIsNotInstant(t *testing.T) {
	b := newSec(t)
	// image vue depuis 3 j (cooldown normal écoulé) mais avis publié il y a 1 h : le raccourci ne s'applique pas
	// (et le délai normal étant écoulé, la mise à jour a de toute façon lieu : le raccourci ne retarde jamais)
	u := b.mkSec(M{"cooldown": "0"})
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1, "un raccourci ne peut que raccourcir")
}

func TestSecurity_CandidateStillVulnerableKeepsNormalDelay(t *testing.T) {
	b := newSec(t)
	b.world.DigestVersions[dg(2)] = "3.6.2" // la « nouvelle » image porte encore la version touchée
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("pas de correctif dans la nouvelle image", b.posted())
	b.clock.days(3)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1, "le délai normal reste appliqué")
}

func TestSecurity_CandidateVersionUnreadableIsAlertOnly(t *testing.T) {
	b := newSec(t)
	delete(b.world.DigestVersions, dg(2))
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("pas lisible", b.posted())
	b.in("docker-update now traefik --yes", b.posted())
}

func TestSecurity_AlertOnlyPolicy(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(M{"security_policy": "alert"})
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0, "politique alert : jamais d'accélération")
	b.in("docker-update now traefik --yes", b.posted())
}

func TestSecurity_SeverityAndLookbackFilters(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{200, []any{b.adv(advSev("medium")), b.adv(advGHSA("GHSA-5555-6666-7777"), advHours(24*40))}, "e"}
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.eq(len(b.cmds("docker", "pull")), 0, "aucun avis pertinent : aucun téléchargement anticipé")
	b.eq(len(b.posts), 0)
	u2 := b.mkSec(M{"min_severity": "medium"})
	b.cycle(u2, true)
	b.in("Correctif de sécurité détecté", b.posted())
}

func TestSecurity_WithdrawnAdvisoryIsIgnored(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advWithdrawn()), "e"}
	b.cycle(b.mkSec(nil), true)
	b.eq(len(b.posts), 0)
}

func TestSecurity_UnaffectedCurrentVersionIsIgnored(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advRange(">= 2.0.0, < 3.0.0")), "e"}
	b.cycle(b.mkSec(nil), true)
	b.eq(len(b.posts), 0)
	b.eq(len(b.cmds("docker", "pull")), 0)
}

func TestSecurity_CriticalIgnoresWindowButHighDoesNot(t *testing.T) {
	for _, c := range []struct {
		sev      string
		expected int
	}{{"high", 0}, {"critical", 1}} {
		t.Run(c.sev, func(t *testing.T) {
			b := newSec(t)
			b.minutes = 14 * 60 // 14 h : hors de la fenêtre 02:00-05:00
			b.gh["traefik/traefik"] = ghReply{200, b.advs(advSev(c.sev)), "e"}
			u := b.mkSec(M{"window": "02:00-05:00"})
			b.cycle(u, true)
			b.clock.days(7.0 / 24)
			b.cycle(u, true)
			b.eq(len(b.world.Ups), c.expected)
			if c.sev == "critical" {
				b.in("CRITIQUE", b.logtext())
			}
		})
	}
}

func TestSecurity_UpToDateButVulnerableAlertsOnce(t *testing.T) {
	b := newSec(t)
	b.world.Registry["traefik:v3.6"] = manifest(1) // l'étiquette n'a pas bougé
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.cycle(u, true)
	b.clock.days(1)
	b.cycle(u, true)
	b.eq(strings.Count(b.posted(), "VULNÉRABLE"), 1)
	b.in("3.6.2", b.posted())
	b.eq(len(b.world.Ups), 0)
}

func TestSecurity_UnknownCurrentVersionAlertsOnce(t *testing.T) {
	b := newSec(t)
	b.world.Labels = map[string]map[string]any{}
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(strings.Count(b.posted(), "version en cours"), 1)
	b.eq(len(b.world.Ups), 0)
}

func TestSecurity_UnreadableRangeIsAlertOnly(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advRange("autour de la 3.6")), "e"}
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.in("plage de versions illisible", b.posted())
	b.eq(len(b.cmds("docker", "pull")), 0)
}

func TestSecurity_CheckNeverPullsOrNotifies(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	b.cycle(u, false)
	b.eq(len(b.cmds("docker", "pull")), 0)
	b.eq(len(b.posts), 0)
	b.in("prochain passage", b.logtext())
	b.eq(len(u.state.Alerts), 0)
}

func TestSecurity_DryRunNeitherPullsNorPersists(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil, mk{Dry: true, NoSave: true})
	b.eq(b.cycle(u, true), 0)
	b.eq(len(b.cmds("docker", "pull")), 0)
	b.eq(len(b.world.Ups), 0)
	_, err := os.Stat(b.statePath)
	b.ok(os.IsNotExist(err))
	b.in("simulation", b.logtext())
	b.notIn("pas lisible", b.posted(), "pas de fausse alerte en simulation")
}

func TestSecurity_FailedFastTrackIsNotRetried(t *testing.T) {
	b := newSec(t)
	b.world.FailUp[id(2)] = true
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.eq(b.cycle(u, true), 1)
	n := len(b.world.Ups)
	b.clock.days(1)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), n)
	b.eq(b.containerImage("/stack/traefik", "traefik"), "id-traefik-old")
}

func TestSecurity_GithubOutageDoesNotBlockNormalUpdates(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{500, nil, ""}
	u := b.mkSec(M{"cooldown": "1d"})
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(strings.Count(b.logtext(), "injoignable"), 1, "l'indisponibilité n'est journalisée qu'une fois")
	b.clock.days(1.1)
	b.eq(b.cycle(u, true), 0)
	b.eq(len(b.world.Ups), 1)
	b.ok(!(len(b.posts) > 0 && strings.Contains(b.posted(), "Correctif de sécurité détecté")))
}

func TestSecurity_RateLimitReturnsOldItems(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.clock.days(2.0 / 24)
	b.gh["traefik/traefik"] = ghReply{403, nil, ""} // limite de débit
	b.cycle(u, true)
	b.clock.days(5.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1, "les anciens avis restent connus malgré l'erreur")
}

func TestSecurity_PollIntervalAndEtag(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 1, "pas de nouvelle requête dans l'intervalle")
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 2)
	b.eq(b.ghCalls[1].etag, "etag-1", "requête conditionnelle")
	b.eq(u.state.Advisories["traefik/traefik"].Items[0].ID, "GHSA-2222-3333-4444")
}

func (b *env) tokenFile() string {
	tf := filepath.Join(b.tmp, "gh_token")
	if err := os.WriteFile(tf, []byte("jeton-SUPER-SECRET\n"), 0o600); err != nil {
		b.t.Fatal(err)
	}
	return tf
}

func stateText(b *env, u *Updater) string {
	raw, err := u.state.Marshal()
	if err != nil {
		b.t.Fatal(err)
	}
	return string(raw)
}

func TestSecurity_TokenIsSentButNeverLogged(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil, mk{Settings: M{"github_token_file": b.tokenFile()}})
	b.cycle(u, true)
	b.eq(b.ghCalls[0].token, "jeton-SUPER-SECRET")
	b.notIn("SUPER-SECRET", b.logtext()+b.posted()+stateText(b, u))
}

func TestSecurity_UntrustedSummaryCannotInject(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advSummary("@everyone\n**URGENT** ```rm -rf /``` " + strings.Repeat("z", 900))), "e"}
	b.world.Labels = map[string]map[string]any{} // force l'alerte qui cite le résumé
	b.cycle(b.mkSec(nil), true)
	var body map[string]any
	if err := json.Unmarshal(b.posts[0].body, &body); err != nil {
		t.Fatal(err)
	}
	content := b.posts[0].content()
	b.eq(body["allowed_mentions"], map[string]any{"parse": []any{}})
	b.ok(utf8.RuneCountInString(content) <= notify.DiscordMax)
	b.notIn("@everyone", content)
	b.notIn("\n**URGENT**", content, "le résumé ne peut pas ouvrir de nouvelle ligne ni de mise en forme")
	for _, line := range strings.Split(content, "\n") {
		ok := utf8.RuneCountInString(line) <= notify.ReportLine+10
		for _, p := range []string{"🚨", "⚠️", "ℹ️", "docker-update"} {
			ok = ok || strings.HasPrefix(line, p)
		}
		b.ok(ok, "chaque ligne du rapport est bornée : "+line)
	}
}

func TestSecurity_OldAlertsArePruned(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	u.state.Alerts = map[string]int64{"vieille": b.clock.t - 130*day, "recente": b.clock.t - 5*day}
	b.cycle(u, true)
	_, old := u.state.Alerts["vieille"]
	_, recent := u.state.Alerts["recente"]
	b.ok(!old)
	b.ok(recent)
}

func TestSecurity_PublicationDateDelaysALateAdvisory(t *testing.T) {
	b := newSec(t)
	// l'image est vue à t0 ; l'avis n'est publié qu'à t0+10 h. Le délai de sécurité (6 h) court depuis la
	// plus tardive des deux dates : prêt à t0+16 h, pas à t0+6 h.
	b.gh["traefik/traefik"] = ghReply{200, []any{}, "e0"}
	u := b.mkSec(nil)
	b.cycle(u, true) // t0 : rien à signaler
	b.clock.days(10.0 / 24)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advHours(0)), "e1"}
	b.cycle(u, true) // t0+10 h : avis tout frais
	b.eq(len(b.world.Ups), 0)
	b.clock.days(2.0 / 24) // t0+12 h : > 6 h depuis la 1re observation
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0, "il faut attendre 6 h APRÈS la publication de l'avis")
	b.clock.days(5.0 / 24) // t0+17 h
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1)
}

func TestSecurity_UnreadableSecondRangeBlocksAcceleration(t *testing.T) {
	b := newSec(t)
	a := b.adv()
	a["vulnerabilities"] = append(a["vulnerabilities"].([]any), M{"vulnerable_version_range": "plage bizarre"})
	b.gh["traefik/traefik"] = ghReply{200, []any{a}, "e"}
	u := b.mkSec(nil)
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0, "on ne conclut jamais « corrigé » sur une plage qu'on ne sait pas lire")
	b.in("pas de correctif dans la nouvelle image", b.posted())
}

func TestSecurity_CheckReportsDetectedVersionOrItsAbsence(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	b.cycle(u, false)
	b.in("version détectée 3.6.2", b.logtext())
	b.logs = nil
	b.world.Labels = map[string]map[string]any{}
	b.cycle(u, false)
	b.in("version NON détectée", b.logtext())
}

func TestSecurity_TokenNotLoggedWhenTheAPIFails(t *testing.T) {
	b := newSec(t)
	b.gh["traefik/traefik"] = ghReply{500, nil, ""}
	u := b.mkSec(nil, mk{Settings: M{"github_token_file": b.tokenFile()}})
	b.cycle(u, true)
	b.in("injoignable", b.logtext())
	b.notIn("SUPER-SECRET", b.logtext()+b.posted()+stateText(b, u))
}

func TestSecurity_NotifyModeServiceGetsAnAlertNotAnAcceleration(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(M{"mode": "notify"})
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("mode non automatique", b.posted())
	b.in("docker-update now traefik --yes", b.posted())
	b.notIn("Mise à jour accélérée", b.posted())
}

func TestSecurity_GroupGetsOneAcceleratedUpdate(t *testing.T) {
	b := newSec(t)
	for _, s := range []string{"migrate", "web"} {
		b.world.Add(s, "/stack/t", s, "ghcr.io/org/tirelire:1.2", 1, fakeworld.WithVersion("1.2.0"))
	}
	b.world.Registry["ghcr.io/org/tirelire:1.2"] = manifest(7)
	b.world.DigestVersions[dg(7)] = "1.2.1"
	b.gh["org/tirelire"] = ghReply{200, b.advs(advRange("< 1.2.1")), "e"}
	var stack []M
	for _, s := range []string{"migrate", "web"} {
		stack = append(stack, svcN(s, "/stack/t", s, M{"group": "t", "advisories": []string{"org/tirelire"}}))
	}
	u := b.make(stack)
	b.cycle(u, true)
	b.clock.days(7.0 / 24)
	b.cycle(u, true)
	b.eq(ups(b.world), []string{"migrate", "web"})
	b.eq(len(b.posts), 2, "détection (passage 1) puis mise à jour (passage 2) : un rapport par passage")
	b.eq(strings.Count(b.posted(), "Mises à jour réussies"), 1)
}
