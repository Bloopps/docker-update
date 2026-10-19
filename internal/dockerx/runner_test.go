package dockerx

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Le binaire de test sert de commande factice : `HELPER=… go test` relance ce même programme.
func TestMain(m *testing.M) {
	switch os.Getenv("DOCKERX_HELPER") {
	case "":
		os.Exit(m.Run())
	case "ok":
		os.Stdout.WriteString("ok\n")
	case "exit3":
		os.Exit(3)
	case "sleep":
		time.Sleep(5 * time.Second)
	case "bytes":
		os.Stdout.WriteString("{\r\n}\r\n\x00")
	case "stderr":
		os.Stderr.WriteString("boom\r\n")
		os.Exit(1)
	case "stdin":
		b := make([]byte, 10)
		n, _ := os.Stdin.Read(b)
		os.Stdout.WriteString(string(b[:n]) + "|fin")
	case "args":
		os.Stdout.WriteString(strings.Join(os.Args[1:], "\x1f"))
	case "kill":
		p, _ := os.FindProcess(os.Getpid())
		p.Kill()
		time.Sleep(time.Second)
	}
	os.Exit(0)
}

func helper(mode string, args ...string) []string {
	return append([]string{os.Args[0], "-x"}, args...)
}

func withHelper(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("DOCKERX_HELPER", mode)
}

func TestRunner(t *testing.T) {
	r := &Exec{Log: func(string) {}}
	withHelper(t, "ok")
	if res := r.Run(helper("ok"), false, 10*time.Second); res.RC != 0 || strings.TrimSpace(res.Out) != "ok" {
		t.Errorf("ok : %+v", res)
	}
	withHelper(t, "exit3")
	if res := r.Run(helper("exit3"), false, 10*time.Second); res.RC != 3 {
		t.Errorf("code de retour : %+v", res)
	}
	if res := r.Run([]string{"/nope/nope"}, false, 10*time.Second); res.RC != 127 || !strings.Contains(res.Err, "commande introuvable : /nope/nope") {
		t.Errorf("introuvable : %+v", res)
	}
	if res := r.Run([]string{"commande-qui-n-existe-pas"}, false, 10*time.Second); res.RC != 127 {
		t.Errorf("introuvable (PATH) : %+v", res)
	}
	withHelper(t, "sleep")
	start := time.Now()
	res := r.Run(helper("sleep"), false, time.Second)
	if res.RC != 124 || !strings.Contains(res.Err, "délai dépassé (1 s)") || time.Since(start) > 4*time.Second {
		t.Errorf("délai : %+v après %v", res, time.Since(start))
	}
	withHelper(t, "kill")
	if res := r.Run(helper("kill"), false, 10*time.Second); res.RC != -9 {
		t.Errorf("tué par un signal : %+v", res)
	}
	dir := t.TempDir()
	noexec := filepath.Join(dir, "x")
	os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644)
	if res := r.Run([]string{noexec}, false, 10*time.Second); res.RC != 126 {
		t.Errorf("non exécutable : %+v", res)
	}
}

func TestRunnerTextUsesUniversalNewlines(t *testing.T) {
	r := &Exec{Log: func(string) {}}
	withHelper(t, "stderr")
	res := r.Run(helper("stderr"), false, 10*time.Second)
	if res.RC != 1 || res.Err != "boom\n" {
		t.Errorf("%+v", res)
	}
}

func TestRunnerBytesKeepsExactBytes(t *testing.T) {
	r := &Exec{Log: func(string) {}}
	withHelper(t, "bytes")
	rc, out, _ := r.RunBytes(helper("bytes"), 10*time.Second)
	if rc != 0 || string(out) != "{\r\n}\r\n\x00" {
		t.Errorf("aucune traduction de fin de ligne : %d %q", rc, out)
	}
	if rc, _, _ := r.RunBytes([]string{"/nope/nope"}, time.Second); rc != 127 {
		t.Errorf("introuvable : %d", rc)
	}
	withHelper(t, "sleep")
	if rc, _, _ := r.RunBytes(helper("sleep"), time.Second); rc != 124 {
		t.Errorf("délai : %d", rc)
	}
}

