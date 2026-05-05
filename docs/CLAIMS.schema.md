# CLAIMS.yaml Schema Specification (Draft v0.1)

> A lightweight schema for decomposing hypothesis issues into testable claims,
> tracked either as repo-borne YAML or as Forgejo DB records.

## Motivation

In research and agent-driven workflows, an issue like "Amortized inference
scales sub-linearly" is not actionable until broken into claims with
disprovable status.  CLAIMS.yaml provides that decomposition layer.

## Top-Level Structure

```yaml
claims_version: "0.1"
source_issue: "#42"          # parent hypothesis issue
source_repo: "fjadmin/testbed"
last_updated: "2026-05-05T16:30:00Z"

claims:
  - id: "claim-001"
    statement: >
      Amortized neural posterior estimation requires O(1) forward passes
      per subject after a single training phase on the population.
    status: pending                  # pending | testable | supported | rejected | partially_supported
    priority: high                   # low | medium | high | critical
    evidence_refs:
      - "Cranmer2020"
      - "Radev2020"
    counter_refs:                    # literature that contradicts this
      - []
    testability_score: 0.85          # 0..1 heuristic
    decomposition:
      - "Implement population training script"
      - "Measure per-subject inference time (n=100)"
      - "Compare with sequential SMC-ABC baseline"
    labels:
      - "methodology"
      - "performance"

  - id: "claim-002"
    statement: >
      Privacy-preserving amortization is achievable via federated
      posterior aggregation without centralised patient data.
    status: testable
    priority: critical
    evidence_refs:
      - "Ziaeemehr2025"
    decomposition:
      - "Design federated averaging protocol for posteriors"
      - "Simulate honest-but-curious adversary"
      - "Compute differential-privacy epsilon bound"

labels:
  status_legend:
    pending: "Claim identified but no test designed"
    testable: "Test designed; awaiting execution"
    supported: "Empirical evidence supports the claim"
    rejected: "Empirical evidence contradicts the claim"
    partially_supported: "Evidence is mixed or domain-restricted"
```

## Validation Rules

| Rule | Severity |
|------|----------|
| `id` must be unique within file | Error |
| `statement` ≤ 500 chars (fits issue body) | Warning |
| `status` must be in legend | Error |
| `decomposition` must contain ≥ 1 item for `testable` claims | Warning |
| `testability_score` ∈ [0,1] | Error |

## Integration Points

### Fordjent (Agent) Behaviour

When an issue body contains `[HYPOTHESIS]` or `## Claims` section:

1. Agent extracts claim-like statements via regex / LLM parsing.
2. If `CLAIMS.yaml` exists in repo root, agent adds entries.
3. Agent creates child issues for each claim via `forgejo_create_issue`
   with `Depends on: #N` pointing to the parent hypothesis issue.
4. Agent adds labels `claim/{status}` to child issues.

### Forgejo DB Injection (Option A — Native Table)

A `Claim` struct registered in `db.RegisterModel` alongside `IssueDependency`.

```go
// models/issues/claim.go
package issues

type ClaimStatus int

const (
	ClaimStatusPending ClaimStatus = iota
	ClaimStatusTestable
	ClaimStatusSupported
	ClaimStatusRejected
	ClaimStatusPartial
)

type Claim struct {
	ID            int64              `xorm:"pk autoincr"`
	IssueID       int64              `xorm:"INDEX NOT NULL"`  // FK to Issue
	RepoID        int64              `xorm:"INDEX NOT NULL"`
	Statement     string             `xorm:"LONGTEXT"`
	Status        ClaimStatus        `xorm:"NOT NULL DEFAULT 0"`
	Priority      string             `xorm:"VARCHAR(16)"`
	Testability   float64            `xorm:"DOUBLE"`
	EvidenceRefs  []string           `xorm:"JSON"`  // or separate table
	CreatedUnix   timeutil.TimeStamp `xorm:"created"`
	UpdatedUnix   timeutil.TimeStamp `xorm:"updated"`
}

func init() {
	db.RegisterModel(new(Claim))
}
```

Migration: `models/forgejo_migrations/v15a_add-claim-table.go`

API: REST endpoints under `/api/v1/repos/{owner}/{repo}/claims`

### Fordjent DB Injection (Option B — Side Table)

Instead of modifying Forgejo, Fordjent keeps its own SQLite side-table:

```go
// internal/claims/store.go
type ClaimStore struct{ db *sql.DB }
```

Advantage: No Forgejo fork required.
Disadvantage: Claims are invisible in Forgejo UI.
Hybrid: Agent writes `CLAIMS.yaml` to repo AND populates side-table for query.

## Next Steps

1. Commit this schema to `fjadmin/testbed` as `CLAIMS.example.yaml`.
2. Implement a `forgejo_create_claim` tool in Fordjent.
3. Add a `claims` subcommand to the Fordjent CLI for manual inspection.
