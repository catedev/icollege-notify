#!/usr/bin/env bash
# iCollege direct mode (USE_ICOLLEGE=true) needs no Chrome scrape — just run the
# service: `notifier bot` (see systemd/). This script is only for WebAssign-scrape
# mode (USE_ICOLLEGE=false), which calls Claude in Chrome, then DMs due reminders.
# Chrome must be open with the Claude extension connected.
set -uo pipefail
cd "$(dirname "$0")"

claude -p --chrome \
  --allowedTools "mcp__claude-in-chrome" "Write(./assignments.json.tmp)" "Bash(mv assignments.json.tmp assignments.json)" \
  "$(cat SCRAPE_PROMPT.md)" \
  || echo "run.sh: scrape failed (exit $?); notifier will flag stale data" >&2

# Run the notifier even if the scrape failed, so stale data gets reported.
exec ./notifier
