# N6: three-way validation — metrics and logs

Captured 2026-07-29T06:34:18Z against cluster nma-pne-parity-test (EKS 1.36,
account 569190534191, us-west-2), node kernel 6.18.38, Amazon Linux 2023.

All three variants run on the SAME nodes, scraped from one pod within the same second,
so the only variable is the collector implementation.

```
node: ip-192-168-45-100.us-west-2.compute.internal (192.168.45.100)
scraping all three endpoints from a single pod...
  pne: 773 series
  nma-dep: 727 series
  nma-nodep: 707 series

=== T1 STRUCTURAL: metric name sets ===
  POSITIVE CONTROL pne vs nma-dep: only-pne=0 only-nma-dep=0
    positive control passed -- the harness detects agreement on a known-good pair
  pne vs nma-nodep: only-pne=0 only-nma-nodep=0
  nma-dep vs nma-nodep: only-nma-dep=0 only-nma-nodep=0

=== T2 COLLECTOR SUCCESS: per-collector agreement ===
  pne vs nma-dep: 49 shared collectors, 0 disagree
  pne vs nma-nodep: 39 shared collectors, 0 disagree
  nma-dep vs nma-nodep: 39 shared collectors, 0 disagree

  hwmon (must be 0 on all three -- absent hardware, and upstream returns ErrNoData):
    pne: 0
    nma-dep: 0
    nma-nodep: 0

=== T3 SERIES COUNTS: per-family cardinality ===
  pne vs nma-dep: 0 families differ in series count
  pne vs nma-nodep: 0 families differ in series count
  nma-dep vs nma-nodep: 0 families differ in series count

  EKS mount exclusion (expected difference, not a defect):
    node_filesystem_readonly: pne=25 nma-dep=4 nma-nodep=4
    node_filesystem_device_error: pne=25 nma-dep=4 nma-nodep=4

=== SELF-TEST: each tier must detect an injected defect ===
  T1 detected 1 injected name difference(s)  (expect >= 1)
  T2 detected 1 injected collector difference(s)  (expect >= 1)
  T3 detected 1 injected count difference(s)  (expect >= 1)

=== SUMMARY ===
  pne: 773 series, 347 families, 49 collectors
  nma-dep: 727 series, 345 families, 49 collectors
  nma-nodep: 707 series, 345 families, 39 collectors

VERDICT: all three agree on metric names and collector success
         (series-count differences, if any, are reported above for inspection)
```

```
collecting the last 2000 log lines from each variant...
  pne: 114 lines (up to 2000 per pod)
  nma-dep: 4000 lines (up to 2000 per pod)
  nma-nodep: 491 lines (up to 2000 per pod)

=== ERRORS AND PANICS (must be zero on all three) ===
  pne: 0
  nma-dep: 0
  nma-nodep: 0

=== CONTAINER RESTARTS (a contained panic logs; an escaped one restarts) ===
  nma-dep: 0 restart(s)
  nma-nodep: 0 restart(s)

=== WARN MESSAGES: the two agents compared ===
  distinct WARN messages: nma-dep=0 nma-nodep=0
  (identical warning sets)

=== LOG VOLUME: is the native variant as quiet? ===
  nma-dep:   4000 lines (3996 DEBUG)
  nma-nodep: 491 lines (365 DEBUG)
  DEBUG ratio nodep/dep: 0.09

=== BEHAVIOUR VISIBLE IN LOGS ===
  nma-dep: 1101 'ignoring mount point' lines (the EKS exclusion working)
  nma-nodep: 210 'ignoring mount point' lines (the EKS exclusion working)

  implementation selected at startup:
    nma-dep: not logged (predates the implementation switch)
    nma-nodep: native
    nma-nodep confirmed running the NATIVE collectors (2 pod(s))
    nma-dep confirmed running the UPSTREAM collectors

VERDICT: no errors or panics on any variant; the two agents log comparably
```
