// Package store owns the database: the connection pool, the migration runner, and later every
// query the server makes.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

	// An operator who wrote pool_min_conns into the URL has decided, zero included; ParseConfig
	// cannot tell an explicit zero from an absent setting, so the string is what is asked.
	if !strings.Contains(databaseURL, "pool_min_conns") {
		poolConfig.MinConns = minimumPoolConnections(poolConfig.MaxConns)
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

// minimumPoolConnections keeps a connection or two open through quiet periods.
//
// pgxpool's default minimum is zero, and it closes a connection idle for 30 minutes. A personal
// server is quiet most of the time — the change stream holds no connection while it waits — so the
// first sync after a pause would otherwise pay a TCP connect and a SCRAM authentication before it
// could ask its question. A share of the maximum, capped, because each one is an idle backend held
// against PostgreSQL's own connection limit.
func minimumPoolConnections(maximumConnections int32) int32 {
	return max(1, min(4, maximumConnections/8))
}
