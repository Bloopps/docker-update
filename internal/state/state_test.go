package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bloopps/docker-update/internal/textutil"
)

func TestRoundtripAndCorruption(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	s.Put("a:1", &Image{Digest: "sha256:abc", FirstSeen: 5})
	s.Holds = append(s.Holds, "x")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("droits : %v %v", info, err)
	}
	if dirInfo, _ := os.Stat(filepath.Dir(p)); dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dossier créé en %o", dirInfo.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Errorf("fichier temporaire oublié : %s", e.Name())
		}
	}
	s2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get("a:1"); got == nil || got.FirstSeen != 5 || got.Digest != "sha256:abc" {
		t.Errorf("image relue : %+v", got)
	}
	if !slices.Equal(s2.Holds, []string{"x"}) || !s2.HasHold("x") || s2.HasHold("y") {
		t.Errorf("holds : %v", s2.Holds)
	}
	for _, bad := range []string{"{pas du json", "[1,2]", `{"images": []}`, `{"holds": {}}`, `{"images": null}`, `{"images": {"a": null}}`,
		`{"alerts": {"a": "x"}}`, `{"errors": {"a": 1}}`, `{"images": {"a": {"first_seen": 1.5}}}`, `"texte"`, ``} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%q devrait être refusé", bad)
		} else if !errors.As(err, new(*Error)) {
			t.Errorf("%q : erreur d'un autre type : %T", bad, err)
		}
	}
	if err := os.WriteFile(p, []byte("{pas du json"), 0o600); err == nil {
		if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "état illisible : "+p+" (le déplacer pour repartir de zéro)") {
			t.Errorf("message : %v", err)
		}
	}
	// Un état JSON valide mais d'un autre type est « invalide », pas « illisible ».
	os.WriteFile(p, []byte("[1,2]"), 0o600)
	if _, err := Load(p); err == nil || err.Error() != "état invalide : "+p {
		t.Errorf("message : %v", err)
	}
	os.WriteFile(p, []byte(`{"images": []}`), 0o600)
	if _, err := Load(p); err == nil || err.Error() != "état invalide (images) : "+p {
		t.Errorf("message : %v", err)
	}
}

func TestMissingFileGivesAnEmptyState(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Images) != 0 || len(s.Holds) != 0 || s.Errors == nil || s.Alerts == nil || s.Advisories == nil || s.Releases == nil {
		t.Errorf("état vide mal initialisé : %+v", s)
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	const want = "{\n \"advisories\": {},\n \"alerts\": {},\n \"errors\": {},\n \"holds\": [],\n \"images\": {},\n \"releases\": {},\n \"schema\": 1\n}"
	if string(data) != want {
		t.Errorf("JSON d'un état vide :\n%s", data)
	}
}

