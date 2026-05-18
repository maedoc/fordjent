package forgejo

import (
	"net/url"
	"path"
	"strings"
)

// EscapeRepoPath escapes each segment of an "owner/repo" path while preserving
// the slash separator. Using url.PathEscape on the whole string would encode the
// slash, breaking Gitea/Forgejo's two-segment path routing.
func EscapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return path.Join(parts...)
}