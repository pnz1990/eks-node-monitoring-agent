# node_exporter parity — exceptions

Tracks every place the agent's metrics endpoint differs from upstream
`prometheus/node_exporter`. **An empty "gaps" section means literal parity.**

Verified against upstream `v1.12.1` (`b401dcfc`) on EKS 1.36 / AL2023 /
kernel 6.18.38, with both exporters running on the same nodes.

## Gaps

**None.** No upstream collector, metric name, label, or flag is unavailable.

## Verified parity claims

| Surface | Claim | Evidence |
|---|---|---|
| **S1 collectors** | All 86 registered upstream collectors are available, and the default-enabled set matches upstream exactly | 49 enabled collectors on each endpoint, **identical sets**, zero asymmetry (compared via `node_scrape_collector_success`) |
| **S1 opt-in** | Collectors that upstream ships default-*disabled* remain opt-in-able | `--collector.buddyinfo`, `--collector.processes`, `--collector.interrupts` all enable correctly through `extraArgs` |
| **S2 metric identity** | Name, type and label key sets are identical | 305 shared node metric names, zero present-in-one-only, `hack/parity-test.sh` exits 0 |
| **S3 flags** | Every upstream collector flag is honoured, including `--collector.textfile.directory` and the `--path.*` family | Flags are the upstream `kingpin` definitions verbatim; `HostPathArgs` rebases the `--path.*` set onto `HOST_ROOT` |
| **S4 meta metrics** | `node_exporter_build_info`, `node_scrape_collector_success`, `node_scrape_collector_duration_seconds`, `node_textfile_mtime_seconds`, `node_textfile_scrape_error` all served | Asserted in unit tests and the live e2e suite |
| **S5 endpoint semantics** | `/metrics` on port 9100, HTML landing page on `/`, 404 on unknown paths, concurrent scrape limiting | Unit tests cover each; live scrape confirms the endpoint |

## Documented behavioural differences

These are **not** gaps — every metric is present with the correct name, type and
labels. They are differences in observed *value* that a migration should know
about.

### `node_procs_running` — observer effect

| | 30-minute average |
|---|---|
| upstream node_exporter | 1.11, 1.17 |
| forked agent | 6.33, 3.84 |

`procs_running` counts currently-runnable threads from `/proc/stat`, and the
process doing the counting is itself runnable. The agent runs the health monitors,
a controller-runtime manager and the collectors in one binary, so it is a larger
process than a standalone exporter and perturbs this specific metric more.

`node_load1` over the same window agrees (0.12 vs 0.12), confirming the machine's
actual load is identical — only the instantaneous runnable count differs, and it
differs because of who is asking.

**Migration impact:** an alert thresholded on absolute `node_procs_running` needs
retuning. Rate- and load-based alerting is unaffected.

### Exporter self-metrics are off by default

`go_*`, `process_*` and `promhttp_*` are not served unless
`includeExporterMetrics: true`, whereas upstream serves them by default. This is
deliberate: the agent already publishes its own runtime metrics on its existing
controller-runtime endpoint, and duplicating them under a second port would
double-count the same process.

Set `nodeAgent.metrics.includeExporterMetrics: true` to match upstream exactly.

### Collectors that report failure on an EKS node

Ten collectors report `node_scrape_collector_success 0` because the hardware or
filesystem is absent: `bcachefs`, `bonding`, `fibrechannel`, `hwmon`, `ipvs`,
`nfs`, `nfsd`, `rapl`, `tapestats`, `zfs`.

**Upstream node_exporter reports failure for exactly the same ten** on the same
node — verified by diffing `node_scrape_collector_success` between the two live
endpoints, which produced identical failure sets. This is a property of the
hardware, not of either exporter.

## Scope decision

The endpoint deliberately carries **all** upstream collectors, including ones with
no plausible meaning on an EKS node (`zfs`, `bcachefs`, `infiniband`, `wifi`,
`drbd`, `tapestats`). Shipping an opinionated subset would mean a workload that
scrapes any upstream metric could break on migration. Collectors that find no
matching hardware export nothing, so the cost of the unused ones is negligible.

Operators who want a smaller surface can trim it explicitly with `collectors` or
`extraArgs`.
