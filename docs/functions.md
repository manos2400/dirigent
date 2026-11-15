# Using Dirigent: Register, Invoke, and Get Metrics

This guide shows how to:

- Register a function image with the control plane
- Invoke the function through the data plane
- Fetch live metrics and export tracing results

It applies to both single-node setups and clusters. For first-time setup, see SINGLE_NODE_DEPLOYMENT.md.

## Prerequisites

- Dirigent components are running (Redis, Control Plane, Data Plane, Worker Node)
- Registration server reachable (default: 127.0.0.1:9091)
- At least one data plane proxy reachable (default: 127.0.0.1:8080)

Tip: When you register a function, the API returns the endpoint you should use to invoke it (either a load balancer address if configured, or a list of dataplane proxy addresses).

## 1) Register a Function

POST form parameters (required unless noted):

- name: Unique function name (e.g., test-function)
- image: Container image (e.g., docker.io/cvetkovic/dirigent_empty_function:latest)
- port_forwarding: Must be provided twice:
  - First occurrence: guest port inside the function container (e.g., 80)
  - Second occurrence: protocol (tcp or udp)
- requested_cpu: Optional integer, requested vCPUs (default 1)
- requested_memory: Optional integer, workload memory in MiB (default 1)
- scaling_upper_bound / scaling_lower_bound: Optional integers for autoscaling bounds
- cold_start_sweep: Optional bool (true/false) to enable sweep mode for experiments

Example:

```bash
curl -sS -X POST http://127.0.0.1:9091/register \
  -d "name=test-function" \
  -d "image=docker.io/cvetkovic/dirigent_empty_function:latest" \
  -d "port_forwarding=80" \
  -d "port_forwarding=tcp" \
  -d "scaling_upper_bound=2" \
  -d "scaling_lower_bound=0" \
  -d "requested_cpu=1" \
  -d "requested_memory=1"
```

Runtime note:
- `firecracker`: uses `requested_memory` as workload memory (MiB) and adds VM guest overhead internally when sizing the microVM.

On success, the API returns the endpoint to invoke the function, for example:

- A load balancer address (e.g., 127.0.0.1:8079), or
- A semicolon-separated list of dataplane proxy addresses (e.g., 127.0.0.1:8080)

You can verify registration via gRPC:

```bash
grpcurl -plaintext 127.0.0.1:9090 data_plane.CpiInterface/ListServices
```

## 2) Invoke the Function

Send requests to the address returned by registration. The data plane routes by the HTTP Host header.

Required header:

- Host: The function name you registered (e.g., test-function)

Example (sync path):

```bash
curl -sS -X POST http://127.0.0.1:8080 \
  -H "Host: test-function" \
  --data-binary "Hello from Dirigent"
```

Notes:

- Dirigent only requires Host for routing. Any additional headers/body are passed to the function container and interpreted by your function code.
- The sample function image used above returns a JSON payload acknowledging the function name and execution info.

## 3) Live Metrics (/metrics)

Each data plane exposes a JSON metrics endpoint at /metrics on its proxy port (default 8080).

- Aggregate metrics across all functions on that data plane:

  ```bash
  curl -sS http://127.0.0.1:8080/metrics | jq .
  ```

- Metrics for a single function (by name):

  ```bash
  curl -sS "http://127.0.0.1:8080/metrics?service=test-function" | jq .
  ```

Returned fields:

- SuccessfulInvocations: Total successful requests counted
- Inflight: Current number of in-flight requests
- QueueDepth: Current number of queued requests (waiting for capacity or cold start)

If you run multiple dataplanes, query each one directly for its local metrics. If you use a load balancer for invocations, the /metrics endpoint is still served on the dataplane processes, not through the load balancer by default.

## 4) Tracing Results (CSV)

Dirigent emits detailed tracing for offline analysis.

- Control Plane: Cold start breakdowns → data/cold_start_trace.csv
- Data Plane: Proxy path breakdowns → data/proxy_trace.csv

Make sure the output directories exist (created by SINGLE_NODE_DEPLOYMENT or manually):

```bash
mkdir -p cmd/master_node/data cmd/data_plane/data cmd/worker_node/data
```

Cold start CSV columns (control plane):

- time, service_name, container_id, success, image_fetch, sandbox_create, sandbox_start, network_setup, iptables, readiness_probe, data_plane_propagation, snapshot_creation, configure_monitoring, find_snapshot, db, other_worker_node

Proxy CSV columns (data plane):

- time, service_name, container_id, get_metadata, add_deployment, cold_start, load_balancing, cc_throttling, proxying, serialization, persistence_layer, other

You can copy these files off the nodes and use the plotting scripts under artifact_evaluation/ or scripts/plotting/ for analysis.

## 5) Reset Measurements (optional)

To clear cold-start tracing headers on the control plane:

```bash
grpcurl -plaintext -d '{}' 127.0.0.1:9090 data_plane.CpiInterface/ResetMeasurements
```

## Troubleshooting

- HTTP 404 on invoke: The function name in Host may not match a registered service.
- HTTP 410 on invoke: Cold start resolved but no sandbox available; retry shortly or adjust scaling bounds.
- "Invocation for non-existing service" in logs: Register the function first and ensure Host matches exactly.
- Permission/network errors locally: Ensure the sysctl settings in the README are applied and the worker runs with sudo.

---

References:

- Registration API: Control plane registration server (default :9091)
- Invocation: Data plane proxy (default :8080) or configured load balancer
- gRPC API: data_plane.CpiInterface on control plane (default :9090)
