# Gap Analysis: 6 Remaining Code Quality Issues

## Files Retrieved

1. `internal/tool/local_tools.go` (entire file ~580 lines) — bash tool scope checking, write_file tool, read_file tool, git tool
2. `internal/session/agent.go` (lines 1-400 approx) — `extractScopePrefixes`, scope wiring via `buildContext`, `SetWriteScope`/`SetBashScope`/`SetPRScope` calls
3. `internal/session/manager.go` (lines 1-1600, 1600-end) — `cleanupOldWorkDirs`, `reapIdle`, `evictOldest`, `shutdownAll`, `restoreSessions`, `runSchedulerReconcile`, `runAutoRetry`, `runPeriodicRecovery`
4. `internal/session/store.go` (entire file ~170 lines) — SessionRecord schema, no cleanup/death flag
5. `internal/scheduler/scheduler.go` (entire file ~580 lines) — `OnPRMerged`, `checkAndUnblock`, `parseDependsOn`, `isIssueClosed`, `ReconcileBlocked`, `detectCircularDeps`
6. `internal/scheduler/scheduler_test.go` (entire file ~200 lines) — `parseDependsOn` test cases

---

## Gap 1: Bash Scope Bypass via tee/dd/pipes

### Exact File / Line Numbers
- **Regex patterns**: `internal/tool/local_tools.go`, lines ~176–181 (in `Execute`, scope restriction block)
- **`SetScopePrefixes`**: `internal/tool/local_tools.go`, line ~81
- **Scope wiring**: `internal/session/agent.go`, lines in `buildContext` (~620–630 range):
  ```go
  a.scopePrefixes = extractScopePrefixes(issue.Title + " " + issue.Body)
  if len(a.scopePrefixes) > 0 {
      a.tools.SetWriteScope(a.scopePrefixes)
      a.tools.SetPRScope(a.scopePrefixes)
      a.tools.SetBashScope(a.scopePrefixes)
  }
  ```

### Current Code

```go
// Lines ~176-181 in local_tools.go
writePatterns := []*regexp.Regexp{
    regexp.MustCompile(`>(?:>]){0,1}\s*([a-zA-Z0-9_./-]+)`),
    regexp.MustCompile(`(?:cat|tee)\s+(?:-Ae Shelter)?\s*([a-zA-Z0-9_./-]+)`),
    regexp.MustCompile(`(?:cp|mv)\s+(?:\S+\s+)+([a-zA-Z0-9_./-]+)$`),
}
```

### Analysis: What Each Pattern Catches vs Misses

| Pattern | Catches | Misses |
|---------|---------|--------|
| `>(?:>]){0,1}\s*([a-zA-Z0-9_./-]+)` | `> file`, `>> file`, `/bin/cmd > out` | **`2> file`**, **`3> file`** (fd redirects like `exec 3>/path`), **`&>/file`**, **`1>/path`**, **`| cmd > out`** (pipe → redirect) |
| `(?:cat\|tee) (?:-Ae Shelter)? (file)` | `cat file`, `tee file`, `cat > file` (by accident since `>` is a prefix char), `tee -a file` | **`echo content | tee file`**, **`cat file1 cat file2 \| tee file`**, **`dd of=/path`** (no keyword), **`printf '' > /path`** (not cat/tee/cp/mv) |
| `(?:cp\|mv) (\S+ )*(file)` | `cp src dst`, `mv src dst`, `cp src1 src2 dst` | **`tar cf archive.tar -C /path`**, **`rsync src dst`**, **`install file /path`**, **`ln -s /path link`** |

### Attack/Failure Scenarios

1. **Command substitution bypass**: `$(echo "payload" | tee pkg/str/str.go)` — The regex looks for `tee\s+(?:-flags)\s*PATH`, but `tee pkg/str/str.go` is preceded by `| ` not by `tee `. The regex would NOT match `tee` here because it expects `tee` at the word boundary. Actually, the regex `\b(?:cat|tee)\s+...` would still match `tee pkg/str/str.go` because `findAllString` scans the entire command string. **However**, the path extraction would capture `pkg/str/str.go` — so **THIS IS CAUGHT**. But:
   
   `$(echo content > pkg/str/str.go)` — the `>` pattern would match `pkg/str/str.go`, and the scope check WOULD catch it. So this is actually caught too.

2. **`dd of=/path`**: The `bashBlockedPatterns` at line ~78 blocks `dd if=` but NOT `dd of=`. A scoped agent could do:
   ```bash
   dd of=pkg/str/str.go bs=1 count=100
   ```
   The `of=` path has no `>`, no `cat`/`tee`, no `cp`/`mv` — **NOT CAUGHT**.

