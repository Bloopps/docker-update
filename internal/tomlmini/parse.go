package tomlmini

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Error est une erreur de lecture, avec le numéro de ligne.
type Error struct {
	Line        int
	Msg         string
	Unsupported bool // vrai : TOML valide mais hors du sous-ensemble
}

func (e *Error) Error() string {
	if e.Unsupported {
		return fmt.Sprintf("TOML non pris en charge : %s (ligne %d)", e.Msg, e.Line)
	}
	return fmt.Sprintf("TOML invalide : %s (ligne %d)", e.Msg, e.Line)
}

type parser struct {
	src  string
	pos  int
	line int
	doc  *Document
	cur  *Table
}

// Parse lit un document TOML (sous-ensemble strict, voir la documentation du paquet).
func Parse(src []byte) (*Document, error) {
	if !utf8.Valid(src) {
		return nil, &Error{Line: 1 + strings.Count(string(src[:invalidAt(src)]), "\n"), Msg: "le fichier n'est pas de l'UTF-8 valide"}
	}
	p := &parser{src: string(src), line: 1}
	p.doc = &Document{Root: newTable("", 0), Tables: map[string]*Table{}, Arrays: map[string][]*Table{}}
	p.cur = p.doc.Root
	if err := p.run(); err != nil {
		return nil, err
	}
	return p.doc, nil
}

func invalidAt(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(b)
}

func (p *parser) fail(format string, a ...any) error {
	return &Error{Line: p.line, Msg: fmt.Sprintf(format, a...)}
}

