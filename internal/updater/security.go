package updater

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/notify"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// tri : verdict à trois valeurs (vrai, faux, impossible à dire).
type tri int

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

// affects dit si l'avis touche la version ; triUnknown si on ne peut pas conclure (plage illisible).
func affects(a textutil.Advisory, version string) tri {
	if len(a.Ranges) == 0 {
		return triUnknown
	}
	verdict := triFalse
	for _, rng := range a.Ranges {
		ok, err := textutil.InRange(version, rng)
		if err != nil {
			verdict = triUnknown
			continue
		}
		if ok {
			return triTrue
		}
	}
	return verdict
}

// flush envoie UN message pour tout ce que le passage a à dire (rien s'il n'y a rien).
func (u *Updater) flush() {
	rep := u.report
	u.report = notify.NewReport()
	if rep.Empty() {
		return
	}
	u.notifier.Send(rep.Title(), rep.Render(u.cfg.Settings.NotifyFormat == "discord", notify.ReportBudget), rep.Level())
}

// alert journalise, puis signale une seule fois par clé (plusieurs clés : une ligne pour le lot) dans le rapport.
func (u *Updater) alert(keys []string, title, text, level, label, kind string) {
	u.log(fmt.Sprintf("  %s : %s — %s", label, title, text))
	if !u.alertsOn {
		return
	}
	known := true
	for _, k := range keys {
		if _, ok := u.state.Alerts[k]; !ok {
			known = false
		}
	}
	if known {
		return
	}
	now := u.now()
	for _, k := range keys {
		u.state.Alerts[k] = now
	}
	u.report.Add(kind, title, text, level)
}

func (u *Updater) secAlert(key, title, text, level string) {
	u.alert([]string{key}, title, text, level, "ALERTE sécurité", "secu")
}

// recentAdvisories : avis récents, non retirés, de sévérité suffisante, pour les dépôts `advisories` du service.
func (u *Updater) recentAdvisories(svc *config.Service) []textutil.Advisory {
	now := u.now()
	var items []textutil.Advisory
	for _, repo := range svc.Advisories {
		items = append(items, u.advisories(repo)...)
	}
	var out []textutil.Advisory
	for _, a := range items {
		if !a.Withdrawn && now-a.Pub <= u.cfg.Settings.AdvisoryLookback && textutil.SevRank(a.Sev) >= textutil.SevRank(svc.MinSeverity) {
			out = append(out, a)
		}
	}
	return out
}

