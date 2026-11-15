#!/bin/bash
# Stop a monitored Dirigent run started by start_monitored.sh.
#   ./stop_monitored.sh [run_name]
# Run analyze.py yourself afterwards on results/<run_name>.csv.

PROJECT_DIR=~/dirigent
RUN_NAME="${1:-run1}"
CSV="$PROJECT_DIR/results/$RUN_NAME.csv"

echo "Stopping sampler..."
sudo pkill -f 'monitoring/cgroup_monitor.sh' 2>/dev/null || true

echo "Stopping Dirigent components..."
sudo kill -TERM $(sudo lsof -t -i:10010) 2>/dev/null || true # Worker node
for _ in $(seq 1 20); do
    sudo lsof -t -i:10010 >/dev/null 2>&1 || break
    sleep 0.5
done
sudo kill -9 $(sudo lsof -t -i:10010) 2>/dev/null || true   # fallback if hung

# SIGKILL the scopes (don't wait on SIGTERM -> avoids the systemctl-stop hang)
sudo systemctl kill --signal=SIGKILL 'dirigent-*.scope' 2>/dev/null || true
sudo kill -9 $(sudo lsof -t -i:9090) 2>/dev/null || true  # Control plane
sudo kill -9 $(sudo lsof -t -i:8081) 2>/dev/null || true  # Data plane gRPC
sudo kill -9 $(sudo lsof -t -i:8080) 2>/dev/null || true  # Data plane proxy
sudo pkill -TERM -f firecracker 2>/dev/null || true

echo "Cleaning containerd containers (namespace cm)..."
for t in $(sudo ctr -n cm task ls -q 2>/dev/null); do
    sudo ctr -n cm task kill -s SIGKILL "$t" 2>/dev/null || true
    sudo ctr -n cm task rm -f "$t" 2>/dev/null || true
done
for c in $(sudo ctr -n cm container ls -q 2>/dev/null); do
    sudo ctr -n cm container rm "$c" 2>/dev/null || true
done

sudo find /sys/fs/cgroup/dirigent-sandbox -depth -type d -exec rmdir {} + 2>/dev/null || true

echo "Sweeping leftover Firecracker network devices..."
for d in $(ip -o link show | sed 's/@.*//' | awk -F': ' '{print $2}' | grep -E '^fc-.*-veth$'); do
    sudo ip link delete "$d" 2>/dev/null || true
done
for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep '^firecracker-'); do
    sudo ip netns delete "$ns" 2>/dev/null || true
done
sudo iptables -D FORWARD -j FC_FWD 2>/dev/null || true
sudo iptables -F FC_FWD 2>/dev/null || true
sudo iptables -X FC_FWD 2>/dev/null || true
sudo iptables -t nat -F FC_MASQ 2>/dev/null || true
sudo iptables -t nat -X FC_MASQ 2>/dev/null || true

echo "Done. Results in $CSV"
