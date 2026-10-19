package updater

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/fakeworld"
)

// ── TestCleanup ─────────────────────────────────────────────────────────────

func (b *env) goCleanup(svcKw M, settings M) *Updater {
	b.traefik()
	b.newRemote(2)
	st := M{"default_cooldown": "0"}
	for k, v := range settings {
		st[k] = v
	}
	return b.make([]M{svc(svcKw)}, mk{Settings: st})
}

func TestCleanup_PreviousImageIsRemovedAfterASuccessfulUpdate(t *testing.T) {
	b := newBase(t)
	u := b.goCleanup(nil, nil)
	b.eq(b.cycle(u, true), 0)
	b.eq(b.world.Removed, []string{"id-traefik-old"})
	b.in("nettoyage : image précédente de traefik:v3.6 supprimée", b.logtext())
	var kinds []string
	for _, c := range b.world.Calls {
		switch {
		case c[0] == "docker" && (c[1] == "pull" || c[1] == "tag"):
			kinds = append(kinds, c[1])
		case c[0] == "docker" && c[1] == "image" && c[2] == "rm":
			kinds = append(kinds, "rm")
		case c[0] == "docker" && c[1] == "compose" && contains(c, "up"):
			kinds = append(kinds, "up")
		}
	}
	b.eq(kinds[len(kinds)-1], "rm", "le nettoyage vient APRÈS la mise à jour réussie")
	b.eq(b.cmds("docker", "image", "rm")[0], []string{"docker", "image", "rm", "id-traefik-old"}, "jamais forcé, jamais de joker")
}

func TestCleanup_NothingIsRemovedWhenTheUpdateFails(t *testing.T) {
	b := newBase(t)
	u := b.goCleanup(nil, nil)
	b.world.FailUp[id(2)] = true
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.world.Removed), 0, "l'image précédente reste le moyen de revenir à la main")
	b.eq(len(b.cmds("docker", "image", "rm")), 0)
}

func TestCleanup_NothingIsRemovedWithoutAnUpdate(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.cycle(b.make([]M{svc(nil)}), true)
	b.eq(len(b.cmds("docker", "image", "rm")), 0)
}

func TestCleanup_ARefusedRemovalDoesNotFailTheUpdate(t *testing.T) {
	b := newBase(t)
	u := b.goCleanup(nil, nil)
	b.world.KeepImages["id-traefik-old"] = true // docker refuse : encore utilisée ailleurs
	b.eq(b.cycle(u, true), 0)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2))
	b.in("image précédente de traefik:v3.6 conservée", b.logtext())
	b.in("stopped container", b.logtext())
	b.notIn("ÉCHEC", b.posted())
}

func TestCleanup_CanBeDisabledGloballyAndPerService(t *testing.T) {
	b := newBase(t)
	b.cycle(b.goCleanup(nil, M{"cleanup": false}), true)
	b.eq(len(b.world.Removed), 0)
	b = newBase(t)
	b.cycle(b.goCleanup(M{"cleanup": false}, nil), true)
	b.eq(len(b.world.Removed), 0)
	b = newBase(t)
	b.cycle(b.goCleanup(M{"cleanup": true}, M{"cleanup": false}), true)
	b.eq(b.world.Removed, []string{"id-traefik-old"}, "le réglage du service l'emporte")
}

