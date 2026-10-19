package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/fakeworld"
	"github.com/Bloopps/docker-update/internal/notify"
)

type relOpts struct {
	pre, draft bool
	name       string
}

type relOpt func(*relOpts)

func relPre() relOpt          { return func(o *relOpts) { o.pre = true } }
func relDraft() relOpt        { return func(o *relOpts) { o.draft = true } }
func relName(n string) relOpt { return func(o *relOpts) { o.name = n } }

// release : une release GitHub telle que l'API la renvoie (JSON décodé).
func release(c *clock, tag, body string, opts ...relOpt) M {
	o := relOpts{name: tag}
	for _, f := range opts {
		f(&o)
	}
	var published any
	if !o.draft {
		published = isoAt(c.t - 3600)
	}
	return M{"tag_name": tag, "name": o.name, "body": body, "prerelease": o.pre, "draft": o.draft,
		"published_at": published, "html_url": "https://evil.example/phish"}
}

func newRel(t *testing.T) *env {
	b := newBase(t)
	b.world.Add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1, fakeworld.WithVersion("v3.7.13"))
	return b
}

// rel : paramètres de mkRel (version par défaut v3.7.13 ; none retire l'étiquette de version).
type rel struct {
	version string
	none    bool
	repos   []string
	svc     M
	mk      mk
}

func (b *env) mkRel(r rel) *Updater {
	if r.version == "" {
		r.version = "v3.7.13"
	}
	if r.repos == nil {
		r.repos = []string{"traefik/traefik"}
	}
	if !r.none {
		b.world.Labels["id-traefik-old"] = map[string]any{"org.opencontainers.image.version": r.version}
	} else {
		delete(b.world.Labels, "id-traefik-old")
	}
	kw := M{"release_notes": true, "release_notes_repos": r.repos}
	for k, v := range r.svc {
		kw[k] = v
	}
	return b.make([]M{svc(kw)}, r.mk)
}

func (b *env) releases(etag string, rels ...M) { b.releasesOf("traefik/traefik", etag, rels...) }

func (b *env) releasesOf(repo, etag string, rels ...M) {
	list := make([]any, len(rels))
	for i, r := range rels {
		list[i] = r
	}
	b.ghRel[repo] = ghReply{200, list, etag}
}

func TestReleaseNotes_RealWorldCases(t *testing.T) {
	cases := []struct {
		current, tag, body string
		strong             bool
	}{
		{"v15.3.0", "v15.4.0", "Improved security of the login form", false},                            // wg-easy
		{"0.18.0", "v0.19.0", "Security hardening of the agent connection", false},                      // beszel
		{"2.44.0", "2.45.1", "Fixes CVE-2026-11111, CVE-2026-22222 and CVE-2026-33333", true},           // portainer
		{"v3.7.13", "v3.7.14", "Fix GHSA-aaaa-bbbb-cccc is not valid but GHSA-qxcr-4rv3-v23r is", true}, // traefik
		{"8.1.9", "8.1.10", "Security fixes: a crash in the cluster bus", true},                         // valkey
		{"v2.5.0", "v2.6.0", "Patch for GHSA-qxcr-4rv3-v23r", true},                                     // homepage
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			b := newRel(t)
			b.releases("e1", release(b.clock, c.tag, c.body))
			b.cycle(b.mkRel(rel{version: c.current}), true)
			b.eq(len(b.posts), 1, b.logtext())
			content := b.posts[0].content()
			if c.strong {
				b.in(strings.TrimLeft(c.current, "v")+" → "+c.tag, content)
				b.in("Notes de release à lire", content)
				b.in("https://github.com/traefik/traefik/releases/tag/"+c.tag, content)
				b.in("Motifs :", content)
				b.in("⚠️", content)
			} else {
				b.in(c.tag, content)
				b.in("Signal faible", content)
				b.in("ℹ️", content)
				b.notIn("Motifs :", content)
			}
		})
	}
}

func TestReleaseNotes_MotifsAreListed(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Fixes CVE-2026-1111 (SSRF) and CVE-2026-2222 (path traversal)"))
	b.cycle(b.mkRel(rel{}), true)
	for _, want := range []string{"CVE-2026-1111", "CVE-2026-2222", "SSRF", "path traversal", "3.7.13 → v3.7.14"} {
		b.in(want, b.posted())
	}
}

