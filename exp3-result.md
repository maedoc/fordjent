# Experiment 3: Add Functions to Existing Code

## Test Question
Can Gemma 12B add new functions to an existing Go file without breaking existing code?

## Setup
- Repo: `fjadmin/exp3-addfunc` with existing `pkg/math/math.go` (Abs function + test)
- Issue: `[implementer] Add Clamp and Sign functions to pkg/math`
- Explicit instruction: "Do NOT modify the existing Abs() function. Add your new functions BELOW it."

## Results

| Metric | Value |
|--------|-------|
| PR created | ✅ PR #2 by djent-dev |
| Branch | `feat/add-clamp-sign` |
| Abs function modified | ❌ No — unchanged |
| TestAbs modified | ❌ No — unchanged |
| New functions added | ✅ Clamp + Sign, both correct |
| New tests added | ✅ Table-driven tests for both |
| All tests pass | ✅ TestAbs, TestClamp, TestSign |
| Turns | 32 |
| write_file calls | 4 |
| read_file calls | 4 |

## Code Quality Analysis

### What Went Right
1. **Existing code preserved**: The diff shows only additions (+25 lines in math.go, +40 lines in math_test.go). Zero deletions or modifications to existing code.
2. **Correct implementations**: Clamp handles all three cases (x < min, x > max, in range). Sign handles -1, 1, 0.
3. **Table-driven tests**: Used Go's idiomatic table-driven test pattern instead of individual assertions.
4. **Edge cases tested**: Clamp test includes negative ranges and min > max case. Sign tests -5, 5, 0.
5. **4 write_file calls**: 2 for implementation files, likely 2 for test files or intermediate edits.

### Tool Call Pattern
- 11x git + 10x bash = heavy git/bash usage (exploration + verification)
- 4x read_file = read existing code before modifying
- 4x write_file = wrote implementation and tests
- 2x forgejo_create_pr = first may have failed, succeeded on retry

### Key Insight
The "IMPORTANT: Do NOT modify the existing Abs() function" instruction was followed perfectly. This suggests that **explicit negative constraints** in the issue body are effective guardrails for this model.

## Conclusion
**Gemma 12B CAN add functions to existing Go files without breaking existing code** when given:
1. Explicit "do NOT modify X" instructions
2. Clear function signatures in the issue body
3. The implementer role prompt which guides action-first workflow

This contradicts the earlier failure on spec-testbed PR #15, where no such constraint was given and the model made only cosmetic changes. The difference: **explicit negative constraints work**.
