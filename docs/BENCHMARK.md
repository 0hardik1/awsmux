# Benchmark: agent + awsmux MCP vs agent + raw aws CLI

The claim under test: giving an AI agent awsmux's five MCP tools is
cheaper and faster than giving the same agent a raw shell with the aws
CLI, on identical read-only fleet tasks against an identical fleet.

This document describes the methodology (v2, incorporating an external
review of the original design) and the results of the full sweep run on
2026-07-20. Summary: the awsmux arm won every cell on cost (1.3x to
2.9x cheaper), model-generated tokens (1.8x to 7.4x fewer), and
wall-clock time (2.3x to 5.4x faster), with every cost and token
difference significant after multiple-comparison correction.

## Setup

Every trial is a fresh headless Claude Code session (`claude -p
--output-format json`) with `--setting-sources ""`,
`--disable-slash-commands`, `--no-session-persistence`,
`--strict-mcp-config`, a pinned model and effort, and an empty temp
working directory. The built-in tool surface is declared exactly via
`--tools`, and the session-init event of every run is validated after
the fact: any session whose tool surface, MCP servers, or model
deviates from its arm's contract is scored `invalid_env` and excluded,
never silently counted.

Three arms, identical prompt (the prompt never names awsmux):

- **awsmux**: the five awsmux MCP tools only, no built-in tools at all.
- **cli**: the Bash built-in only, with `AWS_CONFIG_FILE` pointing at
  the demo-generated config, reaching the same fleet. The baseline is
  allowed to be smart; one parallel shell loop is the legitimate
  competitor.
- **mixed**: Bash plus the awsmux MCP tools. The adoption question:
  does an agent pick awsmux when both paths are available, and does it
  pay off?

The fleet is the awsmux demo fleet: 100 LocalStack-backed profiles,
each its own emulated account, seeded with VPCs and security groups
including one deliberately world-open group.

### Conditions

| cell | task | profile glob | fleet size |
|---|---|---|---|
| t1-10 / t1-50 / t1-100 | list every VPC ID per profile | `payments-*` / `*-prod-*` / `*-*-*` | 10 / 50 / 100 |
| t2-50 / t2-100 | find the world-open security group | `*-prod-*` / `*-*-*` | 50 / 100 |

10 repetitions per cell per arm: 5 cells x 3 arms x 10 reps = 150
sessions. Repetitions run as blocks; within each block the (cell, arm)
jobs are shuffled with a seeded RNG so no arm systematically runs first
against shared caches and backends.

### Environment

| component | value |
|---|---|
| model | claude-opus-4-8, effort high |
| agent runtime | Claude Code CLI 2.1.216 |
| aws CLI | 2.36.2 (arm64) |
| fleet | LocalStack 3.8 (digest-pinned), 100 profiles |
| awsmux under test | commit `cefb9ca` (includes the MCP result-shaping work of PR #4) |
| schedule | seed 42, concurrency 4, 1200s session timeout |
| experiment fingerprint | `8f7171bf38069b38` |

The experiment is fingerprinted over the model, effort, arms, prompts,
task grid, the awsmux binary, and the exact fleet state; the harness
refuses to aggregate runs from mismatched fingerprints.

## Grading

A deterministic checker, independent of all arms, snapshots ground
truth from the fleet before the sweep. Each run is graded twice:

- **strict**: the final message must be exactly one JSON object, no
  fences, no prose, exact key set.
- **task-correct**: content-only grading of a leniently extracted
  answer.

Cost and token statistics condition on task-correct runs; strict-format
compliance is reported separately. Failures are priced, not hidden:
alongside conditional medians, the harness reports total spend over all
attempts, spend per correct completion, success probability with a
Wilson 95% CI, and expected cost with retries.

## Statistics

Per cell: median cost ratio with a seeded percentile-bootstrap 95% CI,
seeded permutation tests (difference of medians, 20k permutations), and
Holm correction across the five cells. Primary comparison: cli vs
awsmux. Secondary: cli vs mixed.

## Results (2026-07-20)

All 150 sessions completed; 138 passed environment validation and every
one of those produced the correct answer, in all three arms. Total
spend for the sweep was $22.44 over 32 minutes. Differences below are
therefore about efficiency, not capability: the raw CLI agent always
gets there, it just spends more tokens, more turns, and more time.

Medians over task-correct runs; ratio > 1 favors awsmux.

| cell | cost cli | cost awsmux | ratio [95% CI] | Holm-adj p |
|---|---|---|---|---|
| t1-10 | $0.090 | $0.068 | 1.32x [1.17..1.56] | 0.038 |
| t1-50 | $0.239 | $0.120 | 1.98x [1.81..2.44] | 0.011 |
| t1-100 | $0.274 | $0.193 | 1.42x [1.25..1.85] | 0.019 |
| t2-50 | $0.229 | $0.079 | 2.90x [1.76..3.79] | 0.038 |
| t2-100 | $0.152 | $0.098 | 1.54x [1.10..3.22] | 0.038 |

| cell | out-tok cli | out-tok awsmux | turns cli | turns awsmux | wall cli | wall awsmux |
|---|---|---|---|---|---|---|
| t1-10 | 1,166 | 633 | 4 | 4 | 34s | 13s |
| t1-50 | 4,470 | 1,462 | 10.5 | 4 | 84s | 23s |
| t1-100 | 5,706 | 2,674 | 10.5 | 4 | 94s | 41s |
| t2-50 | 4,080 | 549 | 8.5 | 4 | 75s | 14s |
| t2-100 | 2,328 | 546 | 6 | 4 | 90s | 17s |

Output-token differences are Holm-adjusted p < 0.05 in every cell. The
awsmux arm ran a flat 4 turns regardless of fleet size: list targets,
plan, execute, answer. The CLI arm's turn count grew with the fleet.

The mixed arm answers the adoption question: given both Bash and the
MCP tools, agents chose the MCP path and landed near the pure awsmux
arm on cost, turns, and time (cli vs mixed cost ratios 1.28x to 2.81x,
all Holm-adjusted p < 0.05).

Strict output-format compliance also favored the structured-tool arm:
1 format miss across the awsmux runs vs 7 in the CLI arm.

## Caveats, stated up front

- The fleet is LocalStack on localhost, not real AWS, so absolute wall
  times are optimistic for both arms. The relative speedup comes mostly
  from turn count (a flat 4 vs up to ~12), which does not depend on
  backend latency, but real-AWS margins may differ.
- The awsmux arms pay the five MCP tool schemas as input on every
  request; that overhead is counted, and at small fleet sizes it eats
  most of the margin (see t1-10).
- 12 of 150 sessions (all in the MCP-bearing arms) were excluded as
  `invalid_env` because the MCP server had not finished connecting at
  session init: a startup race under concurrency, not a task failure.
  Those sessions never had working MCP tools, so excluding them from
  token statistics is correct, but per-cell n is 8 to 10 rather than
  10.
- Sessions are capped at 1200s; the watchdog records a timeout as a
  failure, censoring the most expensive tail, so measured margins are
  conservative. No session hit the cap in this sweep.
- The t2 prompt says exactly one world-open group exists, leaking
  cardinality to all arms equally.
- n=10 per cell supports directional conclusions with the reported CIs;
  it is not a high-powered study.
- The harness (prompt templates, sweep driver, grader, and statistics)
  is not currently published in this repository. This document records
  the full design so the experiment can be reproduced independently;
  seeds, versions, and the experiment fingerprint above pin what was
  run.
