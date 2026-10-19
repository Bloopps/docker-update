package updater

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/fakeworld"
)

// ── TestCycle ───────────────────────────────────────────────────────────────

func TestCycle_UpToDateDoesNothing(t *testing.T) {
	b := newBase(t)
	b.traefik()
	u := b.make([]M{svc(nil)})
	b.eq(b.cycle(u, true), 0)
	b.eq(ups(b.world), []string{})
	b.eq(len(u.state.Images), 0)
	b.in("traefik: à jour", b.logtext())
}

func TestCycle_NewDigestIsDeferredThenApplied(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)})
	b.cycle(u, true)
	b.eq(ups(b.world), []string{}, "pas de mise à jour avant la fin du délai")
	b.eq(u.state.Images["traefik:v3.6"].FirstSeen, b.clock.t)
	b.in("différée", b.logtext())
	b.eq(len(b.posts), 0, "aucune notification pour une simple différée")
	b.clock.days(2.9)
	b.cycle(u, true)
	b.eq(ups(b.world), []string{})
	b.clock.days(0.2) // 3,1 jours
	b.eq(b.cycle(u, true), 0)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2))
	// tirage par digest exact, pas par étiquette
	b.eq(b.cmds("docker", "pull"), [][]string{{"docker", "pull", "traefik@" + dg(2)}})
	b.eq(b.cmds("docker", "tag")[0], []string{"docker", "tag", "traefik@" + dg(2), "traefik:v3.6"})
	b.eq(len(b.posts), 1)
	b.in("Mises à jour réussies", b.posts[0].content())
	b.in("traefik → "+dg(2)[7:19], b.posts[0].content())
}

func TestCycle_OrderOfOperations(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}})
	b.cycle(u, true)
	var kinds []string
	var up []string
	for _, c := range b.world.Calls {
		switch {
		case c[0] == "docker" && c[1] == "pull":
			kinds = append(kinds, "pull")
		case c[0] == "docker" && c[1] == "tag":
			kinds = append(kinds, "tag")
		case c[0] == "docker" && c[1] == "compose" && contains(c, "up"):
			kinds = append(kinds, "up")
			if up == nil {
				up = c
			}
		}
	}
	b.eq(kinds, []string{"pull", "tag", "up"})
	for _, flag := range []string{"--no-deps", "--wait", "never"} {
		b.ok(contains(up, flag), "drapeau absent : "+flag)
	}
	b.eq(b.sleeps, []time.Duration{7 * time.Second}, "attente de stabilisation après démarrage")
}

func TestCycle_DigestMovingResetsTimer(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)})
	b.cycle(u, true)
	b.clock.days(2)
	b.newRemote(3) // l'étiquette bouge avant la fin du délai
	b.cycle(u, true)
	b.clock.days(1.5) // 3,5 j depuis D2, 1,5 j depuis D3
	b.cycle(u, true)
	b.eq(ups(b.world), []string{})
	b.clock.days(1.6) // 3,1 j depuis D3
	b.cycle(u, true)
	b.eq(b.cmds("docker", "pull"), [][]string{{"docker", "pull", "traefik@" + dg(3)}})
}

func TestCycle_TagMovingBackClearsState(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)})
	b.cycle(u, true)
	b.ok(u.state.Images["traefik:v3.6"] != nil)
	b.newRemote(1) // retour au digest en cours
	b.cycle(u, true)
	b.eq(len(u.state.Images), 0)
}

func TestCycle_StatePersistsBetweenRuns(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	b.cycle(b.make([]M{svc(nil)}), true)
	first := b.clock.t
	b.clock.days(4)
	u2 := b.make([]M{svc(nil)}) // nouveau processus : recharge l'état du disque
	b.eq(u2.state.Images["traefik:v3.6"].FirstSeen, first)
	b.cycle(u2, true)
	b.eq(len(b.world.Ups), 1)
}

func TestCycle_CheckNeverAppliesOrNotifies(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}})
	b.cycle(u, false)
	b.eq(ups(b.world), []string{})
	b.eq(len(b.cmds("docker", "pull")), 0)
	b.eq(len(b.posts), 0)
	_, err := os.Stat(b.statePath)
	b.ok(err == nil, "check mémorise ce qu'il a vu")
}

