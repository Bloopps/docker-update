package tomlmini

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, src string) *Document {
	t.Helper()
	doc, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse(%q) : %v", src, err)
	}
	return doc
}

func get(t *testing.T, tb *Table, key string) Value {
	t.Helper()
	v, ok := tb.Get(key)
	if !ok {
		t.Fatalf("clé %q absente (clés : %v)", key, tb.Keys())
	}
	return v
}

func TestScalars(t *testing.T) {
	doc := mustParse(t, "s = \"a\\tb\\n\\\"c\\\\ \\u00e9 \\U0001F600\"\nl = 'a\\n\"b'\ni = 42\nn = -7\nu = 1_000\nh = 0xFF\nb = true\nf = false\ne = \"\"\n")
	r := doc.Root
	checks := map[string]Value{
		"s": {Kind: String, Str: "a\tb\n\"c\\ é 😀"}, "l": {Kind: String, Str: `a\n"b`}, "i": {Kind: Int, Int: 42}, "n": {Kind: Int, Int: -7},
		"u": {Kind: Int, Int: 1000}, "h": {Kind: Int, Int: 255}, "b": {Kind: Bool, Bool: true}, "f": {Kind: Bool}, "e": {Kind: String},
	}
	for k, want := range checks {
		got := get(t, r, k)
		if got.Kind != want.Kind || got.Str != want.Str || got.Int != want.Int || got.Bool != want.Bool {
			t.Errorf("%s = %+v ; attendu %+v", k, got, want)
		}
	}
	if !slices.Equal(r.Keys(), []string{"s", "l", "i", "n", "u", "h", "b", "f", "e"}) {
		t.Errorf("l'ordre du fichier est conservé : %v", r.Keys())
	}
}

func TestArrays(t *testing.T) {
	doc := mustParse(t, "a = [ \"x\", 'y', 3, ]\nm = [\n  \"un\", # commentaire\n  \"deux\",\n\n  # seul\n]\nv = []\n")
	a := get(t, doc.Root, "a")
	if a.Kind != Array || len(a.Items) != 3 || a.Items[0].Str != "x" || a.Items[1].Str != "y" || a.Items[2].Int != 3 {
		t.Errorf("a = %+v", a)
	}
	if m := get(t, doc.Root, "m"); len(m.Items) != 2 || m.Items[1].Str != "deux" || m.Items[1].Line != 4 {
		t.Errorf("m = %+v", m)
	}
	if v := get(t, doc.Root, "v"); v.Kind != Array || len(v.Items) != 0 {
		t.Errorf("v = %+v", v)
	}
}

func TestTablesAndArraysOfTables(t *testing.T) {
	doc := mustParse(t, "racine = 1\n# commentaire\n[settings]\nx = 1 # fin\n\n[ autre ]\ny = 2\n[[service]]\nname = \"a\"\n[[service]]\nname = \"b\"\r\n[[ service ]]\nname = \"c\"\n")
	if get(t, doc.Root, "racine").Int != 1 {
		t.Error("racine")
	}
	if get(t, doc.Tables["settings"], "x").Int != 1 || get(t, doc.Tables["autre"], "y").Int != 2 {
		t.Error("tables")
	}
	var names []string
	for _, s := range doc.Arrays["service"] {
		names = append(names, get(t, s, "name").Str)
	}
	if !slices.Equal(names, []string{"a", "b", "c"}) {
		t.Errorf("l'ordre des [[service]] est celui du fichier : %v", names)
	}
	if doc.Arrays["service"][1].Line != 10 {
		t.Errorf("ligne de l'en-tête : %d", doc.Arrays["service"][1].Line)
	}
}

func TestQuotedKeys(t *testing.T) {
	doc := mustParse(t, "\"a b\" = 1\n'c' = 2\n")
	if get(t, doc.Root, "a b").Int != 1 || get(t, doc.Root, "c").Int != 2 {
		t.Error("clés entre guillemets")
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"a":                    "« = » attendu",
		"a =":                  "valeur attendue",
		"= 1":                  "clé attendue",
		"a = 1 b":              "texte inattendu",
		"a = 1\na = 2":         "clé en double",
		"[t]\n[t]":             "définie deux fois",
		"[[t]]\n[t]":           "déjà défini comme tableau de tables",
		"[t]\n[[t]]":           "déjà défini comme table",
		"t = 1\n[t]":           "déjà défini comme clé",
		"[t]\nt = 1\n[u]\n[t]": "définie deux fois",
		"[t":                   "« ] » attendu",
		"[[t]":                 "« ]] » attendu",
		"[]":                   "clé attendue",
		"a = [1":               "non terminé",
		"a = [1 2]":            "« , » ou « ] » attendu",
		"a = [,]":              "valeur attendue",
		"a = \"x":              "non terminée",
		"a = 'x":               "non terminée",
		"a = \"\\q\"":          "échappement inconnu",
		"a = \"\\u00zz\"":      "invalide",
		"a = \"\\ud800\"":      "point de code invalide",
		"a = \"x\x01\"":        "contrôle",
		"# c\x01\na = 1":       "contrôle",
		"a = 1\rb = 2":         "retour chariot",
		"a = nimporte":         "valeur invalide",
		"a = 01":               "valeur invalide",
		"a = 1__0":             "valeur invalide",
		"a = 1_":               "valeur invalide",
		"\xef\xbb\xbfa = 1":    "clé attendue",
		"a = \xff":             "UTF-8",
		"a = 1 # c\x00":        "contrôle",
		"[t] x":                "texte inattendu",
		"a = \"x\" \"y\"":      "texte inattendu",
	}
	for src, want := range cases {
		_, err := Parse([]byte(src))
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%q devrait échouer", src)
			continue
		}
		if e.Unsupported || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), "TOML invalide : ") {
			t.Errorf("%q : %v (attendu %q)", src, err, want)
		}
	}
}

