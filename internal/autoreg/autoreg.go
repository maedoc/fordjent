// Package autoreg implements automatic webhook and label registration
// for Forgejo repositories. When Fordjent receives an event from a repo
// it hasn't seen before, AutoRegistrar ensures the webhook and FSM labels
// are in place.
package autoreg

import (
	"context"
	"log/slog"
	"sync"

	"github.com/fordjent/fordjent/internal/forgejo"
)

// AutoRegistrarConfig holds configuration for creating an AutoRegistrar.
type AutoRegistrarConfig struct {
	ForgejoClient *forgejo.Client
	WebhookURL    string // public URL for webhook, e.g. https://fordjent.wdmn.fr/acp/v1/events
	WebhookSecret string
	Logger        *slog.Logger
}

// AutoRegistrar ensures that a Forgejo repository has the required webhook
// and FSM labels before Fordjent processes events from it.
type AutoRegistrar struct {
	forgejoClient *forgejo.Client
	webhookURL    string
	webhookSecret string
	knownRepos   map[string]bool // cache: repo → is registered
	mu            sync.RWMutex
	logger        *slog.Logger
}

// NewAutoRegistrar creates a new AutoRegistrar.
func NewAutoRegistrar(cfg AutoRegistrarConfig) *AutoRegistrar {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AutoRegistrar{
		forgejoClient: cfg.ForgejoClient,
		webhookURL:    cfg.WebhookURL,
		webhookSecret: cfg.WebhookSecret,
		knownRepos:   make(map[string]bool),
		logger:        logger,
	}
}

// webhookEvents are the Forgejo events the webhook should subscribe to.
var webhookEvents = []string{"issues", "issue_comment", "pull_request", "pull_request_review_comment"}

// EnsureRegistered checks if a repo has the required webhook and labels.
// If not, it creates them. Uses an in-memory cache to avoid redundant API calls.
// Errors are non-fatal — the caller should log them but continue processing.
func (ar *AutoRegistrar) EnsureRegistered(ctx context.Context, repo string) error {
	// Fast path: check cache
	ar.mu.RLock()
	if ar.knownRepos[repo] {
		ar.mu.RUnlock()
		return nil
	}
	ar.mu.RUnlock()

	// Slow path: check and register
	ar.mu.Lock()
	defer ar.mu.Unlock()

	// Double-check after acquiring write lock
	if ar.knownRepos[repo] {
		return nil
	}

	// Ensure webhook exists (only if webhook URL is configured)
	if ar.webhookURL != "" {
		if err := ar.ensureWebhook(ctx, repo); err != nil {
			return err
		}
	}

	// Ensure FSM labels exist
	if err := ar.forgejoClient.EnsureLabels(ctx, repo); err != nil {
		return err
	}

	ar.knownRepos[repo] = true
	ar.logger.Info("auto-registered repo", "repo", repo, "webhook", ar.webhookURL != "")
	return nil
}

// ensureWebhook checks if the repo has a webhook pointing to our URL,
// and creates one if not.
func (ar *AutoRegistrar) ensureWebhook(ctx context.Context, repo string) error {
	hooks, err := ar.forgejoClient.ListWebhooks(ctx, repo)
	if err != nil {
		return err
	}

	// Check if a webhook with our URL already exists
	for _, hook := range hooks {
		if hook.Config != nil {
			if url, ok := hook.Config["url"]; ok && url == ar.webhookURL {
				ar.logger.Debug("webhook already exists for repo", "repo", repo, "hook_id", hook.ID)
				return nil
			}
		}
	}

	// No matching webhook — create one
	_, err = ar.forgejoClient.CreateWebhook(ctx, repo, "forgejo", ar.webhookURL, ar.webhookSecret, webhookEvents)
	if err != nil {
		return err
	}
	ar.logger.Info("created webhook for repo", "repo", repo, "url", ar.webhookURL)
	return nil
}