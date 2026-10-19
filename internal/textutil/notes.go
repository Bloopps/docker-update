package textutil

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxNotes : on n'analyse que les 65 536 premiers caractères d'une note de release.
	MaxNotes = 64 << 10
	// MaxStrongHits et MaxWeakHits bornent le nombre de résultats par note.
	MaxStrongHits = 30
	MaxWeakHits   = 3
)

// Hit est un motif de sécurité trouvé dans une note de release. Les champs sont déclarés dans l'ordre
// alphabétique de leurs clés JSON : l'état est écrit comme celui du Python (clés triées).
type Hit struct {
	K string `json:"k"` // famille du motif
	T string `json:"t"` // texte trouvé, nettoyé et borné (60)
	W bool   `json:"w"` // vrai pour un signal faible
	X string `json:"x"` // extrait nettoyé et borné (160)
}

// Un motif « fort » : mots entiers (les frontières \b de Python sont reproduites à la main, avec la notion
// Unicode de « caractère de mot », car \b de RE2 est ASCII), quantificateurs bornés, pas de retour arrière.
type pattern struct {
	kind        string
	re          *regexp.Regexp
	lead, trail bool // frontière de mot exigée avant / après
}

const word = `[\p{L}\p{N}_]`

func pat(kind, expr string, lead, trail bool) pattern {
	return pattern{kind, regexp.MustCompile(`(?i)` + expr), lead, trail}
}

var releasePatterns = []pattern{
	pat("CVE", `CVE-\p{Nd}{4}-\p{Nd}{4,}`, true, true),
	pat("GHSA", `GHSA(?:-[23456789cfghjmpqrvwx]{4}){3}`, true, true),
	pat("correctif de sécurité", `security[ \t]+(?:fix(?:es)?|updates?|releases?|patch(?:es)?|advisor(?:y|ies))`, true, true),
	pat("vulnérabilité", `vulnerab`+word+`{0,20}`, true, false),
	pat("RCE/XSS/SSRF/CSRF", `(?:RCE|XSS|SSRF|CSRF)`, true, true),
	pat("injection SQL", `SQL[ \t]+injections?`, true, true),
	pat("élévation de privilèges", `privilege[ \t]+escalations?`, true, true),
	pat("contournement d'authentification", `auth`+word+`{0,15}[ \t]+bypass(?:es)?`, true, true),
	pat("path traversal", `path[ \t]+traversals?`, true, true),
}

// Le mot « security » seul est trop bruyant : signal faible (alerte d'information, regroupée).
var releaseWeak = pat("security", `security`, true, true)

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

// boundaryOK vérifie les frontières de mot d'une correspondance [start, end[ dans body.
func (p pattern) boundaryOK(body string, start, end int) bool {
	if p.lead && start > 0 {
		if r, _ := utf8.DecodeLastRuneInString(body[:start]); isWord(r) {
			return false
		}
	}
	if p.trail && end < len(body) {
		if r, _ := utf8.DecodeRuneInString(body[end:]); isWord(r) {
			return false
		}
	}
	return true
}

// matches renvoie les correspondances successives, sans chevauchement (comme finditer), frontières comprises.
// Après un rejet, la recherche reprend un caractère après le début de la correspondance rejetée.
func (p pattern) matches(body string, yield func(start, end int) bool) {
	pos := 0
	for pos <= len(body) {
		loc := p.re.FindStringIndex(body[pos:])
		if loc == nil {
			return
		}
		start, end := pos+loc[0], pos+loc[1]
		if !p.boundaryOK(body, start, end) {
			_, size := utf8.DecodeRuneInString(body[start:])
			pos = start + max(size, 1)
			continue
		}
		if !yield(start, end) {
			return
		}
		if end == start {
			pos = end + 1
		} else {
			pos = end
		}
	}
}

// headRunes renvoie les n premiers caractères de s.
func headRunes(s string, n int) string {
	i := 0
	for count := 0; i < len(s); count++ {
		if count == n {
			return s[:i]
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s
}

// around renvoie l'extrait de body qui va de 70 caractères avant start à 70 caractères après end.
func around(body string, start, end int) string {
	a := start
	for n := 0; n < 70 && a > 0; n++ {
		_, size := utf8.DecodeLastRuneInString(body[:a])
		a -= size
	}
	b := end
	for n := 0; n < 70 && b < len(body); n++ {
		_, size := utf8.DecodeRuneInString(body[b:])
		b += size
	}
	return body[a:b]
}

// ScanNotes cherche les motifs de sécurité d'une note de release (texte non fiable : borné, jamais interprété).
func ScanNotes(text string) []Hit {
	body := headRunes(text, MaxNotes)
	var hits []Hit
	seen := map[[2]string]bool{}
	for _, p := range releasePatterns {
		if len(hits) >= MaxStrongHits {
			break
		}
		p.matches(body, func(start, end int) bool {
			key := [2]string{p.kind, strings.ToLower(body[start:end])}
			if seen[key] {
				return true
			}
			seen[key] = true
			hits = append(hits, Hit{K: p.kind, T: CleanText(body[start:end], 60), X: CleanText(around(body, start, end), 160)})
			return len(hits) < MaxStrongHits
		})
	}
	weak := 0
	releaseWeak.matches(body, func(start, end int) bool {
		hits = append(hits, Hit{K: "security", T: CleanText(body[start:end], 60), W: true, X: CleanText(around(body, start, end), 160)})
		weak++
		return weak < MaxWeakHits
	})
	return hits
}
