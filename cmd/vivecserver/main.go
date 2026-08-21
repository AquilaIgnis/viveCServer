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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AquilaIgnis/viveCServer/internal/blob"
	"github.com/AquilaIgnis/viveCServer/internal/blobsweep"
	"github.com/AquilaIgnis/viveCServer/internal/config"
	"github.com/AquilaIgnis/viveCServer/internal/httpapi"
	"github.com/AquilaIgnis/viveCServer/internal/livelog"
	"github.com/AquilaIgnis/viveCServer/internal/store"
	"github.com/AquilaIgnis/viveCServer/migrations"
)

func main() {
	// The recovery and automation subcommands, dispatched by hand rather than through a CLI
	// framework. The ordinary first account comes from browser setup; these are what an operator
	// with a shell on the box has when the browser cannot help — which for `set-password` is the
	// only thing they have, since the setup wizard closes for good once an account exists.
	//
	// A map rather than a chain of ifs so that `vivecserver help` can list them, and so adding a
	// third verb is a line rather than a decision about where to put it.
	subcommands := map[string]func([]string) error{
		"create-account": runCreateAccount,
		"set-password":   runSetPassword,
	}
	if len(os.Args) > 1 {
		if run, known := subcommands[os.Args[1]]; known {
			if err := run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, os.Args[1]+":", err)
				os.Exit(1)
			}
			return
		}
		// An unrecognised first argument used to start the server and ignore it, so a typo in a
		// recovery command looked like a server that would not stop. Only flag-shaped arguments
		// reach the server, and it takes none, so anything else is a mistake worth naming.
		if !strings.HasPrefix(os.Args[1], "-") {
			fmt.Fprintf(os.Stderr, "vivecserver: unknown command %q\n", os.Args[1])
			fmt.Fprintln(os.Stderr, "commands: create-account, set-password")
			fmt.Fprintln(os.Stderr, "with no command, the server runs and is configured from the environment")
			os.Exit(2)
		}
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

	liveLogBroker := livelog.NewBroker()
	stdoutLogHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: settings.LogLevel})
	logger := slog.New(livelog.NewHandler(stdoutLogHandler, liveLogBroker))

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

	// Opened before the listeners, so a blob directory that is missing or read-only is a line at
	// boot rather than the first failed picture upload a week later.
	blobs, err := blob.OpenFileStore(settings.BlobDirectory)
	if err != nil {
		return err
	}
	logger.Info("attachment store ready",
		"directory", blobs.Root(),
		"max_attachment_bytes", settings.MaxBlobBytes,
	)

	// Its own context, cancelled with the process, so a sweep in progress stops at its next check
	// instead of holding shutdown open for the length of a pass.
	sweeper := blobsweep.NewSweeper(pool, blobs, logger, settings.BlobSweepInterval, settings.BlobRetention)
	var sweeperFinished sync.WaitGroup
	sweeperFinished.Add(1)
	go func() {
		defer sweeperFinished.Done()
		sweeper.Run(ctx)
	}()
	defer sweeperFinished.Wait()

	adminServer := newHTTPServer(settings.AdminListenAddress, httpapi.NewAdminHandler(pool, logger, liveLogBroker))
	syncServer := newHTTPServer(settings.SyncListenAddress, httpapi.NewSyncHandler(pool, logger, httpapi.Options{
		SignupMode:        settings.SignupMode,
		BatchReplayWindow: settings.BatchReplayWindow,
		Blobs:             blobs,
		BlobLimits:        httpapi.BlobLimits{MaxBlobBytes: settings.MaxBlobBytes},
	}))
	servers := []namedHTTPServer{
		{name: "admin", server: adminServer},
		{name: "sync", server: syncServer},
	}

	type serverResult struct {
		name string
		err  error
	}
	serveResults := make(chan serverResult, len(servers))
	for _, namedServer := range servers {
		go func() {
			logger.Info("listening", "surface", namedServer.name, "address", namedServer.server.Addr)
			serveResults <- serverResult{name: namedServer.name, err: namedServer.server.ListenAndServe()}
		}()
	}

	var serveError error
	select {
	case result := <-serveResults:
		if !errors.Is(result.err, http.ErrServerClosed) {
			serveError = fmt.Errorf("serving %s surface: %w", result.name, result.err)
		}
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", settings.ShutdownTimeout.String())

	// Deliberately not derived from ctx: that context is already cancelled, and a shutdown
	// context inheriting the cancellation would abandon in-flight requests instantly rather than
	// giving them the grace period this setting exists to provide.
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.ShutdownTimeout)
	defer cancelShutdown()

	var shutdownWaitGroup sync.WaitGroup
	shutdownErrors := make(chan error, len(servers))
	for _, namedServer := range servers {
		shutdownWaitGroup.Add(1)
		go func() {
			defer shutdownWaitGroup.Done()
			if err := namedServer.server.Shutdown(shutdownContext); err != nil {
				shutdownErrors <- fmt.Errorf("shutting down %s surface: %w", namedServer.name, err)
			}
		}()
	}
	shutdownWaitGroup.Wait()
	close(shutdownErrors)

	if serveError != nil {
		return serveError
	}
	for err := range shutdownErrors {
		return err
	}
	logger.Info("stopped")
	return nil
}

type namedHTTPServer struct {
	name   string
	server *http.Server
}

func newHTTPServer(listenAddress string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    listenAddress,
		Handler: handler,

		// A server with no timeouts leaks a goroutine and a file descriptor per client that opens a
		// connection and then says nothing, which is the cheapest denial of service there is.
		//
		// ReadTimeout and WriteTimeout cover the whole body, so they are the two a large request
		// strains. S4's ink batches did not need them raised — a push is capped at 4 MB — but S5's
		// attachments do: 32 MB over a slow mobile connection is minutes of legitimate transfer, and
		// 60 seconds would cut off a client that was making steady progress, every time, for ever.
		//
		// The answer was to extend the deadline on those two routes per request rather than raise it
		// for everybody (httpapi.blobTransferDeadline), so every other route keeps the cheap defence
		// against a connection that opens and then says nothing.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