func (p *parser) unsupported(format string, a ...any) error {
	return &Error{Line: p.line, Msg: fmt.Sprintf(format, a...), Unsupported: true}
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) skipSpaces() {
	for !p.eof() && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func isControl(c byte) bool { return (c < 0x20 && c != '\t') || c == 0x7f }

// skipComment avance jusqu'à la fin de la ligne (le saut de ligne n'est pas consommé).
func (p *parser) skipComment() error {
	for !p.eof() {
		c := p.src[p.pos]
		if c == '\n' || c == '\r' {
			return nil
		}
		if isControl(c) {
			return p.fail("caractère de contrôle interdit dans un commentaire")
		}
		p.pos++
	}
	return nil
}

// newline consomme « \n » ou « \r\n » et compte la ligne.
func (p *parser) newline() error {
	if p.peek() == '\r' {
		if p.pos+1 >= len(p.src) || p.src[p.pos+1] != '\n' {
			return p.fail("retour chariot isolé")
		}
		p.pos++
	}
	p.pos++
	p.line++
	return nil
}

// endOfLine vérifie que le reste de la ligne est vide (espaces, commentaire) et la termine.
func (p *parser) endOfLine() error {
	p.skipSpaces()
	if p.peek() == '#' {
		if err := p.skipComment(); err != nil {
			return err
		}
	}
	if p.eof() {
		return nil
	}
	if c := p.peek(); c != '\n' && c != '\r' {
		return p.fail("texte inattendu après la valeur")
	}
	return p.newline()
}

func (p *parser) run() error {
	for {
		p.skipSpaces()
		if p.eof() {
			return nil
		}
		switch c := p.peek(); {
		case c == '\n' || c == '\r':
			if err := p.newline(); err != nil {
				return err
			}
		case c == '#':
			if err := p.skipComment(); err != nil {
				return err
			}
		case c == '[':
			if err := p.header(); err != nil {
				return err
			}
		default:
			if err := p.keyValue(); err != nil {
				return err
			}
		}
	}
}

func (p *parser) header() error {
	line := p.line
	isArray := strings.HasPrefix(p.src[p.pos:], "[[")
	p.pos++
	if isArray {
		p.pos++
	}
	p.skipSpaces()
	name, err := p.key()
	if err != nil {
		return err
	}
	p.skipSpaces()
	if p.peek() == '.' {
		return p.unsupported("clé pointée dans un en-tête de table")
	}
	if p.peek() != ']' {
		return p.fail("« ] » attendu à la fin de l'en-tête de table")
	}
	p.pos++
	if isArray {
		if p.peek() != ']' {
			return p.fail("« ]] » attendu à la fin de l'en-tête de tableau de tables")
		}
		p.pos++
	}
	if _, clash := p.doc.Root.vals[name]; clash {
		return p.fail("« %s » est déjà défini comme clé", name)
	}
	if isArray {
		if _, ok := p.doc.Tables[name]; ok {
			return p.fail("« %s » est déjà défini comme table", name)
		}
		t := newTable(name, line)
		p.doc.Arrays[name] = append(p.doc.Arrays[name], t)
		p.cur = t
	} else {
		if _, ok := p.doc.Tables[name]; ok {
			return p.fail("table « %s » définie deux fois", name)
		}
		if _, ok := p.doc.Arrays[name]; ok {
			return p.fail("« %s » est déjà défini comme tableau de tables", name)
		}
		t := newTable(name, line)
		p.doc.Tables[name] = t
		p.cur = t
	}
	return p.endOfLine()
}

func isBareKeyChar(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func (p *parser) key() (string, error) {
	switch c := p.peek(); {
	case c == '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			return "", p.unsupported("chaîne multiligne utilisée comme clé")
		}
		return p.basicString()
	case c == '\'':
		if strings.HasPrefix(p.src[p.pos:], `'''`) {
			return "", p.unsupported("chaîne multiligne utilisée comme clé")
		}
		return p.literalString()
	}
	start := p.pos
	for !p.eof() && isBareKeyChar(p.src[p.pos]) {
		p.pos++
	}
	if p.pos == start {
		return "", p.fail("clé attendue")
	}
	return p.src[start:p.pos], nil
}

func (p *parser) keyValue() error {
	line := p.line
	k, err := p.key()
	if err != nil {
		return err
	}
	p.skipSpaces()
	if p.peek() == '.' {
		return p.unsupported("clé pointée (« %s. »)", k)
	}
	if p.peek() != '=' {
		return p.fail("« = » attendu après la clé « %s »", k)
	}
	p.pos++
	p.skipSpaces()
	if p.eof() || p.peek() == '\n' || p.peek() == '\r' || p.peek() == '#' {
		return p.fail("valeur attendue après « %s = »", k)
	}
	v, err := p.value(true)
	if err != nil {
		return err
	}
	v.Line = line
	if _, dup := p.cur.vals[k]; dup {
		return &Error{Line: line, Msg: fmt.Sprintf("clé en double : « %s »", k)}
	}
	if p.cur == p.doc.Root {
		if _, ok := p.doc.Tables[k]; ok {
			return &Error{Line: line, Msg: fmt.Sprintf("« %s » est déjà défini comme table", k)}
		}
	}
	p.cur.keys = append(p.cur.keys, k)
	p.cur.vals[k] = v
	return p.endOfLine()
}

var (
	intDecRE = regexp.MustCompile(`^[+-]?(0|[1-9](_?[0-9])*)$`)
	intHexRE = regexp.MustCompile(`^0x[0-9A-Fa-f](_?[0-9A-Fa-f])*$`)
	intOctRE = regexp.MustCompile(`^0o[0-7](_?[0-7])*$`)
	intBinRE = regexp.MustCompile(`^0b[01](_?[01])*$`)
	dateRE   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}|^[0-9]{2}:[0-9]{2}`)
	floatRE  = regexp.MustCompile(`^[+-]?((0|[1-9](_?[0-9])*)(\.[0-9](_?[0-9])*([eE][+-]?[0-9](_?[0-9])*)?|[eE][+-]?[0-9](_?[0-9])*)|inf|nan)$`)
)

// value lit une valeur ; allowArray est faux à l'intérieur d'un tableau (pas de tableaux imbriqués).
func (p *parser) value(allowArray bool) (Value, error) {
	switch c := p.peek(); c {
	case '"':
		if strings.HasPrefix(p.src[p.pos:], `"""`) {
			return Value{}, p.unsupported("chaîne multiligne")
		}
		s, err := p.basicString()
		return Value{Kind: String, Str: s}, err
	case '\'':
		if strings.HasPrefix(p.src[p.pos:], `'''`) {
			return Value{}, p.unsupported("chaîne multiligne")
		}
		s, err := p.literalString()
		return Value{Kind: String, Str: s}, err
	case '[':
		if !allowArray {
			return Value{}, p.unsupported("tableau imbriqué")
		}
		return p.array()
	case '{':
		return Value{}, p.unsupported("table en ligne")
	}
	start := p.pos
	for !p.eof() {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == ']' || c == '#' || c == '}' {
			break
		}
		p.pos++
	}
	tok := p.src[start:p.pos]
	switch {
	case tok == "":
		return Value{}, p.fail("valeur attendue")
	case tok == "true":
		return Value{Kind: Bool, Bool: true}, nil
	case tok == "false":
		return Value{Kind: Bool, Bool: false}, nil
	}
	if n, ok, err := p.integer(tok); ok || err != nil {
		return Value{Kind: Int, Int: n}, err
	}
	switch {
	case dateRE.MatchString(tok):
		return Value{}, p.unsupported("date ou heure « %s »", tok)
	case floatRE.MatchString(tok):
		return Value{}, p.unsupported("nombre à virgule « %s »", tok)
	}
	return Value{}, p.fail("valeur invalide « %s »", tok)
}

func (p *parser) integer(tok string) (int64, bool, error) {
	var base int
	digits := tok
	switch {
	case intDecRE.MatchString(tok):
		base = 10
	case intHexRE.MatchString(tok):
		base, digits = 16, tok[2:]
	case intOctRE.MatchString(tok):
		base, digits = 8, tok[2:]
	case intBinRE.MatchString(tok):
		base, digits = 2, tok[2:]
	default:
		return 0, false, nil
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(digits, "_", ""), base, 64)
	if err != nil {
		return 0, true, p.unsupported("entier hors des 64 bits « %s »", tok)
	}
	return n, true, nil
}

func (p *parser) array() (Value, error) {
	line := p.line
	p.pos++ // [
	v := Value{Kind: Array, Line: line}
	for {
		if err := p.skipArraySpace(); err != nil {
			return Value{}, err
		}
		if p.eof() {
			return Value{}, &Error{Line: line, Msg: "tableau non terminé"}
		}
		if p.peek() == ']' {
			p.pos++
			return v, nil
		}
		itemLine := p.line
		item, err := p.value(false)
		if err != nil {
			return Value{}, err
		}
		item.Line = itemLine
		v.Items = append(v.Items, item)
		if err := p.skipArraySpace(); err != nil {
			return Value{}, err
		}
		switch p.peek() {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return v, nil
		default:
			if p.eof() {
				return Value{}, &Error{Line: line, Msg: "tableau non terminé"}
			}
			return Value{}, p.fail("« , » ou « ] » attendu dans le tableau")
		}
	}
}

func (p *parser) skipArraySpace() error {
	for {
		p.skipSpaces()
		switch p.peek() {
		case '\n', '\r':
			if p.eof() {
				return nil
			}
			if err := p.newline(); err != nil {
				return err
			}
		case '#':
			if err := p.skipComment(); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (p *parser) literalString() (string, error) {
	p.pos++ // '
	start := p.pos
	for !p.eof() {
		c := p.src[p.pos]
		switch {
		case c == '\'':
			s := p.src[start:p.pos]
			p.pos++
			return s, nil
		case c == '\n' || c == '\r':
			return "", p.fail("chaîne non terminée")
		case isControl(c):
			return "", p.fail("caractère de contrôle interdit dans une chaîne")
		}
		p.pos++
	}
	return "", p.fail("chaîne non terminée")
}

func (p *parser) basicString() (string, error) {
	p.pos++ // "
	var b strings.Builder
	for !p.eof() {
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c == '\n' || c == '\r':
			return "", p.fail("chaîne non terminée")
		case isControl(c):
			return "", p.fail("caractère de contrôle interdit dans une chaîne")
		case c == '\\':
			p.pos++
			if p.eof() {
				return "", p.fail("échappement incomplet")
			}
			e := p.src[p.pos]
			p.pos++
			switch e {
			case 'b':
				b.WriteByte('\b')
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			case 'f':
				b.WriteByte('\f')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'u', 'U':
				n := 4
				if e == 'U' {
					n = 8
				}
				if p.pos+n > len(p.src) {
					return "", p.fail("échappement \\%c incomplet", e)
				}
				code, err := strconv.ParseUint(p.src[p.pos:p.pos+n], 16, 32)
				if err != nil || strings.ContainsAny(p.src[p.pos:p.pos+n], "+-_") {
					return "", p.fail("échappement \\%c invalide", e)
				}
				if code > 0x10FFFF || (code >= 0xD800 && code <= 0xDFFF) {
					return "", p.fail("point de code invalide dans \\%c", e)
				}
				b.WriteRune(rune(code))
				p.pos += n
			default:
				return "", p.fail("échappement inconnu « \\%c »", e)
			}
		default:
			b.WriteByte(c)
			p.pos++
		}
	}
	return "", p.fail("chaîne non terminée")
}
