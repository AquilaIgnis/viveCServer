// Command vivecserver is the viveNotes sync server.
//
// It stores, orders and authorises; it never interprets a note. Page documents are opaque payloads
// here, which is what lets this be a Go server at all without a second copy of the app's document
// model living in it. See memory/syncPlan.md §2.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/httpapi"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

func main() {
	// One subcommand, dispatched by hand rather than through a CLI framework. `create-account`
	// exists because the default signup mode is closed, so there has to be some way to make the
	// first account; anything more elaborate than this would be machinery for a single verb.
	if len(os.Args) > 1 && os.Args[1] == "create-account" {
		if err := runCreateAccount(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "create-account:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// The logger belongs to run, which may have failed before building one, so this last-resort
		// line goes through the default logger to stderr.
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// run holds the real body of main so that every failure path returns an error rather than calling
// os.Exit, which would skip the deferred pool and server shutdowns.
func run() error {
	settings, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: settings.LogLevel}))

	// Signals cancel this context, which then unwinds the whole startup sequence — including a
	// migration that is still waiting on the advisory lock a slower instance is holding.
	ctx, stopListeningForSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopListeningForSignals()

	// Migrations run before the pool opens, so no request can arrive against a half-known schema.
	if err := store.ApplyMigrations(ctx, settings.DatabaseURL, migrations.Files, logger); err != nil {
		return err
	}

	pool, err := store.OpenPool(ctx, settings.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	httpServer := &http.Server{
		Addr: settings.ListenAddress,
		Handler: httpapi.NewHandler(pool, logger, httpapi.Options{
			SignupMode: settings.SignupMode,
		}),

		// A server with no timeouts leaks a goroutine and a file descriptor per client that opens a
		// connection and then says nothing, which is the cheapest denial of service there is.
		//
		// ReadTimeout and WriteTimeout cover the whole body, so they are the two that will need
		// raising when S4 starts pushing large ink batches over a slow connection. Revisit them
		// there against a measured worst case rather than guessing now.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveFailed := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", settings.ListenAddress)
		serveFailed <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveFailed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving: %w", err)

	case <-ctx.Done():
		logger.Info("shutting down", "timeout", settings.ShutdownTimeout.String())

		// Deliberately not derived from ctx: that context is already cancelled, and a shutdown
		// context inheriting the cancellation would abandon in-flight requests instantly rather
		// than giving them the grace period this setting exists to provide.
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.ShutdownTimeout)
		defer cancelShutdown()

		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shutting down: %w", err)
		}
		logger.Info("stopped")
		return nil
	}
}
