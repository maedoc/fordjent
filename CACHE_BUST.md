# Cache Hit Rate Analysis — NeuralWatt + Fordjent

## Problem

Fordjent's LLM calls to NeuralWatt show **intermittent cache hits** — the pattern is HIT/MISS/HIT/MISS rather than the expected monotonic increase. We need to understand why and fix it.

## What We Already Fixed

1. **Turn counter in system prompt** — message[0] changed every turn → moved to trailing user message
2. **Tool descriptions in system prompt** — changed when `excludeTools` was non-empty → moved to trailing user message  
3. **Added `cache_control: {type: "ephemeral"}` hints** on system prompt + last conversation message
4. **Removed old system message on compaction** — was creating two system-role messages

See commits `2af04a9..12719aa` on `master`.

## Current Model/Provider

```yaml
providers:
  - name: neuralwatt-qwen
    api_base: "https://api.neuralwatt.com/v1"
    model: qwen3.5-397b      # SEE BELOW: switch between qwen3.5-397b and qwen3.6-35b
```

## Key Session Data (27-turn session: `fjadmin/e2e-wave3/pulls/18`)

```
turn  tok_in  tok_out  cached   cache%  lat_ms  rc
   1    4,922      154        0    0.0%   5,705   1
   2    5,751       93        0    0.0%   6,156   2
   3    6,573      101        0    0.0%   6,583   3
   4    7,315       97        0    0.0%   6,461   4
   5    8,698       95        0    0.0%   6,076   5
   6    9,520       98    8,448   88.7%   8,228   6   ← first cache hit!
   7   10,779       96    9,504   88.2%   4,937   7
   8   11,601      238        0    0.0%   8,216   8   ← MISS after HIT
   9   14,501       82   10,560   72.8%   4,861   9
  10   15,258       58        0    0.0%   5,157  10   ← MISS after HIT
  11   16,011      117   13,728   85.7%   6,572  11
  12   16,843      726        0    0.0%  35,730  12   ← long reasoning output
  13   17,682    1,195        0    0.0%  32,156  13   ← long reasoning output
  14   18,704      114        0    0.0%   6,391  14
  15   19,443      544        0    0.0%  12,519  15
  16   20,501      595   17,952   87.6%  13,504  16   ← big cache
  17   22,512      361   20,064   89.1%   9,719  17   ← peak
  18    5,788      187        0    0.0%   5,272  18   ← compaction/reset
  19   11,182      248        0    0.0%   5,746  19
  20   12,005      217    5,280   44.0%   5,361  20
  21   12,828      183        0    0.0%   5,612  21   ← MISS
  22   13,690      220   10,560   77.1%   4,591  22
  23   14,552      234   11,616   79.8%   5,643  23
  24   15,493      179        0    0.0%   8,450  24   ← MISS after 2 HITs
  25   17,988      126        0    0.0%   5,425  25
  26   18,811      546        0    0.0%  11,251  26
  27   19,574      338        0    0.0%  10,824  27   ← 4 MISSes in a row
```

## Observations

### 1. Cached token values are multiples of ~1,056

```
8,448 = 8  × 1,056
9,504 = 9  × 1,056
10,560 = 10 × 1,056
13,728 = 13 × 1,056
17,952 = 17 × 1,056
20,064 = 19 × 1,056
5,280 = 5  × 1,056
11,616 = 11 × 1,056
```

NeuralWatt rounds cached tokens to 1,056-token KV cache pages (VLLM block_size=16 × page_size=66).

### 2. Cache hits are intermittent — not monotonic

Expected: Turn N caches prefix → Turn N+1 reuses it → cache grows  
Actual: HIT, MISS, HIT, MISS, MISS, HIT, HIT, MISS, ...

### 3. Latency correlates with cache hits

- **HIT avg**: 7,046 ms (481 ms/Ktok)
- **MISS avg**: 10,472 ms (793 ms/Ktok)
- **Speedup**: 1.65x from cache

### 4. Missing fields in NeuralWatt responses

- `prompt_tokens_details` is `null` on most calls — cache data only reported sporadically
- `request_cost_usd` is always 0.0 (free tier?)
- `energy` field is populated with per-call joules but is NOT being logged by our turn executor (bug in `turn.go`)

### 5. `rc` (request_count) increments every turn

This is NOT a multi-call request — each turn makes exactly ONE API call. The count is just a sequence number.

## Hypotheses

### H1: Worker bouncing (MOST LIKELY)

