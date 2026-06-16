// Command hookd is the Teamster hook event receiver.
// It accepts POST /event from hook clients, writes records to JSONL.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bmjdotnet/teamster/internal/config"
	"github.com/bmjdotnet/teamster/internal/logging"
	"github.com/bmjdotnet/teamster/internal/server"
	"github.com/bmjdotnet/teamster/internal/version"
)

func main() {
	var readOnly bool
	for _, arg := range os.Args[1:] {
		switch arg {
		case "version", "--version", "-v":
			fmt.Printf("hookd %s\n", version.String())
			os.Exit(0)
		case "--read-only":
			readOnly = true
		}
	}

	cfg, err := config.Load()
	if err == nil && readOnly {
		cfg.ReadOnly = true
	}
	if err != nil {
		slog.Error("config load failed", "component", "hookd", "error", err)
		os.Exit(1)
	}

	logger := logging.Init("hookd")

	srv, err := server.NewServer(cfg)
	if err != nil {
		logger.Error("init failed", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	addr := fmt.Sprintf("%s:%d", cfg.HookServerBind, cfg.HookServerPort)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           server.SecurityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("listening", "addr", addr, "log", cfg.LogFile, "version", version.Version, "commit", version.Commit, "read_only", cfg.ReadOnly)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("listen error", "error", err)
			stop <- syscall.SIGTERM
		}
	}()

	<-stop
	logger.Info("shutting down")
	httpSrv.Shutdown(context.Background()) //nolint:errcheck
	srv.Close()                            //nolint:errcheck
}