3. **`exec 3>/path`**: The `>` pattern matches `> ([a-zA-Z0-9_./-]+)`, but the regex captures only `3` as the "filename" (or `3` + path depending on character set). The character class `[a-zA-Z0-9_./-]` does NOT include `3>/` as a single token — it would stop at `3` or misparse. Specifically: for `exec 3>/tmp/foo`, the regex would match `> /tmp/foo` — actually **WOULD IT?** Let me trace: `exec 3>/tmp/foo` — the regex `>(?:>]){0,1}\s*([a-zA-Z0-9_./-]+)` would find `>/tmp/foo` and capture `/tmp/foo`. So actually this IS caught. But `exec 3>>/tmp/foo` would also match. However, `>/tmp` — the `/` in the regex pattern `>` followed by `/` means the `[a-zA-Z0-9_./-]+` starting at `/` IS valid. **This IS caught** for bash.

4. **Redirection via shell builtins**: `echo "data" | cat > pkg/str/str.go`, `printf "%s" "content" > pkg/str/str.go` — the `>` pattern catches `pkg/str/str.go` in both cases. **CAUGHT**.

5. **`tar` or `cpio` extraction to scope target**: `tar xf data.tar -C pkg/str/` — **NOT CAUGHT**. `tar`, `cpio`, `rsync`, `unzip`, `7z`, `git worktree add` — all bypass.

6. **`base64` decode**: `echo "base64data" \| base64 -d > pkg/str/str.go` — `>` pattern catches it. **CAUGHT**.

### Severity: Medium-High
The core risk is `dd of=` and `tar/cpio/rsync` which are common Unix utilities not covered at all. Command substitution and pipes are partially mitigated by the broad `>` pattern, but structured write commands are not.

### Fix Approach
Replace regex heuristics with a command-name whitelist approach:
```go
// Whitelist allowed commands when scoped
allowedBashCmds := map[string]bool{
    "git":    true,
    "go":     true,
    "cat":    true, // read-only cat: cat file (no flags that change behavior for reads)
    "ls":     true,
    "find":   true,
    "grep":   true,
    "echo":   true, // only echo to stdout: echo "text" not echo "text" > file
    "pwd":    true,
    "head":   true,
    "tail":   true,
    "wc":     true,
    "stat":   true,
    "touch":  true, // mkdir -p needs touch, but touch itself is read-alias
}

// Parse the command name (first token)
firstToken := parts[0] // after splitting by whitespace
if _, allowed := allowedBashCmds[firstToken]; !allowed {
    return "", fmt.Errorf("command '%s' is not allowed in scoped mode", firstToken)
}

// For commands that CAN write (cat with -, echo), also regex-check for write targets
if isWriteCapable(firstToken) {
    // Re-use existing write detection but only for whitelist commands
}
```

Alternatively: use `filepath.EvalSymlinks` in the scope check so that even if `dd of=` writes via a symlink, the resolved path is checked against scope prefixes.

---

## Gap 2: write_file Symlink Escape

### Exact File / Line Numbers
- **write_file tool**: `internal/tool/local_tools.go`, lines ~290–380 (the `writeFileTool.Execute` method)
- **read_file tool**: `internal/tool/local_tools.go`, lines ~200–289 (the `readFileTool.readFile` method)
- **Scope checking**: `internal/session/agent.go`, `SetWriteScope` wiring in `buildContext`

### Current Code (write_file)

```go
// Lines ~327-345
absPath := filepath.Join(t.repoDir, filepath.Clean(params.Path))
repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
if !strings.HasPrefix(absPath, repoClean) {
    slog.Warn("file access blocked: path escapes repository root",
        "tool", "write_file",
        "requested_path", params.Path,
        "resolved_path", absPath,
        "repo_root", t.repoDir,
    )
    return "", fmt.Errorf("path escapes repository root: %s", params.Path)
}

// Defense-in-depth: verify resolved path is within repo root using filepath.Rel.
if rel, err := filepath.Rel(filepath.Clean(t.repoDir), absPath); err != nil || strings.HasPrefix(rel, "..") {
    // ...
}

os.MkdirAll(filepath.Dir(absPath), 0755)
os.WriteFile(absPath, []byte(params.Content), 0644)
```

### Current Code (read_file)

