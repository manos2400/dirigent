#!/bin/bash
set -e

# Start ONLY the resource sampler (platform + sandbox cgroups), assuming dirigent
# is already running via ./start_monitored.sh. Call this after warming up the
# functions and right before launching the load generator.
#
#   ./start_sampler.sh [run_name]     # run_name -> results/<run_name>.csv
#
# Must use the same run_name you passed to start_monitored.sh / will pass to
# stop_monitored.sh.

PROJECT_DIR=~/dirigent
MON="$PROJECT_DIR/scripts/monitoring"
RUN_NAME="${1:-run1}"
RESULTS="$PROJECT_DIR/results"
CSV="$RESULTS/$RUN_NAME.csv"
SLICE="dirigent.slice"
SANDBOX_CG="dirigent-sandbox"
SAMPLE_INTERVAL="${SAMPLE_INTERVAL:-1}"

mkdir -p "$RESULTS" "$PROJECT_DIR/logs"

# Refuse to start a second sampler on top of a running one.
if sudo pgrep -f '[c]group_monitor.sh' >/dev/null 2>&1; then
  echo "A sampler is already running. Stop it first (./stop_monitored.sh)." >&2
  exit 1
fi

echo "Starting cgroup sampler -> $CSV ..."
"$MON/cgroup_monitor.sh" "$CSV" "$SAMPLE_INTERVAL" \
  platform="$SLICE" sandbox="$SANDBOX_CG" > "$PROJECT_DIR/logs/sampler.log" 2>&1 &

echo "Sampler started (interval ${SAMPLE_INTERVAL}s). Now run your load generator."
echo "When done: ./stop_monitored.sh $RUN_NAME"
