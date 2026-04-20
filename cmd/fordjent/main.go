// Fordjent — a Forgejo-driven agent harness.
//
// Architecture:
//
//	Forgejo ──webhook──▶ Router ──queue──▶ SessionManager ──spawn──▶ Agent
//	                                                                   │
//	Forgejo ◀──API────── Reaction ◀──────────────────────────────────┘
//	                                        │
//	                                   LLM Provider
//	                                        │
//	                                   Tool Execution
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/session"
	"github.com/fordjent/fordjent/internal/webhook"
)

func main() {
	configPath := flag.String("config", "fordjent.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bus := event.NewBus()
	mgr := session.NewManager(cfg, bus)

	router := webhook.NewRouter(cfg, bus, logger)

	go func() {
		addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		slog.Info("starting webhook server", "addr", addr)
		if err := router.ListenAndServe(ctx, addr); err != nil {
			slog.Error("webhook server error", "error", err)
			cancel()
		}
	}()

	go mgr.Run(ctx)

	slog.Info("fordjent agent harness started",
		"forgejo_url", cfg.Forgejo.URL,
		"provider", cfg.DefaultProvider().Name,
		"model", cfg.DefaultProvider().Model,
	)

	<-ctx.Done()
	slog.Info("shutting down")
}