```go
// Lines ~240-268
cleanPath := filepath.Clean(path)
absPath := filepath.Join(t.repoDir, cleanPath)

// ... several sanitization passes ...

absPath = filepath.Clean(absPath)
repoClean := filepath.Clean(t.repoDir) + string(os.PathSeparator)
if !strings.HasPrefix(absPath, repoClean) {
    return "", fmt.Errorf("path escapes repository root: %s", path)
}

if rel, err := filepath.Rel(filepath.Clean(t.repoDir), absPath); err != nil || strings.HasPrefix(rel, "..") {
    return "", fmt.Errorf("path escapes repository root: %s", path)
}

f, err := os.Open(absPath)
```

### Analysis

**What's missing**: Neither `write_file` nor `read_file` calls `filepath.EvalSymlinks()` on the resolved `absPath` before the containment check.

**Attack scenario**: Suppose an agent creates a symlink inside the repo:
```bash
# Agent has write_file access to pkg/math/
write_file(path="pkg/math/symlink.txt", content="...")  # Creates real file
# Then via bash:
bash(command="ln -s /etc/passwd pkg/math/ptr")
# Now read_file(path="pkg/math/ptr") → reads /etc/passwd via symlink → NOT CAUGHT
# And write_file(path="pkg/math/ptr", content="hacked") → follows symlink to /etc/passwd
```

The containment check resolves `filepath.Join(repoDir, "pkg/math/ptr")` which is inside `repoDir`. It does NOT follow the symlink to `/etc/passwd`. The `os.Open`/`os.WriteFile` calls DO follow symlinks, but the containment check does not.

**Symlink in allowed prefix attacking scope check**: An allowed prefix like `pkg/math/` contains a symlink pointing outside the repo. write_file with `path="pkg/math/symlink.txt"` passes the prefix check (starts with `pkg/math/`) and passes the path containment check (`pkg/math/symlink.txt` is inside repo). But the symlink points to `/etc/...`, so the write silently escapes.

**`../` handling**: `filepath.Clean()` does handle `../` by removing parent path components. So `write_file(path="../etc/passwd")` → `filepath.Clean("../etc/passwd")` = `../etc/passwd` → `filepath.Join(repoDir, "../etc/passwd")` = `.../etc/passwd` → the `strings.HasPrefix(absPath, repoClean + "/")` check WOULD catch this because `.../etc/passwd` does NOT prefix with `.../repoDir/`. **This path is caught**.

**But symlinks change everything**: `filepath.Join(repoDir, "pkg/../../etc/passwd")` = `../etc/passwd` → caught. But `filepath.Join(repoDir, "pkg/link.txt")` where `pkg/link.txt` is a symlink to `../../etc/passwd` → NOT caught because the containment check never follows the symlink.

### Severity: High
Symlink-based escape is a real privilege escalation path for an agent with write access.

### Fix Approach
Add `filepath.EvalSymlinks` resolution after path joining but before the containment check:

```go
absPath := filepath.Join(t.repoDir, filepath.Clean(params.Path))

// Resolve any symlinks in intermediate components
resolvable := absPath
var seenComponents []string
for {
    dir, base := filepath.Split(resolvable)
    dir = strings.TrimSuffix(dir, string(os.PathSeparator))
    if dir == t.repoDir || dir == filepath.Clean(t.repoDir) {
        // We've reached the trusted root — stop resolving upward
        break
    }
    link, err := os.Readlink(resolvable)
    if err == nil {
        // It's a symlink — resolve it
        if !filepath.IsAbs(link) {
            link = filepath.Join(filepath.Dir(resolvable), link)
        }
        resolvable = link
        if seen, ok := seen[resolvable]; ok {
            return "", fmt.Errorf("symlink loop detected: %s", resolvable)
        }
        seen[resolvable] = true
        continue // Check this new path
    }
    break
}
absPath = resolvable

// NOW do containment check on the fully-resolved path
```

---

## Gap 3: Transitive Dependencies

### Exact File / Line Numbers
- **`parseDependsOn`**: `internal/scheduler/scheduler.go`, lines ~310–330
- **`checkAndUnblock`**: `internal/scheduler/scheduler.go`, lines ~90–200
- **`isIssueClosed`**: `internal/scheduler/scheduler.go`, lines ~390–420
- **`ReconcileBlocked`**: `internal/scheduler/scheduler.go`, lines ~520–570
- **`detectCircularDeps`**: `internal/scheduler/scheduler.go`, lines ~250–300

### Current Code (parseDependsOn)

