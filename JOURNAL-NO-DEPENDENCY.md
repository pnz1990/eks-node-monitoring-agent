# Journal — host metrics with no node_exporter dependency

Append-only. Newest at the bottom. **Read this first on any resumption.** Companion to
`GOAL-NO-DEPENDENCY.md`; the dependency-based branch's record is in `JOURNAL.md`.

Rules: never rewrite history — a wrong conclusion gets a *new* correcting entry. Record refutations and
measurement mistakes as prominently as successes. Paste real values, not summaries.

---

## [2026-07-29T02:15Z] N0 — branch created, scope measured, interpretation question raised

**Phase:** N0
**Status:** confirmed

**What I did:** Branched `feat/metrics-no-upstream-dependency` from
`feat/prometheus-node-exporter-parity` so the entire validation apparatus carries over. Measured the
actual porting surface before planning.

**What I observed:**

| Component | Non-test lines | Files |
|---|---|---|
| `node_exporter/collector`, all platforms | 26,075 | 152 |
| `node_exporter/collector`, linux only | 19,636 | 100 |
| **the 49 collectors enabled on a live EKS node** | **~10,653** | 59 |
| **`prometheus/procfs`** | **21,639** | 13 packages |

**The finding that shapes the whole branch:** *54 of 152 collector files import `prometheus/procfs`.*
node_exporter's collectors are largely a thin metric-descriptor layer over `procfs`, which does the
actual `/proc` and `/sys` parsing. So vendoring `collector/` removes the *smaller* half of the code and
leaves the harder half as an external dependency.

This means "no dependency" is ambiguous, with three readings:

| Interpretation | Removes | Vendored lines |
|---|---|---|
| A | `node_exporter` only | ~10.6k |
| B | `node_exporter` + `procfs` | ~32k |
| C | clean-room, no copied code | write from scratch |

**Planned A**, with the residual stated openly: `procfs` is a general-purpose Prometheus-org parsing
library, the same class of dependency as `client_golang`, which we keep regardless because Prometheus
metric types come from it. B doubles the surface for little strategic gain and puts us in the business
of re-implementing `/proc/mountinfo` parsing, which is exactly where silent bugs live. C forfeits the
battle-testedness that makes any of this tractable.

**Open question posted to Slack (blocking N3):** if the intent behind "no dependency on PNE" is
specifically *no code from a competing exporter in our supply chain*, A satisfies it. If the intent is
*zero Prometheus-org dependencies*, A does **not**, and the scope roughly triples. Confirm before
porting begins.

**Two consequences recorded now so they are not discovered late:**

1. **Parity gets weaker, not stronger.** The dependency branch achieves parity on *all* upstream
   collectors including default-disabled ones. This branch ports only the 49 that run on EKS, so a
   customer enabling `--collector.buddyinfo` loses it. The honest claim becomes "parity on the 49
   collectors that run on EKS," and every unimplemented collector must be recorded.
2. **This cuts against the original motivation.** Contributing upstream becomes *harder* after this —
   it is a fork of a fork. That has to be weighed against the gains (no version coupling, direct bug
   fixes, no kingpin global-flag hack, smaller audit surface).

**Next:** N1 — extend the parity harness to three-way *before* writing any collector, and prove it
correctly **fails** against a stubbed implementation. A harness that cannot fail the new code is
worthless, and this is the same negative-control discipline that caught the dashboard verdict bug on
the previous branch.

---
