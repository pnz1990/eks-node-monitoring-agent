# N7: stress and load across all three variants

Captured 2026-07-29T06:39:55Z. Cluster nma-pne-parity-test, 2x t3.large,
EKS 1.36, kernel 6.18.38. Pressure: pod churn + mount churn + CPU saturation,
20 -> 70 pods. All three variants measured in the SAME scrape pass from one pod.

```
node: ip-192-168-45-100.us-west-2.compute.internal (192.168.45.100)

=== BASELINE (no pressure) ===

  --- baseline ---
  variant       series   failed   latency   panics  timeouts
  pne              772       10    0.0212        0         0
  nma-dep          726       10    0.5528        0         0
  nma-nodep        706        1    0.0242        0         0

  cluster: 20 pods

=== APPLYING PRESSURE ===
  applied: 01-pod-churn.yaml
  applied: 03-mount-churn.yaml
  applied: 02-cpu-saturation.yaml

  settling for 60s so the pressure is actually in effect when measured...
  cluster: 70 pods (was 20)

=== UNDER PRESSURE ===

  --- pressure ---
  variant       series   failed   latency   panics  timeouts
  pne              771       10    0.0235        0         0
  nma-dep          725       10    0.3940        0         0
  nma-nodep        705        1    0.0210        0         0

=== ANALYSIS ===
  pne: series 772 -> 771 | failed 10 -> 10 | latency 0.0212s -> 0.0235s (1.11x)
  nma-dep: series 726 -> 725 | failed 10 -> 10 | latency 0.5528s -> 0.3940s (0.71x)
  nma-nodep: series 706 -> 705 | failed 1 -> 1 | latency 0.0242s -> 0.0210s (0.87x)

  restarts (an escaped panic restarts rather than logging):
    nma-dep: 0
    nma-nodep: 0

  native/upstream collection-time ratio under pressure: 0.05x
    (native runs 39 collectors, upstream 49, so <1 is expected)

  over the 39 SHARED collectors only (apples to apples):
    39 shared: nma-dep=0.3288s  nma-nodep=0.0210s  ratio=0.064x
    per-collector floor: nma-dep min=0.004912s  nma-nodep min=0.000011s

VERDICT: all three variants held under pressure -- no new collector failures,
         no panics, no timeouts, no restarts

=== CLEANUP ===
  deleted: 01-pod-churn.yaml
  deleted: 03-mount-churn.yaml
  deleted: 02-cpu-saturation.yaml
```
