package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/arloliu/profgate/internal/auth"
)

const (
	// maxHashPasswordBytes is where bcrypt stops reading; a longer password
	// is refused rather than silently hashed as a shorter one.
	maxHashPasswordBytes = 72
	// maxHashLineBytes bounds what a piped stdin is read for: a line longer
	// than any accepted password plus its line ending.
	maxHashLineBytes = 4096
)

// runAuth dispatches the "auth" subcommands; "hash" reads a password and prints its bcrypt hash at cost 12.
// The password never appears in any output: it is read without echo from a
// terminal, and the only line printed is the hash.
func runAuth(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Help is answered here as well as before the dispatch reaches this function,
	// because the password prompt is inside the command help replaces:
	// a help argument must read no byte of stdin by either road.
	if operatorHelp(stdout, "auth", args) {
		return exitOK
	}
	if len(args) == 0 || args[0] != "hash" {
		_, _ = fmt.Fprintln(stderr, usageLine(clientVerbs()))

		return 2
	}

	password, err := readPassword(stdin, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return 2
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		return 1
	}
	_, _ = fmt.Fprintln(stdout, hash)

	return 0
}

// readPassword reads one password: without echo when stdin is the process's
// own terminal, otherwise one line from stdin with its line ending removed.
// An empty password and one longer than bcrypt reads are refused.
func readPassword(stdin io.Reader, stderr io.Writer) ([]byte, error) {
	var password []byte
	if f, ok := stdin.(*os.File); ok && f == os.Stdin && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprint(stderr, "Password: ")
		p, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			return nil, fmt.Errorf("read password: %w", err)
		}
		password = p
	} else {
		line, err := bufio.NewReader(io.LimitReader(stdin, maxHashLineBytes)).ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read password: %w", err)
		}
		password = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	}
	if len(password) == 0 {
		return nil, errors.New("password is empty")
	}
	if len(password) > maxHashPasswordBytes {
		return nil, fmt.Errorf("password is %d bytes; bcrypt reads only the first %d, so a longer one would silently match a shorter one",
			len(password), maxHashPasswordBytes)
	}

	return password, nil
}
