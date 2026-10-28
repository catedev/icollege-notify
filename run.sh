#!/usr/bin/env bash
# Only for source: webassign/both. Scrapes WebAssign via Claude in Chrome, then runs
# one pass. source: icollege doesn't need this, just the service (`icollege-notify bot`).
# Chrome must be open with the Claude extension connected.
set -uo pipefail
cd "$(dirname "$0")"

claude -p --chrome \
  --allowedTools "mcp__claude-in-chrome" "Write(./assignments.json.tmp)" "Bash(mv assignments.json.tmp assignments.json)" \
  "$(cat SCRAPE_PROMPT.md)" \
  || echo "run.sh: scrape failed (exit $?); icollege-notify will flag stale data" >&2

# Run the notifier even if the scrape failed, so stale data gets reported.
exec ./icollege-notify