```go
var dependsOnRegex = regexp.MustCompile(`(?i)#(\d+)`)
var dependsOnKeywordRegex = regexp.MustCompile(`(?i)depends\s+on`)

func parseDependsOn(body string) []int {
    lines := strings.Split(body, "\n")
    seen := make(map[int]struct{})
    var nums []int
    for _, line := range lines {
        if !dependsOnKeywordRegex.MatchString(line) {
            continue
        }
        matches := dependsOnRegex.FindAllStringSubmatch(line, -1)
        for _, m := range matches {
            if len(m) >= 2 {
                n, err := strconv.Atoi(m[1])
                if err == nil {
                    if _, ok := seen[n]; !ok {
                        seen[n] = struct{}{}
                        nums = append(nums, n)
                    }
                }
            }
        }
    }
    return nums
}
```

### Test Cases (from scheduler_test.go)

```go
func TestParseDependsOn(t *testing.T) {
    cases := []struct {
        body string
        want []int
    }{
        {"Depends on: #15", []int{15}},
        {"depends on: #15, #16", []int{15, 16}},
        {"DEPENDS ON #15, #16, #17", []int{15, 16, 17}},
        {"Depends on: #15\nSee also #16", []int{15}},
        {"No deps here.", nil},
    }
    // ...
}
```

### Analysis

#### What's NOT Supported (Syntax)

| Syntax | Supported? | Reason |
|--------|-----------|--------|
| `Depends on: #15` | ✅ | Matches `depends\s+on` + `#(\d+)` |
| `depends on: #15, #16` | ✅ | `#(\d+)` matches both numbers |
| `Depends on: #15 and #20` | ⚠️ Partial | `depends\s+on` matches, `#(\d+)` grabs `15` and `20` — but "and" is not a separator, it's just ignored by `FindAllStringSubmatch` — **actually this works** because `(?i)#(\d+)` finds ALL occurrences of `#N` after the keyword line |
| `depends-on: #15` (hyphen) | ❌ | Keyword is `depends\s+on` (space between), `depends-on` has a hyphen not matching `\s+` |
| `- Depends on: #15` (bullet) | ✅ | `depends\s+on` matches within the line |
| `- #15` (list, no keyword) | ❌ | No `depends\s+on` keyword, line is skipped |
| `Requires: #15` | ❌ | No match for `requires` |
| `Blocked by: #15` | ❌ | No match for `blocked` + `by` |
| `Closes: #15` | ❌ | No match |
| `Fixes: #15` | ❌ | No match |
| `Related: #15` | ❌ | No match |
| `Depends on: #15, #20, #25` | ✅ | All 3 numbers extracted by `FindAllStringSubmatch` |

Wait — let me re-check `Depends on: #15 and #20`:
- Line: `"Depends on: #15 and #20"`
- `dependsOnKeywordRegex.MatchString(line)` — `(?i)depends\s+on` matches because `depends\s+on` is found (case-insensitive, `Depends on:` matches)
- `dependsOnRegex.FindAllStringSubmatch(line, -1)` — `(?i)#(\d+)` finds `#15` and `#20` — so **YES, this IS supported**

#### What's NOT Supported (Transitive)

The real Gap here is about transitive dependency **resolution**, not syntax:

When issue #15 `Depends on: #10` and issue #20 `Depends on: #15`, after PR #10 is merged:
1. `OnPRMerged` → `checkAndUnblock`: Lists all open issues, finds #15 and #20
2. For #15: `parseDependsOn` returns `[10]`, `isIssueClosed(10)` returns `true` → **#15 is unblocked ✅**
3. For #20: `parseDependsOn` returns `[15]`, `isIssueClosed(15)` returns... **`true`** (because #15's state is "open" but `isIssueClosed` returns true for PM-type issues without `pull_request` field)

Wait — #15 is an implementation issue, so it WILL have a `pull_request` field. When its PR is not yet merged: `isIssueClosed` returns `false` (blocking). When the PR IS merged: `isIssueClosed` returns `true` (closed). 

So actually the transitive case works for PR issues: #20 depends on #15, #15's PR is merged → `isIssueClosed(15)` returns true → #20 unblocked.

**The actual problem is with PM/coordination issues that don't have PRs**:

If issue #30 is a PM issue that `Depends on: #15`, and #15 is an implementation issue whose PR hasn't merged yet:
- `isIssueClosed(15)` checks: #15 has `pull_request` field with open state → returns `false` ✅ blocking as expected

