# Forgejo CLI (`fj`)

A simple Python CLI for common Forgejo API operations. Eliminates the need for repetitive `curl` commands when working with Forgejo/Gitea.

## Installation

```bash
# Make executable
chmod +x fj

# Optional: install python-dotenv for .env support
pip install python-dotenv
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

The `.fj` file is searched starting from the current directory and walking up to parent directories (like `.git`).

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FORGEJO_URL` | `http://localhost:3000` | Forgejo server URL |
| `FORGEJO_TOKEN` | (none) | API token (preferred auth) |
| `FORGEJO_USER` | `fjadmin` | Username for basic auth |
| `FORGEJO_PASSWORD` | `REDACTED` | Password for basic auth |

### Command Line

Override any config with `--url`:

```bash
./fj --url http://other-server:3000 issue list owner/repo
```

## Auto-Detection

When running from inside a git repository, `fj` can auto-detect the repository from the git remote. Just omit the repo argument:

```bash
cd /path/to/your/repo
./fj issue list              # Auto-detected from git remote
./fj pr list --state all     # Works too
```

Supported URL formats:
- SSH: `git@host:owner/repo.git`
- HTTPS: `https://host/owner/repo.git`
- HTTPS with path: `https://host/git/owner/repo.git`

## Commands

### Repository

```bash
fj repo list                                    # List your repos
fj repo create my-repo --private                # Create a repo
```

### Issues

```bash
fj issue list owner/repo                        # List open issues
fj issue list owner/repo --state closed        # List closed issues
fj issue get owner/repo 42                     # Get issue details
fj issue create owner/repo "Title" --body "..." # Create issue
fj issue close owner/repo 42                   # Close issue
fj issue open owner/repo 42                    # Reopen issue
fj issue comment owner/repo 42 "My comment"    # Add comment (alias)
```

### Comments

```bash
fj comment list owner/repo 42                  # List comments
fj comment add owner/repo 42 "My comment"       # Add comment
```

### Pull Requests

```bash
fj pr list owner/repo                           # List open PRs
fj pr list owner/repo --state all               # List all PRs
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
fj token username token-name                    # Create access token
fj detect                                       # Show detected repo from git
fj raw GET /repos/owner/repo/issues            # Raw API call
```

## Global Options

```bash
-u, --url URL    Override Forgejo URL
-j, --json       Output as JSON (for list/get commands)
```

## Examples

### Create issue and add labels

```bash
./fj issue create duke/testbed "Fix bug" --body "Description here"
# Output: Created issue #42: http://...

./fj label add duke/testbed 42 bug,priority
```

### Check PR status before merging

```bash
./fj pr get duke/testbed 5
# Output:
# #5 Fix the thing
# State: open
# Head: feature/fix
# Base: main
# Mergeable: true
# Has conflicts: false

./fj pr merge duke/testbed 5
```

### Browse repository files

```bash
./fj file list duke/testbed
# .gitignore
# README.md
# go.mod
# [DIR] pkg

./fj file list duke/testbed pkg
# [DIR] vi
```

### Get file contents as JSON

```bash
./fj -j file get duke/testbed go.mod | jq '.content' -r | base64 -d
```

## Comparison with curl

**Before (curl):**
```bash
curl -s -X POST http://localhost:3000/api/v1/repos/fjadmin/testbed/issues \
  -u fjadmin:REDACTED \
  -H "Content-Type: application/json" \
  -d '{"title":"Fix bug","body":"Description"}' | jq
```

**After (fj):**
```bash
./fj issue create fjadmin/testbed "Fix bug" --body "Description"
```

## Pattern Analysis from Session Logs

Analyzed 652 curl calls across Fordjent development sessions. Most common patterns:

| Pattern | Count | Covered by |
|---------|-------|------------|
| List issues | 133+ | `issue list` |
| Create issue | 50+ | `issue create` |
| List PRs | 45+ | `pr list` |
| Merge PR | 43+ | `pr merge` |
| List comments | 46+ | `comment list` |
| Get file | 46+ | `file get` |
| List webhooks | 30+ | `hook list` |
| Create webhook | 25+ | `hook create` |
| List branches | 35+ | `branch list` |
| Add reaction | 4+ | `reaction add` |
