// Package dockerx exécute les commandes docker et regctl : toujours une liste d'arguments, jamais de shell.
package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Result est le résultat d'une commande. RC vaut 124 si le délai est dépassé, 127 si la commande est introuvable.
type Result struct {
	RC  int
	Out string
	Err string
}

// Runner lance des commandes. En simulation, les commandes « modifiantes » ne sont que journalisées.
type Runner interface {
	// Run lance la commande ; la sortie est du texte (fins de ligne normalisées en \n).
	Run(argv []string, mutating bool, timeout time.Duration) Result
	// RunBytes garde la sortie en octets : indispensable pour hacher un manifeste.
	RunBytes(argv []string, timeout time.Duration) (rc int, out []byte, errText string)
	// DryRun dit si l'on est en simulation.
	DryRun() bool
}

// Exec est le Runner réel.
type Exec struct {
	Dry bool
	Log func(string)
}

// DefaultRunTimeout et DefaultBytesTimeout sont les délais habituels de Run et RunBytes.
const (
	DefaultRunTimeout   = 900 * time.Second
	DefaultBytesTimeout = 60 * time.Second
)

// DryRun dit si l'on est en simulation.
func (e *Exec) DryRun() bool { return e.Dry }

// Run lance la commande sans shell.
func (e *Exec) Run(argv []string, mutating bool, timeout time.Duration) Result {
	if mutating && e.Dry {
		e.Log("[simulation] " + JoinArgs(argv))
		return Result{}
	}
	rc, out, errText := execute(argv, timeout)
	return Result{RC: rc, Out: text(out), Err: text(errText)}
}

// RunBytes lance la commande sans shell et garde la sortie standard en octets.
func (e *Exec) RunBytes(argv []string, timeout time.Duration) (int, []byte, string) {
	rc, out, errText := execute(argv, timeout)
	return rc, out, errText
}

// text décode une sortie comme subprocess en mode texte : fins de ligne universelles, octets invalides remplacés.
func text[T string | []byte](b T) string {
	s := strings.ToValidUTF8(string(b), "\xef\xbf\xbd")
	return universalNewlines(s)
}

func universalNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// execute lance argv ; l'entrée standard est vide (jamais celle de l'outil : elle peut porter un secret).
func execute(argv []string, timeout time.Duration) (int, []byte, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second // un petit-enfant qui garde les tubes ouverts ne bloque pas
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return 124, nil, fmt.Sprintf("délai dépassé (%d s)", int(timeout/time.Second))
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			rc := exitErr.ExitCode()
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				rc = -int(ws.Signal())
			}
			return rc, stdout.Bytes(), stderr.String()
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return 127, nil, "commande introuvable : " + argv[0]
		}
		return 126, nil, fmt.Sprintf("commande impossible à lancer : %s", argv[0])
	}
	return 0, stdout.Bytes(), stderr.String()
}

// Quote reproduit shlex.quote de Python : le texte est protégé par des apostrophes s'il contient autre chose que
// des caractères sûrs (lettres, chiffres, _ @ % + = : , . / -).
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune("@%+=:,./-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// JoinArgs reproduit shlex.join de Python.
func JoinArgs(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = Quote(a)
	}
	return strings.Join(parts, " ")
}
