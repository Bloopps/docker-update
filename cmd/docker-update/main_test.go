package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func capture(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	errf, err := os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	defer errf.Close()
	code := run(args, out, errf)
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errf.Name())
	return code, string(o), string(e)
}

func TestVersion(t *testing.T) {
	code, out, _ := capture(t, "version")
	if code != 0 {
		t.Fatalf("code = %d, attendu 0", code)
	}
	for _, want := range []string{"docker-update", runtime.GOOS + "/" + runtime.GOARCH, runtime.Version()} {
		if !strings.Contains(out, want) {
			t.Errorf("sortie %q sans %q", out, want)
		}
	}
}

func TestUnknownCommandIsAConfigurationError(t *testing.T) {
	if code, _, _ := capture(t, "inconnue"); code != 2 {
		t.Fatalf("code = %d, attendu 2", code)
	}
}
