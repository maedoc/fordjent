package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fordjent/fordjent/internal/event"
	"github.com/fordjent/fordjent/internal/forgejo"
)

type SessionChecker interface {
	HasActiveSession(repo string, issueNumber int) bool
}

type ScannerConfig struct {
	ForgejoClient *forgejo.Client
	Checker       SessionChecker
	Bus           *event.Bus
	Repo          string
	Interval      time.Duration
	Logger        *slog.Logger
}

type Scanner struct {
	forgejoClient *forgejo.Client
	checker       SessionChecker
	bus           *event.Bus
	repo          string
	interval      time.Duration
	done          chan struct{}
	wg            sync.WaitGroup
	logger        *slog.Logger
}

func NewScanner(cfg ScannerConfig) *Scanner {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Scanner{
		forgejoClient: cfg.ForgejoClient,
		checker:       cfg.Checker,
		bus:           cfg.Bus,
		repo:          cfg.Repo,
		interval:      cfg.Interval,
		done:          make(chan struct{}),
		logger:        cfg.Logger,
	}
}

func (s *Scanner) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.logger.Info("scanner started", "repo", s.repo, "interval", s.interval)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		s.scan()
		for {
			select {
			case <-s.done:
				s.logger.Info("scanner stopped", "repo", s.repo)
				return
			case <-ticker.C:
				s.scan()
			}
		}
	}()
}

func (s *Scanner) Stop() {
	close(s.done)
	s.wg.Wait()
}

func (s *Scanner) scan() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	issues, err := s.forgejoClient.ListOpenIssues(ctx, s.repo)
	if err != nil {
		s.logger.Warn("scanner: failed to list issues", "repo", s.repo, "error", err)
		return
	}

	for _, issue := range issues {
		hasReady := false
		for _, label := range issue.Labels {
			if label.Name == "ready" {
				hasReady = true
				break
			}
		}
		if !hasReady {
			continue
		}

		if s.checker.HasActiveSession(s.repo, issue.Number) {
			s.logger.Debug("scanner: skipping issue with active session", "issue", issue.Number, "repo", s.repo)
			continue
		}

		s.logger.Info("scanner: creating session for orphaned ready issue", "issue", issue.Number, "repo", s.repo)

		evt := event.NewEvent(event.IssueOpened, s.repo, issue.Number, 0, "fordjent-scanner", "green_light")
		evt.SessionKey = fmt.Sprintf("%s/issues/%d", s.repo, issue.Number)
		evt.Payload = map[string]interface{}{
			"issue": map[string]interface{}{
				"title":  issue.Title,
				"number": issue.Number,
				"body":   issue.Body,
			},
		}

		pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.bus.Publish(pubCtx, evt)
		pubCancel()
	}
}