Now if issue #30 is a PM issue that `Depends on: #25` (another PM issue #25 which has no PR):
- `isIssueClosed(25)` checks: #25 is open, no `pull_request` field → **returns `true`** (not blocking) — because the code says "open issue without PR = PM issue = not blocking"

This is the transitive gap: **PM dependencies are transitively satisfied by being open and having no PR**. But a PM issue #25 might itself depend on #15 (an implementation issue), and #15's PR is NOT merged yet. The transitive chain is not validated.

The `detectCircularDeps` function exists and builds a full adjacency graph to find cycles. But `checkAndUnblock` does NOT traverse transitive dependencies — it only checks direct ones.

#### `ReconcileBlocked` (line ~520+)

```go
func (s *Scheduler) ReconcileBlocked(ctx context.Context, repo string) (int, error) {
    issues, _ := s.listOpenIssues(ctx, repo)
    for _, issue := range issues {
        deps := parseDependsOn(issue.Body)  // Only direct deps
        for _, depNum := range deps {
            closed, _ := s.isIssueClosed(ctx, repo, depNum)
        }
    }
    // Same single-level check — NO transitive traversal
}
```

### Severity: Medium for syntax, Low-Medium for transitive (rare in practice)
Transitive PM dependencies are an edge case that rarely manifests because PM issues typically don't have hard `Depends on:` chains. The main practical impact is the syntax fragility Gap 6.

### Fix Approach (syntax)

Add multiple keyword patterns to `parseDependsOn`:

```go
var dependsOnKeywords = regexp.MustCompile(`(?i)(?:depends\s+on|depends-on|requires|blocked\s+by|needs|relates\s+to|related\s+to|subtask\s+of|tracking)\s*:?`)
```

**Transitive fix** (if needed later):

```go
func (s *Scheduler) isTransitivelyClosed(ctx context.Context, repo string, deps []int, visited map[int]bool) bool {
    if visited == nil {
        visited = make(map[int]bool)
    }
    for _, dep := range deps {
        if visited[dep] {
            continue // Cycle guard
        }
        visited[dep] = true
        closed, _ := s.isIssueClosed(ctx, repo, dep)
        if !closed {
            return false
        }
        // For open non-PR issues (PM), also check ITS dependencies
        issue, _ := s.getIssue(ctx, repo, dep)
        if issue != nil && issue.PullRequest == nil && issue.State == "open" {
            subDeps := parseDependsOn(issue.Body)
            if !s.isTransitivelyClosed(ctx, repo, subDeps, visited) {
                return false
            }
        }
    }
    return true
}
```

---

## Gap 4: Session Cleanup (Stale Branches + workDirs)

### Exact File / Line Numbers
- **`cleanupOldWorkDirs`**: `internal/session/manager.go`, line ~1075
- **`reapIdle`**: `internal/session/manager.go`, line ~1048
- **`evictOldest`**: `internal/session/manager.go`, line ~1114
- **`shutdownAll`**: `internal/session/manager.go`, line ~1133
- **`Drain`**: `internal/session/manager.go`, line ~1143

### Current Code

```go
// Line ~1075 — cleanupOldWorkDirs
func (m *Manager) cleanupOldWorkDirs(ctx context.Context) {
    if m.cfg.Agent.WorkDir == "" { return }
    archiveBase := filepath.Join(m.cfg.Agent.WorkDir, "..", "archive")
    const maxAge = 7 * 24 * time.Hour

    records, err := m.store.ListAll()
    for _, rec := range records {
        state, _ := m.lc.GetState(ctx, rec.SessionKey)
        if state != StateCompleted && !strings.HasPrefix(state, "failed") { continue }
        if time.Since(rec.LastActive) < maxAge { continue }
        // Archive memory.jsonl, then RemoveAll
        safedir := filepath.Join(archiveBase, strings.ReplaceAll(rec.SessionKey, "/", "_"))
        // ... archive then os.RemoveAll(rec.WorkDir)
    }
}

// Line ~1048 — reapIdle
func (m *Manager) reapIdle(ctx context.Context) {
    for key, sess := range m.sessions {
        if time.Since(sess.LastActive) > m.cfg.Agent.IdleTimeout {
            sess.Cancel()
            delete(m.sessions, key)
            m.store.Delete(key)
        }
    }
}

// Line ~1114 — evictOldest
func (m *Manager) evictOldest() {
    // Find oldest idle session, cancel + delete from map + store
}

// Line ~1133 — shutdownAll (called on context cancellation)
func (m *Manager) shutdownAll() {
    for key, sess := range m.sessions {
        sess.Cancel()
        delete(m.sessions, key)  // Does NOT call m.store.Delete or cleanup workDir
    }
}
```