func TestReleaseNotes_NoReleasesMeansNoAlertAndNoError(t *testing.T) {
	b := newRel(t)
	for _, resp := range []ghReply{{404, nil, ""}, {200, []any{}, "e"}} {
		b.ghRel["traefik/traefik"] = resp
		b.statePath = filepath.Join(b.tmp, "s"+itoa(resp.code)+".json")
		u := b.mkRel(rel{})
		b.eq(b.cycle(u, true), 0)
		b.eq(len(b.posts), 0)
		b.notIn("ERREUR", b.logtext())
	}
	// dépôt jamais déclaré côté faux serveur (404 par défaut), service sans erreur de vérification
	b.ghRel = map[string]ghReply{}
	b.eq(b.cycle(b.mkRel(rel{}), true), 0)
}

func TestReleaseNotes_OlderOrEqualReleasesAreIgnored(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.13", "CVE-2026-1111"), release(b.clock, "v3.7.12", "CVE-2026-2222"),
		release(b.clock, "v3.6.99", "CVE-2026-3333"), release(b.clock, "v2.0.0", "GHSA-qxcr-4rv3-v23r"))
	b.cycle(b.mkRel(rel{}), true)
	b.eq(len(b.posts), 0)
}

func TestReleaseNotes_OneAlertPerServiceAndTagAndPersisted(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"), release(b.clock, "v3.7.15", "CVE-2026-2222"))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "un seul rapport")
	b.eq(strings.Count(b.posted(), "Notes de release à lire"), 2, "une entrée par release")
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "pas de répétition")
	b.clock.days(1)
	b.cycle(b.mkRel(rel{}), true) // nouveau processus : l'état disque dédoublonne
	b.eq(len(b.posts), 1)
	b.releases("e2", release(b.clock, "v3.7.16", "CVE-2026-3333"), release(b.clock, "v3.7.15", "CVE-2026-2222"),
		release(b.clock, "v3.7.14", "CVE-2026-1111"))
	b.clock.days(1)
	b.cycle(b.mkRel(rel{}), true)
	b.eq(len(b.posts), 2, "seule la nouvelle release est signalée")
	b.in("v3.7.16", b.posts[1].content())
	b.notIn("v3.7.15", b.posts[1].content())
}

func TestReleaseNotes_DedupKeyIsPerService(t *testing.T) {
	b := newRel(t)
	b.world.Add("traefik2", "/stack/t2", "traefik2", "traefik:v3.6", 1, fakeworld.WithVersion("v3.7.13"))
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	svcs := []M{svc(M{"release_notes": true, "release_notes_repos": []string{"traefik/traefik"}}),
		svcN("traefik2", "/stack/t2", "", M{"release_notes": true, "release_notes_repos": []string{"traefik/traefik"}})}
	b.cycle(b.make(svcs), true)
	b.eq(len(b.posts), 1)
	b.eq(strings.Count(b.posts[0].content(), "Notes de release à lire"), 2, "une entrée par service")
	b.eq(len(b.ghCalls), 1, "un seul appel pour un dépôt partagé")
}

func TestReleaseNotes_DraftsAndPrereleasesAreIgnored(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.8.0", "CVE-2026-1111", relPre()), release(b.clock, "v3.9.0", "CVE-2026-2222", relDraft()),
		release(b.clock, "v3.10.0-rc1", "CVE-2026-3333"), release(b.clock, "v3.11.0-beta.2", "CVE-2026-4444", relPre()))
	b.cycle(b.mkRel(rel{}), true)
	b.eq(len(b.posts), 0)
}

func TestReleaseNotes_UnreadableReleaseTagsAreSkipped(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "nightly", "CVE-2026-1111"), release(b.clock, "release-3.8.0", "CVE-2026-2222"),
		release(b.clock, "latest", "CVE-2026-3333"))
	b.eq(b.cycle(b.mkRel(rel{}), true), 0)
	b.eq(len(b.posts), 0)
}

func TestReleaseNotes_UnreadableCurrentVersionAlertsOnceAndScansNothing(t *testing.T) {
	for _, label := range []string{"latest", "main", "18-vectorchord0.5.3", "", "garbage"} {
		name := label
		if name == "" {
			name = "absente"
		}
		t.Run(name, func(t *testing.T) {
			b := newRel(t)
			b.releases("e1", release(b.clock, "v9.9.9", "CVE-2026-1111"))
			u := b.mkRel(rel{version: label, none: label == ""})
			b.cycle(u, true)
			b.cycle(u, true)
			b.eq(len(b.posts), 1)
			b.in("Impossible de comparer", b.posts[0].content())
			b.notIn("CVE-2026-1111", b.posted())
			b.eq(len(b.ghCalls), 0, "aucune requête quand la version ne peut pas être comparée")
		})
	}
}

