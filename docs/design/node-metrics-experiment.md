# Serving node_exporter-compatible metrics from the EKS Node Monitoring Agent

**A design document and a report on two implementations, one of which should ship.**

| | |
|---|---|
| **Status** | Complete. Awaiting a decision on §9. |
| **Date** | 2026-07-29 |
| **Branches** | `feat/prometheus-node-exporter-parity` ("**dependency**") · `feat/metrics-no-upstream-dependency` ("**native**") |
| **Validated on** | EKS 1.36, Amazon Linux 2023, kernel 6.18.38, 2 and 6 × t3.large, us-west-2 |
| **Reference** | `prometheus/node_exporter` v1.12.1 (`b401dcfc`), deployed as the `prometheus-node-exporter` addon |
| **Evidence** | `evidence/` (this branch and the native branch), `JOURNAL.md`, `JOURNAL-NO-DEPENDENCY.md` |

---

## 1. Why any of this exists

EKS customers who want node-level host metrics install the **`prometheus-node-exporter` (PNE)
addon**. Separately, every EKS Auto Mode node already runs the **Node Monitoring Agent (NMA)**,
which reads `/proc` and `/sys` to publish `NodeConditions` that EKS node auto-repair acts on.

The two processes read the same files on the same nodes for different purposes. So a customer on
Auto Mode runs two host-metrics agents, and one of them — NMA — is already there by default.

**The thesis:** if NMA served a node_exporter-compatible `/metrics` endpoint, the PNE addon would
become unnecessary for most customers. One fewer DaemonSet, one fewer thing to install, upgrade
and reconcile, and no scrape-config change — provided the endpoint is *actually* compatible, not
approximately so.

"Approximately" is the crux. A dashboard, alert or recording rule binds to a metric **name**, its
**type**, and its **label key set**. Miss `node_network_speed_bytes` and someone's capacity panel
goes blank; emit `node_disk_read_bytes_total` in 4096-byte sectors instead of 512 and the number
is wrong by 8× while looking entirely plausible. So the goal was never "expose some node
metrics" — it was **literal parity, demonstrated against the real endpoint on a real cluster.**

### The question this document answers

Two ways to get there:

- **Dependency** — import `github.com/prometheus/node_exporter/collector` and serve its
  collectors from the agent.
- **Native** — own the collector code in-tree, with no dependency on `node_exporter`.

The dependency approach was built first and reached parity. The native approach was then built to
answer: **what does it actually cost to own this, and what does owning it buy?** Not whether it
*can* be done — that was never in doubt — but what the two trade against each other once both
exist and can be measured side by side on the same nodes in the same scrape pass.

Both now exist. This document is the design of both, the full record of what testing found, and a
recommendation.

---

## 2. What "parity" was defined as, before measuring anything

Defining the target first mattered, because a vague target is one you can always claim to have
hit. Five surfaces, each with a check that can fail:

| | Surface | The claim | How it is falsifiable |
|---|---|---|---|
| **S1** | Collectors | The default-enabled set matches upstream exactly | `node_scrape_collector_success` label sets differ → fail |
| **S2** | Metric identity | Name, type and label key set identical | any name present in one endpoint and not the other → fail |
| **S3** | Flags | Every upstream collector flag honoured, including `--path.*` | flag rejected or ignored → fail |
| **S4** | Meta-metrics | `node_scrape_collector_{success,duration_seconds}`, `node_exporter_build_info` | absent → fail |
| **S5** | Endpoint | `:9100/metrics`, landing page on `/`, concurrency bounded | existing scrape config breaks → fail |

**Two decisions here shaped everything downstream.**

**Metric names are compared at runtime, not read from source.** Upstream builds many names with
`prometheus.BuildFQName(namespace, subsystem, name)`, so the string never appears literally in the
code. Any source-grep approach silently misses them. Every comparison in this work therefore
scrapes both live endpoints and diffs what they actually served.

**Values were deliberately excluded from automated comparison.** Two scrapes seconds apart
legitimately differ on every counter, so a value diff produces noise indistinguishable from a
defect. This was the right call and it is *also* a real blind spot — §7.4 documents a defect that
lived in it for 13 hours.

---

## 3. Design: the dependency approach

**Shape:** a second HTTP listener inside the agent process, serving upstream's collectors through
upstream's own registry, on the conventional port.

```
pkg/metrics/
  server.go       the listener, handler tree, landing page, concurrency bound
  collectors.go   upstream flag resolution (kingpin), host-path rebasing, EKS defaults
  resilience.go   the panic/timeout boundary  <- the substantive part
```

**786 non-test lines, 1,614 test lines** on this branch. 47 files changed, 10,825 insertions.

