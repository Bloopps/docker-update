package cli

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestColorizeMarksOnlyTheStartOfLine(t *testing.T) {
	for line, color := range map[string]string{
		"✓ Docker — démon joignable":             ansiGreen,
		"⚠ Binaire — introuvable":                ansiYellow,
		"✗ Docker — absent":                      ansiRed,
		"ERREUR : dépôt introuvable":             ansiRed,
		"AVERTISSEMENT : webhook absent":         ansiYellow,
		"    → correction : sudo install -m 755": ansiCyan,
		"  ✓ indenté":                            ansiGreen,
	} {
		got := colorize(line)
		if !strings.Contains(got, color) || !strings.HasSuffix(got, ansiReset+got[strings.Index(got, ansiReset)+len(ansiReset):]) {
			t.Errorf("%q -> %q", line, got)
		}
		if ansiRe.ReplaceAllString(got, "") != line {
			t.Errorf("le texte a changé : %q -> %q", line, got)
		}
	}
	if got := colorize("une ligne ordinaire"); got != "une ligne ordinaire" {
		t.Errorf("ligne ordinaire colorée : %q", got)
	}
}

func TestNoColorOutsideTerminalOrWithNoColor(t *testing.T) {
	r := newRig(t, "")
	r.run("validate")
	if strings.Contains(r.out.String(), "\x1b[") {
		t.Fatalf("couleurs hors terminal : %q", r.out.String())
	}
	if useColor(r.out, func(string) string { return "" }) {
		t.Fatal("un tampon n'est pas un terminal")
	}
	r2 := newRig(t, "")
	r2.env.Color = true
	r2.run("validate")
	if !strings.Contains(r2.out.String(), "\x1b[") {
		t.Fatalf("pas de couleur alors que forcée : %q", r2.out.String())
	}
}
