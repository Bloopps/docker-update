package textutil

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Severities : sévérités des avis GitHub, de la plus faible à la plus forte.
var Severities = []string{"low", "medium", "high", "critical"}

// SevRank donne le rang d'une sévérité ; une sévérité inconnue est traitée comme « medium ».
func SevRank(s string) int {
	s = strings.ToLower(s)
	for i, v := range Severities {
		if v == s {
			return i
		}
	}
	return 1
}

// Version : (majeure, mineure, correctif, stable). Stable vaut 0 pour une pré-version (donc plus petite).
type Version [4]int

// Compare renvoie -1, 0 ou 1.
func (v Version) Compare(o Version) int {
	for i := range v {
		switch {
		case v[i] < o[i]:
			return -1
		case v[i] > o[i]:
			return 1
		}
	}
	return 0
}

var (
	verRE   = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:([-+])([0-9A-Za-z.-]+))?$`)
	boundRE = regexp.MustCompile(`^(>=|<=|>|<|=)?[ \t\n\r\f\v]*v?([0-9]+)\.([0-9]+)(?:\.([0-9]+))?(?:([-+])([0-9A-Za-z.-]+))?$`)
	rverRE  = regexp.MustCompile(`^v?([0-9]{1,6})\.([0-9]{1,6})(?:\.([0-9]{1,6}))?(?:([-+])([0-9A-Za-z.-]{1,64}))?$`)
)

func atoi(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func stable(sign string) int {
	if sign == "-" {
		return 0
	}
	return 1
}

// ParseVersion lit « v3.6.3 » ; le dernier élément vaut 0 pour une pré-version. Une valeur illisible (ou hors
// de la plage des entiers) donne false : on ne devine jamais.
func ParseVersion(value string) (Version, bool) {
	m := verRE.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return Version{}, false
	}
	a, ok1 := atoi(m[1])
	b, ok2 := atoi(m[2])
	c, ok3 := atoi(m[3])
	if !ok1 || !ok2 || !ok3 {
		return Version{}, false
	}
	return Version{a, b, c, stable(m[4])}, true
}

// InRange dit si la version est dans une plage d'avis GitHub (« >= 3.0.0, < 3.6.3 », contraintes liées par ET).
// Une version ou une plage illisible donne une erreur : on ne devine jamais.
func InRange(version, rng string) (bool, error) {
	v, ok := ParseVersion(version)
	if !ok {
		return false, fmt.Errorf("version illisible : %s", Repr(version))
	}
	var parts []string
	for _, p := range strings.Split(rng, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return false, fmt.Errorf("plage vide")
	}
	// Comme en Python, les contraintes sont lues et évaluées l'une après l'autre : la première qui échoue conclut.
	for _, p := range parts {
		m := boundRE.FindStringSubmatch(p)
		if m == nil {
			return false, fmt.Errorf("contrainte illisible : %s", Repr(p))
		}
		a, ok1 := atoi(m[2])
		b, ok2 := atoi(m[3])
		c, ok3 := atoi(m[4])
		if !ok1 || !ok2 || !ok3 {
			return false, fmt.Errorf("contrainte illisible : %s", Repr(p))
		}
		cmp := v.Compare(Version{a, b, c, stable(m[5])})
		var ok bool
		switch m[1] {
		case ">=":
			ok = cmp >= 0
		case ">":
			ok = cmp > 0
		case "<=":
			ok = cmp <= 0
		case "<":
			ok = cmp < 0
		default:
			ok = cmp == 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// ParseReleaseVersion lit la version d'une étiquette de release ou d'une image : « v2.45.1 », « 24.04 »,
// « 2026.7.8 ». Plus souple que ParseVersion (X.Y accepté, correctif = 0), mais toujours stricte :
// « latest », « main » ou « 18-vectorchord0.5.3 » sont illisibles.
func ParseReleaseVersion(value string) (Version, bool) {
	m := rverRE.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return Version{}, false
	}
	a, _ := atoi(m[1])
	b, _ := atoi(m[2])
	c, _ := atoi(m[3])
	return Version{a, b, c, stable(m[4])}, true
}
