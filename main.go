// Command packrat automates topping up a MyAnonamouse account's number of
// unsatisfied/downloading torrents to a target (the account's rank-based
// limit minus a configurable reserve), searching MAM and adding matching
// torrents to a configured download client. Behind a first-launch admin
// login and env-var-overridable settings.
package main

import (
	"net/http"
	"os"
	"time"

	_ "time/tzdata"

	"github.com/rs/zerolog"

	"github.com/miista/packrat/internal/api"
	"github.com/miista/packrat/internal/auth"
	"github.com/miista/packrat/internal/scheduler"
	"github.com/miista/packrat/internal/store"
)

func main() {
	log := newLogger()

	dataDir := os.Getenv("PACKRAT_DATA_DIR")
	if dataDir == "" {
		dataDir = "/app/data"
	}
	addr := os.Getenv("PACKRAT_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8766"
	}
	staticDir := os.Getenv("PACKRAT_STATIC_DIR")
	if staticDir == "" {
		staticDir = "web/static"
	}

	st, err := store.Open(dataDir)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open state store")
	}

	authManager, err := auth.NewManager(st)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize auth")
	}

	sched := scheduler.New(st, log)
	sched.Start()

	server := api.New(st, authManager, sched, log)

	mux := http.NewServeMux()
	server.Routes(mux)
	mux.Handle("/", http.FileServer(http.Dir(staticDir)))

	log.Info().Str("addr", addr).Str("data_dir", dataDir).Str("static_dir", staticDir).Msg("starting packrat")

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatal().Err(err).Msg("server stopped")
	}
}

func newLogger() zerolog.Logger {
	level := zerolog.InfoLevel
	if lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil {
		level = lvl
	}
	return zerolog.New(os.Stdout).
		Level(level).
		With().
		Timestamp().
		Logger()
}
