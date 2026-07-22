# awsmux

**Run one AWS CLI command across your whole fleet of accounts, safely.**

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/go-stdlib%20%2B%20cobra-00ADD8?logo=go)
![AI agents](https://img.shields.io/badge/AI%20agents-MCP%20built--in-8A2BE2)

One command, every account and region, identities verified before
anything runs, results merged into one stream. Anything that mutates is
stopped by an approval boundary that even an AI agent with your admin
credentials cannot talk its way past.

**And it is measurably cheaper and faster for agents. In a 150-session
A/B benchmark (identical Claude Opus 4.8 agents, identical prompts,
same 100-account fleet), the awsmux arm beat a raw-shell aws CLI arm in
every cell: 1.3x to 2.9x cheaper, 2.3x to 5.4x faster, a flat 4 turns
at every fleet size, and up to 7.4x fewer output tokens (all
differences Holm-adjusted p < 0.05).**

| task | fleet | cost per run (cli vs awsmux) | wall time |
|---|---|---|---|
| enumerate all VPCs | 10 accounts | $0.090 vs $0.068 (1.3x) | 34s vs 13s |
| enumerate all VPCs | 50 accounts | $0.239 vs $0.120 (2.0x) | 84s vs 23s |
| enumerate all VPCs | 100 accounts | $0.274 vs $0.193 (1.4x) | 94s vs 41s |
| find the world-open group | 50 accounts | $0.229 vs $0.079 (2.9x) | 75s vs 14s |
| find the world-open group | 100 accounts | $0.152 vs $0.098 (1.5x) | 90s vs 17s |

Full design, statistics, and caveats:
[docs/BENCHMARK.md](docs/BENCHMARK.md).

## Try it on 100 accounts (none of them real)

Prerequisites: Docker and the aws CLI.

```sh
make fleet-up && source .tmp/fleet/env.sh
```

One make target boots a pinned LocalStack container and provisions a
fictional 100-account fleet (10 teams, prod and stage, 5 shards, 3
regions) to break for fun. The real aws CLI talks to a real emulated
AWS on localhost, every profile is its own account, and what you change
actually persists. Zero credentials, zero real AWS, zero risk. (The
old `awsmux demo --synthetic` no-Docker path is gone; the sandbox now
needs Docker.)

```sh
./bin/awsmux targets --profiles 'payments-*'     # verified identities, per account
./bin/awsmux run --dedupe --format jsonl -- ec2 describe-vpcs --query 'Vpcs[].VpcId'
                                                 # all 100 accounts in a few seconds
./bin/awsmux run --profiles '*-prod-*' -- ec2 describe-security-groups \
  --filters Name=ip-permission.cidr,Values=0.0.0.0/0   # find the world-open group
./bin/awsmux plan --profiles payments-prod-1 -- ec2 revoke-security-group-ingress \
  --group-name legacy-bastion --protocol tcp --port 22 --cidr 0.0.0.0/0
```

That last one is destructive, so it will not just run: you get an
immutable plan to approve first. Apply it with the token and re-run the
hunt; the finding disappears for real, because the sandbox is a real
(emulated) AWS. Try editing the plan file between approve and apply and
watch the hash check refuse it.

`make e2e` runs this whole storyline as an automated smoke test;
`make fleet-down` removes the container.

## Real fleet

Same commands, pointed at your own AWS config instead of the sandbox
environment. awsmux discovers profiles from your
existing AWS config (SSO or keys) and verifies every identity with STS
before running anything:

```sh
awsmux targets --regions us-east-1,us-west-2

awsmux run --profiles 'prod-*' --exclude '*-sandbox' --format jsonl \
  -- ec2 describe-instances --query 'Reservations[].Instances[].InstanceId'

awsmux plan -- ssm put-parameter --name /app/flag --value on --type String
awsmux approve plan-01k...          # prints a one-time token, never stored
awsmux apply plan-01k... --approval-token <token>
```

Useful flags: `--concurrency` (default 100: fleet-wide fan-out is the
point; each in-flight target is one aws CLI subprocess), `--timeout
30s`, `--max-errors N`,
`--stop-on-access-denied`, `--dedupe` (collapse profiles that resolve to
the same account), `--output-dir` (one result file per target),
`--interactive` (checkbox target picker). Every run lands in
`awsmux history` and can be re-run with `awsmux replay`.

## Give it to your AI agent

```sh
claude mcp add awsmux -- /path/to/awsmux mcp
```

The agent gets five structured tools (`list_aws_targets`,
`plan_aws_operation`, `execute_aws_plan`, `get_aws_execution`,
`cancel_aws_execution`) instead of a raw shell. Read-only plans execute
freely. Anything else refuses until a human runs `awsmux approve` and
hands over the token, which binds to the plan's sha256 hash, so the
agent cannot alter an approved plan or execute it twice. Want to watch
an agent hit the boundary with zero blast radius? Run `make fleet-up`,
`source .tmp/fleet/env.sh`, and register `./bin/awsmux mcp` from that
shell: the agent gets the 100-account sandbox fleet over MCP.

The cost and speed numbers in the table at the top come from exactly
this setup. One more result from the same benchmark: when an agent was
handed both a shell and the MCP tools, it picked awsmux on its own and
kept nearly all of the margin.

## Safety model

| Class | Examples | Policy |
|-------|----------|--------|
| `read_only` | describe-*, list-*, get-*, s3 ls | runs freely |
| `mutating` | create-*, put-*, update-*, s3 cp | `--yes`, interactive confirm, or approved plan |
| `destructive` | delete-*, terminate-*, revoke-*, s3 rm | never `--yes`; typed confirm or plan/approve/apply |
| `unknown` | anything unrecognized | treated as mutating |

Where the verb convention lies, awsmux overrides it: sts calls that mint
credentials (assume-role*, get-session-token, get-federation-token) and
s3api operations that write a local outfile (get-object,
get-object-torrent, select-object-content) are classified `mutating`
despite their read-style names.

Stable exit codes for CI and agents: 0 all succeeded, 1 some failed,
2 selection/config error, 3 approval required or rejected, 4 stopped by
threshold.

## The 03:00 story

A scanner pages: SSH is open to the world somewhere in the fleet. Your
agent sweeps 14 verified accounts in one read-only call, finds
`sg-0a1b2c3d` in payments-prod, and proposes the revoke. The revoke is
destructive, so the engine hands back a plan instead of running it. You
read the plan on your phone: exact command, exact account, verified
principal, hash. You approve, paste one token, and go back to sleep
while the fix lands in history with the hash attached.

You can replay this exact story against a fleet of fakes right now:
`make fleet-up`.

## How it compares

| | awsmux | awsrun | aws-vault / granted / awsume |
|---|---|---|---|
| Fleet execution across accounts | yes | yes | no (credential tools) |
| STS identity preflight per target | yes | no | n/a |
| Risk classification + approval boundary | yes | prompt only | n/a |
| Agent interface (MCP) | yes | no | no |
| 100-account LocalStack test fleet (`make fleet-up`) | yes | no | no |
| History + replay | yes | no | no |
| Runtime | single Go binary | Python + plugins | Go / varies |

The credential tools are complementary: awsmux happily executes through
profiles they manage.

## Docs

- [Architecture](docs/ARCHITECTURE.md): the engine, the plan boundary,
  the executor, test-fleet internals.
- [Benchmark](docs/BENCHMARK.md): the agent-cost A/B methodology and
  results behind the table above.

## Development

```sh
make setup                 # git hooks + pre-warm the pinned linter
make build test lint       # or: go build ./... && go vet ./... && go test ./...
make fleet-up e2e          # LocalStack fleet + end-to-end smoke test
make fleet-down clean
```

CI runs `go test` and `make build` on ubuntu and macos, gofmt + go vet,
golangci-lint repo-wide, the LocalStack e2e smoke test on ubuntu, and a
Conventional Commits check on PR titles (the commit-msg hook enforces
the same convention locally).

Dependencies: stdlib plus cobra. Roadmap: organization-aware discovery
(`--ou` plus role assumption), policy packs, Homebrew tap and release
binaries.

Licensed under [Apache-2.0](LICENSE).
