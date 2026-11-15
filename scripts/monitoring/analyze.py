#!/usr/bin/env python3
"""
Analyze a Dirigent experiment run.

Inputs:
    - cgroup CSV          (resource sampler: cgroup_monitor.sh)
    - load-generator JSONL (optional)
    - Dirigent trace CSVs  (optional, auto-discovered next to the cgroup CSV):
        cold_start_trace.csv  -> per-cold-start sandbox-boot breakdown (master node)
        proxy_trace.csv       -> per-request data-plane path breakdown (data plane)

Usage:
    ./analyze.py <cgroup.csv> [loadgen.jsonl] [num_invocations_override]

The trace CSVs are picked up automatically if they sit in the same directory as
<cgroup.csv>. All trace durations are microseconds; cgroup memory is bytes.

Outputs:
    - printed summary (CPU-seconds, memory, latency, cold/warm, boot breakdown),
      also written to <run_dir>/summary
    - PNG saved next to the cgroup CSV: <basename>_analysis.png
    - PNG with a per-part CDF of the sandbox-boot breakdown:
      <basename>_boot_cdf.png
"""

import sys, json, csv, os
import statistics as st
from pathlib import Path
import numpy as np
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
import matplotlib.gridspec as gridspec
from collections import defaultdict

# request is "cold" if it waited more than this on a sandbox creation (us).
COLD_THRESHOLD_US = 1000

# loadgen writes status_code 0 (STATUS_SEND_ERROR) when a request timed out or
# never reached the platform; 200 is a success, anything else is an error.
STATUS_TIMEOUT = 0
STATUS_SUCCESS = 200

# cold_start_trace.csv breakdown columns, in stacking order.
BOOT_COLS = [
    "image_fetch", "sandbox_create", "sandbox_start", "network_setup",
    "iptables", "readiness_probe", "data_plane_propagation",
    "snapshot_creation", "configure_monitoring", "find_snapshot",
    "db", "other_worker_node",
]
# proxy_trace.csv component columns (everything summed = end-to-end latency).
PROXY_COLS = [
    "get_metadata", "add_deployment", "cold_start", "load_balancing",
    "cc_throttling", "proxying", "serialization", "persistence_layer", "other",
]

# ── loaders ─────────────────────────────────────────────────────────────────────

def load_cgroup(path):
    by_label = defaultdict(list)
    for r in csv.DictReader(open(path)):
        by_label[r["label"]].append({
            "ts_ns":  int(r["timestamp_ns"]),
            "cpu":    int(r["cpu_usage_usec"]),
            "mem":    int(r["memory_current_bytes"]),
            "peak":   int(r["memory_peak_bytes"]),
            "nproc":  int(r["nproc"]),
            "nchild": int(r.get("nchild", 0) or 0),   # older CSVs lack this
            # unified sandbox count (containerd: child cgroups, firecracker:
            # PIDs in cgroup.procs); fall back to nchild for pre-nsandbox CSVs.
            "nsandbox": int(r.get("nsandbox", r.get("nchild", 0)) or 0),
        })
    return by_label

def load_jsonl(path):
    return [json.loads(l) for l in open(path) if l.strip()]

def load_trace(path):
    """Generic trace CSV loader -> list of dict rows (ints where possible)."""
    rows = []
    for r in csv.DictReader(open(path)):
        d = {}
        for k, v in r.items():
            try:    d[k] = int(v)
            except (ValueError, TypeError): d[k] = v
        rows.append(d)
    return rows

# ── series helpers ────────────────────────────────────────────────────────────────

def cpu_rate_series(rows):
    times, rates = [], []
    t0 = rows[0]["ts_ns"]
    for i in range(1, len(rows)):
        dt_s = (rows[i]["ts_ns"] - rows[i-1]["ts_ns"]) / 1e9
        dcpu = (rows[i]["cpu"]   - rows[i-1]["cpu"])   / 1e6
        if dt_s > 0:
            times.append((rows[i]["ts_ns"] - t0) / 1e9)
            rates.append(100.0 * dcpu / dt_s)
    return np.array(times), np.array(rates)

def mem_series(rows):
    t0 = rows[0]["ts_ns"]
    return (np.array([(r["ts_ns"] - t0) / 1e9 for r in rows]),
            np.array([r["mem"] / 1e6 for r in rows]))