func TestCleanup_DefaultIsOn(t *testing.T) {
	b := newBase(t)
	one := M{"name": "a", "compose_dir": "/x", "service": "a"}
	with := func(extra M) M {
		d := M{}
		for k, v := range one {
			d[k] = v
		}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	cfg, err := tryConfig(M{}, []M{one})
	b.ok(err == nil && cfg.Services[0].Cleanup)
	cfg, err = tryConfig(M{"cleanup": false}, []M{one})
	b.ok(err == nil && !cfg.Services[0].Cleanup)
	for name, bad := range map[string]struct {
		settings M
		services []M
	}{
		"réglage texte":  {M{"cleanup": "oui"}, []M{one}},
		"service texte":  {M{}, []M{with(M{"cleanup": "non"})}},
		"service entier": {M{}, []M{with(M{"cleanup": 0})}},
		"groupe":         {M{}, []M{with(M{"group": "g"}), with(M{"name": "b", "group": "g", "cleanup": false})}}, // un groupe a un seul réglage
	} {
		if _, err := tryConfig(bad.settings, bad.services); err == nil {
			t.Errorf("%s : la configuration aurait dû être refusée", name)
		}
	}
}

func TestCleanup_GroupCleansEachPreviousImageOnce(t *testing.T) {
	b := newBase(t)
	b.world.Add("web", "/stack/app", "web", "ghcr.io/x/app:1", 1)
	b.world.Add("worker", "/stack/app", "worker", "ghcr.io/x/app:1", 1)
	b.world.Add("db", "/stack/app", "db", "ghcr.io/x/db:1", 1)
	b.world.Registry["ghcr.io/x/app:1"] = manifest(2)
	b.world.Registry["ghcr.io/x/db:1"] = manifest(2)
	var svcs []M
	for _, n := range []string{"web", "worker", "db"} {
		svcs = append(svcs, svcN(n, "/stack/app", "", M{"group": "g"}))
	}
	u := b.make(svcs, mk{Settings: M{"default_cooldown": "0"}})
	b.eq(b.cycle(u, true), 0)
	removed := append([]string{}, b.world.Removed...)
	sort.Strings(removed)
	b.eq(removed, []string{"id-db-old", "id-web-old"}, "une image par référence ; web et worker partagent la leur")
}

func TestCleanup_DryRunRemovesNothing(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}, Dry: true, NoSave: true})
	b.eq(b.cycle(u, true), 0)
	b.eq(len(b.world.Removed), 0)
	b.notIn("supprimée", b.logtext())
}

func TestCleanup_NowAlsoCleans(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(M{"mode": "notify"})})
	b.eq(b.nowCmd(u, true, "traefik"), 0)
	b.eq(b.world.Removed, []string{"id-traefik-old"})
}

func TestCleanup_OptionLikeImageIDIsNeverPassed(t *testing.T) {
	b := newBase(t)
	b.world.Add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1)
	b.world.Images["--force"] = []string{"traefik@" + dg(1)}
	b.world.Containers[fakeworld.Key{Dir: "/stack/traefik", Service: "traefik"}].ImageID = "--force"
	b.newRemote(2)
	b.cycle(b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}}), true)
	b.eq(len(b.cmds("docker", "image", "rm")), 0)
}

// ── TestStoppedContainers ───────────────────────────────────────────────────

func newStopped(t *testing.T) *env {
	b := newBase(t)
	b.world.Add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1, fakeworld.WithVersion("v3.7.13"))
	b.newRemote(2)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(), "a"}
	b.ghRel["traefik/traefik"] = ghReply{200, []any{release(b.clock, "v3.7.14", "Fixes CVE-2026-1111")}, "r"}
	return b
}

func (b *env) mkStopped(kw M) *Updater {
	d := M{"advisories": []string{"traefik/traefik"}, "release_notes": true}
	for k, v := range kw {
		d[k] = v
	}
	return b.make([]M{svc(d)}, mk{Settings: M{"default_cooldown": "0"}})
}

func (b *env) stop(status string) {
	b.world.Containers[fakeworld.Key{Dir: "/stack/traefik", Service: "traefik"}].Status = status
}

