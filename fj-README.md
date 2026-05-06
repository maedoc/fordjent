# Forgejo CLI (`fj`)

A Go CLI for common Forgejo API operations. Built from the same codebase as Fordjent agent tools.

## Installation

```bash
cd /home/duke/src/fordjent
go build -o fj ./cmd/fj/
```

## Quick Start

```bash
# Initialize config (creates .fj file)
./fj init --url http://localhost:3000 --user duke --password ollama

# Now use fj without specifying URL every time
./fj issue list owner/repo
./fj pr merge owner/repo 5
```

## Configuration

### Config File (Recommended)

Create a `.fj` file in your project directory:

```bash
./fj init --url http://localhost:3000 --user youruser --password yourpass
```

Or create `.fj` manually:

```ini
[forgejo]
url = http://localhost:3000
token = your-api-token
user = username
password = password
repo = owner/default-repo
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FORGEJO_URL` | `http://localhost:3000` | Forgejo server URL |
| `FORGEJO_TOKEN` | (none) | API token (preferred auth) |
| `FORGEJO_USER` | (none) | Username for basic auth |
| `FORGEJO_PASSWORD` | (none) | Password for basic auth |

### Command Line

Override any config with `--url`:

```bash
./fj --url http://other-server:3000 issue list owner/repo
```

## Commands

### Issues

```bash
fj issue list owner/repo                        # List open issues
fj issue list owner/repo --state closed        # List closed issues
fj issue get owner/repo 42                     # Get issue details
fj issue create owner/repo "Title" --body "..." # Create issue
fj issue close owner/repo 42                   # Close issue
fj issue open owner/repo 42                    # Reopen issue
fj issue comment owner/repo 42 "My comment"    # Add comment
```

### Pull Requests

```bash
fj pr list owner/repo                           # List open PRs
fj pr list owner/repo --state all              # List all PRs
fj pr get owner/repo 5                          # Get PR details
fj pr files owner/repo 5                        # List changed files
fj pr create owner/repo "Title" --head feature/foo --base main
fj pr merge owner/repo 5                        # Merge PR
fj pr merge owner/repo 5 --style squash         # Squash merge
fj pr close owner/repo 5                        # Close PR
```

### Branches

```bash
fj branch list owner/repo                       # List branches
fj branch delete owner/repo feature/old         # Delete branch
```

### Webhooks

```bash
fj hook list owner/repo                         # List webhooks
fj hook create owner/repo http://example.com/hook --secret xyz
fj hook delete owner/repo 3                    # Delete webhook
```

### Labels

```bash
fj label list owner/repo                        # List repo labels
fj label add owner/repo 42 bug,urgent          # Add labels to issue
fj label remove owner/repo 42 bug              # Remove label
```

### Files

```bash
fj file list owner/repo                         # List root directory
fj file list owner/repo pkg/                   # List subdirectory
fj file get owner/repo path/to/file.txt        # Get file content
fj file get owner/repo README.md --ref develop # From specific branch
fj file create owner/repo path/to/file.txt --message "Add file" --content "..."
```

### Collaborators

```bash
fj collab list owner/repo                       # List collaborators
fj collab add owner/repo username               # Add collaborator (write access)
fj collab add owner/repo username --permission admin
```

### Misc

```bash
fj version                                      # Forgejo version
fj user                                         # Current user info
fj detect                                       # Show detected repo from git
fj repo list                                    # List your repos
fj repo create my-repo --private                # Create a repo
```

## Global Options

```bash
-u, --url URL    Override Forgejo URL
-j, --json       Output as JSON (for list/get commands)
-r, --repo REPO  Override repository
```

## Auto-Detection

When running from inside a git repository, `fj` can auto-detect the repository from the git remote. Just omit the repo argument:

```bash
cd /path/to/your/repo
./fj issue list              # Auto-detected from git remote
./fj pr list --state all     # Works too
```

## Architecture

This CLI shares the same implementation as the Fordjent agent's Forgejo tools:

- `internal/forgejo/client.go` - API client with all methods
- `internal/tool/forgejo_tools.go` - LLM-compatible tool wrappers  
- `cmd/fj/main.go` - CLI that calls client methods directly

This ensures the CLI and agent always have the same capabilities.