> The native branch carries **892 / 2,106** for the same package, because the §7.4 fix and its
> tests landed there first. Those changes are **not yet on this branch** — see §9.1 step 2. Where
> this document cites a file that exists only on the native branch, it says so.

### 3.1 Design choices worth defending

**A separate listener, not the controller-runtime metrics endpoint.** The agent already serves an
endpoint carrying its own condition metrics, with an established contract. Node metrics go
somewhere else, on `:9100`, because the point is to *replace a node_exporter deployment* — which
means matching its port and path so no scrape config changes.

**Upstream's kingpin flags are reused verbatim rather than re-declared.** Upstream's collectors
read package-level flag variables registered in `init()`. Reimplementing that surface would mean
tracking every flag upstream adds. Instead `ResolveUpstreamFlags` parses a synthetic argv into
upstream's own `kingpin.CommandLine`. The cost is a process-wide latch (`sync.Once`) — ugly, but
honest about what upstream's design requires.

**`--path.*` rebased onto `HOST_ROOT`.** The agent runs as a DaemonSet with the host filesystem
at `/host`, so every path flag is rewritten. Order is load-bearing: EKS defaults, then host
paths, then operator `extraArgs` last, because kingpin is last-wins and the operator must be able
to override us.

**One EKS-specific default: excluding per-pod ephemeral mounts** from the filesystem collector.
Found by scaling pods and watching cardinality climb: PNE reports every
`volume-subpaths/…`/containerd-shm mount, one per pod with a unique UID. That is **unbounded
cardinality driven by pod churn** — a real cost to every customer's TSDB. This is a deliberate
divergence from upstream, documented in `docs/parity-exceptions.md`, and §7.5 shows it turned out
to matter for *availability*, not just cost.

### 3.2 The resilience boundary — the part that isn't glue

This is the one place the dependency approach does something upstream does not, and the
motivation is structural rather than stylistic.

In node_exporter, a collector that panics kills a process whose only job is serving metrics.
**In this agent, the same collector shares a process with the health monitors that publish
`NodeConditions`, which node auto-repair acts on.** A collector defect must not be able to take
down node health reporting.

Upstream offers nothing to build on: `NodeCollector.Collect` fans each collector into its own
goroutine and calls `Update()` directly, with no `recover()` and no timeout. **A panic on a
spawned goroutine cannot be recovered by the HTTP handler, because the handler is not on that
stack** — verified experimentally, not assumed. So `resilientCollector` replaces
`NodeCollector` rather than wrapping it, putting the `recover()` on the goroutine that calls
`Update`.

Known upstream failure modes this contains: #1007 (supervisord panic), #3346 (SIGSEGV during
collection), #1987 (textfile crash), #1841 (netclass scrape timeouts), #1353 (indefinite hang on
a stuck NFS mount). Upstream #2585 and #3649 are open requests for collector timeouts that still
do not exist.

**The subtle part is the relay channel.** An abandoned collector may keep emitting after the
timeout, by which time the registry has closed `ch`. Sending on a closed channel panics — on the
abandoned goroutine, where nothing can recover it — so the timeout guard would become a *new*
crash source. Metrics therefore pass through an intermediate channel that is drained into
oblivion after detachment.

Two bugs were found here during development, both by tests that asserted a sibling collector's
metrics survived:
- cutting the forwarder off on the *success* path dropped a healthy collector's metrics, because
  `Update` sends to `done` before the deferred `close(relay)` runs
- `ErrNoData` must map to `success=0` but **not** to an error log — upstream treats "nothing to
  report" as not-broken, and conflating them would fire alerts on every EC2 node

### 3.3 What the dependency approach achieved

Against PNE v1.12.1 on the same nodes: **304/304 `node_*` metric names identical (empty diff both
ways), 49/49 collectors agreeing, 0 series-count differences.** Parity, demonstrated rather than
asserted.

> **On the differing counts in this record.** Earlier documents cite 298 and 305. They are not
> contradictions, they count different sets at different times: **298** was an earlier snapshot
> before later collectors landed, **305** counted a slightly different name set, and **304** is
> the figure from the archived golden corpus (`evidence/golden-{pne,nma}-node45.prom`), which is
> the one that can be re-derived today. The **346** in §6.1 is all families including
> `go_*`/`process_*`/`promhttp_*` on the 6-node run, not just `node_*`. Cited here so a reader
> comparing documents does not have to guess which is authoritative: **304 `node_*` names, and the
> diff is empty, which is the claim that matters.**

---

## 4. Design: the native approach

**Shape:** upstream's collectors reimplemented in-tree, with a `prometheus.Collector` adapter
carrying the same resilience boundary.

