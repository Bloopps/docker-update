// Package tomledit modifie un fichier de configuration TOML par son texte : les commentaires, les lignes vides et la
// mise en forme de l'utilisateur sont conservés. Seuls les blocs [[service]] sont ajoutés ou retirés.
//
// Un bloc va de ses lignes de commentaire collées juste au-dessus de son en-tête (sans ligne vide) jusqu'à sa dernière
// ligne qui n'est ni vide ni un commentaire. Les commentaires et lignes vides qui suivent restent en place.
//
// Invariant (testé) : après Insert d'un bloc, Delete du même bloc redonne le fichier d'origine, octet pour octet.
package tomledit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Bloopps/docker-update/internal/tomlmini"
)

// Error : fichier illisible ou opération impossible.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

type block struct {
	name   string
	start  int // première ligne (commentaires collés compris)
	header int // ligne de l'en-tête
	end    int // dernière ligne qui n'est ni vide ni un commentaire
	region int // dernière ligne non vide avant le bloc suivant (commentaires de fin compris)
}

// File est un fichier découpé en lignes, avec ses blocs [[service]].
type File struct {
	lines  []string
	final  bool // le fichier se termine par un saut de ligne
	blocks []block
}

func isBlank(l string) bool   { return strings.TrimSpace(l) == "" }
func isComment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }

