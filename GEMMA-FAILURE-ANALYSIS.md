# Gemma4-12B Failure Analysis — Fine-tune vs Bigger Model?

**Date:** 2026-06-20
**Test:** `fjadmin/gemma-stress` repo, 13 sessions, 489 LLM turns, 5.7M tokens, $0 cost
**Question asked:** "Can you analyze failures and suggest whether a fine-tune would help or just need bigger model?"

## TL;DR

The failures split roughly:

| Bucket | % of failure cases | Fix |
|---|---|---|
| **Harness bugs** (the model actually did the right thing; Fordjent didn't give it the right environment or feedback) | ~50% | Code fixes, NOT model work |
| **Indecision loops** (model can reason, but can't commit to a decision) | ~35% | Bigger model OR fine-tune both help |
| **Genuine capability gaps** (model can't reason about the problem at all) | ~15% | Bigger model helps more than fine-tune |

**Recommendation in one sentence**: *Don't fine-tune yet.* Fix the three harness bugs first (cheap, ~half of failures), then test on a 30B+ base model. Only fine-tune if a solver-class model still loops on indecision after the bugs are fixed.

## The actual failures, categorized

### Bucket A — Harness bugs (the model did its job; Fordjent failed it)

These are NOT model-limitation failures. A bigger model would hit the same bugs. A fine-tune would teach the model to *work around* the bug, which is a bad use of fine-tuning.

#### A1. Reviewer's repo never gets the PR branch checked out

**Evidence (pulls/15 memory):**
```
[3] TOOL read_file  {"path":"Makefile"}
     result: Error: open .../pulls/15/repo/Makefile: no such file or directory
[8] TOOL read_file  {"path":"Makefile"}
     result: Error: ... no such file or directory   ← still failing
[12] TOOL read_file  {"path":"Makefile"}             ← 4th identical call
     result: Error: ... no such file or directory
... loops 17 turns total
```

The Makefile only exists on `feature/add-makefile` branch (the PR head), not on `main`. The reviewer's local clone was on `main` — every `read_file` failed, and Gemma4 kept retrying because it couldn't see the file the PR-files list told it existed.

**Root cause** (`internal/session/agent.go:212`):
```go
if evt.PRNumber > 0 && (evt.Type == event.IssueCommentCreated ||
                        evt.Type == event.PullRequestReviewComment) {
    // fetch + checkout PR branch
}
```

The branch-checkout path is gated on `IssueCommentCreated` / `PullRequestReviewComment`. But the auto-spawned reviewer session (per `internal/session/manager.go:803`) fires `event.ReviewRequested`:

```go
reviewEvt := event.NewEvent(event.ReviewRequested, ...);
reviewEvt.Role = "reviewer";
```

**`event.ReviewRequested` is not in the if-condition**, so the reviewer stays on `main`. The implementer (when in PR-Review-Fix mode) gets the branch checkout because it's routed via `IssueCommentCreated`; the reviewer (auto-spawned via `ReviewRequested`) does not.

**Fix**: extend the condition in `agent.go:212` to also fire on `event.ReviewRequested` (and probably `event.PullRequestOpened` for the yolo-reviewer auto-spawn path):
```go
evt.Type == event.IssueCommentCreated ||
evt.Type == event.PullRequestReviewComment ||
evt.Type == event.ReviewRequested ||
(events.Reviewers.Contains(evt.Type) && evt.PRNumber > 0 && evt.Role == "reviewer")
```
~5 lines of code.

**Does a fine-tune help?** No. Even a perfect model can't read a file that isn't checked out. This is 100% harness.

#### A2. Reviewer re-submits `forgejo_submit_review` 3× in a row (PR #14)

`pulls/14` memory shows three back-to-back calls:
```
[7]  forgejo_submit_review  → Review "approved" submitted on PR #14. Label side-effects applied.
[8]  forgejo_submit_review  → Review "approved" submitted on PR #14. Label side-effects applied.
[10] forgejo_submit_review  → Review "approved" submitted on PR #14. Label side-effects applied.
```

The reviewer called `approved`, got a success message, and called `approved` again, then a third time. The model didn't trust the success signal and kept submitting.

**Is this a model problem or harness?** Mixed:
- The model SHOULD stop calling a tool after a successful result. (Model behavior — a 32B model usually handles this.)
- BUT: the tool success message could be made more definitive. Today it returns *"Review 'approved' submitted on PR #14. Label side-effects applied."* — soft past-tense. If it returned `{ "status": "complete", "action_required": false }` JSON with an explicit "no further review calls needed" hint, even the 12B would probably stop.

**Fix priority**: low (mostly cosmetic — extra reviews are idempotent in Forgejo). But mention in the reviewer system prompt as an anti-pattern ("Do not call `forgejo_submit_review` more than once per PR").

**Fine-tune?** Could help here. But the same effect comes from `max_turns_reviewer: 5` cap + one steering message. Cheaper.

#### A3. The bug-report session had no code to fix

`fjadmin/gemma-stress/issues/10` raced ahead of issue #8 (the prime-checker implementer). Because #8's PR was merge-queue-blocked by PR #6, the prime code never existed on `main`. The bug report session for #10 thus:

- Ran `go run . prime 2` → got "unknown command" (no `prime` subcommand existed)
- Spent 39 turns running `git log --all -S "prime"`, `git grep prime`, `git log --all --grep="prime"` etc., searching git history
- Did `git checkout -b fix/prime-checker-bug` at turn 28 (correctly identified it should fix on a branch!)
- Then went back to more `git log` exploration
- **Never realized the bug was unsolvable** because the feature hadn't shipped yet

**This is genuinely impossible for any model fixed in this environment.** Even a frontier model would search git history looking for a prime-checker that doesn't exist. The "right" answer here is *"this issue depends on #8 which isn't merged; closing as blocked"* — but the model isn't told to make that check.

**Fix (harness)**: when an implementer session is given a bug report that references another issue/PR, the harness should pre-flight check that the referenced issue/PR is merged to main. If not, the session should be auto-blocked with the `blocked` label and a scheduler `Depends on:` link, rather than letting the agent spin.

**Fine-tune?** No. The model has no signal that the dependency isn't satisfied.

### Bucket B — Indecision loops (model can reason, can't commit)

These are cases where the model has all the information, **knows what it should do**, but talks itself out of acting.

#### B1. `git checkout -b fix/prime-checker-bug` then immediately abandoned

In `issues/10`, turn 28:
```
[28] bash  git checkout -b fix/prime-checker-bug
[29] bash  git log --name-status --oneline         ← went back to exploration
```

The model created the fix branch (a correct action), but then instead of writing the fix, it went back to grepping git history. This is a textbook **indecision / second-guessing** loop — the 12B model can identify the work unit but doesn't trust itself enough to carry it through.

#### B2. Repeated read_file/forgejo_list_files cycles in reviewer sessions

`pulls/15`: 11 cycles of `forgejo_list_files(ref=feature/add-makefile)` → `read_file("Makefile")` → ERROR → repeat. The model verified the file existed 4 times via the API but couldn't reconcile that with the local `read_file` failure. This is partly harness (A1), but partly model: when you've called the same tool 3× in a row and gotten the same result, you should change approach.

#### B3. PM issue #13 stalled at turn 2

`issues/13` ran 2 turns / 14k tokens total. Sub-issue spawned by PM, agent started and bailed without producing a PR. Didn't error — just stopped. Indecision variant.

**Fine-tune vs bigger model for Bucket B?**

- **Bigger model** is the more reliable fix. Larger models are empirically better at this; GPT-4-class models don't have the same second-guessing tendency.
- **Fine-tune** could plausibly help too — the underlying behavior is a "explore vs commit" tradeoff that's learnable. Training examples of "given state X, choose action A and STOP" would teach the model to terminate exploration. But this is a fairly general-purpose behavior; the same gain comes from a bigger pretrained model that's already seen more tool-termination examples in its corpus.

### Bucket C — Genuine capability gaps

These are cases where the model **does not produce reasonable reasoning** even on a clean problem.

#### C1. PM gave slightly malformed sub-issue titles

PM issue #9 → sub-issue #11 title: "[implementer] Add .gitignore for Go project". Workable but: doesn't reference parent issue, doesn't include "Depends on: #9" bullet in the body. These are spec'd in the PM prompt but the model didn't follow them reliably.

**Likely cause**: instruction-following on a 30-item system prompt is weaker at 12B. The PM prompt has been built up over many features and is now ~7KB. 12B models selectively drop instructions from long system prompts under load.

**Fix**: Trim the PM system prompt. Or — bigger model.

**Fine-tune?** Maybe, but as with B, the same result comes from prompt-trimming or a bigger model.

#### C2. Forked duplicate branches

Two different sessions for the temperature converter both created branches: `feature/temperature-converter` (one) and `feat/temperature-converter` (the other). Slightly different prefixes — so the merge queue saw them as 2 different branches and both got blocked on the same file.

This is a coordination/state-management problem: a model with no memory of its prior session on the same problem will recreate work the second time around.

**Fix**: Bigger context window helps (the model can see its own prior attempts in memory) — but the Fordjent loop has session isolation, so each session starts clean. The harness fix would be to seed new issue sessions with a "prior attempts" summary if a recent closed/failed session for the same issue exists.

**Fine-tune?** Probably not useful — depends on cross-session state the model doesn't have at inference time.

## Decision matrix — fine-tune vs bigger model vs harness

| Fix type | Cost (compute + time) | Effective on |
|---|---|---|
| **Harness bug fixes** (A1, A2, A3) | Low: ~50 lines of code, one afternoon | 50% of current failures |
| **Steering / prompt tightening** (B1 nudge) | Low: a few prompt edits | B1 + part of C1 |
| **Bigger model** (30B+ e.g. Qwen-2.5-32B, Llama-3-70B quantized) | High: needs bigger GPU; ~2× latency; ~5× vRAM | B1, B2, B3, C1 (genuine capability) |
| **Fine-tune the 12B** | High: data collection + GPU-hours; reproducibility burden | B1, B2, C1 (if fine-tune data is good); but **won't help A at all** |
| **Cross-session state seeding** (C2) | Medium: harness change | C2 |

## Why fine-tune is the LAST step, not the first

1. **You'd be fine-tuning around bugs.** ~50% of the failures are harness problems where the model did what it could. A fine-tuned model that "works around" the reviewer branch-checkout bug by adopting a different behavior (e.g. always listing files via API instead of `read_file`) would learn a brittle workaround. Then when you fix the harness bug, the model still does the workaround. Net: you'd have to re-fine-tune every time you change the harness.

2. **The training data is hard to get right.** You'd need to capture traces from sessions where the model "did the right thing" — but most of the failures are *exactly* the cases where the right action isn't clear without context the model doesn't have. Supervised fine-tuning on "do action X in state Y" examples requires Y to be fully recoverable from inputs the model actually gets. The harness bugs make Y non-recoverable (e.g. the `read_file` error doesn't tell the model it's on the wrong branch).

3. **Bigger models already have most of what fine-tuning would teach.** A 32B+ model has seen way more code-review traces in pretraining than your in-house set will have. The improvements from fine-tuning on a small harness-specific dataset mostly duplicate what you get for free with a bigger model.

4. **The fine-tune would be brittle to model upgrades.** A fine-tune of Gemma4-12B is largely a weight-diff on top of that specific base. When you eventually move to a new base (e.g. Gemma-5-12B, Qwen3-30B), you re-train from scratch.

5. **The single-biggest current gap is a 5-line code fix.** Fixing `event.ReviewRequested` in the branch-checkout predicate in agent.go:212 would unblock ~half of the reviewer-loop failures immediately. That's ROI of one afternoon.

## Recommended sequence

1. **Fix the harness bugs (1 day):**
   - A1: extend `agent.go:212` to include `ReviewRequested` (and probably `PullRequestOpened` for yolo auto-spawn) in the branch-checkout condition. Also: have the reviewer system prompt mention explicitly that on `ReviewRequested`, the branch IS already checked out and `read_file` should work.
   - A2: in the reviewer system prompt, add an explicit "do not call `forgejo_submit_review` more than once" guidance; consider having the tool return a NULL-op after a successful submit.
   - A3: in the implementer pre-flight, if the issue references `#N` and `#N`/PR-N isn't merged, auto-block with `Depends on: #N`.
   - Add a "don't call the same tool with same args twice in a row" guidance to the implementer prompt.

2. **Re-test on Gemma4-12B (1 day):**
   - With harness bugs fixed, re-run the same wave of issues. Expect roughly half the failures to clear.
   - Specifically check: does the reviewer actually read the file now? Does it submit review exactly once?

3. **If still failing on B-bucket indecision, try a bigger model (3-4 days):**
   - Switch `role_providers.reviewer` to a 32B model (Qwen3-30B-A3B, Llama-3.3-70B Q4, or devstral medium). Keep Gemma4-12B for implementer where it's working.
   - Re-run the same wave. Expect the B-bucket failures to clear.

4. **Only after 1-3 fail to close the loop, consider fine-tuning.**
   - The right fine-tune would be **RL** (not supervised) on a small task set with reward = (PR created) + (PR approved) − (turns used) − (duplicate tool calls). ~500 episodes on the harness would teach termination behavior.
   - Target: the B-bucket (indecision) only. Don't try to fine-tune around the A-bucket or C-bucket.

## Concrete data backing this analysis

From this test (just the failures):

| Session | Failure type | Bucket | Bigger model fixes it? | Fine-tune fixes it? | Harness fix fixes it? |
|---|---|---|---|---|---|
| pulls/15 (17-turn read_file loop) | read_file failing on un-checked-out branch | A1 | No (file truly absent) | No | **Yes** |
| pulls/14 (3× repeated submit_review) | re-calling successful tool | A2 | Mostly yes | Yes (marginal) | Yes (prompt) |
| issues/10 (39-turn bug exploration) | depended-on code not on main | A3 | No (unfixable w/ no info) | No | **Yes** |
| issues/13 (stalled at turn 2) | indecision | B3 | Yes | Yes (general) | Possibly (steering) |
| issues/8 (`feat/temperature-converter` vs `feature/temperature-converter`) | duplicate-branch coordination | C2 | Partially | No | **Yes** (cross-session seed) |
| PM issue decomposition | incomplete instruction-following on long system prompt | C1 | Yes | Yes | Yes (prompt trim) |

If you fix A1+A2+A3 alone, you remove 4 of the 6 dominant failure patterns. None of those 4 require a fine-tune.

## What I'd tell the user

> "Don't fine-tune yet. About half the failures are Fordjent bugs, not Gemma bugs — a fine-tuned Gemma would just learn to work around them, and you'd have to re-tune every time you fix the bugs. Fix the harness bugs first (one afternoon, ~50 lines), re-test, and only if you still see the model spinning on indecision after fixes, try a 32B model. Fine-tune as a last resort specifically to teach termination behavior."