def child_series(rows):
    t0 = rows[0]["ts_ns"]
    return (np.array([(r["ts_ns"] - t0) / 1e9 for r in rows]),
            np.array([r["nsandbox"] for r in rows]))

def summary_stats(rows):
    wall_s = (rows[-1]["ts_ns"] - rows[0]["ts_ns"]) / 1e9
    cpu_s  = (rows[-1]["cpu"]   - rows[0]["cpu"])   / 1e6
    mems   = [r["mem"] / 1e6 for r in rows]
    peak_f = max(r["peak"] for r in rows) / 1e6
    return dict(wall_s=wall_s, cpu_s=cpu_s, nproc=rows[0]["nproc"],
                mean_mem=st.mean(mems), peak_mem=peak_f if peak_f > 0 else max(mems),
                peak_child=max(r["nsandbox"] for r in rows))

def pct(data, p):
    if not data: return 0
    s = sorted(data)
    return s[min(int(p / 100 * len(s)), len(s) - 1)]

# ── inputs ─────────────────────────────────────────────────────────────────────

cgroup_path  = sys.argv[1]
jsonl_path   = sys.argv[2] if len(sys.argv) > 2 else None
inv_override = int(sys.argv[3]) if len(sys.argv) > 3 else None
run_dir      = Path(cgroup_path).parent

by_label = load_cgroup(cgroup_path)
nproc    = by_label[sorted(by_label)[0]][0]["nproc"]
stats    = {lbl: summary_stats(rows) for lbl, rows in by_label.items()}

invocations = load_jsonl(jsonl_path) if (jsonl_path and os.path.exists(jsonl_path)) else []
success   = [r for r in invocations if r["status_code"] == STATUS_SUCCESS]
timeouts  = [r for r in invocations if r["status_code"] == STATUS_TIMEOUT]
errors    = [r for r in invocations if r["status_code"] not in (STATUS_SUCCESS, STATUS_TIMEOUT)]
lats_ms   = sorted(r["latency_ms"] for r in success)
total_inv = inv_override or len(invocations)

def fn_name(r):
    return r["bench"].rsplit("-", 1)[0]

by_bench = defaultdict(list)
for r in success:
    by_bench[fn_name(r)].append(r["latency_ms"])
fn_p99 = {fn: pct(ls, 99) for fn, ls in by_bench.items()}

# invocation count per deployed function over *all* statuses, most popular first.
# keyed on the full name incl. hash suffix (image-processing-4b8b6e3), so two
# deployments of the same benchmark image stay distinct.
fn_counts = defaultdict(int)
for r in invocations:
    fn_counts[r["bench"]] += 1
fn_popular = sorted(fn_counts.items(), key=lambda kv: kv[1], reverse=True)

# error/timeout breakdown per status code, most frequent first
status_counts = defaultdict(int)
for r in invocations:
    status_counts[r["status_code"]] += 1

# auto-discover trace CSVs next to the cgroup CSV
cold_path  = run_dir / "cold_start_trace.csv"
proxy_path = run_dir / "proxy_trace.csv"
boot   = load_trace(cold_path)  if cold_path.exists()  else []
proxy  = load_trace(proxy_path) if proxy_path.exists() else []

# per-request total (us) + cold/warm split from proxy_trace
cold_lat_ms, warm_lat_ms = [], []
for r in proxy:
    total_ms = sum(r.get(c, 0) for c in PROXY_COLS) / 1e3
    (cold_lat_ms if r.get("cold_start", 0) > COLD_THRESHOLD_US else warm_lat_ms).append(total_ms)

# sandbox-boot breakdown (us -> ms), successful cold starts only
boot_ok = [r for r in boot if str(r.get("success")).lower() == "true"]
boot_part_ms = {c: [r.get(c, 0) / 1e3 for r in boot_ok] for c in BOOT_COLS}
boot_mean = {c: (st.mean(boot_part_ms[c]) if boot_ok else 0) for c in BOOT_COLS}

# ── text summary ───────────────────────────────────────────────────────────────

# every summary line goes to stdout and to <run_dir>/summary
summary_lines = []
def emit(line=""):
    print(line)
    summary_lines.append(line)

