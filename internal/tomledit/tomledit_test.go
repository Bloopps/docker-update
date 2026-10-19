package tomledit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/tomlmini"
)

const sample = `# en-tête du fichier
[settings]
state_file = "/tmp/state.json"   # commentaire de fin de ligne

# ── Premier groupe ──
# (deux lignes collées au bloc)
[[service]]
name        = "a"
compose_dir = "/srv/a"
service     = "a"
mode        = "notify"
advisories  = [
  "x/y",   # dans un tableau
  "z/w",
]
# commentaire de fin de bloc a

[[service]]
name = "b"
compose_dir = "/srv/b"
service = "b"
mode = "notify"


# détaché du suivant

# collé à c
[[service]]
name = "c-é"
compose_dir = "/srv/c"
service = "c"
mode = "notify"
`

func newBlock(t *testing.T, name string) string {
	t.Helper()
	b, err := Render([]Entry{{"name", String(name)}, {"compose_dir", String("/srv/zz")}, {"service", String("zz")}, {"mode", String("notify")}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func corpus(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{
		"exemple":              sample,
		"sans saut final":      strings.TrimSuffix(sample, "\n"),
		"un seul bloc":         "[settings]\n\n[[service]]\nname = \"a\"\ncompose_dir = \"/a\"\nservice = \"a\"\n",
		"blocs collés":         "[settings]\n[[service]]\nname = \"a\"\ncompose_dir = \"/a\"\nservice = \"a\"\n[[service]]\nname = \"b\"\ncompose_dir = \"/b\"\nservice = \"b\"\n",
		"lignes vides finales": sample + "\n\n\n",
		"aucun service":        "[settings]\nstate_file = \"/x\"\n",
	}
	for _, n := range []string{"updates.example.toml", "updates.watchtower.toml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", n))
		if err != nil {
			t.Fatal(err)
		}
		files[n] = string(b)
	}
	return files
}

// Un ajout suivi de la suppression du même bloc redonne le fichier d'origine, octet pour octet.
func TestInsertThenDeleteRestoresTheFileByteForByte(t *testing.T) {
	for label, src := range corpus(t) {
		f0, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s : %v", label, err)
		}
		type where struct {
			after string
			first bool
		}
		wheres := []where{{"", false}}
		for _, n := range f0.Names() {
			wheres = append(wheres, where{n, false})
		}
		if len(f0.blocks) > 0 && (f0.blocks[0].start == 0 || isBlank(f0.lines[f0.blocks[0].start-1])) {
			wheres = append(wheres, where{"", true}) // « avant le premier » : s'il est précédé d'une ligne vide (voir doc)
		}
		for _, w := range wheres {
			f, _ := Parse([]byte(src))
			if err := f.Insert(newBlock(t, "zz-new"), w.after, w.first); err != nil {
				t.Fatalf("%s %+v : %v", label, w, err)
			}
			names := f.Names()
			if f.Index("zz-new") < 0 || len(names) != len(f0.Names())+1 {
				t.Fatalf("%s %+v : noms = %v", label, w, names)
			}
			switch {
			case w.first:
				if names[0] != "zz-new" {
					t.Fatalf("%s : pas en premier : %v", label, names)
				}
			case w.after != "":
				if names[f0.Index(w.after)+1] != "zz-new" {
					t.Fatalf("%s : pas après %s : %v", label, w.after, names)
				}
			default:
				if names[len(names)-1] != "zz-new" {
					t.Fatalf("%s : pas à la fin : %v", label, names)
				}
			}
			if _, err := tomlmini.Parse(f.Bytes()); err != nil {
				t.Fatalf("%s %+v : résultat invalide : %v", label, w, err)
			}
			if err := f.Delete("zz-new"); err != nil {
				t.Fatalf("%s %+v : %v", label, w, err)
			}
			if got := string(f.Bytes()); got != src {
				t.Fatalf("%s %+v : le fichier n'est pas revenu à l'identique\n--- attendu\n%q\n--- obtenu\n%q", label, w, src, got)
			}
		}
	}
}

