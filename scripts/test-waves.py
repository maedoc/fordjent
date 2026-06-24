#!/usr/bin/env python3
"""Fordjent Multi-User Test Harness — Wave 7-12

Runs 6 waves of realistic multi-user testing against a local
Fordjent + Forgejo stack. Collects failure modes and metrics.

Usage:
  python3 test-waves.py [--wave N] [--skip-setup] [--wait SECS]
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
from dataclasses import dataclass, field
from typing import Optional

import requests

FJ_HOST = os.environ.get("FJ_HOST", "http://localhost:4230")
FJ_USER = os.environ.get("FJ_USER", "fjadmin")
FJ_PASS = os.environ.get("FJ_PASS", "fordjent-test")
FORDJENT_STATUS = os.environ.get("FORDJENT_STATUS", "http://localhost:8080/status")

# Reading list CLI source (seed for repos)
READING_LIST_SRC = "/tmp/reading-list-cli"


@dataclass
class WaveResult:
    wave: str
    success: bool
    prs_created: int = 0
    writes_made: int = 0
    commits_pushed: int = 0
    review_rounds: int = 0
    failure_modes: list = field(default_factory=list)
    notes: list = field(default_factory=list)
    timeline: list = field(default_factory=list)


def log(msg):
    ts = time.strftime("%H:%M:%S")
    print(f"[{ts}] {msg}", flush=True)


def fj_api(method, path, data=None, user=None, passwd=None):
    """Call Forgejo API."""
    u = user or FJ_USER
    p = passwd or FJ_PASS
    url = f"{FJ_HOST}/api/v1{path}"
    r = requests.request(method, url, json=data, auth=(u, p), timeout=30)
    return r


def docker_exec(cmd):
    """Run command inside fordjent container."""
    result = subprocess.run(
        ["docker", "exec", "fordjent", "sh", "-c", cmd],
        capture_output=True, text=True, timeout=30
    )
    return result.stdout + result.stderr


def docker_logs(grep_pattern, since="5m"):
    """Get recent fordjent logs matching pattern."""
    result = subprocess.run(
        ["docker", "logs", "--since", since, "fordjent"],
        capture_output=True, text=True, timeout=15
    )
    lines = []
    for line in result.stdout.splitlines():
        if grep_pattern in line:
            lines.append(line)
    return lines


def create_repo(name, seed_files=True):
    """Create a Forgejo repo with optional seed files."""
    r = fj_api("DELETE", f"/repos/fjadmin/{name}")
    time.sleep(2)
    
    r = fj_api("POST", "/user/repos", {"name": name, "private": False, "auto_init": True})
    if r.status_code not in (200, 201):
        log(f"  ⚠️ Create repo failed: {r.status_code}")
        return False

    # Add fordjent-yolo topic
    fj_api("PUT", f"/repos/fjadmin/{name}/topics", {"topics": ["fordjent-yolo"]})

    # Add labels
    for label_name, color in [("implementer", "33cc66"), ("ready", "00cc00")]:
        fj_api("POST", f"/repos/fjadmin/{name}/labels", {"name": label_name, "color": color})

    # Add collaborators
    for user in ["djent-pm", "djent-dev", "djent-qa"]:
        fj_api("PUT", f"/repos/fjadmin/{name}/collaborators/{user}", {"permission": "write"})

    # Add webhook
    fj_api("POST", f"/repos/fjadmin/{name}/hooks", {
        "type": "forgejo",
        "config": {
            "url": "http://fordjent:8080/acp/v1/events",
            "content_type": "json",
            "secret": "local-webhook-secret-12345"
        },
        "events": ["push", "issues", "issue_comment", "pull_request", "pull_request_review_comment", "check_run", "workflow_run"],
        "active": True
    })

    if seed_files:
        # Seed with reading-list CLI
        clone_dir = f"/tmp/wave-{name}"
        subprocess.run(["rm", "-rf", clone_dir], capture_output=True)
        clone_url = f"http://fjadmin:{FJ_PASS}@localhost:4230/fjadmin/{name}.git"
        subprocess.run(["git", "clone", clone_url, clone_dir], capture_output=True)
        for f in ["main.go", "main_test.go", "go.mod", ".gitignore"]:
            src = os.path.join(READING_LIST_SRC, f)
            dst = os.path.join(clone_dir, f)
            if os.path.exists(src):
                subprocess.run(["cp", src, dst], capture_output=True)
        subprocess.run(["git", "add", "-A"], cwd=clone_dir, capture_output=True)
        subprocess.run(["git", "commit", "-m", "Seed reading list CLI"], cwd=clone_dir, capture_output=True)
        subprocess.run(["git", "push"], cwd=clone_dir, capture_output=True)

    return True


def file_issue(repo, title, body):
    """File an issue and return the issue number."""
    r = fj_api("POST", f"/repos/{repo}/issues", {"title": title, "body": body})
    if r.status_code in (200, 201):
        num = r.json()["number"]
        log(f"  Issue #{num}: {title[:50]}")
        return num
    return None


def comment_on_pr(repo, pr_num, body, user=None, passwd=None):
    """Comment on a PR/issue."""
    r = fj_api("POST", f"/repos/{repo}/issues/{pr_num}/comments", {"body": body}, user=user, passwd=passwd)
    if r.status_code in (200, 201):
        log(f"  Comment on PR #{pr_num} by {user or FJ_USER}")
        return True
    log(f"  ⚠️ Comment failed: {r.status_code}")
    return False


def get_prs(repo, state="all"):
    """List PRs."""
    r = fj_api("GET", f"/repos/{repo}/pulls?state={state}")
    return r.json() if r.status_code == 200 else []


def get_pr_detail(repo, pr_num):
    """Get PR details."""
    r = fj_api("GET", f"/repos/{repo}/pulls/{pr_num}")
    return r.json() if r.status_code == 200 else {}


def get_session_trace(repo, session_key):
    """Get the tool call trace for a session."""
    raw = docker_exec(f"cat /var/lib/fordjent/work/{repo}/{session_key}/memory.jsonl 2>/dev/null")
    tools = {}
    write_paths = []
    git_actions = []
    comments = []
    for line in raw.splitlines():
        try:
            d = json.loads(line)
            if d.get("event_type") == "tool_call":
                tn = d.get("tool_name", "")
                tools[tn] = tools.get(tn, 0) + 1
                if tn == "write_file":
                    args = d.get("tool_args", {})
                    if isinstance(args, str):
                        args = json.loads(args)
                    write_paths.append(args.get("path", "?"))
                if tn == "git":
                    ta = str(d.get("tool_args", ""))[:50]
                    if "commit" in ta or "push" in ta:
                        git_actions.append(ta)
                if tn == "forgejo_comment":
                    args = d.get("tool_args", {})
                    if isinstance(args, str):
                        args = json.loads(args)
                    comments.append(str(args.get("body", ""))[:80])
        except (json.JSONDecodeError, TypeError):
            pass
    return {
        "tools": tools,
        "write_paths": write_paths,
        "git_actions": git_actions,
        "comments": comments,
        "total_tools": sum(tools.values()),
        "total_writes": tools.get("write_file", 0),
    }


def wait_for_pr(repo, timeout=600, poll=30):
    """Wait for a PR to be created."""
    start = time.time()
    while time.time() - start < timeout:
        prs = get_prs(repo, "open")
        if prs:
            return prs[0]["number"]
        time.sleep(poll)
    return None


def wait_for_pr_update(repo, pr_num, initial_sha, timeout=600, poll=30):
    """Wait for PR head SHA to change (indicating a push)."""
    start = time.time()
    while time.time() - start < timeout:
        pr = get_pr_detail(repo, pr_num)
        new_sha = pr.get("head", {}).get("sha", "")
        if new_sha and new_sha != initial_sha:
            return new_sha
        time.sleep(poll)
    return None


# =============================================================================
# WAVE 1: The Demanding Stakeholder
# =============================================================================
def run_wave1():
    """3 rounds of review feedback on a single PR."""
    result = WaveResult(wave="W1-Demanding-Stakeholder", success=False)
    repo = "fjadmin/wave1-stakeholder"

    log("📦 WAVE 1: The Demanding Stakeholder")
    result.timeline.append(f"t=0: Setup repo")

    if not create_repo("wave1-stakeholder"):
        result.failure_modes.append("F0: Repo creation failed")
        return result

    # File issue
    issue = file_issue(repo, "Add a search command",
        "Add a search command to find books by title substring.\n\nUsage: reading-list search <query>\nIt should return all books whose title contains the query (case-insensitive).\nPrint each matching book on its own line.")
    if not issue:
        result.failure_modes.append("F0: Issue creation failed")
        return result

    result.timeline.append(f"t=0: Issue #{issue} filed")

    # Wait for PR
    pr = wait_for_pr(repo, timeout=600)
    if not pr:
        result.failure_modes.append("F1: No PR created (exploration loop?)")
        # Check session trace for exploration
        trace = get_session_trace(repo, f"issues/{issue}")
        result.notes.append(f"Issue session tools: {trace['tools']}")
        return result

    result.prs_created += 1
    pr_detail = get_pr_detail(repo, pr)
    initial_sha = pr_detail.get("head", {}).get("sha", "?")[:8]
    result.timeline.append(f"t=5m: PR #{pr} created (sha={initial_sha})")

    # Round 1: Add error handling
    log("  🔄 Review Round 1: Add error handling")
    comment_on_pr(repo, pr,
        "Good start! But the search command needs better error handling:\n\n"
        "1. If no query is provided, print a helpful message (not just exit 1)\n"
        "2. If no books match, print 'No books found matching \"<query>\"' instead of nothing\n\n"
        "Push fixes to the same branch.")
    
    # Check that fix session is created
    time.sleep(10)
    fix_logs = docker_logs("review-fix")
    if not fix_logs:
        result.failure_modes.append("F6: Review fix session not created for round 1")
        result.timeline.append("t=5m: ❌ No fix session created")
    else:
        result.timeline.append("t=5m: Fix session created for round 1")

    # Wait for push
    new_sha = wait_for_pr_update(repo, pr, initial_sha, timeout=420)
    if new_sha:
        result.review_rounds += 1
        result.commits_pushed += 1
        result.timeline.append(f"t=10m: Round 1 fix pushed (sha {initial_sha}→{new_sha[:8]})")
    else:
        result.failure_modes.append("F3: Round 1 fix not pushed")
        # Check trace
        trace = get_session_trace(repo, f"pulls/{pr}-fix")
        result.notes.append(f"Round 1 fix trace: writes={trace['total_writes']}, git={trace['git_actions']}")

    # Round 2: Add logging
    log("  🔄 Review Round 2: Add logging")
    current_sha = new_sha or initial_sha
    comment_on_pr(repo, pr,
        "Almost there! One more thing:\n\n"
        "Add a count at the end of search results: 'Found N book(s) matching \"<query>\"'\n"
        "This should be printed after the list of results.\n\n"
        "Push to the same branch.")

    time.sleep(10)
    new_sha2 = wait_for_pr_update(repo, pr, current_sha, timeout=420)
    if new_sha2:
        result.review_rounds += 1
        result.commits_pushed += 1
        result.timeline.append(f"t=15m: Round 2 fix pushed (sha {current_sha[:8]}→{new_sha2[:8]})")
    else:
        result.failure_modes.append("F3: Round 2 fix not pushed")
        trace = get_session_trace(repo, f"pulls/{pr}-fix-1")  # might be a new session
        if not trace["tools"]:
            trace = get_session_trace(repo, f"pulls/{pr}-fix")
        result.notes.append(f"Round 2 fix trace: writes={trace['total_writes']}")

    # Round 3: Looks good + add test
    log("  🔄 Review Round 3: Add test")
    current_sha2 = new_sha2 or current_sha
    comment_on_pr(repo, pr,
        "The implementation looks good now! Just one final thing:\n\n"
        "Add a unit test for Search that verifies case-insensitive matching and the 'no results' case.\n\n"
        "Push to the same branch and we can merge.")

    time.sleep(10)
    new_sha3 = wait_for_pr_update(repo, pr, current_sha2, timeout=420)
    if new_sha3:
        result.review_rounds += 1
        result.commits_pushed += 1
        result.timeline.append(f"t=20m: Round 3 fix pushed (sha {current_sha2[:8]}→{new_sha3[:8]})")
    else:
        result.failure_modes.append("F3: Round 3 fix not pushed")
        result.notes.append("Round 3 fix: no SHA change detected")

    result.success = result.review_rounds >= 2  # At least 2 of 3 rounds must produce commits
    return result


# =============================================================================
# WAVE 2: Design Debate
# =============================================================================
def run_wave2():
    """Two humans disagree on the implementation approach."""
    result = WaveResult(wave="W2-Design-Debate", success=False)
    repo = "fjadmin/wave2-debate"

    log("📦 WAVE 2: Design Debate")
    result.timeline.append("t=0: Setup repo")

    if not create_repo("wave2-debate"):
        result.failure_modes.append("F0: Repo creation failed")
        return result

    issue = file_issue(repo, "Optimize book lookups",
        "The ReadingList currently uses a linear scan for MarkRead and Delete. "
        "Optimize the lookups to be O(1) by adding an index or map.\n\n"
        "Make sure all existing tests still pass after the change.")
    if not issue:
        return result

    result.timeline.append(f"t=0: Issue #{issue} filed")

    pr = wait_for_pr(repo, timeout=600)
    if not pr:
        result.failure_modes.append("F1: No PR created")
        return result

    result.prs_created += 1
    pr_detail = get_pr_detail(repo, pr)
    initial_sha = pr_detail.get("head", {}).get("sha", "?")[:8]
    result.timeline.append(f"t=5m: PR #{pr} created (sha={initial_sha})")

    # Human A: Use a map
    comment_on_pr(repo, pr,
        "I see you're still using a slice scan. Please switch to a map[string]int "
        "for O(1) lookups by title. The map should be a field on ReadingList and "
        "kept in sync with the Books slice.",
        user=FJ_USER, passwd=FJ_PASS)

    time.sleep(10)
    result.timeline.append("t=5m: Human A says 'use a map'")

    # Wait for first fix
    new_sha = wait_for_pr_update(repo, pr, initial_sha, timeout=420)
    if new_sha:
        result.commits_pushed += 1
        result.review_rounds += 1
        result.timeline.append(f"t=10m: Fix pushed after Human A feedback")
    else:
        result.failure_modes.append("F3: Fix not pushed after Human A feedback")

    # Human B: Disagrees — keep the slice
    current_sha = new_sha or initial_sha
    comment_on_pr(repo, pr,
        "Wait, I disagree with using a map. A map doesn't preserve insertion order, "
        "and our List command needs to show books in the order they were added. "
        "Keep the slice but just add a helper method that does binary search on the "
        "sorted slice. That's O(log N) which is good enough, and preserves order.",
        user="fjadmin", passwd=FJ_PASS)  # Same user, different intent

    time.sleep(10)
    result.timeline.append("t=10m: Human B says 'keep slice, add binary search'")

    new_sha2 = wait_for_pr_update(repo, pr, current_sha, timeout=420)
    if new_sha2:
        result.commits_pushed += 1
        result.review_rounds += 1
        result.timeline.append("t=15m: Fix pushed after Human B feedback")
    else:
        result.failure_modes.append("F3: Fix not pushed after Human B feedback")
        result.failure_modes.append("F12: Conflicting feedback confused agent")

    # Check final code for approach
    clone_dir = f"/tmp/wave2-debate"
    subprocess.run(["rm", "-rf", clone_dir], capture_output=True)
    clone_url = f"http://fjadmin:{FJ_PASS}@localhost:4230/fjadmin/wave2-debate.git"
    subprocess.run(["git", "clone", clone_url, clone_dir], capture_output=True)
    subprocess.run(["git", "fetch", "-p"], cwd=clone_dir, capture_output=True)
    # Get PR branch
    pr_info = get_pr_detail(repo, pr)
    branch = pr_info.get("head", {}).get("ref", "?")
    if branch != "?":
        subprocess.run(["git", "checkout", branch], cwd=clone_dir, capture_output=True)
    
    main_go = os.path.join(clone_dir, "main.go")
    if os.path.exists(main_go):
        with open(main_go) as f:
            code = f.read()
        has_map = "map[string]" in code
        has_binary = "sort.Search" in code or "binary" in code.lower()
        result.notes.append(f"Implementation: map={has_map}, binary_search={has_binary}")

    # Check for conflicting feedback handling
    # Get comments from the agent
    r = fj_api("GET", f"/repos/{repo}/issues/{pr}/comments")
    if r.status_code == 200:
        agent_comments = [c for c in r.json() if "djent" in c.get("user", {}).get("login", "")]
        for c in agent_comments[-3:]:
            body = c.get("body", "")[:200]
            result.notes.append(f"Agent comment: {body}")

    result.success = result.review_rounds >= 2
    return result


# =============================================================================
# WAVE 3: PM Decomposition + Sequential Implementation
# =============================================================================
def run_wave3():
    """PM decomposes a feature into sub-issues, each implemented."""
    result = WaveResult(wave="W3-PM-Decomposition", success=False)
    repo = "fjadmin/wave3-pmflow"

    log("📦 WAVE 3: PM Decomposition + Sequential Implementation")
    result.timeline.append("t=0: Setup repo")

    if not create_repo("wave3-pmflow"):
        result.failure_modes.append("F0: Repo creation failed")
        return result

    # File a PM issue
    issue = file_issue(repo, "[pm] Implement book rating and statistics features",
        "Our reading list app needs rating and statistics features. Here's what I want:\n\n"
        "1. Rate books: 1-5 star rating per book\n"
        "2. Average rating: calculate average rating across all books\n"
        "3. Statistics: show count by status (read/unread) and average rating\n\n"
        "Please decompose this into implementer sub-issues with appropriate role tags.")
    if not issue:
        return result

    result.timeline.append(f"t=0: PM Issue #{issue} filed")

    # Wait for PM to create sub-issues (give it time)
    log("  ⏳ Waiting for PM to decompose...")
    time.sleep(300)  # 5 minutes for PM session

    # Check sub-issues
    r = fj_api("GET", f"/repos/{repo}/issues?state=open")
    all_issues = r.json() if r.status_code == 200 else []
    pm_issue = next((i for i in all_issues if i["number"] == issue), None)
    sub_issues = [i for i in all_issues if i["number"] != issue]

    result.notes.append(f"Sub-issues created: {len(sub_issues)}")
    for si in sub_issues:
        tags = ""
        if "[implementer]" in si.get("title", "").lower():
            tags += "[implementer] "
        if "[dev]" in si.get("title", "").lower():
            tags += "[dev] "
        result.notes.append(f"  #{si['number']}: {si['title'][:60]} {tags}")

    # Check if PM included role tags
    has_tags = any(
        "[implementer]" in si.get("title", "").lower() or "[dev]" in si.get("title", "").lower()
        for si in sub_issues
    )
    if not has_tags and sub_issues:
        result.failure_modes.append("F2: PM didn't include role tags in sub-issues")

    result.timeline.append(f"t=5m: PM created {len(sub_issues)} sub-issues")

    # Wait for first implementer PR (if sub-issues exist)
    if sub_issues:
        time.sleep(300)
        prs = get_prs(repo, "all")
        result.prs_created = len(prs)
        for p in prs:
            result.timeline.append(f"t=10m: PR #{p['number']} created: {p['title'][:50]}")
        
        # Check implementer traces
        for si in sub_issues[:2]:
            trace = get_session_trace(repo, f"issues/{si['number']}")
            if trace["total_writes"] > 0:
                result.writes_made += trace["total_writes"]
            else:
                result.failure_modes.append(f"F1: Sub-issue #{si['number']} didn't write code")

    result.success = len(sub_issues) >= 1 and result.prs_created >= 1
    return result


# =============================================================================
# WAVE 4: Bug Report + Verification + Enhancement
# =============================================================================
def run_wave4():
    """Vague bug report → fix → verification → regression test request."""
    result = WaveResult(wave="W4-Bug-Verification", success=False)
    repo = "fjadmin/wave4-bugverify"

    log("📦 WAVE 4: Bug Report + Verification + Enhancement")
    result.timeline.append("t=0: Setup repo")

    if not create_repo("wave4-bugverify"):
        result.failure_modes.append("F0: Repo creation failed")
        return result

    # File a VAGUE bug report (like a real user would)
    issue = file_issue(repo, "Sort command breaks when list is empty",
        "When I run reading-list sort on an empty list, it crashes. Please fix it.\n\n"
        "Also the list command shows nothing when there are no books — it should say something like 'No books in list'.")
    if not issue:
        return result

    result.timeline.append(f"t=0: Vague bug report #{issue} filed")

    pr = wait_for_pr(repo, timeout=600)
    if not pr:
        result.failure_modes.append("F1: No PR created for bug fix")
        return result

    result.prs_created += 1
    pr_detail = get_pr_detail(repo, pr)
    initial_sha = pr_detail.get("head", {}).get("sha", "?")[:8]
    result.timeline.append(f"t=5m: Fix PR #{pr} created (sha={initial_sha})")

    # Human verifies and asks for more
    comment_on_pr(repo, pr,
        "I tested this and the sort crash is fixed. Good work.\n\n"
        "But I noticed the 'empty list' message for the list command isn't quite right. "
        "Instead of 'No books in list', it says 'List is empty'. Can you change it to "
        "'No books in list. Use \"reading-list add\" to add one.' ?\n\n"
        "Also, please add a regression test that verifies the sort doesn't crash on empty list, "
        "and that the empty list message is correct.",
        user=FJ_USER, passwd=FJ_PASS)

    time.sleep(10)
    result.timeline.append("t=5m: Verification comment + enhancement request")

    new_sha = wait_for_pr_update(repo, pr, initial_sha, timeout=420)
    if new_sha:
        result.commits_pushed += 1
        result.review_rounds += 1
        result.timeline.append(f"t=10m: Enhancement pushed (sha {initial_sha}→{new_sha[:8]})")
    else:
        result.failure_modes.append("F3: Enhancement not pushed after verification comment")
        # Check fix session
        trace = get_session_trace(repo, f"pulls/{pr}-fix")
        result.notes.append(f"Fix trace: writes={trace['total_writes']}, tools={trace['tools']}")

    # Verify the regression test exists in the code
    clone_dir = f"/tmp/wave4-bugverify"
    subprocess.run(["rm", "-rf", clone_dir], capture_output=True)
    clone_url = f"http://fjadmin:{FJ_PASS}@localhost:4230/fjadmin/wave4-bugverify.git"
    subprocess.run(["git", "clone", clone_url, clone_dir], capture_output=True)
    subprocess.run(["git", "fetch", "-p"], cwd=clone_dir, capture_output=True)
    pr_info = get_pr_detail(repo, pr)
    branch = pr_info.get("head", {}).get("ref", "?")
    if branch != "?":
        subprocess.run(["git", "checkout", branch], cwd=clone_dir, capture_output=True)

    test_file = os.path.join(clone_dir, "main_test.go")
    if os.path.exists(test_file):
        with open(test_file) as f:
            test_code = f.read()
        has_sort_empty = "Sort" in test_code and ("empty" in test_code.lower() or "0 book" in test_code.lower())
        has_empty_msg = "No books in list" in test_code or "no books" in test_code.lower()
        if not has_sort_empty:
            result.failure_modes.append("F2: No regression test for sort-on-empty")
        if not has_empty_msg:
            result.failure_modes.append("F2: Empty list message not tested")
        result.notes.append(f"Regression test: sort_empty={has_sort_empty}, empty_msg={has_empty_msg}")

    result.success = result.review_rounds >= 1
    return result


# =============================================================================
# WAVE 5: Parallel PRs + Cross-Review
# =============================================================================
def run_wave5():
    """Two simultaneous issues → two PRs → cross-review complexity."""
    result = WaveResult(wave="W5-Parallel-CrossReview", success=False)
    repo = "fjadmin/wave5-parallel"

    log("📦 WAVE 5: Parallel PRs + Cross-Review")
    result.timeline.append("t=0: Setup repo")

    if not create_repo("wave5-parallel"):
        result.failure_modes.append("F0: Repo creation failed")
        return result

    # File TWO issues simultaneously
    issue1 = file_issue(repo, "Add count command",
        "Add a 'count' command that shows the total number of books.\n"
        "Usage: reading-list count\n"
        "Output: 'You have N book(s) in your reading list.'")
    issue2 = file_issue(repo, "Add help command",
        "Add a 'help' command that lists all available commands with brief descriptions.\n"
        "Usage: reading-list help\n"
        "It should show each command name and what it does in one line.")

    if not issue1 or not issue2:
        result.failure_modes.append("F0: Issue creation failed")
        return result

    result.timeline.append(f"t=0: Issues #{issue1} and #{issue2} filed simultaneously")

    # Wait for PRs
    time.sleep(420)
    prs = get_prs(repo, "all")
    result.prs_created = len(prs)

    for p in prs:
        result.timeline.append(f"t=7m: PR #{p['number']}: {p['title'][:50]} (state={p['state']})")

    if len(prs) < 2:
        result.failure_modes.append(f"F1: Only {len(prs)} PR(s) created (expected 2)")
        trace1 = get_session_trace(repo, f"issues/{issue1}")
        trace2 = get_session_trace(repo, f"issues/{issue2}")
        result.notes.append(f"Issue #{issue1} trace: writes={trace1['total_writes']}")
        result.notes.append(f"Issue #{issue2} trace: writes={trace2['total_writes']}")

    # Review the first PR
    if prs:
        pr1 = prs[0]
        pr1_sha = pr1.get("head", {}).get("sha", "?")[:8]
        comment_on_pr(repo, pr1["number"],
            "Thanks for the implementation. Please also add the count of read vs unread books:\n"
            "'You have N book(s): R read, U unread.'\n\n"
            "Push to the same branch.")
        result.timeline.append(f"t=7m: Review comment on PR #{pr1['number']}")

        # Wait for fix
        time.sleep(300)
        pr1_updated = get_pr_detail(repo, pr1["number"])
        new_sha = pr1_updated.get("head", {}).get("sha", "?")[:8]
        if new_sha != pr1_sha:
            result.review_rounds += 1
            result.commits_pushed += 1
            result.timeline.append(f"t=12m: PR #{pr1['number']} updated (sha {pr1_sha}→{new_sha})")
        else:
            result.failure_modes.append("F3: PR not updated after review")

    # Check for merge queue issues
    merge_logs = docker_logs("merge queue blocked", since="15m")
    if merge_logs:
        result.failure_modes.append("F10: Merge queue blocked PR(s)")
        result.notes.append(f"Merge queue blocks: {len(merge_logs)}")

    # Check for cross-contamination
    for p in prs:
        pr_files = fj_api("GET", f"/repos/{repo}/pulls/{p['number']}/files")
        if pr_files.status_code == 200:
            files = [f["filename"] for f in pr_files.json()]
            result.notes.append(f"PR #{p['number']} files: {files}")

    result.success = result.prs_created >= 2
    return result


# =============================================================================
# WAVE 6: The Silent Partner
# =============================================================================
def run_wave6():
    """Agent creates PR → human merges → second agent sees merged code."""
    result = WaveResult(wave="W6-Silent-Partner", success=False)
    repo = "fjadmin/wave6-silent"

    log("📦 WAVE 6: The Silent Partner")
    result.timeline.append("t=0: Setup repo")

    if not create_repo("wave6-silent"):
        result.failure_modes.append("F0: Repo creation failed")
        return result

    # File first issue
    issue1 = file_issue(repo, "Add a clear command",
        "Add a 'clear' command that removes all books from the reading list.\n"
        "Usage: reading-list clear\n"
        "It should print 'Cleared N book(s) from your list.' after removing them.")
    if not issue1:
        return result

    result.timeline.append(f"t=0: Issue #{issue1} filed")

    # Wait for PR
    pr = wait_for_pr(repo, timeout=600)
    if not pr:
        result.failure_modes.append("F1: No PR created for issue 1")
        return result

    result.prs_created += 1
    result.timeline.append(f"t=5m: PR #{pr} created")

    # Human merges the PR silently (via API, no comment)
    merge_result = fj_api("POST", f"/repos/{repo}/pulls/{pr}/merge", {
        "Do": "merge",
        "merge_commit_title": f"Merge PR #{pr}",
        "merge_message": "auto"
    })
    if merge_result.status_code == 200:
        result.timeline.append(f"t=6m: PR #{pr} merged by human (silent)")
    else:
        result.notes.append(f"Merge failed: {merge_result.status_code} {merge_result.text[:100]}")
        # Try direct push to main instead
        clone_dir = f"/tmp/wave6-silent-merge"
        subprocess.run(["rm", "-rf", clone_dir], capture_output=True)
        clone_url = f"http://fjadmin:{FJ_PASS}@localhost:4230/fjadmin/wave6-silent.git"
        subprocess.run(["git", "clone", clone_url, clone_dir], capture_output=True)
        subprocess.run(["git", "fetch", "-p"], cwd=clone_dir, capture_output=True)
        branch = get_pr_detail(repo, pr).get("head", {}).get("ref", "?")
        if branch != "?":
            subprocess.run(["git", "checkout", branch], cwd=clone_dir, capture_output=True)
            subprocess.run(["git", "checkout", "main"], cwd=clone_dir, capture_output=True)
            subprocess.run(["git", "merge", branch], cwd=clone_dir, capture_output=True)
            subprocess.run(["git", "push"], cwd=clone_dir, capture_output=True)
            result.timeline.append(f"t=6m: PR #{pr} merged via direct push")

    time.sleep(10)

    # File second issue that should see the clear command
    issue2 = file_issue(repo, "Add an undo command",
        "Add an 'undo' command that restores the last removed book.\n"
        "This should work with both 'delete' and 'clear' commands.\n"
        "Usage: reading-list undo\n"
        "It should print 'Restored: <title>' or 'Nothing to undo'.")
    if not issue2:
        return result

    result.timeline.append(f"t=7m: Issue #{issue2} filed (should see clear command in codebase)")

    # Wait for second PR — the agent should see the clear command on main
    time.sleep(420)
    prs = get_prs(repo, "all")
    new_prs = [p for p in prs if p["number"] != pr]
    if new_prs:
        result.prs_created += 1
        result.timeline.append(f"t=14m: Second PR #{new_prs[0]['number']} created: {new_prs[0]['title'][:50]}")

        # Check if the second agent's code references or builds on the clear command
        clone_dir = f"/tmp/wave6-silent2"
        subprocess.run(["rm", "-rf", clone_dir], capture_output=True)
        clone_url = f"http://fjadmin:{FJ_PASS}@localhost:4230/fjadmin/wave6-silent.git"
        subprocess.run(["git", "clone", clone_url, clone_dir], capture_output=True)
        subprocess.run(["git", "fetch", "-p"], cwd=clone_dir, capture_output=True)
        branch = new_prs[0].get("head", {}).get("ref", "?")
        if branch != "?":
            subprocess.run(["git", "checkout", branch], cwd=clone_dir, capture_output=True)
        
        main_go = os.path.join(clone_dir, "main.go")
        if os.path.exists(main_go):
            with open(main_go) as f:
                code = f.read()
            has_clear = "clear" in code.lower() or "Clear" in code
            has_undo = "undo" in code.lower() or "Undo" in code
            result.notes.append(f"Second PR code: has_clear_ref={has_clear}, has_undo={has_undo}")
            if not has_clear:
                result.failure_modes.append("F11: Agent didn't see merged clear command")
    else:
        result.failure_modes.append("F1: No second PR created")
        trace2 = get_session_trace(repo, f"issues/{issue2}")
        result.notes.append(f"Issue #{issue2} trace: writes={trace2['total_writes']}, tools={trace2['tools']}")

    result.success = result.prs_created >= 2
    return result


# =============================================================================
# MAIN
# =============================================================================
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--wave", type=int, help="Run specific wave (1-6)")
    parser.add_argument("--skip-setup", action="store_true", help="Skip repo setup")
    parser.add_argument("--wait", type=int, default=300, help="Base wait time in seconds")
    args = parser.parse_args()

    # Ensure seed files exist
    if not os.path.exists(READING_LIST_SRC):
        log(f"⚠️  Seed directory {READING_LIST_SRC} not found, creating reading-list CLI...")
        os.makedirs(READING_LIST_SRC, exist_ok=True)
        # We'll rely on the repo being pre-seeded via API instead

    waves = {
        1: run_wave1,
        2: run_wave2,
        3: run_wave3,
        4: run_wave4,
        5: run_wave5,
        6: run_wave6,
    }

    results = {}

    if args.wave:
        waves_to_run = [args.wave]
    else:
        waves_to_run = [1, 2, 3, 4, 5, 6]

    for w in waves_to_run:
        log(f"\n{'='*70}")
        log(f"Starting Wave {w}")
        log(f"{'='*70}")
        try:
            results[w] = waves[w]()
        except Exception as e:
            log(f"  💥 Wave {w} crashed: {e}")
            results[w] = WaveResult(wave=f"W{w}-CRASHED", success=False,
                                     failure_modes=[f"F0: Exception: {e}"])

    # Print summary
    print(f"\n{'='*70}")
    print("COMPREHENSIVE TEST RESULTS")
    print(f"{'='*70}")
    
    all_failures = {}
    for w, r in sorted(results.items()):
        status = "✅ PASS" if r.success else "❌ FAIL"
        print(f"\nWave {w} ({r.wave}): {status}")
        print(f"  PRs: {r.prs_created} | Writes: {r.writes_made} | Commits: {r.commits_pushed} | Review rounds: {r.review_rounds}")
        if r.failure_modes:
            for fm in r.failure_modes:
                all_failures[fm] = all_failures.get(fm, 0) + 1
                print(f"  ⚠️  {fm}")
        for n in r.notes:
            print(f"  📝 {n}")
        for t in r.timeline:
            print(f"  ⏱  {t}")

    print(f"\n{'='*70}")
    print("FAILURE MODE FREQUENCY")
    print(f"{'='*70}")
    for fm, count in sorted(all_failures.items(), key=lambda x: -x[1]):
        print(f"  {count}x  {fm}")

    # Save results to file
    with open("/tmp/fordjent-wave-results.json", "w") as f:
        json.dump({str(k): {"wave": v.wave, "success": v.success,
                            "prs_created": v.prs_created, "writes_made": v.writes_made,
                            "commits_pushed": v.commits_pushed, "review_rounds": v.review_rounds,
                            "failure_modes": v.failure_modes, "notes": v.notes,
                            "timeline": v.timeline}
                   for k, v in results.items()}, f, indent=2)
    log("\nResults saved to /tmp/fordjent-wave-results.json")


if __name__ == "__main__":
    main()