// Le fichier testdata/state-python.json a été écrit par State.save de la version Python (voir docs/PORTAGE.md).
// Go doit le charger tel quel et le réécrire à l'identique, octet pour octet.
func TestStateWrittenByPythonLoadsAndIsRewrittenIdentically(t *testing.T) {
	want, err := os.ReadFile("testdata/state-python.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load("testdata/state-python.json")
	if err != nil {
		t.Fatal(err)
	}
	img := s.Get("traefik:v3.6")
	if img == nil || img.FirstSeen != 1777629600 || !img.Failed || img.Notified || img.Digest != "sha256:"+strings.Repeat("ab", 32) {
		t.Errorf("image : %+v", img)
	}
	if !slices.Equal(s.Holds, []string{"tirelire", "wg-easy"}) || !strings.HasSuffix(s.Errors["homepage"], "(unauthorized) é") {
		t.Errorf("holds/errors : %v %v", s.Holds, s.Errors)
	}
	feed := s.Advisories["traefik/traefik"]
	if feed == nil || !feed.Ok || feed.Etag != `W/"abc123"` || len(feed.Items) != 2 || feed.Items[0].Pub != 1777629600 ||
		feed.Items[0].Summary != "Contournement d'authentification é — 😀 <b>&</b>" || !slices.Equal(feed.Items[0].Ranges, []string{"< 3.6.3", ">= 4.0.0, < 4.1.0"}) || !feed.Items[1].Withdrawn {
		t.Errorf("avis : %+v", feed)
	}
	rel := s.Releases["traefik/traefik"]
	if rel == nil || rel.Full != 1777600000 || len(rel.Items) != 2 || rel.Items[0].Tag != "v2.45.1" || !rel.Items[1].Pre || len(rel.Items[0].Hits) < 2 {
		t.Errorf("releases : %+v", rel)
	}
	if got := s.Alerts["release-version:app"]; got != 1777630200 {
		t.Errorf("alerts : %d", got)
	}
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// seule différence avec le fichier du Python : la clé « schema » (version du format), ajoutée à la réécriture
	wantStr := strings.TrimSuffix(string(want), "\n}") + ",\n \"schema\": 1\n}"
	if string(data) != wantStr {
		t.Errorf("réécriture différente de celle du Python.\nGo :\n%s\nPython :\n%s", data, want)
	}
}

func TestPythonEmptiesAreRead(t *testing.T) {
	s, err := Load("testdata/state-python-empties.json")
	if err != nil {
		t.Fatal(err)
	}
	if f := s.Advisories["a/b"]; f == nil || f.Ok || f.Err != 403 || f.Fetched != 5 || len(f.Items) != 0 || f.Items == nil {
		t.Errorf("a/b : %+v", f)
	}
	if f := s.Advisories["c/d"]; f == nil || !f.Ok || f.Etag != "" {
		t.Errorf("c/d : %+v", f)
	}
	if f := s.Releases["a/b"]; f == nil || !f.Ok || f.Full != 0 {
		t.Errorf("releases a/b : %+v", f)
	}
	// Les clés vides (« etag »: "", « err »: 0, « full »: 0) sont omises à la réécriture : même sens pour les deux versions.
	data, _ := s.Marshal()
	if strings.Contains(string(data), `"etag": ""`) || strings.Contains(string(data), `"err": 0`) {
		t.Errorf("clés vides non omises :\n%s", data)
	}
	again := &State{}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "x"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = again
}

func TestNumbersStayIntegers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	// 2^53 + 1 ne survit pas à un décodage en float64 : il doit rester exact.
	if err := os.WriteFile(p, []byte(`{"alerts": {"k": 9007199254740993}, "images": {"a": {"first_seen": 9007199254740993}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Alerts["k"] != 9007199254740993 || s.Images["a"].FirstSeen != 9007199254740993 {
		t.Errorf("entiers altérés : %+v %+v", s.Alerts, s.Images["a"])
	}
	data, _ := s.Marshal()
	if !strings.Contains(string(data), "9007199254740993") {
		t.Errorf("entiers altérés à l'écriture : %s", data)
	}
}

func TestMarshalEscapesLikePythonDumps(t *testing.T) {
	s := &State{}
	s.init()
	s.Errors["\u00e9"] = "tab\t nl\n quote\" back\\ <&> \u2028 \x7f \U0001F600 \x01"
	data, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"\u00e9"`, `tab\t nl\n quote\" back\\ <&> `, `\u2028`, "\x7f", `\ud83d\ude00`, `\u0001`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("%q absent de %s", want, data)
		}
	}
	for _, b := range data {
		if b >= 0x80 {
			t.Fatalf("octet non ASCII dans %q", data)
		}
	}
	// Relu, le texte est celui d'origine.
	p := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(p, data, 0o600)
	back, err := Load(p)
	if err != nil || back.Errors["é"] != s.Errors["é"] {
		t.Errorf("aller-retour : %v %q", err, back.Errors["é"])
	}
}

func TestSaveFailureLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target, 0o700); err != nil { // le renommage vers un dossier non vide échoue
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(target, "x"), nil, 0o600)
	s, _ := Load(filepath.Join(dir, "absent"))
	s.path = target
	if err := s.Save(); err == nil {
		t.Fatal("l'écriture devrait échouer")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Errorf("fichier temporaire oublié : %s", e.Name())
		}
	}
}

var _ = textutil.Advisory{}

func TestOldStateIsMigratedAndBackedUpOnFirstSave(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	old := "{\n \"holds\": [\"a\"]\n}"
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p + ".v0.bak"); err != nil || string(b) != old {
		t.Fatalf("sauvegarde : %q %v", b, err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `"schema": 1`) {
		t.Fatalf("schema non écrit : %s", b)
	}
	// deuxième écriture : plus de sauvegarde, celle d'origine reste intacte
	s.Holds = append(s.Holds, "b")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p + ".v0.bak"); string(b) != old {
		t.Fatal("sauvegarde écrasée")
	}
}

func TestStateFromNewerVersionIsRefusedAndUntouched(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	body := fmt.Sprintf("{\"schema\": %d, \"holds\": []}", CurrentSchema+1)
	os.WriteFile(p, []byte(body), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "plus récent") {
		t.Fatalf("err=%v", err)
	}
	for _, bad := range []string{`{"schema": "x"}`, `{"schema": -1}`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := Load(p); err == nil {
			t.Fatalf("%s accepté", bad)
		}
	}
}
