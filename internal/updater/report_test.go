package updater

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/fakeworld"
	"github.com/Bloopps/docker-update/internal/notify"
)

// ── TestReportDelivery ──────────────────────────────────────────────────────

// apps : nAuto services automatiques, un service qui casse, nReady services en mode notify.
func (b *env) apps(nAuto, nReady int) []M {
	var svcs []M
	for i := 0; i < nAuto; i++ {
		name := "auto" + itoa(i)
		b.world.Add(name, "/stack/"+name, name, "img/"+name+":1", 1)
		b.world.Registry["img/"+name+":1"] = manifest(2)
		svcs = append(svcs, svcN(name, "/stack/"+name, "", M{"cooldown": "0"}))
	}
	b.world.Add("casse", "/stack/casse", "casse", "img/casse:1", 1)
	b.world.Registry["img/casse:1"] = manifest(3)
	b.world.FailUp[id(3)] = true
	svcs = append(svcs, svcN("casse", "/stack/casse", "", M{"cooldown": "0"}))
	for i := 0; i < nReady; i++ {
		name := "pret" + itoa(i)
		b.world.Add(name, "/stack/"+name, name, "img/"+name+":1", 1)
		b.world.Registry["img/"+name+":1"] = manifest(2)
		svcs = append(svcs, svcN(name, "/stack/"+name, "", M{"mode": "notify", "cooldown": "0"}))
	}
	return svcs
}

func TestReportDelivery_OneMessageForTheWholePass(t *testing.T) {
	b := newBase(t)
	u := b.make(b.apps(3, 6))
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.posts), 1)
	c := b.posts[0].content()
	b.ok(strings.HasPrefix(c, "🚨 **docker-update : 1 échec · 3 mises à jour · 6 versions prêtes**"), c)
	idx := func(s string) int { return strings.Index(c, s) }
	b.ok(idx("Échecs de mise à jour") >= 0 && idx("Échecs de mise à jour") < idx("Mises à jour réussies"))
	b.ok(idx("Mises à jour réussies") < idx("Nouvelles versions prêtes"))
	for _, name := range []string{"auto0", "auto1", "auto2", "casse", "pret0", "pret5"} {
		b.in(name, c)
	}
	b.ok(utf8.RuneCountInString(c) <= notify.DiscordMax)
}

func TestReportDelivery_SecurityAlertsAndUpdatesShareTheSameMessage(t *testing.T) {
	b := newBase(t)
	svcs := b.apps(1, 1)
	for _, v := range [][2]string{{"vuln1", "org/one"}, {"vuln2", "org/two"}} {
		name, repo := v[0], v[1]
		b.world.Add(name, "/stack/"+name, name, "img/"+name+":1", 1, fakeworld.WithVersion("1.0.0"))
		b.gh[repo] = ghReply{200, b.advs(advRange("< 2.0.0")), "e"}
		svcs = append(svcs, svcN(name, "/stack/"+name, "", M{"advisories": []string{repo}}))
	}
	b.cycle(b.make(svcs), true)
	b.eq(len(b.posts), 1, b.logtext())
	c := b.posts[0].content()
	b.eq(strings.Count(c, "VULNÉRABLE"), 2)
	for _, section := range []string{"Sécurité (avis)", "Mises à jour réussies", "Nouvelles versions prêtes"} {
		b.in(section, c)
	}
}

func TestReportDelivery_NothingNewMeansNoMessage(t *testing.T) {
	b := newBase(t)
	u := b.make(b.apps(3, 6))
	b.cycle(u, true)
	b.posts = nil
	b.eq(b.cycle(u, true), 0, "le digest en échec n'est pas retenté")
	b.eq(len(b.posts), 0, "rien de nouveau : silence")
}

func TestReportDelivery_AllUpToDateMeansNoMessage(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.cycle(b.make([]M{svc(nil)}), true)
	b.eq(len(b.posts), 0)
}

func TestReportDelivery_CheckSendsNothing(t *testing.T) {
	b := newBase(t)
	u := b.make(b.apps(3, 6))
	b.cycle(u, false)
	b.eq(len(b.posts), 0)
}

func TestReportDelivery_NowSendsItsOwnSingleReport(t *testing.T) {
	b := newBase(t)
	svcs := b.apps(2, 0)[:2]
	u := b.make(svcs, mk{Settings: M{"default_cooldown": "30d"}})
	b.eq(b.nowCmd(u, true, "auto0", "auto1"), 0)
	b.eq(len(b.posts), 1)
	b.in("2 mises à jour", b.posts[0].content())
}

