package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// isTerminal dit si le lecteur est un terminal.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	var t syscall.Termios
	return ioctl(int(f.Fd()), syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

// readHidden lit une ligne au terminal sans afficher la saisie (équivalent de getpass). L'écho est rétabli à la sortie
// et sur une interruption (Ctrl-C) : un terminal ne doit jamais rester muet.
func readHidden(f *os.File, out io.Writer, prompt string) (string, error) {
	fd := int(f.Fd())
	var old syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return "", errors.New("pas de terminal")
	}
	silent := old
	silent.Lflag &^= syscall.ECHO
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&silent)); err != nil {
		return "", errors.New("saisie masquée impossible")
	}
	restore := func() { _ = ioctl(fd, syscall.TCSETS, unsafe.Pointer(&old)) } // au mieux : le terminal peut avoir disparu
	defer restore()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		if s, ok := <-sig; ok {
			restore()
			fmt.Fprintln(out)
			code := 130
			if s == syscall.SIGTERM {
				code = 143
			}
			os.Exit(code)
		}
	}()

	fmt.Fprint(out, prompt)
	var line []byte
	one := make([]byte, 1)
	for {
		n, err := f.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			line = append(line, one[0])
		}
		if err != nil {
			break
		}
	}
	fmt.Fprintln(out)
	return string(line), nil
}
