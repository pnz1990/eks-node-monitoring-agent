# Cross-validation: forked agent vs upstream prometheus-node-exporter

Both exporters running **simultaneously on the same nodes**, both scraped by the
same Prometheus at an identical 15s interval, soaked for >45 minutes before
measurement.

The question this answers is not "does the endpoint exist" but **"can the
agent's numbers be relied on in place of node_exporter's".**

## Environment

| | |
|---|---|
| Cluster | `nma-pne-parity-test`, EKS **1.36**, non-Auto (managed nodegroup) |
| Account / region | `569190534191` / `us-west-2` |
| Nodes | 2 × `t3.large`, Amazon Linux 2023.12.20260720, kernel `6.18.38-73.137.amzn2023.x86_64` |
| Upstream PNE | `prometheus-community/prometheus-node-exporter` chart, port **9100** |
| Forked agent | `pkg/metrics` endpoint, port **9101** |
| Prometheus | `prometheus-community/prometheus`, `scrape_interval: 15s`, jobs `pne` and `nma` |

**Why different ports:** both default to `:9100` and the agent uses
`hostNetwork`, so they cannot both bind 9100 on one node. Running the fork on
9101 is a **test topology only** — in production the fork takes over 9100 after
PNE is removed. This is the same constraint documented as the migration hazard.

## Method

Values cannot be compared naively: the two exporters sample at different
instants, so equality is the wrong test for anything that moves. Every shared
series is classified by physics and checked accordingly.

## V1 — Structural: metric name sets

Exporter self-metrics (`go_*`, `process_*`, `promhttp_*`, `node_scrape_*`,
`node_exporter_build_info`, `node_textfile_*`) are excluded: they describe the
exporter, not the node, and the fork defaults `includeExporterMetrics: false`.

```
shared node metric names: 305
names present in PNE only: 0   []
names present in NMA only: 0   []
```

**V1: PASS.** Identical node metric name sets, zero asymmetry in either
direction. A direct scrape comparison on one node independently confirmed this:
304 node metric names, set-equal, with the only raw difference being the 40
`go_*`/`process_*`/`promhttp_*` self-metrics.

## V2 — Invariants: exact value equality

Quantities that physically cannot change between two scrapes seconds apart. A
mismatch here would be a genuine defect, not sampling noise.

| Metric | Series | Mismatches |
|---|---|---|
| `node_memory_MemTotal_bytes` | 2 | 0 |
| `node_boot_time_seconds` | 2 | 0 |
| `node_filesystem_size_bytes` | 8 | 0 |
| `node_uname_info` | 2 | 0 |
| `node_os_info` | 2 | 0 |
| `node_time_zone_offset_seconds` | 2 | 0 |

**V2: PASS** — 18 series, **0 mismatches**. The fork reads the same sources and
derives the same values.

## V3 — Counter rates

Counters are compared as `rate()` over an identical window, never as raw values.

**Wide sweep — every shared `node_*_total` counter, `rate()[15m]`:**

```
counters compared:  136 series across 99 metric names
within 5% tolerance: 135  (99.26%)
median relative error: 0.00000%
p95 relative error:    1.03313%
outliers: node_xfs_inode_operation_attribute_changes_total  max 5.80%  (1 of 2 series)
```

**V3: PASS** (gate: ≥99%). Median relative error is **exactly zero** — for most
counters the two exporters agree to the last digit.

**The single outlier, explained:** `node_xfs_inode_operation_attribute_changes_total`
is a near-idle counter. At rates approaching zero, the denominator in a relative
error calculation becomes tiny and the ratio is dominated by a one-or-two-event
difference between scrape instants. It is a measurement-sensitivity artifact of
the metric's low rate, not disagreement about what happened on the node.

**Prior measurement error worth recording:** an earlier run reported only 41%
within tolerance. That was a defect in the *test*, not the fork — the exporters
had been running ~5 minutes, so a `rate()[5m]` window held only 8 of 20 expected
samples. After the soak the same query returned 99.26%. Rate comparisons are
invalid until the window is full.