func TestReportDelivery_ReportLevelIsTheWorstEvent(t *testing.T) {
	b := newBase(t)
	u := b.make(b.apps(1, 1)[:1])
	b.cycle(u, true)
	b.ok(strings.HasPrefix(b.posts[0].content(), "✅"), "seulement une réussite : niveau ok")
}

func TestReportDelivery_NtfyAndJSONFormatsGetTheSameSingleReport(t *testing.T) {
	for _, fmtName := range []string{"ntfy", "json"} {
		t.Run(fmtName, func(t *testing.T) {
			b := newBase(t)
			u := b.make(b.apps(1, 2), mk{Fmt: fmtName})
			b.cycle(u, true)
			b.eq(len(b.posts), 1)
			text := string(b.posts[0].body)
			if fmtName == "json" {
				var body map[string]any
				if err := json.Unmarshal(b.posts[0].body, &body); err != nil {
					t.Fatal(err)
				}
				text = body["text"].(string)
			}
			b.notIn("```", text, "pas de balises Discord hors Discord")
			b.in("pret1", text)
		})
	}
}

func TestReportDelivery_ReportsDoNotPileUpBetweenPasses(t *testing.T) {
	b := newBase(t)
	u := b.make(b.apps(1, 0)[:1])
	b.cycle(u, true)
	b.eq(len(b.posts), 1)
	b.ok(u.report.Empty())
}

func TestReportDelivery_NotificationFailureDoesNotBreakThePass(t *testing.T) {
	b := newBase(t)
	u := b.make(b.apps(1, 0)[:1])
	u.notifier.Post = func(url string, body []byte, headers map[string]string) error {
		return errors.New("réseau coupé")
	}
	b.eq(b.cycle(u, true), 0)
	b.in("notification non envoyée", b.logtext())
}

// ── TestManualScan ──────────────────────────────────────────────────────────

func (b *env) manualStack(names []string, kw M) []M {
	var svcs []M
	for _, n := range names {
		b.world.Add(n, "/stack/"+n, n, "img/"+n+":1", 1)
		b.world.Registry["img/"+n+":1"] = manifest(2)
		svcs = append(svcs, svcN(n, "/stack/"+n, "", kw))
	}
	return svcs
}

func abc() []string { return []string{"a", "b", "c"} }

// inspected : les dépôts dont le manifeste a été lu, sans doublon, triés.
func (b *env) inspected() []string {
	set := map[string]bool{}
	for _, c := range b.world.Calls {
		if len(c) >= 5 && strings.Join(c[:4], " ") == "docker buildx imagetools inspect" {
			set[strings.SplitN(c[4], ":", 2)[0]] = true
		}
	}
	out := []string{}
	for k := range set {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func TestManualScan_CheckOnASelectionScansOnlyThose(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack(abc(), nil))
	b.eq(b.cycle(u, false, "b"), 0)
	b.eq(b.inspected(), []string{"img/b"})
	b.in("scan limité à : b", b.logtext())
	b.in("b: nouvelle image", b.logtext())
	b.notIn("a: ", b.logtext())
}

func TestManualScan_NoNameScansEverything(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack(abc(), nil))
	b.cycle(u, false)
	b.eq(b.inspected(), []string{"img/a", "img/b", "img/c"})
	b.notIn("scan limité", b.logtext())
}

func TestManualScan_AGroupNameSelectsAllItsMembers(t *testing.T) {
	b := newBase(t)
	svcs := b.manualStack(abc(), nil)
	svcs[0]["group"], svcs[1]["group"] = "g", "g"
	u := b.make(svcs)
	b.cycle(u, false, "g")
	b.eq(b.inspected(), []string{"img/a", "img/b"})
}

func TestManualScan_SeveralNamesKeepTheFileOrderAndUnknownIsRefused(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack(abc(), nil))
	pick := func(n ...string) []string {
		sel, err := u.selectServices(n)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, s := range sel {
			out = append(out, s.Name)
		}
		return out
	}
	b.eq(pick("c", "a"), []string{"a", "c"})
	b.eq(pick("a", "a"), []string{"a"})
	_, err := u.selectServices([]string{"a", "nope"})
	b.ok(err != nil && strings.Contains(err.Error(), "inconnu : nope"))
	b.eq(len(pick()), 3)
	b.eq(len(pick([]string{}...)), 3)
}

