package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AquilaIgnis/viveCServer/internal/auth"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

// runSetEmail replaces an existing account's sign-in address from the command line.
//
// The account ID does not change, so the password, registered devices, browser sessions and synced
// data all remain attached to the account. Only the email used for future sign-ins changes:
//
//	docker compose exec vivecserver /usr/local/bin/vivecserver set-email -email new@example.com
func runSetEmail(arguments []string) error {
	flags := flag.NewFlagSet("set-email", flag.ContinueOnError)
	newEmailFlag := flags.String("email", "", "new email address for the admin account (required)")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "usage: vivecserver set-email -email <new-address>\n\n")
		fmt.Fprintf(flags.Output(), "Changes the sole admin account's sign-in email without requiring the old address.\n")
		fmt.Fprintf(flags.Output(), "Its password, devices, browser sessions, and synced data are unaffected.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	if strings.TrimSpace(*newEmailFlag) == "" {
		flags.Usage()
		return errors.New("-email is required")
	}

	newEmail, err := auth.NormaliseEmail(*newEmailFlag)
	if err != nil {
		return err
	}

	settings, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()
	if err := store.ApplyMigrations(ctx, settings.DatabaseURL, migrations.Files, logger); err != nil {
		return err
	}

	pool, err := store.OpenPool(ctx, settings.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := setAdminEmail(ctx, pool, newEmail); err != nil {
		return err
	}

	fmt.Printf("email changed to %s\n", newEmail)
	return nil
}

// setAdminEmail contains the database-facing part of the command so its guarantees can be tested
// without coupling those tests to process flags and environment variables.
func setAdminEmail(ctx context.Context, pool *pgxpool.Pool, newEmail string) error {
	err := store.UpdateAdminEmail(ctx, pool, newEmail)
	if errors.Is(err, store.ErrAccountNotFound) {
		return errors.New("no admin account exists; complete setup first")
	}
	if errors.Is(err, store.ErrEmailTaken) {
		return fmt.Errorf("an account already exists for %s", newEmail)
	}
	return err
}
