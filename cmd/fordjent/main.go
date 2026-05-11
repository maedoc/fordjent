// Fordjent — a Forgejo-driven agent harness.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/session"
	"github.com/fordjent/fordjent/internal/webhook"
)

func main() {
	configPath := flag.String("config", "fordjent.yaml", "path to config file")
	cleanFlag := flag.Bool("clean", false, "clear all persistent sessions on startup")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
		slog.SetDefault(log)
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus := event.NewBus()
	mgr, err := session.NewManager(cfg, bus)
	if err != nil {
		slog.Error("failed to create session manager", "error", err)
		os.Exit(1)
	}

	if *cleanFlag {
		if err := mgr.CleanSessions(ctx); err != nil {
			slog.Warn("failed to clean sessions", "error", err)
		} else {
			slog.Info("cleaned all persistent sessions")
		}
	}

	// Forgejo webhook router (always started)
	router := webhook.NewRouter(cfg, bus, logger)
	router.SetLifecycle(mgr.Lifecycle())
	router.SetForgejoClient(mgr.ForgejoClient())

	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		slog.Info("starting webhook server", "addr", addr)
		if err := router.ListenAndServe(ctx, addr); err != nil {
			slog.Error("webhook server error", "error", err)
			cancel()
		}
	}()

	// Session manager
	go mgr.Run(ctx)

	slog.Info("fordjent agent harness started",
		"forgejo_url", cfg.Forgejo.URL,
		"provider", cfg.DefaultProvider().Name,
		"model", cfg.DefaultProvider().Model,
	)

	<-ctx.Done()
	slog.Info("shutting down, draining sessions")
	router.SetShutdown()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer drainCancel()
	mgr.Drain(drainCtx)
	slog.Info("shutdown complete")
}