// Parse découpe le fichier. Il doit être un TOML valide pour l'outil.
func Parse(src []byte) (*File, error) {
	if strings.Contains(string(src), "\r") {
		return nil, &Error{"configuration : fins de ligne Windows (\\r\\n) non gérées par l'édition ; convertir le fichier (dos2unix)"}
	}
	doc, err := tomlmini.Parse(src)
	if err != nil {
		return nil, &Error{fmt.Sprintf("configuration illisible : %v", err)}
	}
	f := &File{}
	text := string(src)
	if text != "" {
		f.final = strings.HasSuffix(text, "\n")
		f.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	tables := doc.Arrays["service"]
	for i, t := range tables {
		name, _ := t.Get("name")
		h := t.Line - 1
		if h < 0 || h >= len(f.lines) {
			return nil, &Error{"configuration : en-tête [[service]] introuvable"}
		}
		start := h
		floor := 0
		if i > 0 {
			floor = f.blocks[i-1].header + 1
		}
		for start-1 >= floor && isComment(f.lines[start-1]) {
			start--
		}
		f.blocks = append(f.blocks, block{name: name.PyStr(), start: start, header: h})
	}
	for i := range f.blocks {
		limit := len(f.lines)
		if i+1 < len(f.blocks) {
			limit = f.blocks[i+1].start
		}
		b := &f.blocks[i]
		b.end, b.region = b.header, b.header
		for l := b.header; l < limit; l++ {
			if !isBlank(f.lines[l]) {
				b.region = l
				if !isComment(f.lines[l]) {
					b.end = l
				}
			}
		}
	}
	return f, nil
}

// Names renvoie les noms des services dans l'ordre du fichier.
func (f *File) Names() []string {
	out := make([]string, len(f.blocks))
	for i, b := range f.blocks {
		out[i] = b.name
	}
	return out
}

// Index renvoie la position d'un service dans le fichier, ou -1.
func (f *File) Index(name string) int {
	for i, b := range f.blocks {
		if b.name == name {
			return i
		}
	}
	return -1
}

// Block renvoie le texte d'un bloc (commentaires collés compris), sans saut de ligne final.
func (f *File) Block(name string) (string, bool) {
	i := f.Index(name)
	if i < 0 {
		return "", false
	}
	b := f.blocks[i]
	return strings.Join(f.lines[b.start:b.end+1], "\n"), true
}

// Bytes réécrit le fichier.
func (f *File) Bytes() []byte {
	if len(f.lines) == 0 {
		return nil
	}
	s := strings.Join(f.lines, "\n")
	if f.final {
		s += "\n"
	}
	return []byte(s)
}

// Delete retire un bloc, avec la ligne vide qui le précédait (une seule).
func (f *File) Delete(name string) error {
	i := f.Index(name)
	if i < 0 {
		return &Error{fmt.Sprintf("service inconnu : %s", name)}
	}
	b := f.blocks[i]
	from := b.start
	if from > 0 && isBlank(f.lines[from-1]) {
		from--
	}
	f.lines = append(f.lines[:from:from], f.lines[b.end+1:]...)
	return f.reparse()
}

// Insert ajoute un bloc (texte sans saut de ligne final) après le service `after` ; after == "" : à la fin du fichier ;
// first : avant le premier bloc. Une ligne vide sépare le nouveau bloc de ce qui précède.
func (f *File) Insert(blockText, after string, first bool) error {
	lines := strings.Split(blockText, "\n")
	var at int // indice d'insertion
	var ins []string
	switch {
	case first && len(f.blocks) > 0:
		at = f.blocks[0].start
		ins = append(lines, "")
	case after != "":
		i := f.Index(after)
		if i < 0 {
			return &Error{fmt.Sprintf("service inconnu : %s", after)}
		}
		at = f.blocks[i].region + 1
		ins = append([]string{""}, lines...)
	default:
		at = 0
		for l := len(f.lines) - 1; l >= 0; l-- {
			if !isBlank(f.lines[l]) {
				at = l + 1
				break
			}
		}
		ins = lines
		if at > 0 {
			ins = append([]string{""}, lines...)
		}
	}
	wasEmpty := len(f.lines) == 0
	out := append([]string{}, f.lines[:at]...)
	out = append(out, ins...)
	f.lines = append(out, f.lines[at:]...)
	if wasEmpty {
		f.final = true // un fichier neuf se termine par un saut de ligne ; sinon l'état d'origine est conservé
	}
	return f.reparse()
}

// reparse recalcule les blocs après une modification (et vérifie que le texte reste un TOML valide).
func (f *File) reparse() error {
	g, err := Parse(f.Bytes())
	if err != nil {
		return err
	}
	f.lines, f.final, f.blocks = g.lines, g.final, g.blocks
	return nil
}

// Entry est une clé d'un bloc à écrire.
type Entry struct {
	Key string
	Val tomlmini.Value
}

// Ordre canonique des clés d'un bloc écrit par l'outil.
var keyOrder = []string{"name", "compose_dir", "service", "compose_files", "mode", "cooldown", "group", "window", "cleanup",
	"advisories", "min_severity", "security_cooldown", "security_policy", "release_notes", "release_notes_repos",
	"release_notes_ignore", "smoke_url", "smoke_expect", "health_timeout"}

// String, Strings, Ints, Bool, Int construisent des valeurs.
func String(s string) tomlmini.Value { return tomlmini.Value{Kind: tomlmini.String, Str: s} }
func Bool(b bool) tomlmini.Value     { return tomlmini.Value{Kind: tomlmini.Bool, Bool: b} }
func Int(n int64) tomlmini.Value     { return tomlmini.Value{Kind: tomlmini.Int, Int: n} }
func Strings(list []string) tomlmini.Value {
	v := tomlmini.Value{Kind: tomlmini.Array}
	for _, s := range list {
		v.Items = append(v.Items, String(s))
	}
	return v
}
func Ints(list []int) tomlmini.Value {
	v := tomlmini.Value{Kind: tomlmini.Array}
	for _, n := range list {
		v.Items = append(v.Items, Int(int64(n)))
	}
	return v
}

// Render écrit un bloc [[service]] canonique : clés dans l'ordre fixe, alignées sur « = ». Une clé inconnue est refusée.
func Render(entries []Entry) (string, error) {
	byKey := map[string]tomlmini.Value{}
	for _, e := range entries {
		if !contains(keyOrder, e.Key) {
			return "", &Error{fmt.Sprintf("clé inconnue : %s", e.Key)}
		}
		byKey[e.Key] = e.Val
	}
	var b strings.Builder
	b.WriteString("[[service]]")
	for _, k := range keyOrder {
		if v, ok := byKey[k]; ok {
			s, err := value(v)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "\n%-*s = %s", 19, k, s)
		}
	}
	return b.String(), nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func value(v tomlmini.Value) (string, error) {
	switch v.Kind {
	case tomlmini.String:
		return Quote(v.Str), nil
	case tomlmini.Int:
		return strconv.FormatInt(v.Int, 10), nil
	case tomlmini.Bool:
		return strconv.FormatBool(v.Bool), nil
	}
	parts := make([]string, len(v.Items))
	for i, it := range v.Items {
		s, err := value(it)
		if err != nil {
			return "", err
		}
		parts[i] = s
	}
	return "[" + strings.Join(parts, ", ") + "]", nil
}

// Quote écrit une chaîne TOML entre guillemets ; les caractères de contrôle sont échappés (\uXXXX).
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// SetString remplace la valeur (texte sur une seule ligne) de la clé `key` dans le bloc `name` ; le commentaire de fin de
// ligne est conservé. Renvoie false si la clé n'est pas dans le bloc. Une valeur d'une autre forme (tableau, entier…) est refusée.
func (f *File) SetString(name, key, value string) (bool, error) {
	i := f.Index(name)
	if i < 0 {
		return false, &Error{fmt.Sprintf("service inconnu : %s", name)}
	}
	b := f.blocks[i]
	for l := b.header + 1; l <= b.end; l++ {
		m := setLine.FindStringSubmatch(f.lines[l])
		if m == nil || m[2] != key {
			continue
		}
		if m[3] == "" {
			return false, &Error{fmt.Sprintf("la clé %s du service %s n'est pas un texte sur une ligne : modification à la main", key, name)}
		}
		f.lines[l] = m[1] + Quote(value) + m[4]
		return true, f.reparse()
	}
	return false, nil
}

// setLine : indentation + clé + « = », valeur texte entre guillemets (m[3] vide si autre forme), reste de la ligne.
var setLine = regexp.MustCompile(`^(\s*([A-Za-z0-9_-]+)\s*=\s*)("(?:[^"\\]|\\.)*"|'[^']*')?(.*)$`)