### Analysis

#### workDir Cleanup — Partial
| Scenario | workDir Cleaned? | Memory Archived? | Feature Branches Deleted? |
|----------|-----------------|-------------------|--------------------------|
| `cleanupOldWorkDirs` (every hour) | ✅ After 7 days | ✅ Before removal | ❌ Not checked |
| `reapIdle` (every minute) | ❌ Only in-memory deletion | ❌ | ❌ |
| `evictOldest` (on capacity) | ❌ Only in-memory deletion | ❌ | ❌ |
| `shutdownAll` (on exit) | ❌ Only in-memory deletion | ❌ | ❌ |

**Key gap**: `shutdownAll` at process exit does NOT clean workDirs or feature branches. The `store.Delete` is also missing from `shutdownAll` — meaning persisted session records survive across restarts even though in-memory sessions are cleared.

#### Feature Branch Cleanup — None
No code exists to delete feature branches (`feature/*`, `bugfix/*`, `spec/*`, `test-rebuild/*`) when sessions end. After many parallel waves, the repo clone accumulates dozens of stale branches.

#### Store — No Death/Cleanup Flag
The `Session` table in `store.go` has: `session_key`, `repository`, `issue_number`, `pr_number`, `work_dir`, `repo_dir`, `created_at`, `last_active`. No `completed_at`, `deleted`, or `cleaned_up` column. The store is only deleted in `evictOldest`/`reapIdle`/`shutdownAll` (in-memory only).

### Severity: Low
Disk accumulation is slow (7-day archive window). No data corruption risk. But long-running instances will accumulate stale feature branches and work files.

### Fix Approach

```go
// Add to shutdownAll:
func (m *Manager) shutdownAll() {
    m.mu.Lock()
    defer m.mu.Unlock()
    for key, sess := range m.sessions {
        sess.Cancel()
        // Clean up work directory
        if sess.WorkDir != "" {
            if err := os.RemoveAll(sess.WorkDir); err != nil {
                slog.Warn("shutdown: failed to remove workdir", "key", key, "err", err)
            }
        }
        // Delete from persistent store
        m.store.Delete(key)
        delete(m.sessions, key)
    }
}

// Add branch cleanup:
func (m *Manager) cleanupOldBranches(repoDir string) {
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "branch", "-v")
    out, err := cmd.CombinedOutput()
    if err != nil { return }
    // Parse branches, compare with main HEAD, delete if stale
    // (keep "main" and current HEAD branch)
}
```

---

## Gap 5: Restart Double-Run Protection

### Exact File / Line Numbers
- **`restoreSessions`**: `internal/session/manager.go`, line ~355
- **`Run`**: `internal/session/manager.go`, line ~410
- **`getOrCreate`**: `internal/session/manager.go`, line ~680
- **`runSession`**: `internal/session/manager.go`, line ~770

### Current Code

```go
// Line ~355 — restoreSessions
func (m *Manager) restoreSessions() error {
    records, _ := m.store.ListAll()
    for _, rec := range records {
        state, _ := m.lc.GetState(ctx, rec.SessionKey)
        if state == StateCompleted || strings.HasPrefix(state, "failed") {
            continue  // Skip completed/failed
        }
        sess := &Session{Key: rec.SessionKey, ...}
        m.sessions[rec.SessionKey] = sess
        go m.runSession(sessCtx, sess)  // Spawns goroutine
        
        // Recovery window: if within 24h, post synthetic comment to re-activate
        if time.Since(rec.LastActive) < recoveryWindow {
            // Post "Resuming work after agent restart..."
        }
    }
}
```

```go
// Line ~680 — getOrCreate (called when handleEvent routes to a session)
func (m *Manager) getOrCreate(ctx context.Context, evt *event.Event) (*Session, error) {
    m.mu.RLock()
    sess, exists := m.sessions[evt.SessionKey]
    m.mu.RUnlock()
    if exists {
        sess.mu.Lock()
        sess.LastActive = time.Now()
        sess.mu.Unlock()
        return sess, nil  // Returns existing session
    }
    // ... creates new session, clones repo, runs session
}
```

### Analysis

#### Restart Scenario (Process Dies → Starts Again)

**State A**: Session `repo/issues/42` was running (`working` state). Process crashes.
**State B**: Process restarts.
1. `restoreSessions` reads `sessions.db`, finds `repo/issues/42` with state `working`
2. Spawns a NEW goroutine: `go m.runSession(sessCtx, sess)`
3. Posts a synthetic comment: "Resuming work after agent restart..." if within recovery window
4. The new goroutine starts processing from the **memory.jsonl** file

