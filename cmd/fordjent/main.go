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

	"github.com/fordjent/fordjent/internal/config"
	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/session"
	"github.com/fordjent/fordjent/internal/telegram"
	"github.com/fordjent/fordjent/internal/webhook"
)

func main() {
	configPath := flag.String("config", "fordjent.yaml", "path to config file")
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
	mgr := session.NewManager(cfg, bus)

	// Forgejo webhook router (always started)
	router := webhook.NewRouter(cfg, bus, logger)

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

	// Telegram bot (optional)
	if cfg.Telegram.Enabled {
		tgRouter, err := telegram.NewRouter(cfg, bus)
		if err != nil {
			slog.Error("failed to init telegram bot", "error", err)
			os.Exit(1)
		}
		if tgRouter != nil {
			defer tgRouter.Close()
			go tgRouter.Start(ctx)
			slog.Info("telegram bot started", "bot_user", tgRouter.Bot().Me.Username)
		}
	}

	slog.Info("fordjent agent harness started",
		"forgejo_url", cfg.Forgejo.URL,
		"provider", cfg.DefaultProvider().Name,
		"model", cfg.DefaultProvider().Model,
		"telegram", cfg.Telegram.Enabled,
	)

	<-ctx.Done()
	slog.Info("shutting down")
}
