#!/bin/bash
#
# cgroup v2 resource sampler for Dirigent (single-node / local).
# Samples one or more labeled cgroups into a single CSV: cumulative CPU time,
# current + peak memory. Cumulative CPU-seconds is sampling-bias free and is
# the fair metric to compare against Knative.
#
# Usage:
#   ./cgroup_monitor.sh <out.csv> <interval_sec> <label=cgroup> [label=cgroup ...]
#
#   <cgroup> is the path under /sys/fs/cgroup (e.g. dirigent.slice).
#
# Example (platform + sandbox together):
#   ./cgroup_monitor.sh results/run1.csv 1 \
#       platform=dirigent.slice sandbox=dirigent-sandbox
#
# A cgroup that does not exist yet (no sandbox started) is written as zeros so
# the timeline stays aligned. Stop with Ctrl-C.

set -euo pipefail

OUT="${1:?output csv path required}"
INTERVAL="${2:?interval seconds required}"
shift 2
[[ $# -ge 1 ]] || { echo "give at least one label=cgroup" >&2; exit 1; }

CGROOT="/sys/fs/cgroup"
NPROC="$(nproc)"
mkdir -p "$(dirname "$OUT")"
echo "timestamp_ns,label,cpu_usage_usec,cpu_user_usec,cpu_system_usec,memory_current_bytes,memory_peak_bytes,nproc,nchild,nsandbox" > "$OUT"

read_field() { # file key -> value (0 if missing)
  [[ -f "$1" ]] && awk -v k="$2" '$1==k{print $2}' "$1" || echo 0
}

echo "Sampling every ${INTERVAL}s -> $OUT (nproc=$NPROC). Ctrl-C to stop."
for pair in "$@"; do echo "  ${pair%%=*} <- $CGROOT/${pair#*=}"; done

while true; do
  ts="$(date +%s%N)"
  for pair in "$@"; do
    label="${pair%%=*}"
    cg="$CGROOT/${pair#*=}"
    usage=$(read_field "$cg/cpu.stat" usage_usec)
    user=$(read_field  "$cg/cpu.stat" user_usec)
    sys=$(read_field   "$cg/cpu.stat" system_usec)
    mem=$(cat "$cg/memory.current" 2>/dev/null || echo 0)
    peak=$(cat "$cg/memory.peak"   2>/dev/null || echo 0)
    nchild=$(find "$cg" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)
    ndirect=$(wc -l < "$cg/cgroup.procs" 2>/dev/null || echo 0)
    nsandbox=$(( nchild > ndirect ? nchild : ndirect ))
    echo "${ts},${label},${usage},${user},${sys},${mem},${peak},${NPROC},${nchild},${nsandbox}" >> "$OUT"
  done
  sleep "$INTERVAL"
done
