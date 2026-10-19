package tomlmini

import (
	"strconv"
	"strings"

	"github.com/Bloopps/docker-update/internal/textutil"
)

// Kind est le type d'une valeur TOML.
type Kind int

// Types de valeurs pris en charge.
const (
	String Kind = iota
	Int
	Bool
	Array
)

// Value est une valeur TOML décodée.
type Value struct {
	Kind  Kind
	Str   string
	Int   int64
	Bool  bool
	Items []Value // pour Array
	Line  int     // ligne du fichier où la valeur commence
}

// PyStr reproduit `str(valeur)` de Python (utilisé par la configuration pour lire une chaîne quel que soit le type).
func (v Value) PyStr() string {
	switch v.Kind {
	case String:
		return v.Str
	case Int:
		return strconv.FormatInt(v.Int, 10)
	case Bool:
		if v.Bool {
			return "True"
		}
		return "False"
	}
	return v.Repr()
}

// Repr reproduit `repr(valeur)` de Python (messages d'erreur identiques à ceux du script d'origine).
func (v Value) Repr() string {
	switch v.Kind {
	case String:
		return textutil.Repr(v.Str)
	case Array:
		parts := make([]string, len(v.Items))
		for i, it := range v.Items {
			parts[i] = it.Repr()
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return v.PyStr()
}

// Truthy reproduit la valeur de vérité Python.
func (v Value) Truthy() bool {
	switch v.Kind {
	case String:
		return v.Str != ""
	case Int:
		return v.Int != 0
	case Bool:
		return v.Bool
	}
	return len(v.Items) > 0
}

// Table est une table TOML : ses clés dans l'ordre du fichier.
type Table struct {
	Name string
	Line int // ligne de l'en-tête (0 pour la racine)
	keys []string
	vals map[string]Value
}

func newTable(name string, line int) *Table {
	return &Table{Name: name, Line: line, vals: map[string]Value{}}
}

// Keys renvoie les clés dans l'ordre du fichier.
func (t *Table) Keys() []string { return t.keys }

// Get renvoie la valeur d'une clé.
func (t *Table) Get(key string) (Value, bool) {
	v, ok := t.vals[key]
	return v, ok
}

// Document est le résultat de la lecture d'un fichier.
type Document struct {
	Root   *Table              // clés placées avant le premier en-tête
	Tables map[string]*Table   // [nom]
	Arrays map[string][]*Table // [[nom]], dans l'ordre du fichier
}
