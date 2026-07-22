#!/usr/bin/env bash
# End-to-end smoke test against the LocalStack test fleet.
# Run via `make e2e` (which builds the binary and provisions the fleet first).
set -euo pipefail
cd "$(dirname "$0")/.."

ENV_FILE=.tmp/fleet/env.sh
[ -f "$ENV_FILE" ] || { echo "e2e: run make fleet-up first ($ENV_FILE missing)" >&2; exit 1; }
# shellcheck source=/dev/null
. "$ENV_FILE"

BIN=./bin/awsmux
[ -x "$BIN" ] || { echo "e2e: run make build first ($BIN missing)" >&2; exit 1; }
EXPECT=${AWSMUX_FLEET_SIZE:?env.sh did not set AWSMUX_FLEET_SIZE}

fail() { echo "e2e: FAIL: $*" >&2; exit 1; }

# 1. Discovery plus STS verification of every profile.
tgt_out=$($BIN targets --format jsonl)
n=$(echo "$tgt_out" | wc -l | tr -d ' ')
[ "$n" = "$EXPECT" ] || fail "expected $EXPECT targets, got $n"

# 1b. The fleet writes every profile to both shared files, so each target
# must be sourced from "both".
if echo "$tgt_out" | grep -v '"source":"both"' >/dev/null; then
  fail "expected every target sourced from both shared files"
fi

# 1c. Doctor reports a healthy environment.
$BIN doctor >/dev/null

# 2. Dedupe collapses the planted admin-legacy duplicate.
nd=$($BIN targets --dedupe --format jsonl | wc -l | tr -d ' ')
[ "$nd" = "$((EXPECT - 1))" ] || fail "dedupe: expected $((EXPECT - 1)) targets, got $nd"

# 3. Read-only fan-out across the whole fleet exits 0.
$BIN run --format jsonl -- sts get-caller-identity >/dev/null

# 4. Seeding worked: the world-open security group is findable.
# Capture, then match with [[ ]]: any early-exit pipe reader (grep -q, awk
# with exit) closes the pipe while the producer is still writing, and under
# pipefail the producer's EPIPE death (exit 141) fails the whole script.
sg_out=$($BIN run --profiles '*-prod-*' --format jsonl -- ec2 describe-security-groups \
  --filters Name=ip-permission.cidr,Values=0.0.0.0/0)
[[ "$sg_out" == *legacy-bastion* ]] || fail "seeded world-open group not found"

# 5. The approval gate holds: a mutating run without --yes exits 3.
set +e
$BIN run --profiles payments-prod-1 -- ssm put-parameter \
  --name /e2e/probe --value probe-ok --type String --overwrite >/dev/null 2>&1
rc=$?
set -e
[ "$rc" = "3" ] || fail "mutating run without approval exited $rc, want 3"

# 6. Full plan / approve / apply roundtrip. Same rule as step 4: capture
# each command's full output before parsing so no pipe closes early on a
# still-writing awsmux (this exact race killed CI with exit 141 when awk's
# `exit` fired while `plan` was mid-output).
plan_out=$($BIN plan --profiles payments-prod-1 -- ssm put-parameter \
  --name /e2e/probe --value probe-ok --type String --overwrite)
plan_id=$(echo "$plan_out" | awk '/^Plan/ {print $2; exit}')
[ -n "$plan_id" ] || fail "no plan id in plan output"
approve_out=$($BIN approve "$plan_id" --yes)
token=$(echo "$approve_out" | awk '/^approval token/ {print $NF}')
[ -n "$token" ] || fail "no approval token minted"
$BIN apply "$plan_id" --approval-token "$token" >/dev/null
param_out=$($BIN run --profiles payments-prod-1 --format jsonl -- ssm get-parameter \
  --name /e2e/probe --query Parameter.Value)
[[ "$param_out" == *probe-ok* ]] || fail "applied parameter not readable back"

echo "e2e: OK ($n targets)"
