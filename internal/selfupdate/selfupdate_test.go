package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// elf fabrique un en-tête ELF 64 bits minimal pour l'architecture demandée, suivi d'un marqueur.
func elf(arch, marker string) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5] = 2, 1
	if arch == "amd64" {
		b[18] = 0x3e
	} else {
		b[18] = 0xb7
	}
	return append(b, marker...)
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type world struct {
	t     *testing.T
	files map[string][]byte
	calls []string
	d     Deps
	log   []string
}

func newWorld(t *testing.T, current, latest string) *world {
	t.Helper()
	w := &world{t: t, files: map[string][]byte{}}
	dir := t.TempDir()
	target := filepath.Join(dir, "docker-update")
	if err := os.WriteFile(target, []byte("ancien"), 0o750); err != nil {
		t.Fatal(err)
	}
	w.d = Deps{Repo: "o/r", APIBase: "https://api.test", DownloadBase: "https://dl.test", GOOS: "linux", GOARCH: "amd64", Current: current, Target: target}
	w.files["https://api.test/repos/o/r/releases/latest"] = []byte(fmt.Sprintf(`{"tag_name":%q}`, latest))
	bin := elf("amd64", "nouveau")
	base := "https://dl.test/o/r/releases/download/" + latest + "/"
	w.files[base+"docker-update-linux-amd64"] = bin
	w.files[base+"SHA256SUMS"] = []byte(sum(bin) + "  docker-update-linux-amd64\n" + sum(elf("arm64", "x")) + "  docker-update-linux-arm64\n")
	w.d.Fetch = func(url string, limit int64) ([]byte, error) {
		w.calls = append(w.calls, url)
		b, ok := w.files[url]
		if !ok {
			return nil, fmt.Errorf("introuvable : %s", url)
		}
		return b, nil
	}
	return w
}

func (w *world) run(check bool) (Result, error) {
	return Run(w.d, check, func(s string) { w.log = append(w.log, s) })
}

func (w *world) target() string {
	b, err := os.ReadFile(w.d.Target)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

func TestUpdateReplacesBinaryAndKeepsMode(t *testing.T) {
	w := newWorld(t, "v0.1.0", "v0.2.0")
	res, err := w.run(false)
	if err != nil || !res.Updated || res.Latest != "v0.2.0" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if !strings.HasSuffix(w.target(), "nouveau") {
		t.Fatal("binaire non remplacé")
	}
	fi, _ := os.Stat(w.d.Target)
	if fi.Mode().Perm() != 0o750 {
		t.Fatalf("droits perdus : %v", fi.Mode().Perm())
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(w.d.Target), ".docker-update-*"))
	if len(left) != 0 {
		t.Fatalf("fichier temporaire resté : %v", left)
	}
}

func TestUpdateRefusesWrongChecksum(t *testing.T) {
	w := newWorld(t, "v0.1.0", "v0.2.0")
	w.files["https://dl.test/o/r/releases/download/v0.2.0/SHA256SUMS"] = []byte(strings.Repeat("0", 64) + "  docker-update-linux-amd64\n")
	if _, err := w.run(false); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("err=%v", err)
	}
	if w.target() != "ancien" {
		t.Fatal("binaire remplacé malgré une somme fausse")
	}
}

func TestUpdateRefusesMissingSum(t *testing.T) {
	w := newWorld(t, "v0.1.0", "v0.2.0")
	w.files["https://dl.test/o/r/releases/download/v0.2.0/SHA256SUMS"] = []byte(sum([]byte("x")) + "  autre\n")
	if _, err := w.run(false); err == nil {
		t.Fatal("somme absente acceptée")
	}
	if w.target() != "ancien" {
		t.Fatal("binaire remplacé")
	}
}