func TestReleaseNotes_NeverAcceleratesOrApplies(t *testing.T) {
	b := newRel(t)
	b.newRemote(2)
	b.world.DigestVersions[dg(2)] = "v3.7.14"
	b.releases("e1", release(b.clock, "v3.7.14", "Security fix: CVE-2026-1111 GHSA-qxcr-4rv3-v23r critical RCE"))
	u := b.mkRel(rel{svc: M{"cooldown": "3d", "window": "02:00-05:00"}})
	b.minutes = 600 // hors fenêtre
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.eq(len(b.cmds("docker", "pull")), 0, "aucune image n'est tirée pour une note de release")
	b.in("différée — encore 3 j", b.logtext())
	b.clock.days(2)
	b.minutes = 180
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0, "en fenêtre, délai non écoulé : toujours rien")
	b.eq(len(b.cmds("docker", "pull")), 0)
	b.eq(u.state.Get("traefik:v3.6").FirstSeen, int64(1_000_000), "le compteur de délai n'est pas modifié")
	b.eq(len(b.posts), 1, "une alerte, pas de mise à jour")
	b.clock.days(1.1) // le délai NORMAL s'écoule : seul lui déclenche
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1)
}

func TestReleaseNotes_IgnorePatterns(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Bump deps (npm audit: 3 vulnerabilities)"))
	b.cycle(b.mkRel(rel{svc: M{"release_notes_ignore": []string{"npm audit"}}}), true)
	b.eq(len(b.posts), 0)
	b.statePath = filepath.Join(b.tmp, "other.json")
	b.cycle(b.mkRel(rel{svc: M{"release_notes_ignore": []string{"PAS-LA"}}}), true)
	b.eq(len(b.posts), 1, "un motif d'exclusion sans rapport ne masque rien")
}

func TestReleaseNotes_IgnoreIsCaseInsensitiveAndHidesOnlyMatchingHits(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Dependencies: NPM Audit reports vulnerabilities. "+strings.Repeat("x ", 100)+
		"Also fixes CVE-2026-7777 in the router."))
	b.cycle(b.mkRel(rel{svc: M{"release_notes_ignore": []string{"npm audit"}}}), true)
	b.eq(len(b.posts), 1)
	b.in("CVE-2026-7777", b.posted())
	head := strings.SplitN(strings.ToLower(b.posted()), "lien", 2)[0]
	b.notIn("vulnerabilit", strings.SplitN(head, "motifs", 2)[1])
}

func TestReleaseNotes_IgnoreCanSilenceTheWeakSignalToo(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Improved security of the admin page"))
	b.cycle(b.mkRel(rel{svc: M{"release_notes_ignore": []string{"admin page"}}}), true)
	b.eq(len(b.posts), 0)
}

func TestReleaseNotes_StrongReleaseHasNoExtraInfoAlert(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "This security fix improves security."))
	b.cycle(b.mkRel(rel{}), true)
	b.eq(len(b.posts), 1)
	b.in("Notes de release à lire", b.posted())
}

func TestReleaseNotes_WeakSignalsAreGroupedAndDeduplicated(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "better security"), release(b.clock, "v3.7.15", "more security"))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "un seul message groupé")
	b.in("v3.7.14", b.posted())
	b.in("v3.7.15", b.posted())
	b.cycle(u, true)
	b.eq(len(b.posts), 1)
	b.releases("e2", release(b.clock, "v3.7.16", "yet more security"), release(b.clock, "v3.7.15", "more security"),
		release(b.clock, "v3.7.14", "better security"))
	b.clock.days(1)
	b.cycle(u, true)
	b.eq(len(b.posts), 2)
	b.in("v3.7.16", b.posts[1].content())
	b.notIn("v3.7.14", b.posts[1].content(), "les releases déjà signalées ne reviennent pas")
}

func TestReleaseNotes_AReleaseEditedFromWeakToStrongIsAlerted(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "better security"))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.releases("e2", release(b.clock, "v3.7.14", "better security, fixes CVE-2026-1111"))
	b.clock.days(1)
	b.cycle(u, true)
	b.eq(len(b.posts), 2)
	b.in("CVE-2026-1111", b.posts[1].content())
}

