package updater

import "testing"

// Tests ajoutés par le portage (absents du Python) : ils tuent des mutants que la suite d'origine laisse survivre.

func TestExtra_HoldOfAGroupPausesEveryMember(t *testing.T) {
	b := newBase(t)
	u := b.make(b.stack(3), mk{Settings: M{"default_cooldown": "0"}})
	b.hold(u, true, "tirelire") // le nom du groupe, pas celui d'un service
	b.cycle(u, true)
	b.eq(len(b.world.Ups), 0)
	b.in("en pause", b.logtext())
	if _, err := u.NowCmd([]string{"web"}, true); err == nil {
		t.Error("now sur un membre d'un groupe en pause doit échouer")
	}
	b.hold(u, false, "tirelire")
	b.cycle(u, true)
	b.eq(ups(b.world), []string{"migrate", "web", "admin"})
}

func TestExtra_DryRunNeverWaitsNorProbes(t *testing.T) {
	b := newBase(t)
	b.traefik()
	b.newRemote(2)
	url := "https://exemple.test/"
	probes := 0
	u := b.make([]M{svc(M{"smoke_url": url, "smoke_expect": []int{200}})}, mk{Settings: M{"default_cooldown": "0"}, Dry: true, NoSave: true})
	u.http = func(string) int { probes++; return 0 }
	b.eq(b.cycle(u, true), 0, "la simulation ne juge pas l'état d'un conteneur qu'elle n'a pas touché")
	b.eq(len(b.sleeps), 0, "aucune attente en simulation")
	b.eq(probes, 0, "aucune sonde en simulation")
}

// Caractérisation (comportement du Python, voir PORTAGE.md, proposition 3) : une alerte dont l'envoi échoue est quand même
// marquée « signalée » dans l'état ; elle n'est jamais renvoyée.
func TestExtra_AnAlertWhoseDeliveryFailedIsNotSentAgain(t *testing.T) {
	b := newSec(t)
	u := b.mkSec(nil)
	real := u.notifier.Post
	u.notifier.Post = func(url string, body []byte, headers map[string]string) error { return errString("réseau coupé") }
	b.cycle(u, true)
	b.in("notification non envoyée", b.logtext())
	u.notifier.Post = real
	b.clock.days(2.0 / 24)
	b.cycle(u, true)
	b.eq(len(b.posts), 0, "l'alerte de sécurité détectée pendant la panne n'est plus jamais envoyée")
}

type errString string

func (e errString) Error() string { return string(e) }