// ── TestFailures ────────────────────────────────────────────────────────────

func (b *env) goFailure(kw M) *Updater {
	b.traefik()
	b.newRemote(2)
	return b.make([]M{svc(kw)}, mk{Settings: M{"default_cooldown": "0"}})
}

func TestFailures_FailedStartAlertsAndNeverRollsBack(t *testing.T) {
	b := newBase(t)
	u := b.goFailure(nil)
	b.world.FailUp[id(2)] = true
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.world.Ups), 1, "une seule tentative de démarrage : pas de second `up` pour revenir")
	b.eq(b.world.Tags["traefik:v3.6"], id(2), "l'étiquette n'est pas remise en arrière")
	b.eq(b.cmds("docker", "tag"), [][]string{{"docker", "tag", "traefik@" + dg(2), "traefik:v3.6"}})
	b.ok(u.state.Images["traefik:v3.6"].Failed)
	msg := b.allPosts()
	b.eq(len(b.posts), 1)
	b.in("Échecs de mise à jour", msg)
	b.in("Aucun retour arrière automatique", msg)
	b.in("id-traefik-old", msg, "l'image précédente est nommée pour un retour à la main")
}

func TestFailures_FailedDigestIsNotRetriedButNewOneIs(t *testing.T) {
	b := newBase(t)
	u := b.goFailure(nil)
	b.world.FailUp[id(2)] = true
	b.cycle(u, true)
	n := len(b.world.Ups)
	b.cycle(u, true)
	b.eq(len(b.world.Ups), n, "même digest en échec : pas de nouvelle tentative")
	b.in("déjà tentée", b.logtext())
	b.newRemote(4) // nouvelle version publiée
	b.cycle(u, true)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(4))
}

func TestFailures_RestartingAfterUpCountsAsFailure(t *testing.T) {
	b := newBase(t)
	u := b.goFailure(nil)
	b.world.Restarting[id(2)] = true // up réussit mais le conteneur boucle
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.world.Ups), 1, "pas de second démarrage pour « revenir »")
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2))
}

func TestFailures_PullFailureAbortsBeforeAnyChange(t *testing.T) {
	b := newBase(t)
	u := b.goFailure(nil)
	b.world.FailPull[dg(2)] = true
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.cmds("docker", "tag")), 0)
	b.eq(len(b.world.Ups), 0)
	// un échec réseau n'est pas marqué « image défaillante » : on réessaiera
	b.ok(!u.state.Images["traefik:v3.6"].Failed)
}

func TestFailures_RegistryErrorIsReportedOnce(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.world.FailRegistry["traefik:v3.6"] = true
	u := b.make([]M{svc(nil)})
	b.eq(b.cycle(u, true), 1)
	b.eq(len(b.posts), 1)
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(len(b.posts), 1, "même erreur : pas de répétition")
	b.world.FailRegistry = map[string]bool{}
	b.cycle(u, true)
	b.eq(len(u.state.Errors), 0)
	b.world.FailRegistry["traefik:v3.6"] = true
	b.cycle(u, true)
	b.eq(len(b.posts), 2, "l'erreur revient après résolution : on prévient de nouveau")
}

func TestFailures_SmokeTest(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	url := "https://exemple.test/"
	u := b.make([]M{svc(M{"smoke_url": url, "smoke_expect": []int{302, 401}})}, mk{Settings: M{"default_cooldown": "0"}})
	b.httpCodes[url] = 0 // le site ne répond pas après la mise à jour
	b.eq(b.cycle(u, true), 1)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2), "test de fumée en échec : alerte, aucun retour arrière")
	b.eq(len(b.world.Ups), 1)
	b.in("test de fumée", b.logtext())
	five := 0
	for _, s := range b.sleeps {
		if s == 5*time.Second {
			five++
		}
	}
	b.ok(five >= 2, "trois essais espacés")
}