```
pkg/hostmetrics/     60 files, 39 collectors
  collector.go       registry, Config.resolve(), ErrNoData, Set
  paths.go           Paths{ProcFS,SysFS,RootFS,UdevData} -- a struct, not flag globals
  devicefilter.go    shared by 10 upstream collectors
  prometheus.go      the resilience boundary (mirrors resilience.go)
  <37 collector files>
```

**8,523 non-test lines, 11,993 test lines, 100.0% coverage, 565 tests + 88 subtests.**

### 4.1 Where the boundary was drawn, and why `procfs` was kept

`prometheus/procfs` is **kept**. This is load-bearing, not a shortcut.

Upstream's collectors are largely thin wrappers over `procfs`/`sysfs`, which does the actual
parsing. Reimplementing it would mean reimplementing the parsers — and, measured during the port,
**upstream silently drops kernel fields that `procfs` does not model.** In `meminfo`: 55 keys in
`/proc/meminfo` → 51 mapped fields → 49 emitted metrics. Matching that behaviour requires the
same parsing layer. A hand-written parser would emit *more* metrics than upstream and break
parity in the other direction.

So: **own the collectors, keep the parsers.** That is where EKS-specific behaviour lives and it
is what the dependency actually costs.

### 4.2 What the dependency removal is worth, measured

Transitive module closure, `go list -deps`:

| | dependency (`pkg/metrics`) | native (`pkg/hostmetrics`) |
|---|---|---|
| non-stdlib packages | 123 | **85** |
| distinct modules | 39 | **18** |

**21 modules dropped**, including `node_exporter` itself, `kingpin`, `dennwc/btrfs`,
`dennwc/ioctl`, `godbus/dbus`, `mdlayher/{ethtool,genetlink,wifi}`, `safchain/ethtool`,
`hodgesds/perf-utils`, `ema/qdisc`, `beevik/ntp`, `mattn/go-xmlrpc`,
`prometheus-community/go-runit`, `coreos/go-systemd`, `golang.org/x/crypto`.

That list is the real argument for this approach. Most of those modules exist to serve collectors
that **emit nothing on an EC2 instance** — wifi, xmlrpc/supervisord, runit, systemd, qdisc — and
each is attack surface, CVE surface and upgrade surface in a privileged DaemonSet on every node.

### 4.3 Scope: 39 of 49 collectors

The 10 not ported: `bcache`, `bcachefs`, `bonding`, `fibrechannel`, `ipvs`, `nfs`, `nfsd`, `rapl`,
`tapestats`, `zfs`. None emits a single series on an EC2 node — verified on the live endpoint, not
assumed.

**A correction to earlier documentation.** Previous versions of this material said all 10 report
`collector_success=0`. That is wrong for one: **`bcache` reports `success=1` with zero series**,
because `/sys/fs/bcache` exists and is empty, and upstream's glob returns an empty slice rather
than an error. The other nine report `0`. The distinction is exactly the contract described in
§4.4 and it was overstated.

The consequence is honest and worth stating: **native's `node_scrape_collector_success` series
set differs from PNE's** — 39 series against 49. Any dashboard iterating that metric sees 10 fewer
rows. Recorded, not hidden.

### 4.4 The contract that took the most care: absent hardware

Established from the golden corpus and asserted in both directions:

> **Absent hardware is `success=1` with zero series — not `ErrNoData`.**

`filepath.Glob` returns an empty slice, not an error, so a collector finding no devices succeeds
having emitted nothing. The single inversion is **`hwmon`, which must FAIL** — it is the only
`success=0` among the ported 39 on a live EKS node. A port that "tidied" hwmon into succeeding
would pass a names-only comparison and be wrong.

### 4.5 Six unit conventions in one package

Value-correctness, not shape, is where a port silently goes wrong. A wrong unit produces a metric
with the right name, type and labels reporting a number off by a constant factor — invisible to
any structural comparison:

- pressure: **microseconds** · schedstat: **nanoseconds**
- timex: conditional on `STA_NANO`; PPM fields scale by `1e6 × 65536` (**+1 for frequency**)
- diskstats: **512-byte UNIX sectors** regardless of device block size — an NVMe device with
  4096-byte blocks still reports 512s, so reading the real block size gives numbers 8× too large
- btrfs commit: **milliseconds**
- powersupply **deci**-degrees vs thermal_zone **milli**-degrees — a factor of 100 apart, in two
  files that look alike

**The nastiest single field:** `ReadSectors` and `WriteSectors` are multiplied by 512 to become
`*_bytes_total`; `DiscardSectors` is **not**, because the metric is named
`discarded_sectors_total`. All three are "sectors" in `/proc/diskstats` and only two convert.
"Making it consistent" is a 512× error. It has a dedicated test asserting both the right value
*and* that the wrong one is absent.