func TestStopped_AStoppedServiceIsNotTouchedAtAll(t *testing.T) {
	for _, status := range []string{"exited", "created", "dead", "paused", "removing"} {
		t.Run(status, func(t *testing.T) {
			b := newStopped(t)
			b.stop(status)
			u := b.mkStopped(nil)
			b.eq(b.cycle(u, true), 0)
			b.in("conteneur arrêté ("+status+"), non touché", b.logtext())
			b.eq(len(b.world.Ups), 0)
			for _, sub := range [][]string{{"docker", "pull"}, {"docker", "tag"}, {"docker", "image", "rm"},
				{"docker", "buildx", "imagetools"}, {"regctl"}} {
				b.eq(len(b.cmds(sub...)), 0, strings.Join(sub, " ")+" ne doit pas être lancé")
			}
			b.eq(len(b.ghCalls), 0, "ni avis ni notes de release pour un service arrêté")
			b.eq(len(b.posts), 0)
			b.eq(len(u.state.Images), 0, "aucun compteur de délai ne démarre")
		})
	}
}

func TestStopped_NotifyManualAndCheckLeaveItAloneToo(t *testing.T) {
	b := newStopped(t)
	b.stop("exited")
	for _, mode := range []string{"notify", "manual", "auto"} {
		b.cycle(b.mkStopped(M{"mode": mode}), true)
	}
	b.cycle(b.mkStopped(nil), false)
	b.eq(len(b.world.Ups), 0)
	b.eq(len(b.ghCalls), 0)
	b.eq(len(b.posts), 0)
}

func TestStopped_NowDoesNotStartOrUpdateAStoppedService(t *testing.T) {
	b := newStopped(t)
	b.stop("exited")
	u := b.mkStopped(M{"mode": "notify"})
	b.eq(b.nowCmd(u, true, "traefik"), 0)
	b.in("rien à mettre à jour", b.logtext())
	b.eq(len(b.world.Ups), 0)
	b.eq(b.world.Containers[fakeworld.Key{Dir: "/stack/traefik", Service: "traefik"}].Status, "exited")
}

func TestStopped_RestartingIsNotStopped(t *testing.T) {
	b := newStopped(t)
	b.stop("restarting")
	b.eq(b.cycle(b.mkStopped(nil), true), 0)
	b.eq(len(b.world.Ups), 1)
}

func TestStopped_AStartedServiceIsPickedUpAgain(t *testing.T) {
	b := newStopped(t)
	b.stop("exited")
	u := b.mkStopped(nil)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.stop("running")
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1)
}

func TestStopped_OnlyTheStoppedMemberOfAGroupIsLeftAlone(t *testing.T) {
	b := newStopped(t)
	b.world.Add("web", "/stack/app", "web", "ghcr.io/x/app:1", 1)
	b.world.Add("worker", "/stack/app", "worker", "ghcr.io/x/app:1", 1)
	b.world.Registry["ghcr.io/x/app:1"] = manifest(2)
	b.world.Containers[fakeworld.Key{Dir: "/stack/app", Service: "worker"}].Status = "exited"
	var svcs []M
	for _, n := range []string{"web", "worker"} {
		svcs = append(svcs, svcN(n, "/stack/app", "", M{"group": "g"}))
	}
	u := b.make(svcs, mk{Settings: M{"default_cooldown": "0"}})
	b.eq(b.cycle(u, true), 0)
	b.eq(ups(b.world), []string{"web"}, "le service arrêté n'est ni mis à jour ni démarré")
	b.eq(b.world.Containers[fakeworld.Key{Dir: "/stack/app", Service: "worker"}].Status, "exited")
}

func TestStopped_UnreadableStatusIsAnErrorAndChangesNothing(t *testing.T) {
	b := newStopped(t)
	orig := b.world.Run
	u := b.mkStopped(nil)
	u.run = wrapRunner{b.world, func(argv []string, mutating bool, timeout time.Duration) dockerx.Result {
		if argv[0] == "docker" && argv[1] == "inspect" && argv[3] == "{{.State.Status}}" {
			return dockerx.Result{RC: 1, Out: "running\n", Err: "boom"} // échec de docker : on ne se fie pas à la sortie
		}
		return orig(argv, mutating, timeout)
	}}
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.world.Ups), 0)
	b.in("ERREUR", b.logtext())
}