func TestFailures_SmokeTestPasses(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	url := "https://exemple.test/"
	b.httpCodes[url] = 302
	u := b.make([]M{svc(M{"smoke_url": url, "smoke_expect": []int{302, 401}})}, mk{Settings: M{"default_cooldown": "0"}})
	b.eq(b.cycle(u, true), 0)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2))
}

// ── TestModesAndPolicies ────────────────────────────────────────────────────

func TestModes_NotifyModeNotifiesOnceAndNeverApplies(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(M{"mode": "notify", "cooldown": "1d"})})
	b.cycle(u, true)
	b.eq(len(b.posts), 0)
	b.clock.days(1.1)
	b.cycle(u, true)
	b.cycle(u, true)
	b.eq(len(b.posts), 1)
	b.eq(len(b.world.Ups), 0)
	content := b.posts[0].content()
	b.in("Nouvelles versions prêtes", content)
	b.in("docker-update now <service> --yes", content)
	b.regex(`traefik\s+`+dg(2)[7:19]+`\s+1 j`, content)
}

func TestModes_ManualModeIsSilent(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(M{"mode": "manual", "cooldown": "0"})})
	b.cycle(u, true)
	b.eq(len(b.posts), 0)
	b.eq(len(b.world.Ups), 0)
}

func TestModes_Window(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(M{"window": "02:00-05:00"})}, mk{Settings: M{"default_cooldown": "0"}})
	b.minutes = 14 * 60
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("hors de la fenêtre", b.logtext())
	b.minutes = 3 * 60
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1)
}

func TestModes_GlobalWindowAndOverride(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	b.minutes = 14 * 60
	// sans clé « window » : la fenêtre globale s'applique
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0", "window": "02:00-05:00"}})
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0, "la fenêtre globale s'applique")
	// « window = "" » explicite : ce service n'est limité par aucune fenêtre
	u = b.make([]M{svc(M{"window": ""})}, mk{Settings: M{"default_cooldown": "0", "window": "02:00-05:00"}})
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1, "une fenêtre vide désactive la fenêtre globale")
	// une fenêtre propre remplace la globale
	cfg := parseConfig(t, M{"window": "02:00-05:00"}, []M{{"name": "a", "compose_dir": "/x", "service": "a", "window": "10:00-11:00"}})
	w := cfg.Services[0].Window
	b.eq([2]int{w.Start, w.End}, [2]int{600, 660})
}

func TestModes_Hold(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}})
	b.hold(u, true, "traefik")
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("en pause", b.logtext())
	if _, err := u.NowCmd([]string{"traefik"}, true); err == nil {
		t.Error("now sur un service en pause doit échouer")
	}
	if _, err := u.Hold([]string{"inconnu"}, true); err == nil {
		t.Error("hold d'un nom inconnu doit échouer")
	}
	b.hold(u, false, "traefik")
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 1)
}

func TestModes_HoldSurvivesRestart(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.hold(b.make([]M{svc(nil)}), true, "traefik")
	b.eq(b.make([]M{svc(nil)}).state.Holds, []string{"traefik"})
}

func TestModes_Skips(t *testing.T) {
	b := newBase(t)
	b.world.Add("pinned", "/s/p", "p", "app:1@sha256:"+string(bytes.Repeat([]byte("a"), 64)), 1)
	b.world.Add("absent", "/s/a", "a", "app2:1", 1, fakeworld.Absent())
	b.world.Add("local", "/s/l", "l", "app3:1", 1, fakeworld.NoDigests())
	b.world.Registry["app3:1"] = manifest(9)
	u := b.make([]M{svcN("pinned", "/s/p", "p", nil), svcN("absent", "/s/a", "a", nil), svcN("local", "/s/l", "l", nil)})
	b.eq(b.cycle(u, true), 0)
	text := b.logtext()
	b.in("épinglée", text)
	b.in("conteneur absent", text)
	b.in("sans digest", text)
	b.eq(len(b.world.Ups), 0)
}

func TestModes_FloatingTagWarning(t *testing.T) {
	b := newBase(t)
	b.world.Add("x", "/s/x", "x", "app:latest", 1)
	u := b.make([]M{svcN("x", "/s/x", "x", nil)})
	b.cycle(u, true)
	b.in("flottante", b.logtext())
}

