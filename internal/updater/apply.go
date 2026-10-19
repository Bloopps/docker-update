package updater

import (
	"fmt"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// up démarre le service avec l'image déjà en place (jamais de tirage), attend sa santé, puis fait le test de fumée.
func (u *Updater) up(e *Eval) bool {
	svc := e.Svc
	r := u.mutate(time.Duration(svc.HealthTimeout+120)*time.Second,
		with(base(svc), "up", "-d", "--no-deps", "--pull", "never", "--wait", "--wait-timeout", itoa(svc.HealthTimeout), svc.Service)...)
	if r.RC != 0 {
		u.log(fmt.Sprintf("  up a échoué pour %s: %s", svc.Name, textutil.Truncate(textutil.Strip(r.Err), 200)))
		return false
	}
	if u.run.DryRun() {
		return true
	}
	u.sleep(time.Duration(u.cfg.Settings.SettleSeconds) * time.Second)
	cid, err := u.containerID(svc)
	if err != nil || cid == "" {
		return false
	}
	r = u.sh("docker", "inspect", "--format", "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}", cid)
	status, health, _ := strings.Cut(textutil.Strip(r.Out), "|")
	if !(status == "running" && (health == "" || health == "healthy")) {
		h := health
		if h == "" {
			h = "-"
		}
		u.log(fmt.Sprintf("  %s: état après démarrage = %s/%s", svc.Name, status, h))
		return false
	}
	if !u.smoke(svc) {
		u.log(fmt.Sprintf("  %s: test de fumée en échec (%s)", svc.Name, svc.SmokeURL))
		return false
	}
	return true
}

// target : ce qu'il faut tirer et étiqueter pour une référence.
type target struct{ ref, repo, digest string }

// apply met à jour une unité (un service ou un groupe) : tirages d'abord (rien n'est modifié si l'un échoue), puis
// étiquetage local, puis redémarrage service par service, dans l'ordre du fichier.
func (u *Updater) apply(unit string, todo []*Eval) bool {
	names := make([]string, len(todo))
	for i, e := range todo {
		names[i] = e.Svc.Name
	}
	u.log(fmt.Sprintf("mise à jour de %s : %s", unit, strings.Join(names, ", ")))
	var targets []target
	oldIDs := map[string]string{}
	var oldOrder []string
	for _, e := range todo {
		repo, _, _ := textutil.SplitRef(e.Ref)
		if _, ok := oldIDs[e.Ref]; !ok {
			targets = append(targets, target{e.Ref, repo, e.Remote})
			oldIDs[e.Ref] = e.OldImageID
			oldOrder = append(oldOrder, e.Ref)
		}
	}
	for _, t := range targets {
		r := u.mutate(1800*time.Second, "docker", "pull", t.repo+"@"+t.digest)
		if r.RC != 0 {
			msg := fmt.Sprintf("%s: tirage de %s@%s impossible, rien n'a été modifié (%s)", unit, t.repo, short(t.digest), textutil.Truncate(textutil.Strip(r.Err), 150))
			u.log(msg)
			u.report.Add("echec", "Mise à jour impossible : "+unit, msg, "error")
			return false
		}
	}
	for _, t := range targets {
		r := u.mutate(dockerxDefault, "docker", "tag", t.repo+"@"+t.digest, t.ref)
		if r.RC != 0 {
			msg := fmt.Sprintf("%s: étiquetage local de %s impossible, rien n'a été modifié", unit, t.ref)
			u.log(msg)
			u.report.Add("echec", "Mise à jour impossible : "+unit, msg, "error")
			return false
		}
	}
	var touched []*Eval
	for _, e := range todo {
		touched = append(touched, e)
		if !u.up(e) {
			return u.fail(unit, todo, touched, targets)
		}
	}
	shorts := make([]string, len(todo))
	secSet := map[string]bool{}
	for i, e := range todo {
		shorts[i] = fmt.Sprintf("%s → %s", e.Svc.Name, short(e.Remote))
		for _, id := range e.SecIDs {
			secSet[id] = true
		}
	}
	text := strings.Join(shorts, ", ")
	if len(secSet) > 0 {
		ids := make([]string, 0, len(secSet))
		for id := range secSet {
			ids = append(ids, id)
		}
		sortStrings(ids)
		text += "\nCorrectif de sécurité appliqué : " + strings.Join(ids, ", ")
	}
	u.log(fmt.Sprintf("%s: mise à jour réussie (%s)", unit, text))
	first, more, _ := strings.Cut(text, "\n")
	u.report.Add("maj", first, more, "ok")
	u.cleanup(todo, oldOrder, oldIDs)
	return true
}

// cleanup supprime l'image précédente une fois la mise à jour réussie (comme le nettoyage de Watchtower).
// Jamais forcé : docker refuse si un conteneur utilise encore l'image ou si elle porte un autre nom ; ce n'est alors
// qu'une ligne de journal. Un échec de nettoyage n'est pas un échec de mise à jour.
func (u *Updater) cleanup(todo []*Eval, order []string, oldIDs map[string]string) {
	for _, ref := range order {
		old := oldIDs[ref]
		all := true
		for _, e := range todo {
			if e.Ref == ref && !e.Svc.Cleanup {
				all = false
			}
		}
		if old == "" || strings.HasPrefix(old, "-") || !all {
			continue
		}
		r := u.mutate(dockerxDefault, "docker", "image", "rm", old)
		if u.run.DryRun() {
			continue
		}
		if r.RC == 0 {
			u.log(fmt.Sprintf("  nettoyage : image précédente de %s supprimée (%s)", ref, textutil.ShortID(old)))
		} else {
			u.log(fmt.Sprintf("  nettoyage : image précédente de %s conservée (%s)", ref, textutil.CleanText(r.Err, 120)))
		}
	}
}

// fail : échec, on prévient et on s'arrête. AUCUN retour arrière automatique (une base ou une application peut avoir
// migré : défaire à l'aveugle est pire que laisser en l'état). L'image précédente reste sur le disque.
func (u *Updater) fail(unit string, todo, touched []*Eval, targets []target) bool {
	for _, t := range targets {
		rec := u.state.Get(t.ref)
		if rec == nil {
			rec = &state.Image{Digest: t.digest, FirstSeen: u.now()}
		}
		rec.Failed = true
		u.state.Put(t.ref, rec)
	}
	failed := touched[len(touched)-1].Svc.Name
	before := make([]string, len(touched))
	for i, e := range touched {
		before[i] = fmt.Sprintf("%s (%s)", e.Svc.Name, textutil.ShortID(e.OldImageID))
	}
	var untouched []string
	for _, e := range todo {
		found := false
		for _, t := range touched {
			if t == e {
				found = true
			}
		}
		if !found {
			untouched = append(untouched, e.Svc.Name)
		}
	}
	lines := []string{
		failed + " ne repart pas avec la nouvelle image. Aucun retour arrière automatique : intervention manuelle.",
		"Images précédentes conservées : " + strings.Join(before, ", ") + ".",
		"Pour revenir : `docker tag <image> <référence>` puis `docker compose up -d --no-deps <service>`.",
	}
	if len(untouched) > 0 {
		lines = append(lines, "Pas touchés : "+strings.Join(untouched, ", ")+".")
	}
	u.log(unit + ": " + strings.Join(lines, " "))
	title := failed
	if unit != failed {
		title = fmt.Sprintf("%s (%s)", unit, failed)
	}
	u.report.Add("echec", title, strings.Join(lines, "\n"), "error")
	return false
}
