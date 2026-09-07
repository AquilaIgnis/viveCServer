package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

// passwordEnvironmentVariable is the non-interactive way to supply a password.
//
// Stdin is the better one and is what the help text recommends: an environment variable is visible
// in `docker inspect`, in a compose file somebody commits, and in /proc on the host.
const passwordEnvironmentVariable = "VIVE_ACCOUNT_PASSWORD"

// runCreateAccount creates an account from the command line.
//
// Browser setup is the normal way to create the account. This remains as an unattended fallback;
// like browser setup, it refuses to create a second account. Shipping it in the same binary means
// a self-hoster needs nothing else installed:
//
//	docker compose -f deploy/docker-compose.yml run --rm -T vivecserver create-account -email you@example.com
func runCreateAccount(arguments []string) error {
	flags := flag.NewFlagSet("create-account", flag.ContinueOnError)
	emailFlag := flags.String("email", "", "email address for the new account (required)")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: vivecserver create-account -email <address>\n\n")
		fmt.Fprintf(flags.Output(), "On a terminal the password is prompted for, twice and hidden.\n")
		fmt.Fprintf(flags.Output(), "Otherwise it is read from stdin, or from %s.\n\n", passwordEnvironmentVariable)
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	if strings.TrimSpace(*emailFlag) == "" {
		flags.Usage()
		return errors.New("-email is required")
	}

	password, err := readPassword("New password for " + *emailFlag + ": ")
	if err != nil {
		return err
	}

	settings, err := config.Load()
	if err != nil {
		return err
	}

	// Quiet by default: this is a command whose useful output is one line on stdout, and a
	// migration log would bury it.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()

	// A first-run operator may reach for this before ever starting the server, so the schema may
	// not exist yet. Applying migrations here is idempotent and takes the same advisory lock the
	// server does, so it is safe even if the server is starting at the same moment.
	if err := store.ApplyMigrations(ctx, settings.DatabaseURL, migrations.Files, logger); err != nil {
		return err
	}

	pool, err := store.OpenPool(ctx, settings.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The same two rules the HTTP endpoints apply, from the same place, so an account made here is
	// indistinguishable from one made over the wire.
	email, err := auth.NormaliseEmail(*emailFlag)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}

	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hashing the password: %w", err)
	}

	accountID, err := store.CreateInitialAccount(ctx, pool, email, passwordHash)
	if errors.Is(err, store.ErrEmailTaken) || errors.Is(err, store.ErrSetupAlreadyComplete) {
		return errors.New("this community server already has its account")
	}
	if err != nil {
		return err
	}

	fmt.Printf("created account %s for %s\n", accountID, email)
	fmt.Printf("register a device with: POST /v1/devices {\"email\":%q,\"password\":\"…\",\"name\":\"…\",\"platform\":\"…\"}\n", email)
	return nil
}

// readPassword takes the password from the environment if it is set there, from a hidden prompt
// when someone is sitting at a terminal, and otherwise from the first line of stdin.
//
// Three sources rather than one because the two ways of running this are genuinely different. An
// unattended run pipes a password in and must never block on a prompt nobody is there to answer; a
// person recovering a locked-out server types one, and typing it onto a visible line puts it in the
// scrollback, in the terminal's own buffer, and often in a shell history file. Which of the two is
// happening is not a flag to be passed and got wrong -- it is whether stdin is a terminal.
func readPassword(prompt string) (string, error) {
	if fromEnvironment := os.Getenv(passwordEnvironmentVariable); fromEnvironment != "" {
		return fromEnvironment, nil
	}

	if term.IsTerminal(int(os.Stdin.Fd())) {
		return promptForPassword(prompt)
	}
	return readPasswordLine(os.Stdin)
}

// promptForPassword asks twice with the echo off, and refuses two answers that disagree.
//
// Confirming is not ceremony here. The typing is invisible, and both commands this serves end in a
// credential nothing else can check for you: get it wrong in `create-account` and the account you
// just made cannot be signed into, get it wrong in `set-password` and you have replaced a password
// you forgot with one you never knew. The setup wizard asks twice for the same reason.
//
// Prompts go to stderr so that `set-password ... > somewhere` still shows them, and so the line the
// command actually reports stays the only thing on stdout.
func promptForPassword(prompt string) (string, error) {
	first, err := readHiddenLine(prompt)
	if err != nil {
		return "", err
	}
	if first == "" {
		return "", errors.New("no password entered")
	}

	second, err := readHiddenLine("Confirm password: ")
	if err != nil {
		return "", err
	}
	if first != second {
		return "", errors.New("the two entries do not match")
	}
	return first, nil
}

// readHiddenLine writes one prompt and reads one line without echoing it.
//
// The trailing newline is written by hand: the terminal did not echo the one that was typed, so
// without this every prompt after the first would start on the line the password was not shown on.
func readHiddenLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	typed, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading the password from the terminal: %w", err)
	}
	return string(typed), nil
}

// readPasswordLine takes the first line of a pipe, which is the unattended path.
func readPasswordLine(input io.Reader) (string, error) {
	reader := bufio.NewReader(input)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the password from stdin: %w", err)
	}

	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", fmt.Errorf("no password supplied: pipe one into stdin, run this on a terminal to be prompted, or set %s", passwordEnvironmentVariable)
	}
	return password, nil
}