func TestModes_RegctlBackend(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Tool: "regctl", Settings: M{"default_cooldown": "0"}})
	b.cycle(u, true)
	b.eq(b.cmds("regctl", "image", "digest"), [][]string{{"regctl", "image", "digest", "traefik:v3.6"}})
	b.eq(len(b.world.Ups), 1)
}

func TestModes_BadManifestOutputIsAnError(t *testing.T) {
	for name, junk := range map[string][]byte{"vide": {}, "html": []byte("<html>proxy error</html>"),
		"trop gros": bytes.Repeat([]byte("x"), maxManifest+1)} {
		t.Run(name, func(t *testing.T) {
			b := newBase(t)
			b.traefik()
			b.world.Registry["traefik:v3.6"] = junk
			u := b.make([]M{svc(nil)})
			b.eq(b.cycle(u, true), 1)
			b.eq(len(b.world.Ups), 0)
			b.eq(len(u.state.Images), 0, "aucun état créé sur une réponse invalide")
		})
	}
}

func TestModes_RegctlBadOutputIsAnError(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.world.Registry["traefik:v3.6"] = []byte{} // le faux regctl répond « \n »
	u := b.make([]M{svc(nil)}, mk{Tool: "regctl"})
	b.eq(b.cycle(u, true), 1)
}

func TestModes_BuildxReadsTheRawManifestAndNotATemplate(t *testing.T) {
	b := newBase(t)
	b.traefik()
	u := b.make([]M{svc(nil)})
	b.cycle(u, true)
	var call []string
	for _, c := range b.world.Calls {
		if contains(c, "imagetools") {
			call = c
			break
		}
	}
	b.eq(call, []string{"docker", "buildx", "imagetools", "inspect", "traefik:v3.6", "--raw"})
	b.ok(!contains(call, "--format"), "pas de gabarit : il ferait télécharger les attestations")
}

// ── TestGroups ──────────────────────────────────────────────────────────────

func (b *env) stack(n int) []M {
	names := []string{"migrate", "web", "admin"}[:n]
	for _, s := range names {
		b.world.Add(s, "/stack/tirelire", s, "ghcr.io/org/tirelire:main", 1)
	}
	b.world.Registry["ghcr.io/org/tirelire:main"] = manifest(2)
	var out []M
	for _, s := range names {
		out = append(out, svcN(s, "/stack/tirelire", s, M{"group": "tirelire"}))
	}
	return out
}

func TestGroups_UpdatesInOrderWithSinglePull(t *testing.T) {
	b := newBase(t)
	u := b.make(b.stack(3), mk{Settings: M{"default_cooldown": "0"}})
	b.eq(b.cycle(u, true), 0)
	b.eq(ups(b.world), []string{"migrate", "web", "admin"})
	b.eq(len(b.cmds("docker", "pull")), 1)
	b.eq(len(b.posts), 1, "un seul message pour tout le groupe")
}

func TestGroups_FailureStopsThereAndLeavesEverythingAsIs(t *testing.T) {
	b := newBase(t)
	u := b.make(b.stack(3), mk{Settings: M{"default_cooldown": "0"}})
	// échec seulement sur admin (3e) : on le simule via une commande qui échoue pour admin uniquement
	orig := b.world.Run
	u.run = wrapRunner{b.world, func(argv []string, mutating bool, timeout time.Duration) dockerx.Result {
		if argv[0] == "docker" && argv[1] == "compose" && contains(argv, "up") && argv[len(argv)-1] == "admin" &&
			b.world.Tags["ghcr.io/org/tirelire:main"] == id(2) {
			b.world.Ups = append(b.world.Ups, fakeworld.Up{Service: "admin", Image: "échec"})
			return dockerx.Result{RC: 1, Err: "unhealthy"}
		}
		return orig(argv, mutating, timeout)
	}}
	b.eq(b.cycle(u, true), 1)
	for _, s := range []string{"migrate", "web"} {
		b.eq(b.containerImage("/stack/tirelire", s), id(2), s+" reste mis à jour")
	}
	b.eq(ups(b.world), []string{"migrate", "web", "admin"}, "aucun redémarrage en sens inverse")
	msg := b.allPosts()
	b.in("admin ne repart pas", msg)
	b.in("migrate (id-migrate-old)", msg)
	b.notIn("Pas touchés", msg, "tout le groupe avait été touché")
}