// notesLine complète une alerte d'avis avec la release dont la note cite le même avis (GHSA ou CVE), s'il y en a une.
func (u *Updater) notesLine(svc *config.Service, advisories []textutil.Advisory) string {
	index := u.notes[svc.Name]
	var found [][2]string
	for _, a := range advisories {
		for _, key := range []string{a.ID, a.CVE} {
			if key == "" {
				continue
			}
			if hit, ok := index[strings.ToLower(key)]; ok {
				found = append(found, hit)
				break
			}
		}
	}
	if len(found) == 0 {
		return ""
	}
	var uniq [][2]string
	for _, f := range found {
		if !slices.Contains(uniq, f) {
			uniq = append(uniq, f)
		}
	}
	if len(uniq) > 3 {
		uniq = uniq[:3]
	}
	parts := make([]string, len(uniq))
	for i, f := range uniq {
		parts[i] = f[0] + " " + f[1]
	}
	return "\nNotes de release qui le citent : " + strings.Join(parts, ", ") + "."
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (u *Updater) securityPass(evals []*Eval, allowPull bool) {
	now := u.now()
	for _, e := range evals {
		svc := e.Svc
		if len(svc.Advisories) == 0 || (e.Status != "pending" && e.Status != "uptodate") || e.OldImageID == "" {
			continue
		}
		recent := u.recentAdvisories(svc)
		if len(recent) == 0 {
			continue
		}
		current := u.versionOf(e.OldImageID)
		if current == "" {
			a := recent[0]
			u.secAlert("inconnue:"+svc.Name+":"+a.ID, "Avis de sécurité récent : "+svc.Name,
				fmt.Sprintf("%s (%s) %s. Impossible de savoir si la version en cours est touchée (étiquette de version absente de l'image). Vérifiez à la main.",
					a.ID, orDefault(a.Sev, "sévérité ?"), a.Summary)+u.notesLine(svc, []textutil.Advisory{a}), "warn")
			continue
		}
		var affecting, unsure []textutil.Advisory
		for _, a := range recent {
			switch affects(a, current) {
			case triTrue:
				affecting = append(affecting, a)
			case triUnknown:
				unsure = append(unsure, a)
			}
		}
		for _, a := range unsure {
			u.secAlert("plage:"+svc.Name+":"+a.ID, "Avis de sécurité à vérifier : "+svc.Name,
				fmt.Sprintf("%s %s — plage de versions illisible, à comparer à la version %s.", a.ID, a.Summary, current)+
					u.notesLine(svc, []textutil.Advisory{a}), "warn")
		}
		if len(affecting) == 0 {
			continue
		}
		parts := make([]string, len(affecting))
		for i, a := range affecting {
			cve := ""
			if a.CVE != "" {
				cve = " / " + a.CVE
			}
			parts[i] = fmt.Sprintf("%s%s (%s)", a.ID, cve, orDefault(a.Sev, "?"))
		}
		names := strings.Join(parts, ", ")
		if e.Status == "uptodate" {
			u.secAlert(fmt.Sprintf("sans-correctif:%s:%s:%s", svc.Name, current, affecting[0].ID), "VULNÉRABLE : "+svc.Name+" "+current,
				names+". L'étiquette suivie ne propose pas encore d'image corrigée. Surveillez la publication d'une version corrigée."+
					u.notesLine(svc, affecting), "error")
			continue
		}
		if !allowPull {
			u.log(fmt.Sprintf("  %s: la version en cours (%s) est touchée par %s ; l'image candidate sera évaluée au prochain passage `run`", svc.Name, current, names))
			continue
		}
		candidate := u.candidateVersion(e)
		if candidate == "" {
			u.secAlert(fmt.Sprintf("candidate-inconnue:%s:%s:%s", svc.Name, e.Remote, affecting[0].ID),
				fmt.Sprintf("Avis de sécurité : %s %s touché", svc.Name, current),
				fmt.Sprintf("%s. Une nouvelle image existe mais sa version n'est pas lisible : je NE l'accélère PAS. À vous de juger : `docker-update now %s --yes`.", names, svc.Name)+
					u.notesLine(svc, affecting), "error")
			continue
		}
		if candidate == "(simulation)" {
			u.log(fmt.Sprintf("  %s: simulation — l'image candidate serait téléchargée pour lire sa version", svc.Name))
			continue
		}
		var fixed []textutil.Advisory
		for _, a := range affecting {
			if affects(a, candidate) == triFalse {
				fixed = append(fixed, a)
			}
		}
		if len(fixed) == 0 {
			u.secAlert(fmt.Sprintf("candidate-touchee:%s:%s:%s", svc.Name, e.Remote, affecting[0].ID),
				fmt.Sprintf("Avis de sécurité : pas de correctif dans la nouvelle image de %s", svc.Name),
				fmt.Sprintf("%s. L'image candidate (%s) est elle aussi concernée ou non confirmée : délai normal conservé.", names, candidate)+
					u.notesLine(svc, affecting), "warn")
			continue
		}
		e.Fast, e.SecVersion = true, candidate
		e.SecIDs = nil
		var adv int64
		fixedParts := make([]string, len(fixed))
		for i, a := range fixed {
			e.SecIDs = append(e.SecIDs, a.ID)
			if i == 0 || a.Pub > adv {
				adv = a.Pub
			}
			if a.Sev == "critical" {
				e.Urgent = true
			}
			fixedParts[i] = fmt.Sprintf("%s (%s)", a.ID, orDefault(a.Sev, "?"))
		}
		e.AdvPub = adv
		fixedNames := strings.Join(fixedParts, ", ")
		if svc.SecurityPolicy == "fast" && svc.Mode == "auto" {
			if !e.FailedBefore && now >= u.readyAt(e) {
				e.Eligible = true
			}
			u.secAlert(fmt.Sprintf("correctif:%s:%s", svc.Name, e.Remote), "Correctif de sécurité détecté : "+svc.Name,
				fmt.Sprintf("%s corrigé dans %s (en cours : %s). Mise à jour accélérée : délai ramené à %s.", fixedNames, candidate, current,
					textutil.FmtDuration(svc.SecurityCooldown))+u.notesLine(svc, fixed), "warn")
		} else {
			why := "politique « alert »"
			if svc.Mode != "auto" {
				why = "mode non automatique"
			}
			u.secAlert(fmt.Sprintf("correctif:%s:%s", svc.Name, e.Remote), "Correctif de sécurité disponible : "+svc.Name,
				fmt.Sprintf("%s corrigé dans %s (en cours : %s). Pas de mise à jour automatique (%s) : `docker-update now %s --yes` pour l'appliquer.",
					fixedNames, candidate, current, why, svc.Name)+u.notesLine(svc, fixed), "warn")
		}
	}
}

// candidateVersion : version de l'image candidate ; on la télécharge (sans l'exécuter) pour lire ses étiquettes.
func (u *Updater) candidateVersion(e *Eval) string {
	repo, _, _ := textutil.SplitRef(e.Ref)
	target := repo + "@" + e.Remote
	r := u.mutate(1800*time.Second, "docker", "pull", target)
	if u.run.DryRun() {
		return "(simulation)"
	}
	if r.RC != 0 {
		return ""
	}
	return u.versionOf(target)
}

// ── veille des notes de release (ALERTES SEULEMENT) ─────────────────────────
// Une note de release est un texte non fiable : elle ne touche jamais au délai, à l'éligibilité ni à la fenêtre.

// external : texte venu d'une note, nettoyé, borné, et sans mention possible (« @ » neutralisé).
func external(value string, limit int) string {
	return strings.ReplaceAll(textutil.CleanText(value, limit), "@", "@"+string(rune(0x200b)))
}

type releaseItem struct {
	tag, url     string
	strong, weak []textutil.Hit
}

type scanEntry struct {
	svc   *config.Service
	shown string
	items []releaseItem
}

// releaseScan lit les releases plus récentes que la version en cours. N'envoie que l'alerte « version illisible » ;
// indexe les avis (GHSA / CVE) cités par les notes, pour compléter les alertes de sécurité.
func (u *Updater) releaseScan(evals []*Eval) []scanEntry {
	u.notes = map[string]map[string][2]string{}
	var scan []scanEntry
	for _, e := range evals {
		svc := e.Svc
		if !svc.ReleaseNotes || (e.Status != "pending" && e.Status != "uptodate") || e.OldImageID == "" {
			continue
		}
		current := u.releaseVersionOf(e.OldImageID)
		cur, ok := textutil.ParseReleaseVersion(current)
		if !ok {
			u.alert([]string{"release-version:" + svc.Name}, "Notes de release non analysées : "+svc.Name,
				fmt.Sprintf("Impossible de comparer : la version en cours est absente ou illisible (étiquette %s). Aucune note de release n'est lue pour ce service.",
					u.cfg.Settings.VersionLabel), "info", releaseLabel, "info")
			continue
		}
		type newer struct {
			v    textutil.Version
			repo string
			r    textutil.Release
		}
		var found []newer
		seen := map[string]bool{}
		repos := svc.ReleaseNotesRepos
		if len(repos) == 0 {
			repos = svc.Advisories
		}
		for _, repo := range repos {
			for _, r := range u.releases(repo) {
				v, ok := textutil.ParseReleaseVersion(r.Tag)
				if r.Draft || r.Pre || seen[r.Tag] || !ok || v[3] == 0 || v.Compare(cur) <= 0 {
					continue
				}
				seen[r.Tag] = true
				found = append(found, newer{v, repo, r})
			}
		}
		sort.SliceStable(found, func(i, j int) bool { return found[i].v.Compare(found[j].v) < 0 })
		var items []releaseItem
		for _, n := range found {
			var hits []textutil.Hit
			for _, h := range n.r.Hits {
				ignored := false
				for _, p := range svc.IgnoreRE {
					if p.MatchString(h.X) || p.MatchString(h.T) {
						ignored = true
						break
					}
				}
				if !ignored {
					hits = append(hits, h)
				}
			}
			url := fmt.Sprintf("https://github.com/%s/releases/tag/%s", n.repo, n.r.Tag)
			it := releaseItem{tag: n.r.Tag, url: url}
			for _, h := range hits {
				if h.W {
					it.weak = append(it.weak, h)
				} else {
					it.strong = append(it.strong, h)
				}
			}
			items = append(items, it)
			for _, h := range it.strong {
				if h.K == "CVE" || h.K == "GHSA" {
					if u.notes[svc.Name] == nil {
						u.notes[svc.Name] = map[string][2]string{}
					}
					key := strings.ToLower(h.T)
					if _, dup := u.notes[svc.Name][key]; !dup {
						u.notes[svc.Name][key] = [2]string{n.r.Tag, url}
					}
				}
			}
		}
		scan = append(scan, scanEntry{svc, strings.TrimLeft(current, "v"), items})
	}
	return scan
}

// releaseAlerts : alertes de notes de release. Une note qui ne cite QUE des avis déjà connus du service (GHSA / CVE
// d'un avis récent) n'a pas d'alerte à elle : l'alerte de l'avis la complète avec le lien de la release.
func (u *Updater) releaseAlerts(scan []scanEntry) {
	alerts := u.state.Alerts
	for _, s := range scan {
		svc := s.svc
		covered := map[string]textutil.Advisory{}
		for _, a := range u.recentAdvisories(svc) {
			covered[strings.ToLower(a.ID)] = a
			if a.CVE != "" {
				covered[strings.ToLower(a.CVE)] = a
			}
		}
		type weakRef struct{ tag, url string }
		var weak []weakRef
		for _, it := range s.items {
			if len(it.strong) > 0 {
				var known, rest []textutil.Hit
				for _, h := range it.strong {
					if _, ok := covered[strings.ToLower(h.T)]; ok && (h.K == "CVE" || h.K == "GHSA") {
						known = append(known, h)
					}
				}
				for _, h := range it.strong {
					if !slices.Contains(known, h) {
						rest = append(rest, h)
					}
				}
				var ids []string
				for _, h := range known {
					id := covered[strings.ToLower(h.T)].ID
					if !slices.Contains(ids, id) {
						ids = append(ids, id)
					}
				}
				idsText := strings.Join(ids, ", ")
				if len(rest) == 0 {
					u.log(fmt.Sprintf("  notes de release %s: %s ne cite que l'avis %s, déjà suivi", svc.Name, it.tag, idsText))
					continue
				}
				var motifs []string
				for i, h := range rest {
					if i == 12 {
						break
					}
					m := external(h.T, 60)
					if !slices.Contains(motifs, m) {
						motifs = append(motifs, m)
					}
				}
				text := fmt.Sprintf("Motifs : %s\nExtrait : « %s »\nLien : %s\n", strings.Join(motifs, ", "), external(rest[0].X, 160), it.url)
				if len(known) > 0 {
					text += fmt.Sprintf("Déjà couvert par l'avis : %s.\n", idsText)
				}
				u.alert([]string{fmt.Sprintf("release:%s:%s", svc.Name, it.tag)},
					fmt.Sprintf("Notes de release à lire : %s %s → %s", svc.Name, s.shown, it.tag), text, "warn", releaseLabel, "notes")
			} else if len(it.weak) > 0 {
				if _, seen := alerts[fmt.Sprintf("release-info:%s:%s", svc.Name, it.tag)]; !u.alertsOn || !seen {
					weak = append(weak, weakRef{it.tag, it.url})
				}
			}
		}
		if len(weak) > 0 {
			var lines []string
			for i, w := range weak {
				if i == 10 {
					break
				}
				lines = append(lines, fmt.Sprintf("- %s : %s", w.tag, w.url))
			}
			if len(weak) > 10 {
				lines = append(lines, fmt.Sprintf("… et %d autre(s)", len(weak)-10))
			}
			keys := make([]string, len(weak))
			for i, w := range weak {
				keys[i] = fmt.Sprintf("release-info:%s:%s", svc.Name, w.tag)
			}
			u.alert(keys, fmt.Sprintf("Notes de release : %s %s, mot « security » dans %d release(s)", svc.Name, s.shown, len(weak)),
				"Signal faible (le mot seul, sans CVE ni correctif annoncé) :\n"+strings.Join(lines, "\n"), "info", releaseLabel, "notes")
		}
	}
}