func TestRunnerNeverReadsTheToolsStdin(t *testing.T) {
	r := &Exec{Log: func(string) {}}
	withHelper(t, "stdin")
	// L'entrée standard du processus de test n'est pas transmise (elle peut porter un secret).
	old := os.Stdin
	rd, wr, _ := os.Pipe()
	wr.WriteString("SECRET")
	wr.Close()
	os.Stdin = rd
	defer func() { os.Stdin = old }()
	if res := r.Run(helper("stdin"), false, 10*time.Second); res.Out != "|fin" {
		t.Errorf("sortie : %q", res.Out)
	}
}

func TestRunnerDryRunOnlyPrints(t *testing.T) {
	var out []string
	r := &Exec{Dry: true, Log: func(s string) { out = append(out, s) }}
	res := r.Run([]string{"/definitely/not/a/command", "x y", "it's", ""}, true, time.Second)
	if res.RC != 0 || len(out) != 1 {
		t.Fatalf("%+v %v", res, out)
	}
	if want := `[simulation] /definitely/not/a/command 'x y' 'it'"'"'s' ''`; out[0] != want {
		t.Errorf("%q ; attendu %q", out[0], want)
	}
	if !r.DryRun() {
		t.Error("DryRun")
	}
	// Une commande qui ne modifie rien est lancée même en simulation.
	withHelper(t, "ok")
	if res := r.Run(helper("ok"), false, 10*time.Second); res.RC != 0 || strings.TrimSpace(res.Out) != "ok" {
		t.Errorf("lecture en simulation : %+v", res)
	}
}

func TestQuoteLikeShlex(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "a b": "'a b'", "": "''", "é-ok": "é-ok", "a=b:c,d.e/f-g@h%i+j": "a=b:c,d.e/f-g@h%i+j",
		"a;b": "'a;b'", "$HOME": "'$HOME'", "x'y": `'x'"'"'y'`, "a\nb": "'a\nb'"} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s ; attendu %s", in, got, want)
		}
	}
	if got := JoinArgs([]string{"docker", "compose", "-f", "/a b/c.yml"}); got != "docker compose -f '/a b/c.yml'" {
		t.Errorf("JoinArgs : %s", got)
	}
}

// Aucun appel à un shell : les commandes sont toujours des listes d'arguments. Le seul endroit qui lance un processus
// est dockerx/runner.go ; aucune source ne nomme un shell.
func TestNoShellAnywhere(t *testing.T) {
	root := filepath.Join("..", "..")
	var callers []string
	shells := regexp.MustCompile(`"(?:/bin/|/usr/bin/)?(?:sh|bash|zsh|dash)"|"-c"`)
	err := filepathWalk(root, func(path string, data []byte) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		if strings.Contains(string(data), "exec.Command") {
			callers = append(callers, filepath.ToSlash(path))
		}
		if shells.Match(data) {
			t.Errorf("%s nomme un shell", path)
		}
		if strings.Contains(string(data), `"syscall".Exec`) {
			t.Errorf("%s : syscall.Exec", path)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// seconde exception : `edit` lance l'éditeur de l'utilisateur sur son terminal (liste d'arguments, jamais de shell)
	if len(callers) != 2 || !strings.HasSuffix(callers[0], "internal/cli/editcmd.go") || !strings.HasSuffix(callers[1], "internal/dockerx/runner.go") {
		t.Errorf("exec.Command ne doit apparaître que dans dockerx/runner.go et cli/editcmd.go : %v", callers)
	}
	_ = exec.ErrNotFound
}

func filepathWalk(root string, fn func(path string, data []byte)) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fn(path, data)
		}
		return nil
	})
}
