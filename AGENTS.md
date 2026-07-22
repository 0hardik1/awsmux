# AGENTS.md

Guidance for AI agents (and new humans) working on this codebase.

## What awsmux is

awsmux runs one AWS CLI command across a whole fleet of accounts/regions,
safely. It discovers profiles from the AWS shared config, verifies every
target identity with STS before anything runs, classifies each operation by
risk, gates anything non-read-only behind an immutable plan + human approval
token, fans out with a worker pool, and persists every run to a replayable
history. Agents consume it over MCP (`awsmux mcp`); humans use the CLI.

Single Go module (`awsmux`), Go 1.26. Dependencies: **stdlib plus cobra,
nothing else** — this is a deliberate design rule, not an accident. Do not
add dependencies (the MCP layer is hand-rolled JSON-RPC on purpose, and the
AWS shared-config INI parsing is hand-rolled on purpose).

## Build, test, verify

```sh
go build ./... && go vet ./... && go test ./...
```

Run that before every commit, or use the make equivalents: `make build`
(binary at `./bin/awsmux`), `make test`, `make vet`, `make check-fmt`, and
`make lint` (golangci-lint, version pinned in the Makefile). `make setup`
installs the git hooks and pre-warms the lint tool. Tests live next to the
code (`internal/core/*_test.go`, `internal/mcpserver/*_test.go`); they are
plain stdlib `testing`, no test frameworks, and need neither network nor
Docker.

To try the whole thing end to end with zero credentials and zero real AWS
(needs Docker and the aws CLI):

```sh
make fleet-up                    # LocalStack + a 101-profile test fleet
source .tmp/fleet/env.sh && ./bin/awsmux targets
make e2e                         # build + fleet-up + smoke test
make fleet-down                  # remove the LocalStack container
```

## Layout

```
main.go                    thin entry: os.Exit(cmd.Execute())
cmd/                       cobra CLI layer, one file per command
  root.go                  root command, ExitError, shared selector/exec flags
  run.go plan.go approve.go apply.go   the plan/approve/apply workflow
  targets.go history.go replay.go     discovery + history + replay
  mcp.go                   `awsmux mcp` -> internal/mcpserver.Serve
  interactive.go           TTY checkbox target picker + typed confirmations
internal/core/             THE ENGINE - everything meaningful lives here
  types.go                 all shared types + stable exit codes
  discovery.go             profile INI parsing, glob selectors, target expansion
  identity.go              STS preflight, 5m cache, dedup, re-verification
  classify.go              verb -> risk class tables + service overrides
  plan.go                  immutable plans, sha256 hash, ULID-style IDs
  policy.go                approval tokens, CheckApproval gate
  executor.go              worker pool, failure taxonomy, arg validation
  store.go                 ~/.awsmux persistence (plans/, executions/, index.jsonl)
  awscmd.go                aws CLI invocation, AWSMUX_AWS_BIN override
internal/mcpserver/        MCP stdio server: 5 tools, hand-rolled JSON-RPC 2.0
  server.go tools.go       framing + tool schemas/handlers
  results.go               token-economy result shaping (grouping, paging)
  registry.go              in-flight async execution registry
internal/output/           table | json | jsonl rendering (jsonl = agent/CI contract)
scripts/fleet/             LocalStack test-fleet provisioner (stdlib-only dev tool)
scripts/e2e.sh             smoke test run by `make e2e` and the CI e2e job
Makefile                   build/test/lint/fleet-up/e2e/hooks targets
.githooks/                 pre-commit (fmt, vet, lint) + commit-msg (Conventional Commits)
.github/                   CI workflow + dependabot
docs/ARCHITECTURE.md       design decisions, plan-boundary sequence, test-fleet internals
```

Layering rule (from `internal/core/types.go`): everything an agent or the
CLI can do goes through `internal/core`. `cmd/` and `internal/mcpserver/`
are thin wrappers — if you find yourself putting policy, classification, or
execution logic in either, it belongs in core instead, so both consumers see
identical behavior.

## The safety model — invariants you must not weaken

This project's entire value is that an AI agent holding admin credentials
cannot mutate anything without a human in the loop. When editing, preserve
these properties (and if a change touches one, say so explicitly in the PR):

1. **Classification fails safe.** `core.Classify` maps operation verbs to
   `read_only` / `mutating` / `destructive`; anything unrecognized is
   `unknown`, which policy treats as mutating. Service override tables
   (`stsClass`, `s3apiLocalWrite`, `s3Class`) exist where verb naming lies —
   e.g. `sts assume-role` and `s3api get-object` look read-ish but are
   classified mutating. Extend the tables; never loosen a default.
2. **`RequiresApproval`: only `read_only` runs freely.** There is no code
   path — no flag, no env var — that executes a non-read-only plan without a
   valid approval token. Do not add one.
