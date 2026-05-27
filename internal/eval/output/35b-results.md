# Eval Results: Qwen3.6-35B-A3B (llama-cpp, local)

## Bugfix Benchmark (5 trials)

**Overall: 4/5 passes** (trial 1 timed out waiting for agent activity)

### Implementer-Only Sessions (per-trial breakdown)

| Trial | Turns | TokenIn   | TokenOut | ToolCalls | WriteFile | Bash | Git  | Latency(s) |
|-------|-------|-----------|----------|-----------|-----------|------|------|------------|
| 0     | 13    | 92,777    | 2,998    | 13        | 15        | 34   | 3    | 42.6       |
| 1     | 18    | 136,307   | 4,583    | 21        | 29        | 107  | 0    | 71.1       |
| 2     | 34    | 289,381   | 4,954    | 36        | 51        | 257  | 139  | 119.6      |
| 3     | 24    | 189,283   | 4,390    | 27        | 37        | 181  | 23   | 102.2      |
| 4     | 15    | 121,934   | 4,129    | 16        | 17        | 77   | 0    | 73.8       |
| **AVG** | **20.8** | **165,936** | **4,211** | **22.6** | **29.8** | **131.2** | **33.0** | **81.9** |

### Key Observations
- **High variance**: Trial 0 used 13 turns (42s), trial 2 used 34 turns (120s) — 3x difference
- **Bash-heavy exploration**: Avg 131 bash calls per trial, with trial 2 at 257
- **Write efficiency**: 15-51 write_file calls, avg 29.8 (multiple rewrites common)
- **Reviewer overhead**: Each trial also spawned reviewer sessions (10-11 turns each)
- **Zero system-role errors**: Clean API compatibility
- **Zero false error labels**: No `fordjent/failed:error` mislabels

### Tool Call Distribution (all sessions combined)
- `bash`: 656 (37%)
- `read_file`: 432 (24%)
- `write_file`: 149 (8.4%)
- `git`: 165 (9.3%)
- `forgejo_*`: 331 (18.7%)
- `turn`: 5 (0.3%)

### Model: qwen3.6-35b via local llama-cpp server
**Server**: http://100.107.183.102:8181/v1  
**Cost**: $0.00 (local inference)  
**Token generation**: ~172 tokens/sec (measured)  