package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
	"k8gb-ui/internal/password"
)

func run() error {
	if len(os.Args) != 1 {
		return fmt.Errorf("password-hash accepts a password only from the terminal or stdin, never arguments")
	}
	var value []byte
	var err error
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password: ")
		value, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return fmt.Errorf("cannot read password")
		}
		fmt.Fprint(os.Stderr, "Confirm password: ")
		confirmation, readErr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if readErr != nil || !bytes.Equal(value, confirmation) {
			return fmt.Errorf("password confirmation does not match")
		}
	} else {
		value, err = io.ReadAll(io.LimitReader(os.Stdin, password.MaxBytes+3))
		if err != nil {
			return fmt.Errorf("cannot read password")
		}
		value = bytes.TrimSuffix(value, []byte("\n"))
		value = bytes.TrimSuffix(value, []byte("\r"))
	}
	hash, err := password.Hash(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, hash)
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