3. **The plan hash is the boundary.** `Plan.ComputeHash()` covers service,
   operation, args, every verified target identity (profile, region,
   account, principal ARN), classification, policy version, and expiry.
   Approval tokens bind to that hash (`sha256(token)` stored, raw token
   printed exactly once, compared with `subtle.ConstantTimeCompare`). Any
   field that influences what executes must be added to the hash. Bump
   `PolicyVersion` when approval rules change.
4. **Execute at most once, across processes.** `core.ClaimPlan` creates an
   `O_EXCL` `.claim` file; the claim is one-shot and never released. The MCP
   server additionally serializes its approval gate with `execMu`.
5. **Identity is never inferred from profile names.** Preflight via
   `sts get-caller-identity` (cached 5 minutes, successes only) fills
   account/principal; `CheckVerified` blocks planning/execution against
   unverified targets; `VerifyIdentities` re-resolves at apply time and
   refuses if the live identity drifted from what was approved.
6. **`--profile` / `--region` are reserved args.** `ValidateArgs` rejects
   them in user args (the AWS CLI honors the last occurrence, so a duplicate
   would silently redirect an approved plan at another account). Checked at
   plan creation AND again in `CheckApproval` for stored plans.
7. **Stable contracts.** Exit codes 0–4 (`core.Exit*`), the
   `ResultStatus` failure taxonomy, and the jsonl output shape are relied on
   by CI and agents. Never renumber or rename; only add.
8. **Replay is a new decision, not a bypass.** `awsmux replay` re-verifies
   identities and re-applies the run-time gates fresh.

## Conventions

- **AGENT CONTRACT comments.** Several files open with an
  `// AGENT CONTRACT (...)` block that pins exported function signatures and
  documents behavior in the doc comments. Keep those signatures exactly as
  written; add unexported helpers freely in the same file. Keep doc comments
  in sync with behavior — they are the spec.
- **Errors and exit codes.** Commands return `cmd.Exitf(core.ExitX, ...)`;
  `Execute()` maps `ExitError` to the process code. Selection/config
  problems are `ExitConfigError` (2); anything approval-shaped is
  `ExitApprovalRequired` (3).
- **Persistence is atomic and private.** JSON state is written via temp
  file + rename (`writeJSONAtomic`), 0600/0700 modes. Identity cache and
  best-effort writes swallow errors by design (a cache failure must never
  fail a preflight); real state writes report errors.
- **MCP results are built for token economy** (`results.go`): compact
  one-line target rosters, plan echoes with previews instead of full
  rosters, execution results grouped by identical outcome with the dominant
  group elided, everything capped at ~24k chars with offset paging. New tool
  output must follow the same philosophy — the consumer is a model paying
  per token. stdout carries protocol frames exclusively; log to stderr.
- **Streaming order.** jsonl results stream in completion order via the
  `onResult` callback; the stored `Execution.Results` keeps input target
  order. Preserve both.
- **IDs** are `<prefix>-<26 base32 chars>` (ULID-style, time-sortable) from
  `core.NewID`; loaders accept unambiguous prefixes because agents truncate
  IDs constantly.
- **`AWSMUX_AWS_BIN` is the general test seam.** It replaces the `aws`
  invocation everywhere (identity preflight and executor); every feature
  must keep working under an override with zero special-casing in the
  engine.
- **Conventional Commits are enforced.** The commit-msg hook and the CI
  PR-title check share one type list: feat fix docs chore ci build
  refactor test perf style revert. `make install-hooks` activates
  `.githooks/` via `core.hooksPath`.
- **golangci-lint is pinned in one place per side.** `GOLANGCI_LINT_VERSION`
  in the Makefile and the `version:` of golangci-lint-action in
  `.github/workflows/ci.yml` must stay in sync (config in `.golangci.yml`).
- **CI runs `make e2e` against LocalStack on ubuntu.** Breaking fleet
  provisioning (`scripts/fleet`) or the smoke script (`scripts/e2e.sh`)
  fails CI, not just local runs.

## State at runtime

`$AWSMUX_HOME` (default `~/.awsmux`): `plans/<id>.json` (+ `.claim` files),
`executions/<id>.json` + `executions/index.jsonl`, `identity-cache.json`.
The test fleet isolates state under `.tmp/fleet/home` via `AWSMUX_HOME`
(exported by `.tmp/fleet/env.sh`).

## Docs to keep in sync

- `README.md` — user-facing pitch, flag lists, safety table, comparison.
- `docs/ARCHITECTURE.md` — engine diagram, plan-boundary sequence diagram,
  executor semantics, test-fleet internals, token-efficiency methodology.

If you change classification tables, exit codes, flags, MCP tool schemas,
the approval workflow, Makefile targets, or CI job names, update both docs
in the same change.