Large tables (xfs 39 entries, powersupply 49 fields, infiniband 63) were **generated from
upstream source and verified by reflection-probing which struct field each accessor reads**,
rather than transcribed. Transcription is where a 39-row table acquires one wrong row.

### 4.6 Divergences from upstream, deliberate

| Divergence | Why |
|---|---|
| `newDeviceFilter` returns an error where upstream uses `regexp.MustCompile` | a panic in a DaemonSet is a crash loop with no message |
| `Paths` struct instead of flag globals | testable, no process-wide latch, no kingpin |
| sysfs opened once at construction (`clock`) | upstream reopens per scrape |
| injectable seams (`glob`, `rotational`, `udevProperties`) | otherwise-unreachable branches become testable on EKS hardware |

Deliberate parity exceptions, with impact stated: btrfs ioctl device stats (3 families absent on
a btrfs host — needs `CAP_SYS_ADMIN` the DaemonSet lacks, so upstream would fall back to procfs
anyway; no EKS node has btrfs); macOS `SystemVersion.plist` (unreachable); `netdev` detailed
metrics and `address-info` (both default-off); `hwmon` colliding chip label (cannot occur where
`/sys/class/hwmon` is absent).

---

## 5. How both were tested

Testing is the substance of this work, so it gets its own section rather than a footnote.

### 5.1 Tiers

| Tier | What it compares | Catches |
|---|---|---|
| **Unit** | fixtures with known inputs | wrong values, wrong units, truncation |
| **Upstream-source diff** | generated tables vs upstream source | a missing table row, a renamed field |
| **T1 structural** | exact metric-name set equality, live | a missing or extra family |
| **T2 collector** | `node_scrape_collector_success` per collector | the hwmon inversion; absent-hardware contract |
| **T3 series** | per-family series counts | a family that lost a device or CPU |
| **Logs** | errors/panics, WARN sets, DEBUG volume | a collector that works but complains every scrape |
| **Stress** | pod churn, mount churn, CPU saturation | a collector that breaks only when busy |
| **Scale** | 6 nodes, ~2,900 pods, sustained churn | anything rate- or cardinality-dependent |

All three variants are scraped **from a single pod in one pass**, so a difference cannot be an
artefact of measuring them minutes apart under different load.

### 5.2 Two principles that earned their keep

**A check that cannot fail proves nothing.** Every harness self-tests. `compare.sh` injects a
defect per tier and requires each tier to detect it. The no-dependency gate has `--self-test`
that runs it against `pkg/metrics`, which *is* dependent, and requires a non-zero verdict. Every
run reports `T1/T2/T3 detected 1 injected difference` — if it ever reports 0, the verdict above it
is meaningless.

**A positive control on a known-good pair.** PNE vs nma-dep was already measured as an empty
name diff, so
if the harness reports them as differing, the harness is broken rather than the code.

### 5.3 The recurring failure family

One shape of bug appeared over and over, in my own work, across weeks:

> **A check that reports a clean result precisely when it did not run.**

Instances, all real:

| Instance | What it reported | Why |
|---|---|---|
| `grep -c '^--- FAIL'` on a build failure | "0 failing tests" | `declared and not used` killed the build |
| `set -e` + a grep finding nothing | header, then silence | nma-dep logs no `implementation` field, so grep exited 1 and killed the script |
| `kubectl logs -l` on a multi-pod selector | 20 lines where one pod had 348 | doesn't reliably return every pod's history |
| `--tail` on a once-written startup line | "may be running the wrong implementation" | the line had aged out |
| expired credentials | "0 restarts" | the query failed and returned empty |
| `$p` in curl's single-quoted `-w` | every wall time `n/a` | shell never expanded it |
| pressure with no `pressure=true` label | a full passing "under pressure" column | churn pods Unschedulable; the pod-count guard was satisfied by an unrelated DaemonSet |

The mitigation is now structural: **guards fail loudly on a missing measurement rather than
skipping**, credentials are validated *before* measuring, and the wall-time bug was caught *only*
because its guard failed instead of reporting zero.

### 5.4 Test bugs found by coverage, not by failure

Three test helpers read `GetCounter()` only, so every gauge measured 0 and the assertions passed
anyway. A fourth handled neither Untyped, so netstat and textfile measured 0. All now route
through `metricValueOf`, which **fails the test** on an unknown type. And one test reimplemented
`NUD_NOARP` inline — it mirrored the logic it was checking and would have passed against a broken
collector. Caught at 80% coverage, not by a failure.

---

## 6. Measured comparison

All figures from live clusters, all three variants co-resident, scraped in the same pass.

### 6.1 Correctness — identical

```
T1 metric names        pne vs nma-dep     0 differences   <- positive control
                       pne vs nma-nodep   0 differences
                       nma-dep vs nodep   0 differences
T2 collector success   49 shared / 0 disagree  (pne vs nma-dep)
                       39 shared / 0 disagree  (both, vs nma-nodep)
T3 series counts       0 families differ, all three pairs
                       346/346 families identical
```