func TestReleaseNotes_UntrustedNotesCannotInject(t *testing.T) {
	b := newRel(t)
	evil := "@everyone <@&123456> @here\n**URGENT** ```rm -rf /``` \x00\x1b[31m CVE-2026-1111 " + strings.Repeat("z", 900)
	b.releases("e1", release(b.clock, "v3.7.14", evil, relName("@everyone")))
	b.cycle(b.mkRel(rel{}), true)
	p := b.posts[0]
	var body map[string]any
	if err := json.Unmarshal(p.body, &body); err != nil {
		t.Fatal(err)
	}
	content := p.content()
	b.eq(body["allowed_mentions"], map[string]any{"parse": []any{}})
	b.ok(utf8.RuneCountInString(content) <= notify.DiscordMax)
	b.notIn("@everyone", content)
	b.notIn("@here", content)
	b.notIn("\x1b", content+b.logtext())
	b.notIn("evil.example", content+b.logtext(), "le lien est reconstruit, jamais copié")
	b.eq(strings.Count(content, "Extrait"), 1)
	extrait := ""
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "Extrait") {
			extrait = l
			break
		}
	}
	b.ok(utf8.RuneCountInString(extrait) <= notify.ReportLine+10)
}

func TestReleaseNotes_CheckLogsButSendsNothing(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Fixes CVE-2026-1111"))
	u := b.mkRel(rel{})
	b.eq(b.cycle(u, false), 0)
	b.eq(len(b.posts), 0)
	b.in("notes de release traefik: version détectée v3.7.13", b.logtext())
	b.in("ALERTE notes de release", b.logtext())
	b.eq(len(u.state.Alerts), 0, "check n'engage aucune alerte")
	b.cycle(b.mkRel(rel{none: true}), false)
	b.in("version NON détectée", b.logtext())
}

func TestReleaseNotes_APIFailureNeverBlocksNormalUpdates(t *testing.T) {
	for _, code := range []int{500, 0, 403} {
		t.Run(itoa(code), func(t *testing.T) {
			b := newRel(t)
			b.newRemote(2)
			b.ghRel["traefik/traefik"] = ghReply{code, nil, ""}
			u := b.mkRel(rel{svc: M{"cooldown": "0"}})
			b.eq(b.cycle(u, true), 0)
			b.eq(len(b.world.Ups), 1, "la mise à jour normale a eu lieu")
			b.in("injoignable", b.logtext())
			b.ok(!(len(b.posts) > 0 && strings.Contains(b.posted(), "Notes de release")))
		})
	}
}

func TestReleaseNotes_RateLimitWaitsTheWholeIntervalAndKeepsOldItems(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Fixes CVE-2026-1111"))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.clock.days(2.0 / 24)
	b.ghRel["traefik/traefik"] = ghReply{403, nil, ""}
	b.cycle(u, true)
	n := len(b.ghCalls)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), n, "après un 403, pas de nouvelle requête avant advisory_poll")
	b.eq(len(u.state.Releases["traefik/traefik"].Items), 1, "anciennes releases conservées")
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), n+1)
}

func TestReleaseNotes_HistoryOnceThenOnlyTheLatestReleases(t *testing.T) {
	b := newRel(t)
	url := func(n string) string { return "https://api.github.com/repos/traefik/traefik/releases?per_page=" + n }
	last := func() string { return b.ghCalls[len(b.ghCalls)-1].url }
	b.releases("e1", release(b.clock, "v3.7.12", "old"))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.eq(last(), url("30"), "première lecture : tout l'historique")
	for i := 0; i < 3; i++ {
		b.clock.days(2.0 / 24)
		b.cycle(u, true)
		b.eq(last(), url("10"), "ensuite : seulement les dernières")
	}
	b.releases("e2", release(b.clock, "v3.7.14", "Fixes CVE-2026-1111"), release(b.clock, "v3.7.12", "old"))
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "une nouvelle release est vue avec 10 demandées")
	b.clock.days(8) // plus de 7 jours sans lecture complète
	b.cycle(u, true)
	b.eq(last(), url("30"))
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(last(), url("10"))
}

func TestReleaseNotes_AFailedFirstReadIsRetriedWithTheFullHistory(t *testing.T) {
	b := newRel(t)
	b.ghRel["traefik/traefik"] = ghReply{500, nil, ""}
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.releases("e1", release(b.clock, "v3.7.14", "Fixes CVE-2026-1111"))
	b.cycle(u, true)
	b.ok(strings.HasSuffix(b.ghCalls[len(b.ghCalls)-1].url, "per_page=30"))
	b.eq(len(b.posts), 1)
}

func TestReleaseNotes_FirstEverRateLimitAlsoWaitsTheInterval(t *testing.T) {
	b := newRel(t)
	b.ghRel["traefik/traefik"] = ghReply{429, nil, ""}
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 1, "limite de débit dès le premier appel : on n'insiste pas")
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 2)
	b.eq(strings.Count(b.logtext(), "injoignable"), 1)
}

