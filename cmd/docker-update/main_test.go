package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/Bloopps/docker-update/internal/cli"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := cli.Main([]string{"version"}, &cli.Env{Stdout: &out, Stderr: &errb, Version: versionString()})
	if code != 0 {
		t.Fatalf("code = %d, attendu 0 (%s)", code, errb.String())
	}
	for _, want := range []string{"docker-update", runtime.GOOS + "/" + runtime.GOARCH, runtime.Version()} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("sortie %q sans %q", out.String(), want)
		}
	}
}
