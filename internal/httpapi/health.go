package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// readinessProbeTimeout bounds the database check. Long enough that a busy server is not called
// dead, short enough that a probe never outlives the interval it is polled on.
const readinessProbeTimeout = 2 * time.Second

// handleLiveness answers whether the process is running, and deliberately checks nothing else.
//
// Liveness and readiness are separate endpoints because they answer different questions, and one
// endpoint answering both does real harm. A container runtime restarts what fails its liveness
// probe, so a liveness check that pings the database turns a brief database hiccup into a restart
// loop across every instance at once — precisely when the database can least afford a stampede of
// reconnections. This endpoint therefore cannot fail while the process can serve it, which is
// exactly what makes it a useful liveness signal.
func handleLiveness() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// handleReadiness answers whether this instance can serve a real request right now, which means the
// database has to answer. Load balancers and compose's `depends_on` belong here, not on /healthz.
func handleReadiness(pool *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		probeContext, cancelProbe := context.WithTimeout(r.Context(), readinessProbeTimeout)
		defer cancelProbe()

		if err := pool.Ping(probeContext); err != nil {
			logger.Warn("readiness probe failed", "error", err)
			writeJSON(w, logger, http.StatusServiceUnavailable, map[string]string{
				"status": "unavailable",
				"reason": "database",
			})
			return
		}

		writeJSON(w, logger, http.StatusOK, map[string]string{"status": "ok"})
	}
}