// ── TestAdvisoriesAndReleaseNotesTogether ───────────────────────────────────

const (
	ghsa = "GHSA-2222-3333-4444"
	cve  = "CVE-2026-12345"
	link = "Notes de release qui le citent : v3.7.14 https://github.com/traefik/traefik/releases/tag/v3.7.14"
)

func newTogether(t *testing.T) *env {
	b := newBase(t)
	b.world.Add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1, fakeworld.WithVersion("v3.7.13"))
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advGHSA(ghsa), advCVE(cve), advRange("< 3.7.14")), "a"}
	return b
}

func (b *env) mkTogether(kw M, o ...mk) *Updater {
	d := M{"advisories": []string{"traefik/traefik"}, "release_notes": true}
	for k, v := range kw {
		d[k] = v
	}
	return b.make([]M{svc(d)}, o...)
}

func (b *env) notes(rels ...M) {
	list := make([]any, len(rels))
	for i, r := range rels {
		list[i] = r
	}
	b.ghRel["traefik/traefik"] = ghReply{200, list, "r"}
}

func TestTogether_TheReleaseThatCitesTheAdvisoryCompletesItInsteadOfASecondAlert(t *testing.T) {
	b := newTogether(t)
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
	b.cycle(b.mkTogether(nil), true)
	b.eq(len(b.posts), 1, b.logtext())
	b.in("VULNÉRABLE", b.posted())
	b.in(link, b.posted())
	b.notIn("Notes de release à lire", b.posted())
	b.in("ne cite que l'avis", b.logtext())
}

func TestTogether_CVEAndCaseAreMatched(t *testing.T) {
	b := newTogether(t)
	b.notes(release(b.clock, "v3.7.14", "see cve-2026-12345"))
	b.cycle(b.mkTogether(nil), true)
	b.eq(len(b.posts), 1)
	b.in(link, b.posted())
	b = newTogether(t)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advGHSA("GHSA-cfgh-jmpq-rvwx"), advRange("< 3.7.14")), "a"}
	b.notes(release(b.clock, "v3.7.14", "GHSA-CFGH-JMPQ-RVWX"))
	b.cycle(b.mkTogether(nil), true)
	b.eq(len(b.posts), 1)
	b.in(link, b.posted())
}

func TestTogether_AReleaseWithMoreThanTheAdvisoryStillAlertsForTheRest(t *testing.T) {
	b := newTogether(t)
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa+" and CVE-2026-99999"))
	b.cycle(b.mkTogether(nil), true)
	b.eq(len(b.posts), 1, "avis et note dans le même rapport")
	content := b.posts[0].content()
	b.in("VULNÉRABLE", content)
	parts := strings.Split(content, "Notes de release à lire")
	rel := parts[1]
	b.in("CVE-2026-99999", rel)
	b.notIn("Motifs : "+ghsa, rel)
	b.ok(!regexp.MustCompile(ghsa).MatchString(strings.Split(rel, "Extrait")[0]))
	b.in("Déjà couvert par l'avis : "+ghsa, rel)
	b.in(link, parts[0], "le lien complète l'alerte de l'avis")
}

func TestTogether_WeakSignalInAFullyCoveredReleaseIsNotReported(t *testing.T) {
	b := newTogether(t)
	b.notes(release(b.clock, "v3.7.14", "Better security. Fixes "+ghsa))
	b.cycle(b.mkTogether(nil), true)
	b.eq(len(b.posts), 1)
	b.notIn("mot « security »", b.posted())
}

