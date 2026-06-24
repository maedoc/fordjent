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
	IssueLabelUpdated        Type = "issues.label_updated"
	IssueEdited              Type = "issues.edited"
	IssueCommentCreated      Type = "issue_comment.created"
	IssueCommentEdited       Type = "issue_comment.edited"
	PullRequestOpened        Type = "pull_request.opened"
	PullRequestClosed        Type = "pull_request.closed"
	PullRequestMerged        Type = "pull_request.merged"
	PullRequestLabelUpdated  Type = "pull_request.label_updated"
	PullRequestSync          Type = "pull_request.synchronize"
	PullRequestReviewComment Type = "pull_request_review_comment.created"
	PullRequestReview        Type = "pull_request_review.created" // forgejo review state: approved / changes_requested / commented
	Push                     Type = "push"
	// CI events — Forgejo Actions check runs / workflow runs
	CheckRunCompleted    Type = "check_run.completed"
	WorkflowRunCompleted Type = "workflow_run.completed"
	// Internal synthetic events
	PMReactivate           Type = "pm.reactivate"
	SpecPRMerged           Type = "spec.pr_merged"
	ArchiveChangeRequested Type = "pm.archive_requested"
	ReviewRequested        Type = "review.requested" // yolo repos: spawn djent-qa on djent-dev PR open
)

// Event is the normalized internal representation of a Forgejo webhook event.
type Event struct {
	ID              string                 `json:"event_id"`
	Type            Type                   `json:"type"`
	Repository      string                 `json:"repository"`
	IssueNumber     int                    `json:"issue_number,omitempty"`
	PRNumber        int                    `json:"pr_number,omitempty"`
	TriggeringIssue int                    `json:"triggering_issue,omitempty"`
	Sender          string                 `json:"sender"`
	Action          string                 `json:"action"`
	SessionKey      string                 `json:"session_key"`
	Role            string                 `json:"role,omitempty"`   // set by routing table: pm, implementer, reviewer
	Change          string                 `json:"change,omitempty"` // for internal events like ArchiveChangeRequested
	Payload         map[string]interface{} `json:"payload"`

	// CI / review carrier fields — populated for CheckRunCompleted,
	// WorkflowRunCompleted, and PullRequestReview events. Other events leave
	// these at their zero values.
	CheckName       string `json:"check_name,omitempty"`       // e.g. "CI", "tests"
	CheckConclusion string `json:"check_conclusion,omitempty"` // "success" / "failure" / "cancelled" / "pending" / "action_required"
	CheckURL        string `json:"check_url,omitempty"`        // link to the Forgejo Actions run
	HeadSHA         string `json:"head_sha,omitempty"`         // commit SHA the check ran against
	ReviewState     string `json:"review_state,omitempty"`     // "approved" / "changes_requested" / "commented"
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
