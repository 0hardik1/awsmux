#!/usr/bin/env bash
# Self-typing demo used to record the README GIF (docs/demo.gif) in a real
# terminal. It types and RUNS the storyline for real against the LocalStack
# test fleet. scripts/demo.tape renders the same storyline headlessly with
# vhs if you have no screen recorder.
#
# To record:
#   make build && make fleet-up
#   source .tmp/fleet/env.sh && ./bin/awsmux targets >/dev/null   # warm the identity cache
#   run this script in a roughly 130x36 terminal window (font ~16pt)
#   arm the screen recorder (Kap etc.) on the window, then: touch /tmp/awsmux-demo-go
#   stop the recorder once the plan output has lingered ~7s (the script
#   touches /tmp/awsmux-demo-done at that moment, then idles 30s and exits)
#
# Post-process the recording into the README GIF (trim any setup frames at
# the head with -ss, drop the title bar rows with crop):
#   ffmpeg -ss 0.7 -i recording.mp4 -filter_complex \
#     "crop=iw:ih-62:0:62,fps=10,mpdecimate,scale=1440:-2,split[a][b];[a]palettegen=max_colors=48:stats_mode=diff[p];[b][p]paletteuse=dither=none" \
#     docs/demo.gif

cd "$(cd "$(dirname "$0")/.." && pwd)" || exit 1
# shellcheck source=/dev/null
source .tmp/fleet/env.sh
export PATH="$PWD/bin:$PATH"

PROMPT=$'\033[38;5;135m>\033[0m '
GO_FILE=/tmp/awsmux-demo-go

type_line() {
  printf '%s' "$PROMPT"
  local s="$1" i
  for ((i = 0; i < ${#s}; i++)); do
    printf '%s' "${s:i:1}"
    sleep "0.0$((RANDOM % 3 + 2))"
  done
  sleep 0.2
  printf '\n'
}

run_cmd() {
  type_line "$1"
  eval "$1"
}

clear
printf 'waiting for recorder... (touch %s to start)\n' "$GO_FILE"
while [ ! -f "$GO_FILE" ]; do sleep 0.2; done
rm -f "$GO_FILE"
clear
sleep 1.5

type_line "# discover targets, every identity verified with STS before anything runs"
run_cmd "awsmux targets --profiles 'payments-*'"
sleep 3

type_line "# one read-only command, all 100 accounts in parallel"
run_cmd "awsmux run --dedupe --format jsonl -- ec2 describe-vpcs --query 'Vpcs[].VpcId'"
sleep 3.5

run_cmd "clear"

type_line "# destructive commands are refused outright"
run_cmd "awsmux run --profiles payments-prod-1 -- ec2 revoke-security-group-ingress --group-name legacy-bastion --protocol tcp --port 22 --cidr 0.0.0.0/0"
sleep 2.5

type_line "# you get an immutable, hash-bound plan for a human to approve instead"
run_cmd "awsmux plan --profiles payments-prod-1 -- ec2 revoke-security-group-ingress --group-name legacy-bastion --protocol tcp --port 22 --cidr 0.0.0.0/0"
sleep 7

touch /tmp/awsmux-demo-done
sleep 30
