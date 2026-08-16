// Package store owns the database: the connection pool, the migration runner, and later every
// query the server makes.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenPool creates the pool the server uses for everything except migrations, and proves it works
// before returning.
func OpenPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// The parse error quotes the connection string, password included, so it must not be
		// wrapped into something that will be logged.
		return nil, errors.New("VIVE_DATABASE_URL is not a valid PostgreSQL connection string")
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("creating the connection pool: %w", err)
	}

	// pgxpool connects lazily, so a wrong password or an unreachable host would otherwise surface
	// as a 500 on whichever request happened to arrive first. Ping makes it a startup failure with
	// a message an operator can act on, which is the whole promise of the self-host story.
	pingContext, cancelPing := context.WithTimeout(ctx, 10*time.Second)
	defer cancelPing()

	if err := pool.Ping(pingContext); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}

	return pool, nil
}