Re-verified at 6 nodes on 2026-07-29 with **one fewer exception than before**:
`promhttp_metric_handler_requests_*` used to be excluded because the agent didn't call
`InstrumentMetricHandler`. It now does, so the family is **compared like any other and matches**.

One expected difference remains: the EKS mount exclusion (PNE 13 series, both agents 4).

> **Which binary produced these numbers.** The 2026-07-29 re-verification ran **the native
> branch's build on both agent DaemonSets** — nma-dep served the upstream collectors and nma-nodep
> the native ones, selected by `metrics.implementation`, from one image. So the T1/T2/T3 results
> above validate *this branch's collector integration* but were produced by a binary that also
> carries the §7.4 fix. **This branch as it stands still has the duplicate-registration defect**
> (§7.4) and would show non-zero `promhttp_metric_handler_errors_total{cause="gathering"}` with
> `includeExporterMetrics: true`. That is §9.1 step 2, and it is why the step is listed as
> mandatory rather than cleanup.

### 6.2 Cost and footprint

| | dependency | native |
|---|---|---|
| non-test lines in the metrics package | 786 (892 with the §7.4 fix) | 8,523 |
| test lines | 1,614 (2,106 with §7.4 tests) | 11,993 |
| transitive non-stdlib packages | 123 | **85** |
| distinct modules | 39 | **18** |
| `node_exporter` packages in the graph | 2 | **0** |
| collectors registered | 49 | 39 |
| branch insertions vs `main` | 10,825 | 37,243 |

**~10× the code to own** against **21 fewer modules**. That is the trade, and §9 turns on it.

### 6.3 Performance — no difference, and the earlier claim was wrong

**An earlier revision reported native as ~15× faster, median ~250×. That was a measurement
error of mine, and the retraction is more useful than the original claim.**

`node_scrape_collector_duration_seconds` measures **wall** time, and every collector runs
**concurrently**. With fewer usable cores than collectors, each timer keeps running while its
goroutine is descheduled — so all of them report roughly *the whole batch's window*. The stress
harness **summed** them, multiplying the figure by up to the collector count.

The tell was in a distribution already published and misread:

```
GOMAXPROCS=1   dep sum=0.5552s  min=0.010862s  med=0.011322s  max=0.011806s   spread 1.09x
GOMAXPROCS=8   dep sum=0.0620s  min=0.000028s  med=0.001314s  max=0.005501s
```

`netclass` walks every interface; `loadavg` reads one short file. A **1.09× spread across 49
collectors** is not 49 similar measurements — it is one measurement reported 49 times.

Corrected, on wall time, live:

```
                 wall (baseline)  wall (pressure)   sum(cc) -- the misleading number
pne                 0.0136s          0.0132s              0.0149s
nma-dep             0.0107s          0.0157s              0.3390s
nma-nodep           0.0125s          0.0103s              0.0127s
```

**All three within ~1.5×.** Confirmed three ways: the same 49 collectors run **serially** cost
**0.0166s** against the 0.9114s once reported (~55× overstatement); wall times are near-identical
(1.22× by median in-process); and buffering the relay channel — the mechanism credited with ~4× —
**changed nothing** (0.5552s → 0.5243s), so that change was **reverted**.

**Hypothesis tested and rejected:** CPU throttling. Both agents genuinely *are* throttled —
nma-dep 5,124 throttle periods / 337.9s throttled at the chart-default `cpu: 250m`, PNE unlimited
and never throttled — but raising the limit 8× moved the median 7.72ms → 8.06ms. Real finding,
wrong cause; tracked separately as **Q10** since it belongs to neither branch.

**Consequence: performance carries no weight in the decision, in either direction.**

Guarded by 4 tests in `pkg/metrics/concurrentduration_test.go` (**on the native branch** —
they must come across with the §7.4 fix, per §9.1 step 2) asserting the *conclusion* rather than
machine-dependent timings, with a negative control showing a **264.5×** spread when cores are
available.

---

## 7. Every bug found

### 7.1 Upstream defects (5), reachability assessed individually

Conflating "found by reading the code" with "hits customers" would overstate the case, so
reachability is tracked per defect.

| # | Defect | Reachable on EKS? | Native |
|---|---|---|---|
| 1 | `filesystem` data race — two unsynchronised writers on one slice | yes | fixed |
| 2 | `vmstat` panic on a malformed line (no length check before `parts[1]`) | no — needs malformed `/proc` | fixed |
| 3 | **`netclass` #1915/#1841 all-or-nothing device read** | **yes — demonstrated** | **fixed at the cause** |
| 4 | `netstat` panic on an empty line | no — kernel never emits one (verified: 6 and 12 lines, zero blank) | fixed |
| 5 | `os_release` half-used mutex, 3 racing fields | **yes** (`--web.max-requests` defaults to 40) | fixed |

