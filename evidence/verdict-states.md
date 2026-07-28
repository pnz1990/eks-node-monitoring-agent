# Verdict states — negative control

## RED (cpu+meminfo collectors disabled on the agent only)
```
  V1 missing-from-NMA : 51.0
  V1 extra-in-NMA     : 0.0
  V2 mismatches       : 0.0
  V3 %% within 5%%      : 25.00
  VERDICT             : 0.0 -> RED — NOT TRUSTWORTHY
```

## GREEN (healthy configuration restored)
```
  V1 missing-from-NMA : 0.0
  V1 extra-in-NMA     : 0.0
  V2 mismatches       : 0.0
  V3 %% within 5%%      : 100.00
  VERDICT             : 2.0 -> GREEN — METRICS TRUSTWORTHY
```

The RED state proves the dashboard verdict can fail. Without this control
the V1 panel would have shown 0 missing metrics while 51 were absent, due
to a missing 'on(__name__)' matcher in the set-difference query.
