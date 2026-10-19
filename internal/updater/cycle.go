package updater

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/textutil"
)

type unitGroup struct {
	name    string
	members []*Eval
}

// units regroupe les évaluations par unité de mise à jour (groupe, ou service seul), dans l'ordre du fichier.
func units(evals []*Eval) []unitGroup {
	var out []unitGroup
	index := map[string]int{}
	for _, e := range evals {
		name := e.Svc.Unit()
		i, ok := index[name]
		if !ok {
			i = len(out)
			index[name] = i
			out = append(out, unitGroup{name: name})
		}
		out[i].members = append(out[i].members, e)
	}
	return out
}

// select désigne des services par leur nom ou celui de leur groupe, dans l'ordre du fichier ; aucun nom : tous.
func (u *Updater) selectServices(names []string) ([]*config.Service, error) {
	if len(names) == 0 {
		return append([]*config.Service{}, u.cfg.Services...), nil
	}
	chosen := map[*config.Service]bool{}
	for _, n := range names {
		found := false
		for _, s := range u.cfg.Services {
			if s.Name == n || s.Group == n {
				chosen[s] = true
				found = true
			}
		}
		if !found {
			all := make([]string, len(u.cfg.Services))
			for i, s := range u.cfg.Services {
				all[i] = s.Name
			}
			return nil, errf("inconnu : %s (services : %s)", n, strings.Join(all, ", "))
		}
	}
	var out []*config.Service
	for _, s := range u.cfg.Services {
		if chosen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// statusRow : (service, état, détail) pour le tableau d'état.
func (u *Updater) statusRow(e *Eval) (string, string, string) {
	n := e.Svc.Name
	switch e.Status {
	case "uptodate":
		return n, "à jour", ""
	case "skipped":
		return n, "ignoré", e.Detail
	case "error":
		return n, "ERREUR", e.Detail
	}
	age := u.now() - e.FirstSeen
	if e.FailedBefore {
		return n, "à revoir", fmt.Sprintf("image %s déjà tentée sans succès", short(e.Remote))
	}
	if e.Eligible {
		return n, "prête", fmt.Sprintf("image %s, mode %s, vue depuis %s", short(e.Remote), e.Svc.Mode, textutil.FmtDuration(age))
	}
	left := e.Svc.Cooldown - age
	if e.Fast {
		left = u.readyAt(e) - u.now()
	}
	suffix := ""
	if e.Fast {
		suffix = " (sécurité)"
	}
	return n, "différée", fmt.Sprintf("image %s, encore %s%s", short(e.Remote), textutil.FmtDuration(max(0, left)), suffix)
}

func isActive(e *Eval) bool { return e.Status == "pending" || e.Status == "uptodate" }

// Cycle fait un passage : lecture seule (apply faux) ou application (apply vrai). Renvoie le code de sortie.
func (u *Updater) Cycle(apply bool, names []string, notifyReport bool) (int, error) {
	services, err := u.selectServices(names)
	if err != nil {
		return 0, err
	}
	partial := len(services) != len(u.cfg.Services)
	if len(names) > 0 {
		list := make([]string, len(services))
		for i, s := range services {
			list[i] = s.Name
		}
		u.log("scan limité à : " + strings.Join(list, ", "))
	}
	evals := make([]*Eval, len(services))
	for i, s := range services {
		evals[i] = u.evaluate(s)
	}
	if !partial {
		u.cleanupState(evals) // une sélection ne purge rien : un même digest peut servir un service non évalué
	}
	u.alertsOn = apply
	if !apply {
		// diagnostic de calibrage : la veille de sécurité a besoin de la version lue dans l'image en cours
		for _, e := range evals {
			if len(e.Svc.Advisories) > 0 && isActive(e) && e.OldImageID != "" {
				if v := u.versionOf(e.OldImageID); v != "" {
					u.log(fmt.Sprintf("  veille de sécurité %s: version détectée %s", e.Svc.Name, v))
				} else {
					u.log(fmt.Sprintf("  veille de sécurité %s: version NON détectée (étiquette %s absente) — aucun avis ne pourra être évalué",
						e.Svc.Name, u.cfg.Settings.VersionLabel))
				}
			}
		}
		for _, e := range evals {
			if e.Svc.ReleaseNotes && isActive(e) && e.OldImageID != "" {
				if v := u.releaseVersionOf(e.OldImageID); v != "" {
					u.log(fmt.Sprintf("  notes de release %s: version détectée %s", e.Svc.Name, v))
				} else {
					u.log(fmt.Sprintf("  notes de release %s: version NON détectée (étiquette %s absente ou illisible) — aucune note ne pourra être comparée",
						e.Svc.Name, u.cfg.Settings.VersionLabel))
				}
			}
		}
	}
	notes := u.releaseScan(evals) // avant les avis : ils citent la release qui parle du même avis
	u.securityPass(evals, apply)
	u.releaseAlerts(notes)
	for _, e := range evals {
		u.log(u.describe(e))
	}
	failures := 0
	for _, e := range evals {
		if e.Status == "error" {
			failures++
		}
	}
	if !apply {
		for _, e := range evals {
			if e.Svc.SmokeURL != "" && e.Status != "skipped" {
				code := u.http(e.Svc.SmokeURL)
				verdict := "OK"
				if !contains(e.Svc.SmokeExpect, code) {
					verdict = fmt.Sprintf("INATTENDU (attendu %s)", pyIntList(e.Svc.SmokeExpect))
				}
				u.log(fmt.Sprintf("  test de fumée %s: HTTP %d %s", e.Svc.Name, code, verdict))
			}
		}
	}
	if apply {
		minutes := u.minutes()
		for _, g := range units(evals) {
			var todo []*Eval
			for _, e := range g.members {
				if e.Status == "pending" && e.Eligible && !e.FailedBefore && e.Svc.Mode == "auto" {
					todo = append(todo, e)
				}
			}
			if len(todo) == 0 {
				continue
			}
			urgent := false
			for _, e := range todo {
				urgent = urgent || e.Urgent
			}
			inWin := textutil.InWindow(todo[0].Svc.Window, minutes)
			if !inWin && !urgent {
				u.log(g.name + ": prêt mais hors de la fenêtre horaire, repris au prochain passage")
				continue
			}
			if urgent && !inWin {
				u.log(g.name + ": avis CRITIQUE, la fenêtre horaire est ignorée")
			}
			if !u.apply(g.name, todo) {
				failures++
			}
		}
		for _, e := range evals {
			if e.Status == "pending" && e.Eligible && e.Svc.Mode == "notify" {
				rec := u.state.Get(e.Ref)
				if rec != nil && !rec.Notified {
					u.report.AddReady(e.Svc.Name, short(e.Remote), textutil.FmtDuration(u.now()-e.FirstSeen))
					rec.Notified = true
					u.state.Put(e.Ref, rec)
				}
			}
		}
		errs := map[string]string{}
		for _, e := range evals {
			if e.Status == "error" {
				errs[e.Svc.Name] = e.Detail
			}
		}
		// l'ordre d'ajout suit celui des services (comme le dictionnaire Python)
		for _, e := range evals {
			if old, seen := u.state.Errors[e.Svc.Name]; e.Status == "error" && (!seen || old != e.Detail) {
				u.report.Add("erreur", e.Svc.Name, e.Detail, "warn")
			}
		}
		if partial {
			scanned := map[string]bool{}
			for _, s := range services {
				scanned[s.Name] = true
			}
			merged := map[string]string{}
			for k, v := range u.state.Errors {
				if !scanned[k] {
					merged[k] = v
				}
			}
			for k, v := range errs {
				merged[k] = v
			}
			u.state.Errors = merged
		} else {
			u.state.Errors = errs
		}
		horizon := u.now() - 120*86400
		for k, v := range u.state.Alerts {
			if v < horizon {
				delete(u.state.Alerts, k)
			}
		}
	}
	if notifyReport {
		if !u.run.DryRun() && u.notifier.URL() == "" {
			u.log("rapport non envoyé : aucune URL de notification lisible (docker-update set-webhook)")
			failures++
		}
		for _, e := range evals {
			a, b, c := u.statusRow(e)
			u.report.AddRow("etat", a, b, c)
		}
	}
	u.flush()
	if err := u.saveState(); err != nil {
		return 0, err
	}
	if failures > 0 {
		return 1, nil
	}
	return 0, nil
}

// pyIntList écrit une liste d'entiers comme repr() d'une liste Python : [302, 401].
func pyIntList(l []int) string {
	parts := make([]string, len(l))
	for i, n := range l {
		parts[i] = itoa(n)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// NowCmd met à jour tout de suite les services désignés (le délai est ignoré), après confirmation (yes).
func (u *Updater) NowCmd(names []string, yes bool) (int, error) {
	selected, err := u.selectServices(names)
	if err != nil {
		return 0, err
	}
	var held []string
	for _, s := range selected {
		if u.held(s) {
			held = append(held, s.Name)
		}
	}
	if len(held) > 0 {
		return 0, errf("en pause : %s (utilisez d'abord `unhold`)", strings.Join(held, ", "))
	}
	evals := make([]*Eval, len(selected))
	for i, s := range selected {
		evals[i] = u.evaluate(s)
	}
	for _, e := range evals {
		u.log(u.describe(e))
	}
	var todoAll []*Eval
	for _, e := range evals {
		if e.Status == "pending" {
			todoAll = append(todoAll, e)
		}
	}
	if len(todoAll) == 0 {
		u.log("rien à mettre à jour")
		return 0, u.saveState()
	}
	if !yes {
		u.log("simulation : ajoutez --yes pour appliquer (le délai est ignoré pour ces services)")
		return 0, nil
	}
	failures := 0
	for _, g := range units(todoAll) {
		if !u.apply(g.name, g.members) {
			failures++
		}
	}
	u.flush()
	if err := u.saveState(); err != nil {
		return 0, err
	}
	if failures > 0 {
		return 1, nil
	}
	return 0, nil
}

// Hold met en pause (on vrai) ou reprend (on faux) des services ou des groupes.
func (u *Updater) Hold(names []string, on bool) (int, error) {
	known := map[string]bool{}
	for _, s := range u.cfg.Services {
		known[s.Name] = true
		if s.Group != "" {
			known[s.Group] = true
		}
	}
	for _, n := range names {
		if !known[n] {
			return 0, errf("inconnu : %s", n)
		}
	}
	for _, n := range names {
		i := indexOf(u.state.Holds, n)
		if on && i < 0 {
			u.state.Holds = append(u.state.Holds, n)
		}
		if !on && i >= 0 {
			u.state.Holds = append(u.state.Holds[:i:i], u.state.Holds[i+1:]...)
		}
	}
	if err := u.saveState(); err != nil {
		return 0, err
	}
	list := strings.Join(u.state.Holds, ", ")
	if list == "" {
		list = "(rien)"
	}
	u.log("en pause : " + list)
	return 0, nil
}

func indexOf(l []string, v string) int {
	for i, x := range l {
		if x == v {
			return i
		}
	}
	return -1
}

// Status affiche les pauses et les images en attente.
func (u *Updater) Status() {
	if len(u.state.Holds) > 0 {
		u.log("en pause : " + strings.Join(u.state.Holds, ", "))
	}
	if len(u.state.Images) == 0 {
		u.log("aucune image en attente")
	}
	refs := make([]string, 0, len(u.state.Images))
	for ref := range u.state.Images {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		rec := u.state.Images[ref]
		flags := ""
		if rec.Failed {
			flags = " [échec]"
		}
		u.log(fmt.Sprintf("%s → %s vue depuis %s%s", ref, short(rec.Digest), textutil.FmtDuration(u.now()-rec.FirstSeen), flags))
	}
}