func TestUpdateRefusesWrongArchitectureAndNonELF(t *testing.T) {
	for name, bin := range map[string][]byte{"arm64": elf("arm64", "x"), "texte": []byte("#!/bin/sh\nrm -rf /\n")} {
		w := newWorld(t, "v0.1.0", "v0.2.0")
		base := "https://dl.test/o/r/releases/download/v0.2.0/"
		w.files[base+"docker-update-linux-amd64"] = bin
		w.files[base+"SHA256SUMS"] = []byte(sum(bin) + "  docker-update-linux-amd64\n")
		if _, err := w.run(false); err == nil {
			t.Fatalf("%s accepté", name)
		}
		if w.target() != "ancien" {
			t.Fatalf("%s : binaire remplacé", name)
		}
	}
}

func TestUpdateUsesArm64Asset(t *testing.T) {
	w := newWorld(t, "v0.1.0", "v0.2.0")
	w.d.GOARCH = "arm64"
	bin := elf("arm64", "arm")
	base := "https://dl.test/o/r/releases/download/v0.2.0/"
	w.files[base+"docker-update-linux-arm64"] = bin
	w.files[base+"SHA256SUMS"] = []byte(sum(bin) + " *docker-update-linux-arm64\n")
	if _, err := w.run(false); err != nil || !strings.HasSuffix(w.target(), "arm") {
		t.Fatalf("err=%v", err)
	}
}

func TestUpToDateAndNeverDowngrades(t *testing.T) {
	for _, cur := range []string{"v0.2.0", "v0.3.0", "v1.0.0"} {
		w := newWorld(t, cur, "v0.2.0")
		res, err := w.run(false)
		if err != nil || !res.UpToDate || res.Updated {
			t.Fatalf("%s : res=%+v err=%v", cur, res, err)
		}
		if w.target() != "ancien" || len(w.calls) != 1 {
			t.Fatalf("%s : téléchargement inutile : %v", cur, w.calls)
		}
	}
}

func TestVersionComparisonIsNumeric(t *testing.T) {
	w := newWorld(t, "v0.9.0", "v0.10.0")
	if res, err := w.run(false); err != nil || !res.Updated {
		t.Fatalf("v0.10.0 doit être plus récent que v0.9.0 : %+v %v", res, err)
	}
}

func TestDevBuildUpdates(t *testing.T) {
	for _, cur := range []string{"dev", "v0.1.0-3-gabc123-dirty"} {
		w := newWorld(t, cur, "v0.2.0")
		if res, err := w.run(false); err != nil || !res.Updated {
			t.Fatalf("%s : %+v %v", cur, res, err)
		}
	}
}

func TestCheckDownloadsNothingAndChangesNothing(t *testing.T) {
	w := newWorld(t, "v0.1.0", "v0.2.0")
	res, err := w.run(true)
	if err != nil || res.Updated || res.Latest != "v0.2.0" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(w.calls) != 1 || w.target() != "ancien" {
		t.Fatalf("--check a touché quelque chose : %v", w.calls)
	}
}

func TestRejectsOddTagsAndPlatforms(t *testing.T) {
	// une étiquette inattendue ne doit jamais finir dans une URL de téléchargement
	for _, tag := range []string{"v1.2.3/../../x", "latest", "v1.2", "v1.2.3-rc1", ""} {
		w := newWorld(t, "v0.1.0", "v0.2.0")
		w.files["https://api.test/repos/o/r/releases/latest"] = []byte(fmt.Sprintf(`{"tag_name":%q}`, tag))
		if _, err := w.run(false); err == nil {
			t.Fatalf("étiquette %q acceptée", tag)
		}
	}
	w := newWorld(t, "v0.1.0", "v0.2.0")
	w.d.GOARCH = "386"
	if _, err := w.run(false); err == nil {
		t.Fatal("plateforme inconnue acceptée")
	}
}

func TestFailedFetchLeavesBinary(t *testing.T) {
	w := newWorld(t, "v0.1.0", "v0.2.0")
	delete(w.files, "https://dl.test/o/r/releases/download/v0.2.0/docker-update-linux-amd64")
	if _, err := w.run(false); err == nil || w.target() != "ancien" {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPFetchRefusesPlainHTTP(t *testing.T) {
	if _, err := HTTPFetch("http://example.test/x", 10); err == nil {
		t.Fatal("http accepté")
	}
}
