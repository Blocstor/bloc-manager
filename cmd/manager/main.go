package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/blocstor/bloc-manager/internal/agent"
	"github.com/blocstor/bloc-manager/internal/api"
	"github.com/blocstor/bloc-manager/internal/store"
)

func main() {
	listen := flag.String("listen", ":9090", "address to listen on")
	dbPath := flag.String("db", "/var/lib/bloc-manager/state.db", "path to SQLite database")
	agentsPath := flag.String("agents", "agents.yaml", "path to agents config file")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	vg := flag.String("vg", "vg0", "LVM volume group name for DRBD backing storage")
	flag.Parse()

	log := newLogger(*logLevel)

	// Load agent configuration.
	agentCfg, err := agent.LoadConfig(*agentsPath)
	if err != nil {
		log.Error("load agents config", "path", *agentsPath, "err", err)
		os.Exit(1)
	}
	log.Info("loaded agent config", "agents", len(agentCfg.Agents))

	// Open SQLite store.
	if err := os.MkdirAll(dirOf(*dbPath), 0o755); err != nil {
		log.Error("create db directory", "err", err)
		os.Exit(1)
	}

	st, err := store.NewStore(*dbPath)
	if err != nil {
		log.Error("open store", "dsn", *dbPath, "err", err)
		os.Exit(1)
	}
	defer st.Close()
	log.Info("store opened", "path", *dbPath)

	// Build HTTP mux.
	mux := http.NewServeMux()
	h := api.New(st, agentCfg, log, *vg)
	h.RegisterRoutes(mux)

	srv := &http.Server{
		Addr:         *listen,
		Handler:      loggingMiddleware(log, mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Start server in a goroutine.
	go func() {
		log.Info("bloc-manager listening", "addr", *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen and serve", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown on signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("graceful shutdown", "err", err)
	}
	log.Info("stopped")
}

// newLogger builds a slog.Logger at the requested level.
func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// loggingMiddleware logs every request.
func loggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(lrw, r)
		log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (lrw *loggingResponseWriter) WriteHeader(status int) {
	lrw.status = status
	lrw.ResponseWriter.WriteHeader(status)
}

// dirOf returns the directory component of a file path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	// No separator found — current directory.
	return "."
}