func TestReleaseNotes_SameTagInTwoRepositoriesIsAlertedOnce(t *testing.T) {
	b := newRel(t)
	b.releasesOf("a/one", "e", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	b.releasesOf("b/two", "e", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	b.cycle(b.mkRel(rel{repos: []string{"a/one", "b/two"}}), true)
	b.eq(len(b.posts), 1)
	b.eq(len(b.ghCalls), 2)
}

func TestReleaseNotes_404IsCachedForThePollInterval(t *testing.T) {
	b := newRel(t)
	b.ghRel["traefik/traefik"] = ghReply{404, nil, ""}
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 1)
	b.notIn("injoignable", b.logtext())
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 2)
}

func TestReleaseNotes_PollIntervalEtagURLAndSharedCache(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "better security"))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 1)
	b.eq(b.ghCalls[0].url, "https://api.github.com/repos/traefik/traefik/releases?per_page=30")
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(b.ghCalls[1].etag, "e1", "requête conditionnelle (ETag)")
	b.eq(b.ghCalls[1].url, "https://api.github.com/repos/traefik/traefik/releases?per_page=10")
	b.eq(len(u.state.Releases["traefik/traefik"].Items), 1)
	b.eq(len(b.posts), 1, "un 304 ne ré-émet rien")
}

func TestReleaseNotes_RepoDefaultsToAdvisories(t *testing.T) {
	b := newRel(t)
	b.ghRel["traefik/traefik"] = ghReply{200, []any{release(b.clock, "v3.7.14", "CVE-2026-1111")}, "e"}
	b.gh["traefik/traefik"] = ghReply{200, []any{}, "a"}
	b.world.Labels["id-traefik-old"] = map[string]any{"org.opencontainers.image.version": "v3.7.13"}
	b.cycle(b.make([]M{svc(M{"advisories": []string{"traefik/traefik"}, "release_notes": true})}), true)
	b.eq(len(b.posts), 1)
}

func TestReleaseNotes_AreOffByDefault(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	b.world.Labels["id-traefik-old"] = map[string]any{"org.opencontainers.image.version": "v3.7.13"}
	b.cycle(b.make([]M{svc(M{"advisories": []string{"traefik/traefik"}})}), true)
	for _, c := range b.ghCalls {
		b.notIn("/releases", c.url)
	}
	for _, p := range b.posts {
		b.notIn("Notes de release", p.content())
	}
}

func TestReleaseNotes_TokenIsSentButNeverLogged(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	u := b.mkRel(rel{mk: mk{Settings: M{"github_token_file": b.tokenFile()}}})
	b.cycle(u, true)
	b.eq(b.ghCalls[0].token, "jeton-SUPER-SECRET")
	b.notIn("SUPER-SECRET", b.logtext()+b.posted()+stateText(b, u))
	b.notIn("discord.test", b.logtext(), "l'URL du webhook n'est jamais journalisée")
}

func TestReleaseNotes_StateKeepsOnlyCompactData(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "Fixes CVE-2026-1111. "+strings.Repeat("blabla ", 5000)))
	u := b.mkRel(rel{})
	b.cycle(u, true)
	raw, _ := json.Marshal(u.state.Releases)
	b.ok(len(raw) < 2000, "état des releases trop gros")
	b.notIn(strings.Repeat("blabla ", 20), stateText(b, u), "seul l'extrait court est conservé")
}

func TestReleaseNotes_NotifyModeAndUnknownStatusServices(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	b.cycle(b.mkRel(rel{svc: M{"mode": "notify"}}), true)
	b.eq(len(b.posts), 1)
	b.posts, b.ghCalls = nil, nil
	u := b.mkRel(rel{svc: M{"mode": "notify"}})
	u.state.Holds = append(u.state.Holds, "traefik") // en pause : ignoré, aucune requête
	b.cycle(u, true)
	b.eq(len(b.ghCalls), 0)
}

func TestReleaseNotes_AbsentContainerIsSkipped(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	u := b.make([]M{svcN("ghost", "/stack/ghost", "", M{"release_notes": true, "release_notes_repos": []string{"traefik/traefik"}})})
	b.eq(b.cycle(u, true), 0)
	b.eq(len(b.ghCalls), 0)
	b.eq(len(b.posts), 0)
}

func TestReleaseNotes_SimulationChangesNothing(t *testing.T) {
	b := newRel(t)
	b.releases("e1", release(b.clock, "v3.7.14", "CVE-2026-1111"))
	u := b.mkRel(rel{mk: mk{Dry: true, NoSave: true}})
	b.cycle(u, true)
	b.eq(len(b.posts), 0)
	_, err := os.Stat(b.statePath)
	b.ok(os.IsNotExist(err))
}
