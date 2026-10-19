package textutil

import (
	"fmt"
	"strings"
	"unicode"
)

// CleanText nettoie un texte venu de l'extérieur : sans caractères de contrôle, sur une seule ligne, borné à
// limit caractères (et non octets).
func CleanText(value string, limit int) string {
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return truncateRunes(strings.Join(strings.Fields(b.String()), " "), limit)
}

// CleanValue est CleanText pour une valeur JSON décodée de type quelconque (comme `clean_text(valeur)` en
// Python : une valeur absente ou « fausse » donne une chaîne vide).
func CleanValue(value any, limit int) string {
	return CleanText(PyStr(value), limit)
}

// PyStr reproduit `str(valeur or "")` pour une valeur issue de json.Unmarshal.
func PyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return ""
	case float64:
		if v == 0 {
			return ""
		}
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprint(v)
	case []any:
		if len(v) == 0 {
			return ""
		}
	case map[string]any:
		if len(v) == 0 {
			return ""
		}
	}
	return fmt.Sprint(value)
}

// Truthy reproduit la valeur de vérité Python d'une valeur issue de json.Unmarshal.
func Truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func truncateRunes(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit { // au plus autant de caractères que d'octets
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i]
		}
		n++
	}
	return s
}

// ShortID : « sha256:0123456789ab… » devient « 0123456789ab » (comme docker) ; autre forme : telle quelle, bornée.
func ShortID(imageID string) string {
	if strings.HasPrefix(imageID, "sha256:") {
		r := []rune(imageID)
		if len(r) > 19 {
			r = r[:19]
		}
		return string(r[7:])
	}
	return CleanText(imageID, 40)
}

// SplitRef découpe une référence d'image en (dépôt, étiquette, digest épinglé ou "").
// 'traefik:v3.6' donne ("traefik", "v3.6", "") ; sans étiquette : "latest".
func SplitRef(ref string) (repo, tag, digest string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, digest = ref[:i], ref[i+1:]
	}
	last := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		last = ref[i+1:]
	}
	if strings.Contains(last, ":") {
		i := strings.LastIndex(ref, ":")
		return ref[:i], ref[i+1:], digest
	}
	return ref, "latest", digest
}

// Repr reproduit repr() d'une chaîne Python (messages d'erreur identiques à ceux du script d'origine).
func Repr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
