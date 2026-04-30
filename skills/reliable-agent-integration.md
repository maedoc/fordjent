---
title: Fordjent Reliable Agent Integration
skill_id: reliable-agent-integration
description: End-to-end validation of the reliable-agent layer across session manager, cost tracking, turn executor, lifecycle, stale gate, merge queue, and scaffold detection.
category: agent-automation
difficulty: advanced
deprecated: false
---

# Integration Test Plan for Reliable-Agent Layer

## Purpose
Validate that all 6 reliable-agent layer components work together in a single flow.

## Test 1: Session Lifecycle + Cost Tracking
- [ ] Start the binary (or use an in-memory event bus)              
- [ ] Fire `IssueOpened` event for `test/issue-1`                          
- [ ] Verify `lifecycle.db` shows `created` → `working` state transition
- [ ] Verify `costs.db` records the LLM call with correct tokens + cost
- [ ] Let agent run to `completed` or `failed_max_turns`
- [ ] Query `/status` and verify Prometheus metrics reflect the session

## Test 2: Stale Gate + Auto-Rebase
- [ ] Create branch `feature/stale-test` from old commit
- [ ] Push the branch (no PR yet)
- [ ] Fire `PullRequestOpened` event for that branch
- [ ] Verify `stalegate.IsStale()` returns `true` and blocks PR creation
- [ ] Run `git rebase origin/main` + `git push -f` via tool call
- [ ] Fire `PullRequestOpened` again → verify PR created successfully

## Test 3: Merge Queue + Dependency Unblocking
- [ ] Open 2 issues with `Depends on: #1` in body
- [ ] Open PR that closes issue #1
- [ ] Merge that PR
- [ ] Verify scheduler `OnPRMerged` removes `blocked` label and adds `ready` label on dependent issue

## Test 4: Scaffold Detection on Empty Repo
- [ ] Create repo with < 3 files
- [ ] Fire `IssueOpened` → verify scaffold issue created + `blocked` label applied

## Key Gotchas
- The binary needs config at runtime ( Docker bind-mount or env var )
- Cost tracker needs working LLM `Usage` response
- Git operations need `git` binary and valid remote
- All 6 state transitions must be enabled in config

## Commands
```bash
# Build
cd /home/duke/src/fordjent && CGO_ENABLED=0 go build -ldflags="-s -w" -o fordjent ./cmd/fordjent

# Run with local config
cp fordjent.local.yaml /tmp/fordjent.yaml && ./fordjent -config /tmp/fordjent.yaml

# Or Docker (requires config baked into image or bind-mounted)
docker build -t fordjent:integration .
```