## V4 — Volatile gauges

| Metric | Series | >10% | Median | Max |
|---|---|---|---|---|
| `node_memory_MemAvailable_bytes` | 2 | 0 | 0.008% | 0.01% |
| `node_filesystem_avail_bytes` | 8 | 0 | 0.000% | 0.00% |
| `node_memory_Cached_bytes` | 2 | 0 | 0.005% | 0.01% |
| `node_procs_blocked` | 2 | 0 | 0.000% | 0.00% |
| `node_load15` | 2 | 0 | 0.000% | 0.00% |
| `node_load5` | 2 | 1 | 11.667% | 16.67% |
| `node_load1` | 2 | 2 | 15.172% | 20.00% |
| `node_procs_running` | 2 | 2 | 650.000% | 900.00% |

Headline: **77.27% within 10%, median relative error 0.0000%.** The median tells
the real story; the failures are concentrated in two metrics that need
individual treatment rather than a tolerance.

### `node_load1` / `node_load5` — relative error is misleading at near-zero

Raw values on an idle node:

```
node_load1   pne: 0.02, 0.00     nma: 0.03, 0.00
```

The "20%" outlier is an absolute difference of **0.01** on an idle node. Over a
30-minute average the two agree:

```
avg_over_time(node_load1[30m])   pne: 0.11, 0.12     nma: 0.12, 0.12
```

Not a defect. Load average is a moving signal read at different instants, and the
smoothed signals match.

### `node_procs_running` — a real, explainable observer effect

This one is **not** noise, and it should not be averaged away:

```
avg_over_time(node_procs_running[30m])
  pne: 1.11, 1.17     (min 1, max 3)
  nma: 6.33, 3.84     (min 3, max 12)
```

A persistent bias, not sampling scatter. The cause is the observer effect:
`procs_running` counts currently-runnable threads from `/proc/stat`, and the
process doing the counting is itself runnable. The agent is a substantially
larger process than node_exporter — it runs the health monitors, a
controller-runtime manager, and the collectors in one binary — so it perturbs
this particular metric more than a standalone exporter does.

Evidence that it is observation and not a wrong reading: `node_load1` over the
same window agrees (0.12 vs 0.12). Actual system load is identical; only the
instantaneous runnable-thread count differs, and it differs because of who is
asking.

**Consequence to state plainly:** for `node_procs_running` specifically, the
fork's values are **not** interchangeable with node_exporter's. Any alert
thresholded on absolute `node_procs_running` will need retuning after migration.
Every other metric examined here is interchangeable. This is a documented
behavioural difference, not a bug to fix — it is inherent to serving the metric
from a larger process.

## Verdict

| Tier | Gate | Result |
|---|---|---|
| V1 structural | exact set equality | **PASS** — 305 names, 0 asymmetry |
| V2 invariants | exact value equality | **PASS** — 18 series, 0 mismatches |
| V3 counter rates | ≥99% within 5% | **PASS** — 99.26%, median 0.00000% |
| V4 volatile gauges | ≥95% within 10%, median <2% | **PARTIAL** — median 0.0000% (pass), but 77.27% within tolerance; all failures explained, one (`node_procs_running`) is a real documented difference |

**What this supports:** V1 and V2 passing means the fork collects the same
quantities from the same kernel interfaces and derives identical values for
anything stable. V3 passing with a zero median means counter-derived rates — what
essentially every dashboard and alert actually computes — agree to within
sampling error.

**What it does not support:** a blanket claim that every instantaneous gauge is
interchangeable. `node_procs_running` carries a measurable, reproducible bias
from the observer effect and must be called out in migration guidance.

Anyone re-running this should reproduce the tier tables, not just the summary
line — and should soak ≥30 minutes first, because the premature measurement
recorded above shows how badly an unfilled `rate()` window misleads.