func TestGroups_StopsAtTheFirstFailureAndNamesTheUntouched(t *testing.T) {
	b := newBase(t)
	u := b.make(b.stack(3), mk{Settings: M{"default_cooldown": "0"}})
	b.world.FailUp[id(2)] = true
	b.eq(b.cycle(u, true), 1)
	b.eq(ups(b.world), []string{"migrate"}, "on s'arrête au premier échec")
	msg := b.allPosts()
	b.in("Aucun retour arrière automatique", msg)
	b.in("Pas touchés : web, admin", msg)
}

func TestGroups_SharedRefKeepsTimerWhenOnlyOneMemberIsUpToDate(t *testing.T) {
	b := newBase(t)
	u := b.make(b.stack(2))
	b.cycle(u, true)
	t0 := u.state.Images["ghcr.io/org/tirelire:main"].FirstSeen
	// migrate passe à jour tout seul, web reste en retard
	b.world.Images["id-migrate-old"] = []string{"ghcr.io/org/tirelire@" + dg(2)}
	b.clock.days(1)
	b.cycle(u, true)
	b.eq(u.state.Images["ghcr.io/org/tirelire:main"].FirstSeen, t0)
}

func TestGroups_NowTargetsGroupByName(t *testing.T) {
	b := newBase(t)
	u := b.make(b.stack(3), mk{Settings: M{"default_cooldown": "30d"}})
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.eq(b.nowCmd(u, true, "tirelire"), 0)
	b.eq(ups(b.world), []string{"migrate", "web", "admin"})
}

// ── TestNow ─────────────────────────────────────────────────────────────────

func TestNow_BypassesCooldownWindowAndMode(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(M{"mode": "notify", "window": "02:00-05:00", "cooldown": "30d"})})
	b.minutes = 15 * 60
	b.eq(b.nowCmd(u, false, "traefik"), 0)
	b.eq(len(b.world.Ups), 0, "sans --yes : simulation")
	b.in("--yes", b.logtext())
	b.eq(b.nowCmd(u, true, "traefik"), 0)
	b.eq(len(b.world.Ups), 1)
}

func TestNow_UnknownName(t *testing.T) {
	b := newBase(t)
	b.traefik()
	u := b.make([]M{svc(nil)})
	if _, err := u.NowCmd([]string{"nimporte"}, true); err == nil {
		t.Error("un nom inconnu doit échouer")
	}
}

func TestNow_NothingToDo(t *testing.T) {
	b := newBase(t)
	b.traefik()
	u := b.make([]M{svc(nil)})
	b.eq(b.nowCmd(u, true, "traefik"), 0)
	b.in("rien à mettre à jour", b.logtext())
}

func TestNow_StillRetriesAFailedDigest(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}})
	b.world.FailUp[id(2)] = true
	b.cycle(u, true)
	b.world.FailUp = map[string]bool{}
	b.eq(b.nowCmd(u, true, "traefik"), 0)
	b.eq(b.containerImage("/stack/traefik", "traefik"), id(2))
}

// ── TestDryRun ──────────────────────────────────────────────────────────────

func TestDryRun_ChangesNothing(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	u := b.make([]M{svc(nil)}, mk{Settings: M{"default_cooldown": "0"}, Dry: true, NoSave: true})
	b.eq(b.cycle(u, true), 0)
	b.eq(len(b.cmds("docker", "pull")), 0)
	b.eq(len(b.cmds("docker", "tag")), 0)
	b.eq(len(b.world.Ups), 0)
	b.eq(len(b.posts), 0)
	_, err := os.Stat(b.statePath)
	b.ok(os.IsNotExist(err), "la simulation n'écrit pas l'état")
	b.in("[simulation]", b.logtext())
}
