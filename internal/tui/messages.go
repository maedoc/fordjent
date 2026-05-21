package tui

import (
	"time"

	"github.com/fordjent/fordjent/internal/forgejo"
	"github.com/fordjent/fordjent/internal/lifecycle"
)

type statusTickMsg time.Time
type sseEventMsg struct {
	evt lifecycle.SSEEvent
}
type sseConnectedMsg struct{}
type sseDisconnectedMsg struct{}
type sseReconnectMsg struct{}
type itemsLoadedMsg struct {
	issues []forgejo.Issue
	prs    []forgejo.PullRequest
}
type statusLoadedMsg struct {
	data       map[string]interface{}
	statusResp *StatusResponse
	tokenHist  []TokenMinute
}
type commentsLoadedMsg struct {
	issueNumber int
	comments    []forgejo.Comment
}
type prDetailLoadedMsg struct {
	pr      *forgejo.PullRequest
	files   []forgejo.PRFile
	reviews []forgejo.Review
}
type commandResultMsg struct {
	output string
	err    error
}
type forgejoTickErrorMsg struct {
	issueErr error
	prErr    error
}
type statusTickErrorMsg struct {
	err error
}
type errorMsg struct{ err error }