Two are genuine field bugs; three are robustness gaps found by adversarial reading.

**#3 is the one that matters.** Upstream returns on the first device read error, so **one
interface disappearing mid-scrape suppresses metrics for every interface**. On a static host that
window is almost never hit — which is why it has been open since 2020. On EKS the VPC CNI creates
and destroys veth interfaces on **every pod schedule**.

It also *retired a workaround*: excluding pod-side interfaces to dodge the hazard was measured to
**cost** `node_network_speed_bytes` (297 names vs 298 at that snapshot -- see the note in §3.3
on why counts differ across documents). Fixing the read made the exclusion
unnecessary.

**On the dependency branch #3 can only be *contained*** — the resilience boundary catches the
consequence, but the collector still returns nothing. **On native it is *fixed*:** the unreadable
device is skipped and the rest reported. Containment versus repair is the strongest argument for
owning the code.

### 7.2 #1915 reproduced in the field

Under sustained churn at **2,434 pods**, PNE's `netclass` reported `success=0` while **both agents
reported `success=1`, same node, same scrape pass**:

```
s    pne  dep  nodep   pods
5     1    1     1     2033
6     0    1     1     2434   <-- pne netclass FAILED
7     1    1     1     2802
10    1    1     1     2938
13-16 1    1     1       46   (post-pressure)
```

PNE's failing set went 10 → 11. `netdev` was unaffected, which **corroborates**: its default
backend is netlink, so it never performs the per-device sysfs reads #1915 concerns.

**Rate: 1 of 12 pressure samples**, 0 of 4 at rest. **Not correlated with load level** — it fired
at 2,434 pods and did *not* fire at the 2,938 peak, consistent with a race against interface
teardown being a matter of *timing* rather than pod count.

**An earlier run at comparable scale did not reproduce it**, recorded plainly at the time as
"PR1 — did not reproduce". Both runs are kept visible. Reachability is established; **n=1 failure
cannot distinguish "rare" from "unlucky sampling".**

### 7.3 Agent defects found in our own code

| Defect | Impact | Found by |
|---|---|---|
| Panic in a collector goroutine is unrecoverable from the handler | agent death | experiment, not assumption |
| Forwarder cut off on the success path | healthy collector's metrics silently dropped | a test asserting a sibling's metric survived |
| Per-pod mount cardinality unbounded in pod count | TSDB cost, and §7.5 | scaling pods and watching |
| `pb = *m` copies a `dto.Metric` containing a `sync.Mutex` | undefined | `go vet` |
| **Duplicate `promhttp_metric_handler_errors_total` registration** | **every scrape partial** | live endpoint diff |
| **`ErrorLog` unset** | 3,182 errors, zero log lines | the above |

### 7.4 The Q9 defect, in detail — because of how it hid

Adopting `InstrumentMetricHandler` (to close the last metric-name gap) exposed a defect firing on
**every scrape**.

`newHandler` built a handler with `Registry: registry`, then **reassigned** `promHandler` with
`Registry: exporterRegistry`. `HandlerOpts.Registry` registers
`promhttp_metric_handler_errors_total` onto whatever it is given, so the discarded first handler
had **already** placed that counter in the main registry, and the replacement placed it in the
exporter registry too. Gathering `Gatherers{exporterRegistry, registry}` then collected the same
family twice and failed with *"was collected before with the same name and label values"*.

```
nma-dep    3182 gathering errors (13h uptime)   +1 per scrape, verified over 3 consecutive scrapes
nma-nodep     9
pne           0
```

**Every scrape of both agents was serving a partial response** — and **nothing logged it**,
because `HandlerOpts.ErrorLog` was nil. Upstream sets it on both paths
(`node_exporter.go:157,172`); the port dropped it. A counter that records a fault plus a log that
never mentions it is the worst of both.

Fixed by using upstream's `if/else` so only one handler is ever built. **The
assign-then-reassign version reads as equivalent to upstream's and is not.**

**Scope, stated so it isn't overblown:** it fires only with `includeExporterMetrics: true`, which
is **not** the chart default (`values.yaml` sets `false`). Default deployments were unaffected;
the test cluster enables it.

**Why it survived 13 hours:** T1 compares names, T2 success values, T3 series counts. This was a
**value**. The name was present and the count was right. §2's exclusion of values is still
correct — and this is what it costs.

### 7.5 The mount exclusion turned out to be about availability

PNE **restarted 8 times** under churn; both agents **0**. Cause established, not assumed:

```
Liveness probe failed: Get "http://…:9100/": context deadline exceeded
Killing: Container node-exporter failed liveness probe, will be restarted
Last state: terminated, reason=Error, exit=143   (SIGTERM -- the kubelet, not the OOM killer)
```

`MemoryPressure=False` throughout, so **not an OOM**. PNE became too slow to answer its own 1s
liveness probe, three times consecutively.

Mechanism measured: PNE's `node_filesystem_readonly` **tracks pod churn** (64 / 23 / 49 series
across samples) while both agents stay pinned at **4**. And in latency, same node, same instant:

```
pne       :9100   0.422s     <- 1s probe timeout, failureThreshold 3
nma-dep   :9101   0.089s     4.8x faster
nma-nodep :9102   0.093s     4.5x faster
```

**Qualified, because the full credit does not belong to the collectors.** The agents' liveness
probe targets `/healthz` on **port 8002** — a separate endpoint — while PNE's targets `/` on the
metrics port itself. So part of the immunity is *probe design*. The two effects are separable:
the **4.7× endpoint speed-up** is attributable to the exclusion; the **0-vs-8 restart difference**
is that *plus* the probe not sharing the metrics path. Claiming the restarts purely for the
collectors would overstate it.

### 7.6 Harness bugs — the tooling was buggier than the code

Beyond §5.3: `items[0]` node selection (fine at 2 nodes, wrong at 6, where a stray release holds
`hostPort 9100` — so "PNE" could have been a different exporter); a **one-sided threshold** that
could only flag native being chattier, so nma-dep logging **25×** more DEBUG passed in silence;
division-by-zero on `n/a` that **aborted the whole analysis section**, taking out the
collector-failure and panic checks below it.

And a pre-existing flake: `TestManager_Notification` gave a **1ms** deadline to work crossing two
goroutines and a channel. Verified against the pre-change commit — **not a regression**. Raised to
5s, with a negative control confirming a stubbed `Notify` still fails it in 5.013s. **The control
was itself validated by checking the package still compiles**, because a "FAIL" that is really a
build failure proves nothing — the same trap as the `grep -c` incident.

### 7.7 Claims of mine that did not survive checking

Recorded because a doc that only lists successes is not a record.

- **"`/proc/net/arp` truncates device names and merges CNI interfaces."** False. `IFNAMSIZ` caps
  names at 15 usable chars, so a longer name cannot exist. The corrected claim became a test.
- **`node_cpu_frequency_avg_hertz` missing entirely** — descriptors live in `cpufreq_common.go`
  and I read only `cpufreq_linux.go`. Caught by the mechanical name+help+label diff.
- **netdev built on procfs, losing `node_network_receive_nohandler_total`** (7 live series),
  because upstream's default backend is netlink, which exposes `RXNoHandler` where procfs has no
  column. **All 565 unit tests passed** — they compared the port against *its own* table rather
  than the endpoint upstream serves. Only the end-to-end diff caught it. This is the honest shape
  of the porting cost: not "porting is hard" but **"porting produces defects that look correct
  from inside the port."**
- **"All 10 unported collectors report `success=0`"** — `bcache` reports `1` (§4.3).
- **F-N7-1**, the ~250× performance finding (§6.3).

---

## 8. The two alternatives, weighed

### 8.1 Dependency

**For:**
- **892 lines to review, not 8,523.** For an upstream contribution this is decisive — reviewing
  integration is a different proposition from reviewing 8,523 lines of ported collectors, and
  contribution guidance favours the smaller change.
- **Upstream fixes arrive for free.** v1.12.1 → v1.13 is a version bump.
- **All 49 collectors**, so `node_scrape_collector_success` matches PNE exactly.
- **Parity already demonstrated** — 304/304 `node_*` names, empty diff both ways.
- Upstream's flags are upstream's, so flag parity is structural rather than maintained.

**Against:**
- **123 packages / 39 modules**, most serving collectors that emit nothing on EC2, in a privileged
  DaemonSet on every node.
- **#1915 can only be contained, not fixed.** The collector still returns nothing.
- kingpin's process-wide `init()`-registered flag state, latched behind `sync.Once`.
- A version bump can change the metric surface without our knowing; only the live diff would tell
  us.

### 8.2 Native

**For:**
- **21 fewer modules, 38 fewer packages** — the strongest concrete benefit.
- **#1915 fixed at the cause**, plus four more upstream defects.
- Testability: `Paths` as a struct, injectable seams, 100.0% coverage, no process-wide latch.
- EKS-specific behaviour lives where it can be reasoned about.

**Against:**
- **8,523 lines to own**, and **upstream fixes must be ported, not received.** v1.12.1 → v1.13
  becomes a per-collector diff. The mechanical comparison tests exist to make that tractable, but
  it is recurring work.
