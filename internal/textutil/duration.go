package textutil

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var units = map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800}

var (
	durationRE = regexp.MustCompile(`^([0-9]+)([smhdw])$`)
	windowRE   = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})-([0-9]{1,2}):([0-9]{2})$`)
)

// ParseDuration lit une durée « 30m », « 6h », « 3d », « 1w » ou « 0 », en secondes.
func ParseDuration(value string) (int64, error) {
	s := strings.TrimSpace(value)
	if s == "0" {
		return 0, nil
	}
	bad := fmt.Errorf("durée invalide : %s (exemples : 30m, 6h, 3d, 1w, 0)", Repr(value))
	m := durationRE.FindStringSubmatch(s)
	if m == nil {
		return 0, bad
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	unit := units[m[2][0]]
	if err != nil || n > math.MaxInt64/unit {
		return 0, bad
	}
	return n * unit, nil
}

// FmtDuration écrit une durée pour un humain : « 3 j », « 6 h », « 15 min », « 20 s ».
func FmtDuration(sec int64) string {
	for _, u := range []struct {
		name string
		n    int64
	}{{"j", 86400}, {"h", 3600}, {"min", 60}} {
		if sec >= u.n {
			return fmt.Sprintf("%d %s", sec/u.n, u.name)
		}
	}
	return fmt.Sprintf("%d s", sec)
}

// Window est une fenêtre horaire locale, en minutes depuis minuit. La valeur nulle (Set faux) signifie
// « toujours ». Si Start > End, la fenêtre traverse minuit.
type Window struct {
	Start, End int
	Set        bool
}

// ParseWindow lit « 02:00-05:00 » ; une chaîne vide donne la fenêtre « toujours ».
func ParseWindow(spec string) (Window, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return Window{}, nil
	}
	m := windowRE.FindStringSubmatch(s)
	if m == nil {
		return Window{}, fmt.Errorf("fenêtre invalide : %s (exemple : 02:00-05:00)", Repr(spec))
	}
	var v [4]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	h1, m1, h2, m2 := v[0], v[1], v[2], v[3]
	if h1 > 23 || h2 > 23 || m1 > 59 || m2 > 59 {
		return Window{}, fmt.Errorf("fenêtre invalide : %s", Repr(spec))
	}
	a, b := h1*60+m1, h2*60+m2
	if a == b {
		return Window{}, fmt.Errorf("fenêtre vide : %s", Repr(spec))
	}
	return Window{Start: a, End: b, Set: true}, nil
}

// InWindow dit si l'heure locale (en minutes depuis minuit) est dans la fenêtre.
func InWindow(w Window, minutes int) bool {
	if !w.Set {
		return true
	}
	if w.Start < w.End {
		return w.Start <= minutes && minutes < w.End
	}
	return minutes >= w.Start || minutes < w.End
}
