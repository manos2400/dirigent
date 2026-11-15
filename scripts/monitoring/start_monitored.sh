#!/bin/bash
set -e

# Like start.sh, but every component runs inside the `dirigent.slice` cgroup and
# resource sampling (platform + sandbox) is started automatically.
#
#   ./start_monitored.sh [run_name]     # run_name -> results/<run_name>.csv
#
# After it prints "ready": register functions, run your load generator, then
# stop everything with ./stop_monitored.sh and summarize the CSV.

PROJECT_DIR=~/dirigent
GO=/usr/local/go/bin/go
MON="$PROJECT_DIR/scripts/monitoring"
RUN_NAME="${1:-run1}"
RESULTS="$PROJECT_DIR/results"
CSV="$RESULTS/$RUN_NAME.csv"
SLICE="dirigent.slice"
SANDBOX_CG="dirigent-sandbox"

# Kill previous instances
echo "Stopping previous instances..."
sudo systemctl stop 'dirigent-*.scope' 2>/dev/null || true
sudo kill -9 $(sudo lsof -t -i:9090) 2>/dev/null || true  # Control plane
sudo kill -9 $(sudo lsof -t -i:8081) 2>/dev/null || true  # Data plane gRPC
sudo kill -9 $(sudo lsof -t -i:8080) 2>/dev/null || true  # Data plane proxy
sudo kill -9 $(sudo lsof -t -i:10010) 2>/dev/null || true # Worker node
sudo pkill -TERM -f firecracker 2>/dev/null || true
sudo pkill -f 'monitoring/cgroup_monitor.sh' 2>/dev/null || true
sudo ip -all netns delete 2>/dev/null || true
sudo iptables -t nat -F 2>/dev/null || true
sudo rm -rf /tmp/firecracker-overlays /tmp/firecracker-* /tmp/snapshots 2>/dev/null || true
# remove sandbox cgroup (and any leftover per-container child cgroups)
sudo find /sys/fs/cgroup/$SANDBOX_CG -depth -type d -exec rmdir {} + 2>/dev/null || true
echo "Cleaned up"

# Required sysctl settings for Firecracker networking
sudo sysctl -w net.ipv4.conf.all.route_localnet=1
sudo sysctl -w net.ipv4.ip_forward=1

# Build binaries
echo "Building binaries..."
cd "$PROJECT_DIR"
$GO build -o "$PROJECT_DIR/bin/control_plane" ./cmd/master_node/
$GO build -o "$PROJECT_DIR/bin/data_plane"    ./cmd/data_plane/
$GO build -o "$PROJECT_DIR/bin/worker_node"   ./cmd/worker_node/
echo "Build successful"

# Start Redis
sudo docker compose up -d
sleep 5

mkdir -p "$PROJECT_DIR/logs" "$RESULTS"

# Launch a component as a transient scope inside dirigent.slice.
run_scoped() { # unit binary config
  cd "$PROJECT_DIR/cmd/$3"
  sudo setsid -f systemd-run --scope --slice="$SLICE" --unit="dirigent-$1" --collect \
    "$PROJECT_DIR/bin/$2" --config config.yaml \
    < /dev/null > "$PROJECT_DIR/logs/$2.log" 2>&1
}

echo "Starting control plane (in $SLICE)..."
run_scoped master control_plane master_node
sleep 3
echo "Starting data plane (in $SLICE)..."
run_scoped data data_plane data_plane
sleep 3
echo "Starting worker node (in $SLICE)..."
run_scoped worker worker_node worker_node
sleep 2

echo ""
echo "Dirigent ready (no monitoring yet). Logs: $PROJECT_DIR/logs/"
echo "Next:"
echo "  1. go run cmd/register_functions/main.go --file cmd/register_functions/dirigent-functions.yaml --address localhost:9091"
echo "  2. warm up the functions. If needed."
echo "  3. ./start_sampler.sh $RUN_NAME   # begin sampling -> $CSV"
echo "  4. run your load generator"
echo "  5. ./stop_monitored.sh $RUN_NAME  # stops everything + prints summary"
