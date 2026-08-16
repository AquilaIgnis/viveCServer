package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// ApplyMigrations brings the database up to the schema embedded in the binary.
//
// goose runs as a library here rather than as the CLI it also ships as, so that a fresh container
// migrates itself on startup and "single binary" (H1) stays literally true — nothing to install
// beside the server, and no init container that can be skipped. The same migration files are still
// readable by the CLI for the things a CLI is better at: `goose status`, `goose create -s`, and
// stepping back down during development.
//
// Migrations run on a connection of their own rather than through the pool. goose wants a
// `*sql.DB`, the pool is a `*pgxpool.Pool`, and the two want opposite things — the session lock
// below has to be held by one connection for the whole run, which is exactly what a pool exists to
// prevent.
func ApplyMigrations(ctx context.Context, databaseURL string, migrationFiles fs.FS, logger *slog.Logger) error {
	connectionConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		// The parse error quotes the connection string, password included, so it is not wrapped.
		return errors.New("VIVE_DATABASE_URL is not a valid PostgreSQL connection string")
	}

	migrationDatabase := stdlib.OpenDB(*connectionConfig)
	defer migrationDatabase.Close()

	// One connection, so the advisory lock and the statements it protects are guaranteed to share
	// a session. With a larger limit the lock could be taken on one connection and the migrations
	// run on another, which looks like it works right up until two servers start together.
	migrationDatabase.SetMaxOpenConns(1)

	// Two instances starting at the same moment — a rolling restart, or a compose file that scales
	// the service — would otherwise both see the same migration unapplied and both apply it.
	sessionLocker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("building the migration lock: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		migrationDatabase,
		migrationFiles,
		goose.WithSessionLocker(sessionLocker),
	)
	if err != nil {
		return fmt.Errorf("preparing migrations: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}

	for _, r := range results {
		logger.Info("applied migration",
			"version", r.Source.Version,
			"path", r.Source.Path,
			"duration_ms", r.Duration.Milliseconds(),
			"empty", r.Empty,
		)
	}
	if len(results) == 0 {
		logger.Info("database schema is up to date")
	}

	return nil
}