emit(f"\n{'='*58}\n  Run: {Path(cgroup_path).stem}\n{'='*58}")
total_cpu = sum(s["cpu_s"] for s in stats.values())
total_mem = sum(s["mean_mem"] for s in stats.values())
for lbl, s in stats.items():
    emit(f"\n[{lbl}]")
    emit(f"  CPU-seconds : {s['cpu_s']:8.2f} s  "
         f"({100*s['cpu_s']/s['wall_s']:.1f}% of 1 core, "
         f"{100*s['cpu_s']/s['wall_s']/nproc:.2f}% of {nproc})")
    emit(f"  mean mem    : {s['mean_mem']:8.1f} MB   peak: {s['peak_mem']:.1f} MB")
    if s["peak_child"]:
        emit(f"  peak sandboxes : {s['peak_child']}")
    if total_inv:
        emit(f"  per-req CPU : {1e3*s['cpu_s']/total_inv:.3f} ms/req ({total_inv} reqs)")

emit(f"\n[TOTAL]\n  CPU-seconds : {total_cpu:.2f} s\n  mean memory : {total_mem:.1f} MB")
if total_inv:
    emit(f"  per-req CPU : {1e3*total_cpu/total_inv:.3f} ms/req")

if invocations:
    n = len(invocations)
    emit(f"\n[invocation outcome: loadgen]  n={n}")
    emit(f"  success  : {len(success):>7}  ({100*len(success)/n:5.1f}%)")
    emit(f"  errors   : {len(errors):>7}  ({100*len(errors)/n:5.1f}%)  [status_code not in (0, 200)]")
    emit(f"  timeouts : {len(timeouts):>7}  ({100*len(timeouts)/n:5.1f}%)  [status_code 0]")
    other = [(c, k) for c, k in status_counts.items() if c not in (STATUS_SUCCESS, STATUS_TIMEOUT)]
    for code, cnt in sorted(other, key=lambda kv: kv[1], reverse=True):
        emit(f"    status {code:<5} : {cnt:>7}  ({100*cnt/n:5.1f}%)")

if fn_popular:
    emit(f"\n[most popular functions: loadgen]  distinct={len(fn_popular)}")
    for fn, cnt in fn_popular[:3]:
        emit(f"  {fn:<40} {cnt:>7} invocations  ({100*cnt/len(invocations):5.1f}%)")

if lats_ms:
    emit(f"\n[end-to-end latency: loadgen (faasrail-perceived), success only]  n={len(success)}")
    for p in (50, 90, 95, 99, 99.9):
        emit(f"  p{p:<5} : {pct(lats_ms, p):>8.0f} ms")

if proxy:
    n_cold, n_warm = len(cold_lat_ms), len(warm_lat_ms)
    n_tot = max(n_cold + n_warm, 1)
    emit(f"\n[cold/warm: proxy_trace (dirigent-logged)]  n={n_cold+n_warm}  "
         f"cold={n_cold} ({100*n_cold/n_tot:.1f}%)  warm={n_warm}")
    emit(f"  {'':6} {'cold ms':>10} {'warm ms':>10}")
    for p in (50, 95, 99):
        emit(f"  p{p:<4} {pct(cold_lat_ms, p):>10.0f} {pct(warm_lat_ms, p):>10.0f}")

if boot_ok:
    tot = sum(boot_mean.values())
    emit(f"\n[sandbox boot breakdown: cold_start_trace]  n={len(boot_ok)}  "
         f"mean total={tot:.0f} ms")
    emit(f"  {'':22} {'mean':>8} {'p50':>8} {'p99':>8}")
    for c in sorted(BOOT_COLS, key=lambda k: boot_mean[k], reverse=True):
        if boot_mean[c] > 0.05:
            vals = boot_part_ms[c]
            emit(f"  {c:<22} {boot_mean[c]:8.1f} {pct(vals, 50):8.1f} {pct(vals, 99):8.1f} ms"
                 f"  ({100*boot_mean[c]/tot:4.1f}%)")

# ── plots ──────────────────────────────────────────────────────────────────────

COLORS = {"platform": "#2196F3", "sandbox": "#FF9800"}

# build panel list: each is ("full"|"half", draw_callable)
panels = []