func TestManualScan_RunOnASelectionUpdatesOnlyThose(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack(abc(), M{"cooldown": "0"}))
	b.eq(b.cycle(u, true, "b"), 0)
	b.eq(ups(b.world), []string{"b"})
	c := b.posts[0].content()
	b.in("b → ", c)
	b.notIn("a → ", c)
}

func TestManualScan_ASelectionNeverErasesWhatItDidNotLookAt(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack(abc(), nil))
	b.cycle(u, true) // compteurs de délai pour a, b et c
	before := map[string]int64{}
	for k, v := range u.state.Images {
		before[k] = v.FirstSeen
	}
	b.eq(len(before), 3)
	b.world.Images["id-a-old"] = []string{"img/a@" + dg(2)} // a est maintenant à jour
	b.clock.days(1)
	b.cycle(u, true, "a")
	got := map[string]int64{}
	for k, v := range u.state.Images {
		got[k] = v.FirstSeen
	}
	b.eq(got, before, "les compteurs de b et c sont intacts")
}

func TestManualScan_ASelectionKeepsTheKnownErrorsOfTheOthers(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack([]string{"a", "b"}, nil))
	b.world.FailRegistry["img/b:1"] = true
	b.cycle(u, true)
	b.eq(len(b.posts), 1)
	_, known := u.state.Errors["b"]
	b.ok(known)
	b.cycle(u, true, "a")
	_, known = u.state.Errors["b"]
	b.ok(known, "l'erreur de b reste connue")
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "pas de nouvelle alerte pour la même erreur de b")
}

func TestManualScan_CheckNotifySendsTheStateTableOfTheSelection(t *testing.T) {
	b := newBase(t)
	svcs := b.manualStack(abc(), nil)
	b.world.Images["id-c-old"] = []string{"img/c@" + dg(2)} // c à jour
	u := b.make(svcs)
	rc, err := u.Cycle(false, []string{"b", "c"}, true)
	b.ok(err == nil)
	b.eq(rc, 0)
	b.eq(len(b.posts), 1)
	c := b.posts[0].content()
	b.in("État des services", c)
	b.regex(`b\s+différée\s+image `, c)
	b.regex(`c\s+à jour`, c)
	b.ok(!regexpMatch(`\ba\s+(différée|à jour)`, c))
	b.in("2 services scannés", c)
	b.eq(len(b.world.Ups), 0)
}

func TestManualScan_CheckWithoutNotifyStillSendsNothing(t *testing.T) {
	b := newBase(t)
	b.cycle(b.make(b.manualStack(abc(), nil)), false)
	b.eq(len(b.posts), 0)
}

func TestManualScan_CheckNotifyWithoutAWebhookFailsLoudly(t *testing.T) {
	b := newBase(t)
	u := b.make(b.manualStack(abc(), nil))
	if err := os.Remove(b.urlFile); err != nil {
		t.Fatal(err)
	}
	rc, err := u.Cycle(false, nil, true)
	b.ok(err == nil)
	b.eq(rc, 1)
	b.in("aucune URL de notification lisible", b.logtext())
}

func TestManualScan_StatusRows(t *testing.T) {
	b := newBase(t)
	svcs := b.manualStack([]string{"p", "q", "r"}, M{"cooldown": "1d"})
	svcs[2]["mode"] = "notify"
	u := b.make(svcs)
	evals := func() []*Eval {
		var out []*Eval
		for _, s := range u.cfg.Services {
			out = append(out, u.evaluate(s))
		}
		return out
	}
	row := func(e *Eval) []string { a, s, d := u.statusRow(e); return []string{a, s, d} }
	ev := evals()
	b.eq(row(ev[0])[1:], []string{"différée", "image " + dg(2)[7:19] + ", encore 1 j"})
	b.clock.days(1.1)
	ev = evals()
	b.eq(row(ev[2])[1], "prête")
	b.in("mode notify", row(ev[2])[2])
	ev[0].FailedBefore = true
	b.eq(row(ev[0])[1], "à revoir")
	b.world.FailRegistry["img/q:1"] = true
	b.eq(row(u.evaluate(u.cfg.Services[1]))[1], "ERREUR")
	u.state.Holds = append(u.state.Holds, "p")
	b.eq(row(u.evaluate(u.cfg.Services[0]))[1:], []string{"ignoré", "en pause (hold)"})
	b.world.Images["id-r-old"] = []string{"img/r@" + dg(2)}
	b.eq(row(u.evaluate(u.cfg.Services[2])), []string{"r", "à jour", ""})
}
