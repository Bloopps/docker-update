package textutil

import (
	"regexp"
	"strings"
	"time"
)

var (
	// GHSARE reconnaît un identifiant d'avis GitHub.
	GHSARE = regexp.MustCompile(`^GHSA(-[23456789cfghjmpqrvwx]{4}){3}$`)
	cveRE  = regexp.MustCompile(`^CVE-[0-9]{4}-[0-9]{4,}$`)
	// TagRE borne les étiquettes de release acceptées.
	TagRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$`)
)

// Advisory est un avis GitHub réduit aux champs utiles, tel qu'il est mémorisé dans l'état. Les champs sont
// dans l'ordre alphabétique de leurs clés JSON.
type Advisory struct {
	CVE       string   `json:"cve"`
	ID        string   `json:"id"`
	Pub       int64    `json:"pub"`
	Ranges    []string `json:"ranges"`
	Sev       string   `json:"sev"`
	Summary   string   `json:"summary"`
	Withdrawn bool     `json:"withdrawn"`
}

// Release est une release GitHub réduite aux champs utiles (la note elle-même n'est pas conservée).
type Release struct {
	Draft bool   `json:"draft"`
	Hits  []Hit  `json:"hits"`
	Pre   bool   `json:"pre"`
	Pub   int64  `json:"pub"`
	Tag   string `json:"tag"`
}

// isoLayouts : formes ISO 8601 acceptées pour les dates GitHub (le « Z » final est lu comme +00:00).
var isoLayouts = []string{
	"2006-01-02T15:04:05Z07:00", "2006-01-02 15:04:05Z07:00", "2006-01-02T15:04:05-0700", "2006-01-02 15:04:05-0700",
	"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02",
}

// ParseISO lit une date ISO 8601 et renvoie son instant (secondes depuis 1970). Une date sans fuseau est lue
// en heure locale, comme `datetime.fromisoformat(...).timestamp()` en Python.
func ParseISO(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range isoLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// CompactAdvisory réduit un avis GitHub (JSON décodé) aux champs utiles ; false si inutilisable.
func CompactAdvisory(a any) (Advisory, bool) {
	m, ok := asMap(a)
	if !ok {
		return Advisory{}, false
	}
	ghsa := PyStr(m["ghsa_id"])
	if !GHSARE.MatchString(ghsa) {
		return Advisory{}, false
	}
	pub, ok := ParseISO(strings.ReplaceAll(pubString(m["published_at"]), "Z", "+00:00"))
	if !ok {
		return Advisory{}, false
	}
	ranges := []string{}
	if vulns, ok := m["vulnerabilities"].([]any); ok {
		for _, v := range vulns {
			if vm, ok := asMap(v); ok {
				if r, ok := vm["vulnerable_version_range"].(string); ok && strings.TrimSpace(r) != "" {
					ranges = append(ranges, strings.TrimSpace(r))
				}
			}
		}
	}
	cve := PyStr(m["cve_id"])
	if !cveRE.MatchString(cve) {
		cve = ""
	}
	return Advisory{
		ID: ghsa, CVE: cve, Sev: strings.ToLower(PyStr(m["severity"])), Pub: pub, Ranges: ranges,
		Summary: CleanValue(m["summary"], 200), Withdrawn: Truthy(m["withdrawn_at"]),
	}, true
}

// pubString reproduit `str(valeur)` d'une date absente (« None » : illisible) ou textuelle.
func pubString(v any) string {
	if v == nil {
		return "None"
	}
	return PyStr(v)
}

// CompactRelease réduit une release GitHub (JSON décodé) aux champs utiles ; false si inutilisable.
func CompactRelease(r any) (Release, bool) {
	m, ok := asMap(r)
	if !ok {
		return Release{}, false
	}
	tag, ok := m["tag_name"].(string)
	if !ok || !TagRE.MatchString(tag) {
		return Release{}, false
	}
	date := m["published_at"]
	if !Truthy(date) {
		date = m["created_at"]
	}
	pub, ok := ParseISO(strings.ReplaceAll(pubString(date), "Z", "+00:00"))
	if !ok {
		pub = 0
	}
	body, _ := m["body"].(string)
	hits := ScanNotes(body)
	if hits == nil {
		hits = []Hit{}
	}
	return Release{Tag: tag, Pub: pub, Draft: Truthy(m["draft"]), Pre: Truthy(m["prerelease"]), Hits: hits}, true
}