def p_cpu(ax):
    for lbl, rows in by_label.items():
        t, r = cpu_rate_series(rows)
        c = COLORS.get(lbl)
        ax.plot(t, r, label=lbl, color=c, linewidth=1.2)
        if len(r) > 10:
            w = min(15, len(r)//3*2 + 1)
            ax.plot(t, np.convolve(r, np.ones(w)/w, mode="same"), color=c, linewidth=2.5, alpha=0.6)
    ax.set(xlabel="time (s)", ylabel="CPU rate (% of 1 core)", title="CPU utilization over time")
    ax.legend(); ax.grid(True, alpha=0.3)

def p_mem(ax):
    for lbl, rows in by_label.items():
        t, m = mem_series(rows)
        ax.plot(t, m, label=lbl, color=COLORS.get(lbl), linewidth=1.5)
    ax.set(xlabel="time (s)", ylabel="memory (MB)", title="Memory + active sandboxes")
    ax.grid(True, alpha=0.3)
    # overlay sandbox count (nchild of the 'sandbox' label) on a twin axis
    sb = by_label.get("sandbox")
    if sb and max(r["nsandbox"] for r in sb) > 0:
        ax2 = ax.twinx()
        t, n = child_series(sb)
        ax2.plot(t, n, color="#9C27B0", linewidth=1.2, alpha=0.7, label="sandboxes")
        ax2.set_ylabel("# sandboxes", color="#9C27B0")
        ax2.tick_params(axis="y", labelcolor="#9C27B0")
        ax2.legend(loc="upper right", fontsize=8)
    ax.legend(loc="upper left")

def p_cpubar(ax):
    lbls = list(stats) + (["TOTAL"] if len(stats) > 1 else [])
    vals = [s["cpu_s"] for s in stats.values()] + ([total_cpu] if len(stats) > 1 else [])
    bars = ax.bar(lbls, vals, color=[COLORS.get(l, "#9E9E9E") for l in lbls])
    for b, v in zip(bars, vals):
        ax.text(b.get_x()+b.get_width()/2, v+max(vals)*0.01, f"{v:.1f}s", ha="center", va="bottom", fontsize=9)
    ax.set(ylabel="CPU-seconds", title="Total CPU consumed"); ax.grid(True, alpha=0.3, axis="y")

def p_cdf(ax):
    x = np.sort(np.array(lats_ms))
    ax.plot(x, np.linspace(0, 1, len(x)), color="#4CAF50", linewidth=1.8)
    for p, col in [(50,"#2196F3"),(95,"#FF9800"),(99,"#F44336")]:
        v = pct(lats_ms, p)
        ax.axvline(v, color=col, linestyle="--", linewidth=1, label=f"p{p}={v:.0f}ms")
    ax.set(xlabel="latency (ms)", ylabel="CDF",
           title="End-to-end latency CDF (faasrail-perceived, success)")
    ax.legend(fontsize=8); ax.grid(True, alpha=0.3)

def p_fnp99(ax):
    fns = sorted(fn_p99, key=fn_p99.get, reverse=True)[:12]
    ax.barh(fns, [fn_p99[f] for f in fns], color="#673AB7")
    ax.set(xlabel="p99 latency (ms)", title="Per-function p99 latency")
    ax.grid(True, alpha=0.3, axis="x"); ax.invert_yaxis()

def p_coldwarm(ax):
    for data, col, name in [(warm_lat_ms,"#4CAF50","warm"), (cold_lat_ms,"#F44336","cold")]:
        if data:
            x = np.sort(np.array(data))
            ax.plot(x, np.linspace(0, 1, len(x)), color=col, linewidth=1.8,
                    label=f"{name} (n={len(data)}, p99={pct(data,99):.0f}ms)")
    ax.set_xscale("log")
    ax.set(xlabel="end-to-end latency (ms, log)", ylabel="CDF",
           title="Cold vs warm latency CDF (proxy_trace, dirigent-logged)")
    ax.legend(fontsize=8); ax.grid(True, alpha=0.3, which="both")

def p_fnpop(ax):
    fns = [f for f, _ in fn_popular[:12]]
    ax.barh(fns, [fn_counts[f] for f in fns], color="#009688")
    ax.set(xlabel="invocations", title="Most invoked functions (all statuses)")
    ax.grid(True, alpha=0.3, axis="x"); ax.invert_yaxis()

def p_boot(ax):
    parts = [(c, boot_mean[c]) for c in BOOT_COLS if boot_mean[c] > 0.05]
    left = 0.0
    cmap = plt.get_cmap("tab20")
    for i, (name, val) in enumerate(parts):
        ax.barh(["boot"], [val], left=left, color=cmap(i % 20), label=f"{name} ({val:.0f}ms)")
        left += val
    ax.set(xlabel="time (ms)", title=f"Mean sandbox-boot breakdown (n={len(boot_ok)})")
    ax.legend(fontsize=7, ncol=2, loc="lower right"); ax.grid(True, alpha=0.3, axis="x")

panels.append(("full", p_cpu))
panels.append(("half", p_mem))
panels.append(("half", p_cpubar))
if lats_ms:
    panels.append(("half", p_cdf))
    panels.append(("half", p_fnp99))
if fn_popular:
    panels.append(("half", p_fnpop))
if proxy:
    panels.append(("half", p_coldwarm))
if boot_ok:
    panels.append(("full" if not proxy else "half", p_boot))

# lay out: full panels span both columns, half panels pair up
n_rows, layout, i = 0, [], 0
while i < len(panels):
    kind, fn = panels[i]
    if kind == "full":
        layout.append([(fn, slice(None))]); i += 1
    else:
        row = [(fn, 0)]
        if i+1 < len(panels) and panels[i+1][0] == "half":
            row.append((panels[i+1][1], 1)); i += 2
        else:
            i += 1
        layout.append(row)
n_rows = len(layout)

fig = plt.figure(figsize=(14, 5 * n_rows))
fig.suptitle(Path(cgroup_path).stem, fontsize=13, fontweight="bold")
gs = gridspec.GridSpec(n_rows, 2, figure=fig, hspace=0.5, wspace=0.32)
for ri, row in enumerate(layout):
    for fn, col in row:
        fn(fig.add_subplot(gs[ri, col]))

out = run_dir / (Path(cgroup_path).stem + "_analysis.png")
fig.savefig(out, dpi=150, bbox_inches="tight")
print(f"\nPlot saved: {out}")

# ── per-part boot-breakdown CDFs (own figure) ──────────────────────────────────

if boot_ok:
    parts = [c for c in BOOT_COLS if boot_mean[c] > 0.05]
    if parts:
        ncol = min(3, len(parts))
        nrow = (len(parts) + ncol - 1) // ncol
        cmap = plt.get_cmap("tab20")
        fig2, axes = plt.subplots(nrow, ncol, figsize=(5 * ncol, 3.6 * nrow), squeeze=False)
        fig2.suptitle(f"Cold sandbox-boot breakdown: per-part CDF "
                      f"(n={len(boot_ok)}) — {Path(cgroup_path).stem}",
                      fontsize=13, fontweight="bold")
        for i, c in enumerate(parts):
            ax = axes[i // ncol][i % ncol]
            x = np.sort(np.array(boot_part_ms[c]))
            ax.plot(x, np.linspace(0, 1, len(x)), color=cmap(BOOT_COLS.index(c) % 20), linewidth=1.8)
            for p, col in [(50, "#2196F3"), (95, "#FF9800"), (99, "#F44336")]:
                v = pct(boot_part_ms[c], p)
                ax.axvline(v, color=col, linestyle="--", linewidth=1, label=f"p{p}={v:.1f}ms")
            # a part that is zero for every cold start would break the log scale
            if x.max() > 0 and x[x > 0].size:
                ax.set_xscale("log")
            ax.set(xlabel="time (ms)", ylabel="CDF",
                   title=f"{c} (mean={boot_mean[c]:.1f}ms)")
            ax.legend(fontsize=7); ax.grid(True, alpha=0.3, which="both")
        for j in range(len(parts), nrow * ncol):
            axes[j // ncol][j % ncol].axis("off")
        fig2.tight_layout(rect=[0, 0, 1, 0.97])
        out_boot = run_dir / (Path(cgroup_path).stem + "_boot_cdf.png")
        fig2.savefig(out_boot, dpi=150, bbox_inches="tight")
        print(f"Boot CDF saved: {out_boot}")

# ── summary file ───────────────────────────────────────────────────────────────

summary_out = run_dir / "summary"
summary_out.write_text("\n".join(summary_lines) + "\n")
print(f"Summary saved: {summary_out}")