**The double-run scenario**: The original goroutine is DEAD (process crashed), so there's no real double-run. The issue is:
- The **memory.jsonl** file contains tool calls from the PREVIOUS run
- On restart, the new session READS memory.jsonl and injects previous output into context
- The agent might re-execute tool calls that already ran (e.g., `git commit`, `write_file`)

**State A**: Session `repo/issues/42` was running (not crashed). User does `docker compose restart fordjent`.
**State B**: New container starts.
1. Old goroutine gets `ctx.Done()` → session ends in ~seconds
2. `restoreSessions` runs AFTER Run() starts → but the old goroutine was already cancelled
3. New `restoreSessions` goroutine spawns from db record
4. **No conflict** — old goroutine is dead

**But**: What if the old goroutine was in the middle of a long-running tool call?
- `ctx.Done()` propagates to `exec.CommandContext(ctx, ...)` 
- The shell command gets killed
- But the **work directory** and **memory.jsonl** still show partial state
- The new session sees the partial state and might try to redo the last action

#### memory.jsonl Handling on Restart

```go
// agent.go buildContext — checks for previous session memory
if evt.IssueNumber > 0 && evt.Type == event.IssueOpened {
    memFile := filepath.Join(prevWorkDir, "memory.jsonl")
    if data, err := os.ReadFile(memFile); len(data) > 0 {
        // Inject last 5 assistant messages as "[Previous Session Context]"
        // This provides retry context to the agent
    }
}
```

This is **correct behavior** — it's meant to help the agent know what failed and retry differently. But it creates ambiguity: does the agent re-execute a `git commit` that already completed? The `git` tool checks for existing commits, so duplicate commits would be no-ops. `write_file` replaces the file. `forgejo_create_pr` would see the branch already exists.

### Severity: Low
The "double-run" risk is mostly about redundant tool calls, not data corruption. The agent's idempotent tools (write_file replaces, git commits create new SHAs) mitigate most risks. The main concern is wasted tokens and API calls.

### Fix Approach

1. **Add a session shutdown checkpoint**: On session exit, write to a `shutdown.json` file:
```go
// In runSession, before returning:
shutFile := filepath.Join(sess.WorkDir, "shutdown.json")
json.Marshal(map[string]string{
    "state": state,  // "completed", "failed", "cancelled"
    "done":  time.Now().UTC().Format(time.RFC3339),
})
```

2. **Check shutdown checkpoint on restore**:
```go
func (m *Manager) restoreSessions() error {
    records, _ := m.store.ListAll()
    for _, rec := range records {
        shutFile := filepath.Join(rec.WorkDir, "shutdown.json")
        if data, err := os.ReadFile(shutFile); err == nil {
            var info struct { State string; Done string }
            json.Unmarshal(data, &info)
            if info.State == "completed" {
                continue // Already gracefully completed
            }
            // If cancelled by crash, the agent will need to re-run
            // but recovery + injected memory will help
        }
        // ...
    }
}
```

3. **Add `Done` check to `getOrCreate`**: If the session exists in memory but was cancelled (context done), don't recreate — treat as already-shutdown.

---

## Gap 6: Dependency Syntax Fragility

### Exact File / Line Numbers
- **`parseDependsOn`**: `internal/scheduler/scheduler.go`, lines ~310–330
- **Regex definitions**: `internal/scheduler/scheduler.go`, lines ~25–26
- **Test cases**: `internal/scheduler/scheduler_test.go`, lines ~70–95

### Current Code

```go
// Lines ~25-26
var dependsOnRegex = regexp.MustCompile(`(?i)#(\d+)`)
var dependsOnKeywordRegex = regexp.MustCompile(`(?i)depends\s+on`)