- **39 of 49 collectors**, so the `collector_success` series set differs from PNE.
- **Porting produces defects invisible from inside the port** (§7.7, netdev).
- Deliberate exceptions (§4.6) are small but real.

### 8.3 What testing did *not* find

Both are indistinguishable on **metric names, per-collector success, series counts, error and
panic behaviour, and wall-time cost**, at baseline, under pressure, and at 2,938 pods. Anyone
expecting the measurements to separate them should note that they did not. **The decision is an
engineering-economics question, not a measurement question.**

---

## 9. Recommendation

> **Ship the dependency branch. Hold native as its successor. Raise #1915 upstream first — that
> single outcome determines whether native is ever needed.**

**Reasoning, in order of weight:**

1. **#1915 belongs upstream, not in a fork.** It affects every node_exporter user on Kubernetes.
   There is now a **field reproduction** (§7.2) and a regression test
   (`pkg/hostmetrics/netclass_test.go`, on the native branch). If upstream takes it, the
   dependency branch inherits the
   fix and native's single decisive advantage disappears. **This is the highest-value action
   available and everything else depends on its outcome.**

2. **The dependency branch is closer to shippable and far cheaper to review.** 892 lines against
   8,523, with parity already demonstrated.

3. **Ownership cost is recurring and real.** Every upstream release becomes a per-collector diff.
   Worth paying to fix a bug upstream will not take; **not** worth paying pre-emptively.

4. **Performance is not a factor** (§6.3, retracted).

5. **The dependency footprint is the one argument that survives everything.** 21 modules in a
   privileged per-node DaemonSet is a genuine security and maintenance cost, and it does not go
   away if upstream fixes #1915. It is not *by itself* worth 8,523 lines — but if a second
   field-reachable defect appears, footprint plus that defect tips the balance.

**What would change this:** upstream declining or sitting on #1915. That is the difference between
"a dependency with a known, contained bug" and "a dependency with a known bug that hits us on
every pod schedule" — and in the latter case owning the code is clearly correct.

### 9.1 Sequencing

1. **Raise #1915 upstream** with the regression test and the field reproduction. *Needs a
   go-ahead — a public post to a repository we do not own.*
2. **Carry the §7.4 fixes into whichever branch ships.** They live in `pkg/metrics`, so they
   apply to the dependency branch identically. Not optional cleanup: the endpoint was serving
   partial responses.
3. **Split the dependency branch into a clean PR.** Of 10,825 insertions only ~3,566 belong
   upstream; the rest are process artefacts. **`evidence/` contains an AWS account ID and Grafana
   credentials and must not be pushed to a public repository.**
4. **Decide the `cpu: 250m` / `GOMAXPROCS` question (Q10).** Independent of this choice, affects
   every EKS node, cheap to fix. Leaning: set `GOMAXPROCS` to match the quota, since the runtime
   currently schedules 2 threads into a 0.25-core budget.
5. **Hold native.** Merge if upstream declines #1915, or when a second such defect justifies the
   ownership cost.

### 9.2 One thing to decide either way

`metrics.implementation: upstream|native` exists so one binary can serve both, which is what made
the controlled comparison possible. **It should not survive as a public option.** Two collector
implementations behind a config flag is two code paths to support and a question customers should
not have to answer. Whichever wins becomes the only one.

---

## 10. Confidence and limits

**Well established:** the two implementations produce identical metric names, per-collector
success values and series counts on live EKS nodes, at baseline, under pressure and at 2,938
pods, with harnesses that self-test their ability to detect each class of difference and a
positive control on a known-good pair.

**Not established:**

- **Absolute metric values are not compared across implementations.** Structural and
  success-value agreement is strong evidence, not proof that every number matches. A `rate()`
  window driven from Prometheus is the missing tier — and §7.4 is what living without it costs.
- **#1915's failure rate.** One failure in 12 pressure samples, zero in an earlier run at
  comparable scale. Reachable, rate unknown.
- **`nma-dep` containing #1915 is not the same as fixing it.** Its `netclass` did not fail in this
  run; that is evidence, not a guarantee.
- **Single-window scale test** (~38 min, 6 nodes, one instance type, one churn profile). No
  multi-day soak, no arm64 at scale.
- **Wall time is measured client-side** from a pod on the same node, so it includes the kubelet
  network path. That is the honest cost of a Prometheus scrape but it is not pure collection time.
- **Q5 (resource envelope) is unresolved:** nma-dep sits at ~65MB steady versus PNE's ~23MB.
  Confirmed bounded rather than a leak, but on Auto Mode it feeds `EKSTachyonAMIOverhead` →
  Karpenter bin-packing → customer allocatable, and needs a position from the NMA owners.