func TestTogether_AnAdvisoryTheToolIgnoresDoesNotCoverTheRelease(t *testing.T) {
	for name, mkAdv := range map[string]func(b *env) M{
		"gravité trop faible": func(b *env) M { return b.adv(advGHSA(ghsa), advCVE(cve), advSev("low"), advRange("< 3.7.14")) },
		"avis retiré":         func(b *env) M { return b.adv(advGHSA(ghsa), advCVE(cve), advRange("< 3.7.14"), advWithdrawn()) },
		"avis trop ancien":    func(b *env) M { return b.adv(advGHSA(ghsa), advCVE(cve), advRange("< 3.7.14"), advHours(24*45)) },
	} {
		t.Run(name, func(t *testing.T) {
			b := newTogether(t)
			b.gh["traefik/traefik"] = ghReply{200, []any{mkAdv(b)}, "a"}
			b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
			b.cycle(b.mkTogether(nil), true)
			b.eq(len(b.posts), 1)
			b.in("Notes de release à lire", b.posted(), "la note est la seule source")
			b.notIn("Déjà couvert", b.posted())
		})
	}
}

func TestTogether_AnAdvisoryOfAnotherRepositoryDoesNotCover(t *testing.T) {
	b := newTogether(t)
	b.gh["other/repo"] = ghReply{200, []any{}, "o"}
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
	b.cycle(b.mkTogether(M{"advisories": []string{"other/repo"}, "release_notes_repos": []string{"traefik/traefik"}}), true)
	b.eq(len(b.posts), 1)
	b.in("Notes de release à lire", b.posted())
}

func TestTogether_AnAdvisoryThatDoesNotConcernTheRunningVersionStaysSilentWithItsRelease(t *testing.T) {
	b := newTogether(t)
	b.gh["traefik/traefik"] = ghReply{200, b.advs(advGHSA(ghsa), advCVE(cve), advRange("< 3.0.0")), "a"}
	b.notes(release(b.clock, "v3.7.14", "Backport of "+ghsa))
	b.cycle(b.mkTogether(nil), true)
	b.eq(len(b.posts), 0, "l'avis ne touche pas la version en cours : rien à dire, ni de l'un ni de l'autre")
}

func TestTogether_ConfirmedFixAlertCarriesTheReleaseLinkAndAccelerationStaysWithTheAdvisory(t *testing.T) {
	b := newTogether(t)
	b.newRemote(2)
	b.world.DigestVersions[dg(2)] = "3.7.14"
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
	u := b.mkTogether(M{"mode": "auto", "security_cooldown": "0", "cooldown": "30d"})
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "détection + mise à jour réussie dans le même rapport")
	content := b.posts[0].content()
	b.in("Correctif de sécurité détecté", content)
	b.in("Mises à jour réussies", content)
	b.in(link, content)
	b.notIn("Notes de release à lire", content, "jamais de seconde entrée pour la note")
	b.eq(len(b.world.Ups), 1, "accélérée par l'AVIS, pas par la note")
}

func TestTogether_ReleaseAloneAcceleratesNothingEvenWhenTheAdvisoryExistsElsewhere(t *testing.T) {
	b := newTogether(t)
	b.gh["traefik/traefik"] = ghReply{200, []any{}, "a"} // aucun avis publié
	b.newRemote(2)
	b.world.DigestVersions[dg(2)] = "3.7.14"
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
	u := b.mkTogether(M{"mode": "auto", "security_cooldown": "0", "cooldown": "30d"})
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.eq(len(b.posts), 1)
	b.in("Notes de release à lire", b.posted())
}

func TestTogether_NothingIsRepeatedNextTime(t *testing.T) {
	b := newTogether(t)
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
	u := b.mkTogether(nil)
	b.cycle(u, true)
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.posts), 1)
}

func TestTogether_CheckLogsBothAndSendsNothing(t *testing.T) {
	b := newTogether(t)
	b.notes(release(b.clock, "v3.7.14", "Fixes "+ghsa))
	b.cycle(b.mkTogether(nil), false)
	b.eq(len(b.posts), 0)
	b.in("ne cite que l'avis", b.logtext())
}
