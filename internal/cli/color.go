package cli

import (
	"io"
	"os"
	"regexp"
	"strings"
)

const (
	ansiReset  = "\x1b[0m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"
	ansiCyan   = "\x1b[36m"
	ansiBold   = "\x1b[1m"
)

// useColor : couleurs seulement sur un terminal, jamais si NO_COLOR est défini (no-color.org) ou si TERM=dumb.
func useColor(out io.Writer, getenv func(string) string) bool {
	f, ok := out.(*os.File)
	return ok && isTerminal(f) && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

var colorRules = []struct {
	re    *regexp.Regexp
	color string
}{
	{regexp.MustCompile(`^(\s*)(✓)`), ansiGreen},
	{regexp.MustCompile(`^(\s*)(⚠|AVERTISSEMENT)`), ansiYellow},
	{regexp.MustCompile(`^(\s*)(✗|ERREUR)`), ansiRed},
	{regexp.MustCompile(`^(\s*)(→ correction)`), ansiCyan},
	{regexp.MustCompile(`^(\s*)(\[simulation\])`), ansiCyan},
}

// colorize colore le début d'une ligne de compte rendu (marque ✓ ⚠ ✗, ERREUR, AVERTISSEMENT, correction proposée).
// Le texte de la ligne n'est jamais modifié : seules des séquences ANSI sont ajoutées.
func colorize(line string) string {
	for _, r := range colorRules {
		if m := r.re.FindStringSubmatchIndex(line); m != nil {
			return line[:m[4]] + r.color + line[m[4]:m[5]] + ansiReset + line[m[5]:]
		}
	}
	if strings.HasPrefix(line, "Vérification de l'installation") || strings.HasPrefix(line, "À installer") {
		return ansiBold + line + ansiReset
	}
	return line
}
