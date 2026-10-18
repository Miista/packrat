// Command packrat automates topping up a MyAnonamouse account's number of
// unsatisfied/downloading torrents to a target (the account's rank-based
// limit minus a configurable reserve), searching MAM and adding matching
// torrents to a configured download client. Behind an authy sign-in gate
// (OIDC, or a first-launch local account) and env-var-overridable settings.
package main

import (
	"context"
	"net/http"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/miista/authy"
	"github.com/miista/packrat/internal/api"
	"github.com/miista/packrat/internal/scheduler"
	"github.com/miista/packrat/internal/store"
)

func main() {
	log := newLogger()

	dataDir := os.Getenv("PACKRAT_DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	const addr = ":8766"
	staticDir := os.Getenv("PACKRAT_STATIC_DIR")
	if staticDir == "" {
		staticDir = "web/static"
	}

	st, err := store.Open(dataDir)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open state store")
	}

	// Sign-in is owned by authy: OIDC when the PACKRAT_OIDC_* variables are
	// set, otherwise a local account the first visitor creates.
	// PACKRAT_AUTH_DISABLED=true turns the gate off (gate is nil and Wrap
	// leaves the handler unchanged). Misconfigured: refuse to start rather
	// than serve the dashboard unprotected.
	gate, err := authy.FromEnv(context.Background(), "packrat")
	if err != nil {
		log.Fatal().Err(err).Msg("invalid auth configuration")
	}
	if gate == nil {
		log.Warn().Msg("login is OFF (PACKRAT_AUTH_DISABLED): everyone who can reach this port controls your downloads")
	}

	// nil factories mean the real MAM and download clients; tests inject fakes.
	sched := scheduler.New(st, log, nil, nil)
	sched.Start()

	log.Info().Str("addr", addr).Str("data_dir", dataDir).Str("static_dir", staticDir).Msg("starting packrat")

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           newHandler(st, sched, gate, staticDir, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("server stopped")
	}
}

// newHandler is the whole HTTP surface: the API and the dashboard, behind the
// sign-in gate. A nil gate (PACKRAT_AUTH_DISABLED) leaves it unprotected.
func newHandler(st *store.Store, sched *scheduler.Scheduler, gate *authy.Gate, staticDir string, log zerolog.Logger) http.Handler {
	mux := http.NewServeMux()
	api.New(st, sched, log).Routes(mux)
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))
	return gate.Wrap(mux)
}

// newLogger builds the process-wide zerolog.Logger using ConsoleWriter —
// the same colored, human-readable format used elsewhere in this stack
// (tagbrr, reaparr, diun: "TIME | LEVEL | message key=value ..."), instead
// of raw JSON lines. Level is configurable via LOG_LEVEL.
func newLogger() zerolog.Logger {
	level := zerolog.InfoLevel
	// zerolog.ParseLevel("") returns (zerolog.NoLevel, nil) — no error —
	// so checking err == nil alone silently replaced the InfoLevel default
	// with NoLevel whenever LOG_LEVEL was unset (the common case), which
	// suppressed all leveled log output (.Info()/.Warn()/etc.) with no
	// error or other symptom — confirmed live, 2026-10-01: the running
	// container produced zero log lines despite serving requests
	// correctly. Only apply a parsed level when LOG_LEVEL was actually set
	// to something.
	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		if lvl, err := zerolog.ParseLevel(raw); err == nil {
			level = lvl
		}
	}
	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
		// Colors forced on: docker logs / Dozzle render ANSI fine,
		// matching the rest of the stack.
	}
	return zerolog.New(writer).
		Level(level).
		With().
		Timestamp().
		Logger()
}