func TestDeleteRemovesTheBlockItsGluedCommentsAndOneBlankLine(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("c-é"); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(sample, "\n# collé à c\n[[service]]\nname = \"c-é\"\ncompose_dir = \"/srv/c\"\nservice = \"c\"\nmode = \"notify\"\n", "", 1)
	if got := string(f.Bytes()); got != want {
		t.Fatalf("obtenu :\n%q\nattendu :\n%q", got, want)
	}
	if !strings.Contains(string(f.Bytes()), "# détaché du suivant") {
		t.Fatal("un commentaire détaché a été supprimé")
	}

	f, _ = Parse([]byte(sample))
	if err := f.Delete("a"); err != nil {
		t.Fatal(err)
	}
	got := string(f.Bytes())
	for _, gone := range []string{"Premier groupe", "deux lignes collées", `name        = "a"`, "dans un tableau", `"z/w"`} {
		if strings.Contains(got, gone) {
			t.Fatalf("%q devrait avoir disparu :\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "# commentaire de fin de bloc a") {
		t.Fatal("le commentaire de fin de bloc est un texte de l'utilisateur : il reste")
	}
	if !strings.Contains(got, "[settings]\nstate_file") || !strings.Contains(got, `name = "b"`) {
		t.Fatalf("le reste est intact :\n%s", got)
	}
	if err := f.Delete("inconnu"); err == nil {
		t.Fatal("un nom inconnu doit être refusé")
	}
}

func TestRenderedBlockIsCanonicalAndReadsBack(t *testing.T) {
	weird := "a\"b\\c\téé 中 \u0001"
	b, err := Render([]Entry{
		{"smoke_expect", Ints([]int{302, 401})}, {"mode", String("auto")}, {"service", String("web")},
		{"name", String(weird)}, {"compose_dir", String("/srv/x y")}, {"window", String("")},
		{"cleanup", Bool(false)}, {"compose_files", Strings([]string{"compose.yml", "o.yml"})}, {"health_timeout", Int(90)},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[[service]]
name                = "a\"b\\c\té` + "é" + ` 中 \u0001"
compose_dir         = "/srv/x y"
service             = "web"
compose_files       = ["compose.yml", "o.yml"]
mode                = "auto"
window              = ""
cleanup             = false
smoke_expect        = [302, 401]
health_timeout      = 90`
	if b != want {
		t.Fatalf("obtenu :\n%s\nattendu :\n%s", b, want)
	}
	doc, err := tomlmini.Parse([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := doc.Arrays["service"][0].Get("name")
	if v.Str != weird {
		t.Fatalf("nom relu = %q", v.Str)
	}
	if _, err := Render([]Entry{{"inconnue", String("x")}}); err == nil {
		t.Fatal("une clé inconnue doit être refusée")
	}
}

func TestExamplesStayValidAfterAnInsert(t *testing.T) {
	for _, n := range []string{"updates.example.toml", "updates.watchtower.toml"} {
		b, _ := os.ReadFile(filepath.Join("..", "..", "examples", n))
		f, err := Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Insert(newBlock(t, "zz-new"), f.Names()[1], false); err != nil {
			t.Fatal(err)
		}
		doc, err := tomlmini.Parse(f.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Parse(doc)
		if err != nil {
			t.Fatalf("%s : %v", n, err)
		}
		var names []string
		for _, s := range cfg.Services {
			names = append(names, s.Name)
		}
		if !reflect.DeepEqual(names, f.Names()) || names[2] != "zz-new" {
			t.Fatalf("%s : %v", n, names)
		}
	}
}

func TestWindowsLineEndingsAreRefused(t *testing.T) {
	if _, err := Parse([]byte("[settings]\r\n")); err == nil {
		t.Fatal("fins de ligne \\r\\n : doit être refusé")
	}
}

func TestInsertIntoAnEmptyFileEndsWithANewline(t *testing.T) {
	f, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Insert(newBlock(t, "zz-new"), "", false); err != nil {
		t.Fatal(err)
	}
	if got := string(f.Bytes()); !strings.HasPrefix(got, "[[service]]\nname") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("%q", got)
	}
}

// Un commentaire de fin de bloc reste avec son bloc : le nouveau bloc s'insère après lui, pas avant.
func TestInsertGoesAfterTheTrailingCommentsOfTheBlock(t *testing.T) {
	f, _ := Parse([]byte(sample))
	if err := f.Insert(newBlock(t, "zz-new"), "a", false); err != nil {
		t.Fatal(err)
	}
	got := string(f.Bytes())
	i, j := strings.Index(got, "# commentaire de fin de bloc a"), strings.Index(got, `name                = "zz-new"`)
	if i < 0 || j < 0 || i > j {
		t.Fatalf("le commentaire de fin de bloc doit précéder le nouveau bloc :\n%s", got)
	}
	if !strings.Contains(got, "# commentaire de fin de bloc a\n\n[[service]]\nname") {
		t.Fatalf("une ligne vide sépare le nouveau bloc :\n%s", got)
	}
}
