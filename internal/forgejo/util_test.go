package forgejo

import "testing"

func TestEscapeRepoPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"owner/repo", "owner/repo"},
		{"owner/repo-with special", "owner/repo-with%20special"},
		{"org/my.repo", "org/my.repo"},
		{"org/repo%2Ftest", "org/repo%252Ftest"},
		{"a/b/c", "a/b/c"},
		{"", ""},
		{"single", "single"},
		{"owner/repo.sub/name", "owner/repo.sub/name"},
	}

	for _, tt := range tests {
		got := EscapeRepoPath(tt.input)
		if got != tt.want {
			t.Errorf("EscapeRepoPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}