// Lines ~310-330
func parseDependsOn(body string) []int {
    lines := strings.Split(body, "\n")
    seen := make(map[int]struct{})
    var nums []int
    for _, line := range lines {
        if !dependsOnKeywordRegex.MatchString(line) {
            continue  // Skip lines without "depends on" keyword
        }
        matches := dependsOnRegex.FindAllStringSubmatch(line, -1)
        for _, m := range matches {
            n, _ := strconv.Atoi(m[1])
            if n > 0 && !seen[n] {
                seen[n] = struct{}{}
                nums = append(nums, n)
            }
        }
    }
    return nums
}
```

### Test Cases

```go
// Lines ~70-95 in scheduler_test.go
func TestParseDependsOn(t *testing.T) {
    cases := []struct{body string; want []int}{
        {"Depends on: #15", []int{15}},                       // ✅
        {"depends on: #15, #16", []int{15, 16}},              // ✅
        {"DEPENDS ON #15, #16, #17", []int{15, 16, 17}},      // ✅
        {"Depends on: #15\nSee also #16", []int{15}},         // ✅ (#16 line has no keyword → skipped)
        {"No deps here.", nil},                                // ✅
    }
}
```

### Analysis: Unsupported Syntaxes

| Syntax | Example | Supported? | Reason |
|--------|---------|-----------|--------|
| `Depends on: #15` | ✅ Standard | ✅ | `depends\s+on` match |
| `depends on: #15, #16` | ✅ Comma-separated numbers | ✅ | `(?i)#(\d+)` captures ALL `#N` |
| `Depends on: #15 and #20` | ✅ "and" between numbers | ✅ | Both `#15` and `#20` match `#(\d+)` |
| `depends-on: #15` | ❌ Hyphenated keyword | ❌ | `\s+` (space) doesn't match hyphen |
| `Depends on:#15` | ❌ No space after colon | ✅ | `depends\s+on` still matches (before colon) |
| `- Depends on: #15` | ❌ Markdown bullet | ✅ | Line contains "depends on" |
| `Depends on: #15 #16` | ✅ Space-separated numbers | ✅ | Both `#15` and `#16` match |
| `Related issue: #15` | ❌ No depends keyword | ❌ | No keyword match |
| `Closes: #15` | ❌ Close keyword | ❌ | Not in keyword regex |
| `Fixes: #15` | ❌ Fix keyword | ❌ | Not in keyword regex |
| `Resolves: #15` | ❌ Resolve keyword | ❌ | Not in keyword regex |
| `Blocked by: #15` | ❌ Blocked by | ❌ | Not in keyword regex |
| `Requires: #15` | ❌ Requires keyword | ❌ | Not in keyword regex |
| `Tracking #15` | ❌ Tracking | ❌ | Not in keyword regex |
| `Subtask of #15` | ❌ Subtask | ❌ | Not in keyword regex |

### Severity: Medium
The keyword is too narrow. GitHub/GitLab conventions use `Closes`, `Fixes`, `Resolves`, `Relates to`, `Mentions`, `References`, `Depends on`, `Blocked by`, `Requires`, `Subtask of`. Fordjent only recognizes `depends on` (space-separated).

### Fix Approach

```go
// Replace the narrow keyword regex with a broad set
var dependencyKeywords = regexp.MustCompile(`(?i)(?:depends\s+on|depends-on|requires|blocked\s+by|needs|relates\s+to|subtask\s+of|tracking|references?|mentions?\s+as\s+(?:blocker|dependency|pre-requisite|prerequisite)|see\s+also)\s*:?|(?i)(?:closes?|fixes?|resolves?|refs?)\s*:?`)

// Or even broader: extract ALL #N references and let the caller decide which are dependencies
// vs. closing/fixing references.

// For Fordjent's use case (scheduler only cares about blocking deps), broaden to:
var dependencyKeywords = regexp.MustCompile(`(?i)(?:depends\s+on\s*:?|depends-on\s*:?|requires\s*:?|blocks\s+by\s*:?|blocked\s+by\s*:?|needs\s*:?|relies\s+on\s*:?|subtask\s+of\s*:?|tracking\s+of?\s*:?|parent\s+issue\s*:?|blocks?\s*:\s*?)`)
```

A secondary fix: also check `closingRefRe` (already defined but unused in scheduler) for `Closes/Fixes/Resolves`.

---

# Start Here

**Priority ordering:**
1. **Gap 2 (Symlink escape)** — Security risk, high severity. Fix `write_file` and `read_file` path resolution in `internal/tool/local_tools.go`.
2. **Gap 1 (Bash scope bypass)** — Medium-High. Replace regex heuristics with command-name whitelist in `internal/tool/local_tools.go`.
3. **Gap 6 (Dependency syntax)** — Medium. Expand `dependsOnKeywordRegex` in `internal/scheduler/scheduler.go`.
4. **Gap 3 (Transitive deps)** — Low-Medium. Only needs fixing if using deep PM dependency chains.
5. **Gap 5 (Restart protection)** — Low. Add `shutdown.json` checkpoint.
6. **Gap 4 (Stale cleanup)** — Low. Add workDir cleanup to `shutdownAll`.
