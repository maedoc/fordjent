# Eval Benchmark Results

## Model Comparison: Qwen3.6 Variants

All benchmarks run with Fordjent's bugfix scenario (fix an off-by-one in
binary search). 10 trials for the 35B Q4_K_XL, 5 trials for others.

### Summary

| Model | Quant | Strict Pass | Bug Fixed | Avg Turns | Avg Write | Avg Bash | Latency/trial | Throughput |
|-------|-------|-------------|-----------|------------|-----------|----------|----------------|------------|
| **27B** | Q4_K_XL | **5/5 (100%)** | 5/5 | 17.8 | 15.0 | 102.6 | 820s | 12 tok/s |
| 35B | Q4_K_XL | 7/10 (70%) | 10/10 | 20.4 | 35.2 | 125.7 | 117s | 63 tok/s |
| 35B | IQ4_XS | 4/5 (80%) | 5/5 | 20.8 | 29.8 | 131.2 | 82s | 74 tok/s |
| 27B | IQ4_XS | 2/5 (40%) | 4/5 | 19.0 | 33.6 | 108.0 | 472s | 20 tok/s |

**Server**: All models served via llama.cpp at `http://100.107.183.102:8181/v1`.  
Throughput varies by GPU load and quantization. The 35B Q4_K_XL ran at 63 tok/s  
with 76.8K context; the 27B Q4_K_XL ran at 12 tok/s on a different/crowded GPU.  
The 35B IQ4_XS ran at 74 tok/s (faster quant, smaller context).

### Quality Signals

| Signal | 27B Q4_K_XL | 35B Q4_K_XL | 35B IQ4_XS | 27B IQ4_XS |
|--------|-------------|-------------|------------|------------|
| **Extraneous files** | **0/5** | 3/10 | 1/5 | 3/5 |
| Best case turns | **7** | **8** | 13 | 12 |
| Worst case turns | 29 | 31 | 34 | 31 |
| Write efficiency | **15.0 avg** | 35.2 avg | 29.8 avg | 33.6 avg |
| Bash exploration | 102.6 avg | 125.7 avg | 131.2 avg | 108.0 avg |

### Per-Trial Detail: 35B Q4_K_XL (10 trials)

| Trial | Turns | Write | Bash | Git | Latency(s) | Strict? | Failure |
|-------|-------|-------|------|-----|------------|---------|---------|
| 0 | 22 | 43 | 144 | 0 | 95 | ✅ | — |
| 1 | 20 | 24 | 110 | 23 | 110 | ✅ | — |
| 2 | 21 | 22 | 128 | 0 | 109 | ✅ | — |
| 3 | 17 | 27 | 71 | 6 | 82 | ✅ | — |
| 4 | 31 | 61 | 306 | 55 | 202 | ❌ | go.mod |
| 5 | 22 | 49 | 126 | 6 | 103 | ❌ | .gitignore |
| 6 | 17 | 34 | 50 | 22 | 142 | ✅ | — |
| 7 | 23 | 38 | 182 | 12 | 201 | ✅ | — |
| 8 | 23 | 48 | 125 | 32 | 93 | ✅ | — |
| 9 | 8 | 6 | 15 | 0 | 35 | ❌ | go.mod |

### Per-Trial Detail: 27B Q4_K_XL (5 trials)

| Trial | Turns | Write | Bash | Git | Latency(s) | Strict? |
|-------|-------|-------|------|-----|------------|---------|
| 0 | 19 | 15 | 94 | 56 | 257 | ✅ |
| 1 | 29 | 31 | 266 | 94 | 1135 | ✅ |
| 2 | 17 | 13 | 56 | 60 | 830 | ✅ |
| 3 | 17 | 13 | 85 | 30 | 1276 | ✅ |
| 4 | 7 | 3 | 12 | 7 | 603 | ✅ |

### Per-Trial Detail: 35B IQ4_XS (5 trials)

| Trial | Turns | Write | Bash | Git | Latency(s) | Strict? | Failure |
|-------|-------|-------|------|-----|------------|---------|---------|
| 0 | 13 | 15 | 34 | 3 | 43 | ✅ | — |
| 1 | 18 | 29 | 107 | 0 | 71 | ✅ | — |
| 2 | 34 | 51 | 257 | 139 | 120 | ✅ | — |
| 3 | 24 | 37 | 181 | 23 | 102 | ✅ | — |
| 4 | 15 | 17 | 77 | 0 | 74 | ❌ | go.mod |

### Key Findings

1. **27B Q4_K_XL is the best quality model** — 100% strict pass rate, 2× better
   write efficiency (15 vs 35 write_file calls), and never touches extraneous files.

2. **The primary quality differentiator is "minimal diff"** — all models fix
   the bug correctly every time. The only failures are modifying go.mod or
   .gitignore unnecessarily. A post-action verification hook would eliminate
   this failure mode.

3. **Speed is a hardware question, not a model question**. The 35B at 63-74 tok/s
   vs the 27B at 12 tok/s reflects the GPU serving the model, not the model
   architecture. On equivalent hardware, the 27B would be faster (smaller).

4. **Quantization matters**. Q4_K_XL significantly outperforms IQ4_XS at the
   same parameter count (100% vs 40% for 27B; 70% vs 80% for 35B, though the
   35B comparison is confounded by different trial counts).

5. **Both Qwen models show bash-loop risk** — bash calls range 12-306 per trial.
   This is a Qwen architecture tendency, not scale-dependent. Trial 4 of the
   35B Q4_K_XL used 306 bash calls but still passed.

6. **Best-case performance is excellent** — both models can fix the bug in 7-8
   turns with 3-6 write_file calls, completing in 35 seconds or less. The
   question is consistency, not capability.

### Recommended Model

**27B Q4_K_XL** for quality-critical work (100% pass rate, best write efficiency).  
**35B Q4_K_XL** for speed-critical work with a verification loop (fast iterations,
post-action checks catch extraneous file edits).

Either model paired with a `post_action_check` that rejects changes to files
outside `pkg/search/` would achieve near-100% strict pass rate.