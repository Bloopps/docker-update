package updater

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// pollFeed lit la liste GitHub d'un dépôt, en cache (ETag) : au plus une requête par `advisory_poll`.
//
// empty : codes qui signifient « rien à lire » (liste vide valide). backoff : codes après lesquels on attend aussi
// tout l'intervalle (limite de débit) au lieu de réessayer à chaque passage. meta : champs ajoutés à l'entrée de
// cache quand la lecture réussit.
func pollFeed[T any](u *Updater, cache map[string]*state.Feed[T], repo, path string, compact func(any) (T, bool),
	label string, empty, backoff []int, meta func(*state.Feed[T])) []T {
	st := u.cfg.Settings
	rec := cache[repo]
	if rec == nil {
		rec = &state.Feed[T]{}
	}
	now := u.now()
	if now-rec.Fetched < st.AdvisoryPoll && (rec.Ok || contains(backoff, rec.Err)) {
		return rec.Items
	}
	code, body, etag := u.ghGet(fmt.Sprintf("%s/repos/%s/%s", st.GithubAPI, repo, path), readTokenFile(st.GithubTokenFile), rec.Etag)
	list, isList := body.([]any)
	switch {
	case code == 304 && rec.Ok:
		if meta != nil {
			meta(rec)
		}
		rec.Fetched = now
	case code == 200 && isList:
		items := make([]T, 0, len(list))
		for _, a := range list {
			if c, ok := compact(a); ok {
				items = append(items, c)
			}
		}
		rec = &state.Feed[T]{Ok: true, Fetched: now, Etag: etag, Items: items}
		if meta != nil {
			meta(rec)
		}
	case contains(empty, code):
		rec = &state.Feed[T]{Ok: true, Fetched: now, Items: []T{}}
		if meta != nil {
			meta(rec)
		}
	default:
		// échec (réseau, limite de débit, dépôt sans avis publics) : on garde l'ancien contenu, on réessaie plus tard
		if rec.Err == 0 {
			u.log(fmt.Sprintf("  %s : %s injoignable (HTTP %d) ; réessai plus tard", label, repo, code))
		}
		cp := *rec
		cp.Fetched, cp.Err = now, code
		if !cp.Ok || cp.Items == nil {
			cp.Items = []T{}
		}
		rec = &cp
	}
	cache[repo] = rec
	return rec.Items
}

// advisories : avis publiés par le dépôt.
func (u *Updater) advisories(repo string) []textutil.Advisory {
	return pollFeed(u, u.state.Advisories, repo, "security-advisories?per_page=50", textutil.CompactAdvisory,
		"veille de sécurité", nil, nil, nil)
}

// releases : dernières releases du dépôt (compactées) : 30 à la première lecture (historique), puis 10 seulement, car
// on ne cherche que les nouveautés ; 30 de nouveau si la dernière lecture complète a plus de 7 jours.
// 404 : dépôt sans release lisible, ce n'est pas une erreur.
func (u *Updater) releases(repo string) []textutil.Release {
	rec := u.state.Releases[repo]
	if rec == nil {
		rec = &state.Feed[textutil.Release]{}
	}
	now := u.now()
	full := !rec.Ok || now-rec.Full > releasesResync
	n := releasesNext
	if full {
		n = releasesFirst
	}
	lastFull := rec.Full
	return pollFeed(u, u.state.Releases, repo, fmt.Sprintf("releases?per_page=%d", n), textutil.CompactRelease,
		"notes de release", []int{404}, []int{403, 429}, func(f *state.Feed[textutil.Release]) {
			if full {
				f.Full = now
			} else {
				f.Full = lastFull
			}
		})
}

func (u *Updater) labels(target string) map[string]any {
	r := u.sh("docker", "image", "inspect", "--format", "{{json .Config.Labels}}", target)
	if r.RC != 0 {
		return map[string]any{}
	}
	var labels map[string]any
	if json.Unmarshal([]byte(r.Out), &labels) != nil || labels == nil {
		return map[string]any{}
	}
	return labels
}

// versionOf : version de l'image lue dans ses étiquettes (« v3.6.3 » devient « 3.6.3 »), ou "".
func (u *Updater) versionOf(target string) string {
	labels := u.labels(target)
	for _, key := range []string{u.cfg.Settings.VersionLabel, "org.label-schema.version"} {
		v := textutil.PyStr(labels[key])
		if _, ok := textutil.ParseVersion(v); ok {
			return strings.TrimLeft(v, "v")
		}
	}
	return ""
}

func (u *Updater) releaseVersionOf(target string) string {
	labels := u.labels(target)
	for _, key := range []string{u.cfg.Settings.VersionLabel, "org.label-schema.version"} {
		v := textutil.PyStr(labels[key])
		if _, ok := textutil.ParseReleaseVersion(v); ok {
			return v
		}
	}
	return ""
}