func TestUnsupportedIsRefusedClearly(t *testing.T) {
	cases := map[string]string{
		"a = 1.5":                  "nombre à virgule",
		"a = 1e3":                  "nombre à virgule",
		"a = inf":                  "nombre à virgule",
		"a = nan":                  "nombre à virgule",
		"a = 1979-05-27":           "date",
		"a = 1979-05-27T07:32:00Z": "date",
		"a = 07:32:00":             "date",
		"a = \"\"\"x\"\"\"":        "multiligne",
		"a = '''x'''":              "multiligne",
		"a = {b = 1}":              "table en ligne",
		"a.b = 1":                  "clé pointée",
		"[a.b]":                    "clé pointée",
		"[[a.b]]":                  "clé pointée",
		"a = [[1], [2]]":           "imbriqué",
		"a = [{b = 1}]":            "table en ligne",
		"a = 9223372036854775808":  "64 bits",
	}
	for src, want := range cases {
		_, err := Parse([]byte(src))
		var e *Error
		if !errors.As(err, &e) || !e.Unsupported {
			t.Errorf("%q devrait être refusé comme non pris en charge : %v", src, err)
			continue
		}
		if !strings.HasPrefix(err.Error(), "TOML non pris en charge : ") || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "(ligne 1)") {
			t.Errorf("%q : %v", src, err)
		}
	}
}

func TestErrorsCarryTheLineNumber(t *testing.T) {
	_, err := Parse([]byte("[settings]\n# ok\n\nx = 1\ny = {a = 1}\n"))
	if err == nil || !strings.Contains(err.Error(), "ligne 5") {
		t.Errorf("err = %v", err)
	}
	_, err = Parse([]byte("a = [\n1,\n2\n\n"))
	if err == nil || !strings.Contains(err.Error(), "ligne 1") {
		t.Errorf("tableau non terminé : %v", err)
	}
	_, err = Parse([]byte("a = 1\na = 2\n"))
	if err == nil || !strings.Contains(err.Error(), "ligne 2") {
		t.Errorf("clé en double : %v", err)
	}
}

func TestPythonViews(t *testing.T) {
	doc := mustParse(t, "s = \"l'eau\"\ni = 3\nb = true\na = [\"x\", 2, false]\nz = 0\nv = []\n")
	r := doc.Root
	for key, want := range map[string][2]string{
		"s": {"l'eau", `"l'eau"`}, "i": {"3", "3"}, "b": {"True", "True"}, "a": {"['x', 2, False]", "['x', 2, False]"},
	} {
		v := get(t, r, key)
		if v.PyStr() != want[0] || v.Repr() != want[1] {
			t.Errorf("%s : str=%q repr=%q ; attendu %q %q", key, v.PyStr(), v.Repr(), want[0], want[1])
		}
	}
	for key, want := range map[string]bool{"s": true, "i": true, "b": true, "a": true, "z": false, "v": false} {
		if got := get(t, r, key).Truthy(); got != want {
			t.Errorf("%s truthy = %v", key, got)
		}
	}
}

func TestExamplesAreRead(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.toml")
	if err != nil || len(files) < 2 {
		t.Fatalf("exemples introuvables : %v %v", files, err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := Parse(src)
		if err != nil {
			t.Errorf("%s : %v", f, err)
			continue
		}
		if len(doc.Arrays["service"]) < 10 || doc.Tables["settings"] == nil {
			t.Errorf("%s : %d services", f, len(doc.Arrays["service"]))
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "a = 1", "[t]\na = [1, 2,]\n", "[[s]]\nn = \"x\\u00e9\"\n", "a = [\n#c\n'x'\n]", "a = {b=1}", "a = \"\"\"", "\xff", "[[", "a = [[",
		"a = 1__", "[ a . b ]", "a = 0x", "a = \"\\U0001F600\"", "# c\r\n[t]\r\n"} {
		f.Add([]byte(s))
	}
	if files, _ := filepath.Glob("../../examples/*.toml"); len(files) > 0 {
		for _, name := range files {
			if b, err := os.ReadFile(name); err == nil {
				f.Add(b)
			}
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		start := time.Now()
		doc, err := Parse(data)
		if time.Since(start) > 2*time.Second {
			t.Fatalf("trop lent : %v", time.Since(start))
		}
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || e.Line < 1 {
				t.Fatalf("erreur sans ligne : %v", err)
			}
			return
		}
		if doc == nil || doc.Root == nil {
			t.Fatal("document vide")
		}
	})
}
