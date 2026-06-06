## 1. Path Extraction Hardening

- [x] 1.1 Move `regexp.MustCompile` in `extractPathPrefixes` to a package-level `var pathLikeRe`. This eliminates per-call regex compilation. [spec: parallel-validation-hardening/Regex compiled once at package level]
- [x] 1.2 Add `goPathPrefixes` whitelist var (`cmd`, `internal`, `pkg`, `api`, `web`, `docs`, `configs`, `scripts`) and update `extractPathPrefixes` to filter matches: only accept tokens whose first segment is in the whitelist. [spec: parallel-validation-hardening/Path extraction uses Go prefix whitelist]
- [x] 1.3 Update `extractPathPrefixes` tests: add cases for non-whitelisted prefixes (`v1.49/linux-amd64` → no extraction) and verify whitelist extensibility. [spec: parallel-validation-hardening/Path extraction uses Go prefix whitelist]

## 2. Downgrade Traceability

- [x] 2.1 In `handleSpecPRMerged`, after `validateParallelTasks` returns downgraded serial tasks, append `*(auto-downgraded from parallel)*` on a separate line in the issue body for each downgraded task. Non-downgraded tasks keep the standard body format. [spec: parallel-validation-hardening/Downgrade traceability in issue bodies]

## 3. Integration Tests

- [x] 3.1 Add `TestHandleSpecPRMerged_ParallelOverlap_DowngradedToSerial` in `specpr_test.go`: use task descriptions with overlapping paths (e.g., "Implement auth in pkg/auth/handler.go" and "Auth tests in pkg/auth/auth_test.go"), verify both issues are created, verify the downgraded issue body contains `*(auto-downgraded from parallel)*`, verify serial `Depends on:` chain. [spec: speccycle/Parallel tasks conflict at file level]
- [x] 3.2 Update `TestValidateParallelTasks_OverlapDetected` to also verify that the downgraded task has the annotation marker in a hypothetical body (unit-level). [spec: parallel-validation-hardening/Downgrade traceability in issue bodies]

## 4. Verification

- [x] 4.1 Run `go test ./internal/session/... ./internal/e2e/... ./internal/speccycle/...` — all tests pass.
- [x] 4.2 Run `go vet ./internal/session/...` — no warnings.
