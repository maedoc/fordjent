package event

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// Type represents the kind of Forgejo event.
type Type string

const (
	IssueOpened              Type = "issues.opened"
	IssueClosed              Type = "issues.closed"
	IssueReopened            Type = "issues.reopened"
	IssueCommentCreated      Type = "issue_comment.created"
	IssueCommentEdited       Type = "issue_comment.edited"
	PullRequestOpened        Type = "pull_request.opened"
	PullRequestClosed        Type = "pull_request.closed"
	PullRequestMerged        Type = "pull_request.merged"
	PullRequestSync          Type = "pull_request.synchronize"
	PullRequestReviewComment Type = "pull_request_review_comment.created"
	Push                     Type = "push"
)

// Event is the normalized internal representation of a Forgejo webhook event.
type Event struct {
	ID          string                 `json:"event_id"`
	Type        Type                   `json:"type"`
	Repository  string                 `json:"repository"`
	IssueNumber int                    `json:"issue_number,omitempty"`
	PRNumber    int                    `json:"pr_number,omitempty"`
	Sender      string                 `json:"sender"`
	Action      string                 `json:"action"`
	SessionKey  string                 `json:"session_key"`
	Payload     map[string]interface{} `json:"payload"`
}

// NewEvent creates a new event with a UUIDv7-style ID.
func NewEvent(typ Type, repo string, issueNum, prNum int, sender, action string) *Event {
	return &Event{
		ID:          uuid.New().String(),
		Type:        typ,
		Repository:  repo,
		IssueNumber: issueNum,
		PRNumber:    prNum,
		Sender:      sender,
		Action:      action,
	}
}

// Bus is an in-memory event bus that fans out events to subscribers.
type Bus struct {
	mu          sync.RWMutex
	subscribers []chan *Event
}

func NewBus() *Bus {
	return &Bus{}
}

// Subscribe returns a channel that receives all published events.
// The caller must drain the channel to avoid blocking.
func (b *Bus) Subscribe() <-chan *Event {
	ch := make(chan *Event, 256)
	b.mu.Lock()
	b.subscribers = append(b.subscribers, ch)
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel and closes it.
func (b *Bus) Unsubscribe(ch <-chan *Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, sub := range b.subscribers {
		if sub == ch {
			b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
			close(sub)
			return
		}
	}
}

// Publish sends an event to all subscribers.
func (b *Bus) Publish(ctx context.Context, evt *Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, sub := range b.subscribers {
		select {
		case sub <- evt:
		case <-ctx.Done():
			return
		default:
			// Drop event if subscriber is full (back-pressure)
		}
	}
}
