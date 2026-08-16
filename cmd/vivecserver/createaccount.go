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
// Browser setup is the normal way to create the first account. This remains as an unattended and
// recovery path, and can create another account without temporarily opening public registration.
// Shipping it in the same binary means a self-hoster needs nothing else installed:
//
//	docker compose -f deploy/docker-compose.yml run --rm -T vivecserver create-account -email you@example.com
func runCreateAccount(arguments []string) error {
	flags := flag.NewFlagSet("create-account", flag.ContinueOnError)
	emailFlag := flags.String("email", "", "email address for the new account (required)")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: vivecserver create-account -email <address>\n\n")
		fmt.Fprintf(flags.Output(), "The password is read from stdin, or from %s.\n\n", passwordEnvironmentVariable)
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	if strings.TrimSpace(*emailFlag) == "" {
		flags.Usage()
		return errors.New("-email is required")
	}

	password, err := readPassword(os.Stdin)
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

	accountID, err := store.CreateAccount(ctx, pool, email, passwordHash)
	if errors.Is(err, store.ErrEmailTaken) {
		return fmt.Errorf("an account already exists for %s", email)
	}
	if err != nil {
		return err
	}

	fmt.Printf("created account %s for %s\n", accountID, email)
	fmt.Printf("register a device with: POST /v1/devices {\"email\":%q,\"password\":\"…\",\"name\":\"…\",\"platform\":\"…\"}\n", email)
	return nil
}

// readPassword takes the password from the environment if it is set there, and otherwise from the
// first line of stdin.
func readPassword(input io.Reader) (string, error) {
	if fromEnvironment := os.Getenv(passwordEnvironmentVariable); fromEnvironment != "" {
		return fromEnvironment, nil
	}

	// No terminal echo suppression: that needs golang.org/x/term, and every documented way of
	// running this pipes stdin rather than typing into a TTY. Worth revisiting if anyone actually
	// runs it interactively.
	reader := bufio.NewReader(input)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the password from stdin: %w", err)
	}

	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", fmt.Errorf("no password supplied: pipe one into stdin or set %s", passwordEnvironmentVariable)
	}
	return password, nil
}