NeuralWatt load-balances across multiple VLLM GPU workers. Each worker has its own KV cache. When consecutive turns hit different workers, the cached prefix from the previous turn is on a different GPU → MISS.

**Evidence for**: Intermittent pattern consistent with random worker assignment. The HITs cluster when the same worker handles consecutive turns.

**Test**: Send 5 rapid identical requests. If some return cached and others don't → confirmed.

### H2: Steering messages change the prefix

The turn-aware steering messages (injected at 40/60/80/90% budget) might alter message[0] or add a message early in the sequence, invalidating the cached prefix.

**Evidence for**: Turns 12-13 have large output + long latency (reasoning) and are MISS. After these, cache is cold for several turns.

**Test**: Check the debug logs (added in commit `12719aa`) to see if the message structure changes between consecutive turns.

### H3: Tool exclusion changes invalidate the tools block

When the comment cap is hit, `forgejo_comment` is excluded from the tool schema. This changes the `tools` array in the API request, which may invalidate the prefix cache.

**Evidence for**: Minimal — tool exclusion is rare in these test sessions.

**Test**: Check debug logs for `tools=N` count changes between consecutive turns.

## Next Steps (DO ON RTX4)

### 1. Check the debug logs

The new `LLM-DEBUG` line in `internal/provider/client.go` prints the message structure for every API call:
```
LLM-DEBUG msg_count=7 struct=0:user(2341) [cc] 1:assistant(45,1TC) 2:tool(312) 3:assistant(,2TC) 4:tool(89) 5:tool(167) 6:user(234) [cc] tools=6
```

Look for:
- Does `0:user(...)` content length change between consecutive turns? → prefix is NOT stable
- Does `tools=N` count change? → tool exclusion is breaking cache
- Do any messages appear/disappear in the MIDDLE of the sequence? → steering injection

### 2. Fix energy_joules logging

The turn logger in `internal/agent/turn.go` doesn't include `energy_joules` in its structured log. Add it so we can compute energy-per-cached-token as a proxy.

### 3. Run a focused single-session test

Create one simple issue on a fresh repo, let it run 10+ turns, and examine the full LLM-DEBUG output + cache hit data.

### 4. Try the qwen3.6-35b model for comparison

Switch model in `/tmp/fordjent.yaml`:
```yaml
model: qwen3.6-35b    # instead of qwen3.5-397b
```
Restart fordjent. This model is smaller (3B active params vs 397B) and may have different KV cache behavior. The NeuralWatt dashboard shows per-model analytics, so you can compare.

### 5. Ask NeuralWatt about session affinity

If H1 is confirmed, the fix is at the provider level. NeuralWatt may support:
- A session/routing key header for KV cache affinity
- A prefix_cache_key parameter (VLLM native feature)
- Dedicated instances for long-running sessions

## Files Changed (This Session)

| File | Change |
|------|--------|
| `internal/provider/client.go` | `SystemPromptParts`, cached token parsing, cache_control hints, `LLM-DEBUG` logging |
| `internal/session/agent.go` | Moved turn# + tool list out of system prompt into trailing user messages |
| `internal/agent/turn.go` | `cached_tokens`, `cost_usd`, `cache_savings_usd` in turn logs |
| `fordjent.rtx4.yaml` | Model switching qwen3.5↔qwen3.6 for comparisons |

## RTX4 Access

```bash
# Fordjent logs (inside container)
docker logs -f fordjent 2>&1

# Filter for cache data
docker logs fordjent 2>&1 | grep 'LLM-DEBUG'
docker logs fordjent 2>&1 | grep 'turn complete'

# Status endpoint
curl -s -H 'Authorization: Bearer local-admin-token-12345' 'http://localhost:8080/status' | python3 -m json.tool

# Forgejo API
TOKEN="e7647504c70f2d0f644dd820eec517338eb47bf8"
curl -s 'http://localhost:4230/api/v1/repos/fjadmin/e2e-wave3/pulls?state=all' -u fjadmin:$TOKEN | python3 -m json.tool

# Restart fordjent
docker rm -f fordjent
docker run -d --name fordjent --network fordjent-net -p 0.0.0.0:8080:8080 -v fordjent-data:/var/lib/fordjent -v /tmp/fordjent.yaml:/etc/fordjent/fordjent.yaml:ro --env-file /tmp/fordjent.env fordjent:local

# Switch model
sed -i 's/qwen3.5-397b/qwen3.6-35b/' /tmp/fordjent.yaml
# Then restart fordjent
```
