# Coverage — new code

```
github.com/aws/eks-node-monitoring-agent/pkg/metrics/collectors.go:43:	ResolveUpstreamFlags		100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/collectors.go:56:	parseKingpin			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/collectors.go:71:	NewCollector			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/collectors.go:81:	EnabledCollectorNames		100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/collectors.go:100:	HostPathArgs			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:55:	withDefaults			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:96:	NewServer			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:110:	newServer			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:144:	registerCollectors		100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:159:	newHandler			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:211:	Registry			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:214:	Handler				100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:218:	Address				100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:229:	Start				100.0%
github.com/aws/eks-node-monitoring-agent/pkg/metrics/server.go:274:	NeedLeaderElection		100.0%
total:									(statements)			95.6%
```

pkg/metrics is 100.0% of statements. Error paths are reached via injected
seams (listen/serve/shutdown/construction) rather than excluded, so
.covignore is unchanged — no entries were added to reach the number.

New pkg/config symbols:
```
github.com/aws/eks-node-monitoring-agent/pkg/config/monitor.go:61:	IsEnabled			100.0%
github.com/aws/eks-node-monitoring-agent/pkg/config/monitor.go:77:	IsMetricsEnabled		100.0%
github.com/aws/eks-node-monitoring-agent/pkg/config/monitor.go:86:	GetMetricsSettings		100.0%
```

pkg/config totals 92.2% because of pre-existing untested code in that
package; every symbol added by this change is at 100%.
