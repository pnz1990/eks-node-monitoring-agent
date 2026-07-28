// Package metrics contains end-to-end tests for the node_exporter compatible
// metrics endpoint served by the agent.
//
// The tests scrape the endpoint from inside the cluster and assert on the metric
// contract that Prometheus dashboards and alerting rules depend on. Metric names
// are asserted against a live scrape rather than a static list because several
// upstream names are generated at runtime from host state.
package metrics

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	daemonSetName      = "eks-node-monitoring-agent"
	daemonSetNamespace = "kube-system"
	// defaultMetricsPort is the node_exporter convention the agent reuses so
	// existing scrape configuration keeps working.
	defaultMetricsPort = 9100
)

// metricsPort is the port the endpoint is expected on. It is overridable because
// the endpoint may be moved off 9100 while upstream node_exporter still holds
// that port, which is the topology used for side-by-side comparison.
var metricsPort = func() int {
	if v := os.Getenv("NMA_E2E_METRICS_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			return p
		}
	}
	return defaultMetricsPort
}()

// coreMetrics are the metric families that essentially every node dashboard and
// alerting rule depends on. They are asserted individually so a failure names the
// specific missing family rather than reporting a generic mismatch.
var coreMetrics = []string{
	"node_cpu_seconds_total",
	"node_memory_MemTotal_bytes",
	"node_memory_MemAvailable_bytes",
	"node_filesystem_avail_bytes",
	"node_filesystem_size_bytes",
	"node_disk_read_bytes_total",
	"node_disk_written_bytes_total",
	"node_network_receive_bytes_total",
	"node_network_transmit_bytes_total",
	"node_load1",
	"node_load5",
	"node_load15",
	"node_boot_time_seconds",
	"node_uname_info",
	"node_vmstat_pgfault",
	"node_pressure_cpu_waiting_seconds_total",
}

// contractMetrics are the exporter self-metrics that form part of the endpoint
// contract. node_scrape_collector_success in particular is commonly alerted on.
var contractMetrics = []string{
	"node_exporter_build_info",
	"node_scrape_collector_success",
	"node_scrape_collector_duration_seconds",
}

// agentPod returns one running agent pod to scrape.
func agentPod(ctx context.Context, t *testing.T, cfg *envconf.Config) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	err := cfg.Client().Resources(daemonSetNamespace).List(ctx, &pods,
		resources.WithLabelSelector("app.kubernetes.io/name="+daemonSetName),
	)
	if err != nil {
		t.Fatalf("failed to list agent pods: %v", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return &pods.Items[i]
		}
	}
	t.Fatalf("no running agent pod found in %s", daemonSetNamespace)
	return nil
}

// scrapeMetrics fetches the metrics endpoint from inside the cluster by running a
// curl pod on the agent pod's node, reaching it over the host network.
func scrapeMetrics(ctx context.Context, t *testing.T, cfg *envconf.Config, nodeName string) string {
	t.Helper()

	podName := fmt.Sprintf("metrics-scraper-%d", time.Now().UnixNano())
	scraper := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: daemonSetNamespace,
		},
		Spec: corev1.PodSpec{
			// hostNetwork lets the scraper reach the agent's host-network
			// listener on localhost, matching how a node-local Prometheus would.
			HostNetwork:   true,
			NodeName:      nodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "scraper",
				Image: "public.ecr.aws/amazonlinux/amazonlinux:2023",
				Command: []string{"sh", "-c"},
				Args: []string{fmt.Sprintf(
					"command -v curl >/dev/null 2>&1 || dnf install -q -y curl >/dev/null 2>&1; "+
						"curl -sS --max-time 30 --retry 10 --retry-delay 3 --retry-connrefused "+
						"http://127.0.0.1:%d/metrics", metricsPort)},
			}},
		},
	}

	if err := cfg.Client().Resources().Create(ctx, scraper); err != nil {
		t.Fatalf("failed to create scraper pod: %v", err)
	}
	t.Cleanup(func() {
		_ = cfg.Client().Resources().Delete(context.Background(), scraper)
	})

	// Wait for the scrape to finish.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var p corev1.Pod
		if err := cfg.Client().Resources(daemonSetNamespace).Get(ctx, podName, daemonSetNamespace, &p); err == nil {
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}

	logs, err := podLogs(ctx, cfg, daemonSetNamespace, podName)
	if err != nil {
		t.Fatalf("failed to read scraper logs: %v", err)
	}

	if strings.TrimSpace(logs) == "" {
		t.Fatal("scraper returned no output; the metrics endpoint may not be listening")
	}
	return logs
}

// keys returns the sorted keys of m, for stable log output.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// podLogs reads the full stdout of a completed pod. The e2e framework's client
// does not expose a log reader, so this uses a typed clientset built from the
// same rest config.
func podLogs(ctx context.Context, cfg *envconf.Config, namespace, name string) (string, error) {
	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		return "", err
	}
	stream, err := cs.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	b, err := io.ReadAll(stream)
	return string(b), err
}

// EndpointServesCoreMetrics asserts the endpoint serves the metric families that
// dashboards and alerts depend on.
func EndpointServesCoreMetrics() features.Feature {
	return features.New("EndpointServesCoreMetrics").
		WithLabel("suite", "metrics").
		Assess("core node metrics are present", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			pod := agentPod(ctx, t, cfg)
			body := scrapeMetrics(ctx, t, cfg, pod.Spec.NodeName)

			for _, name := range coreMetrics {
				if !strings.Contains(body, "\n"+name) && !strings.HasPrefix(body, name) {
					t.Errorf("metric %q missing from the endpoint", name)
				}
			}
			t.Logf("verified %d core metric families on node %s", len(coreMetrics), pod.Spec.NodeName)
			return ctx
		}).
		Feature()
}

// EndpointServesContractMetrics asserts the exporter self-metrics are present.
func EndpointServesContractMetrics() features.Feature {
	return features.New("EndpointServesContractMetrics").
		WithLabel("suite", "metrics").
		Assess("exporter contract metrics are present", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			pod := agentPod(ctx, t, cfg)
			body := scrapeMetrics(ctx, t, cfg, pod.Spec.NodeName)

			for _, name := range contractMetrics {
				if !strings.Contains(body, name) {
					t.Errorf("contract metric %q missing from the endpoint", name)
				}
			}

			// Collectors for hardware or filesystems that are absent report
			// failure, and upstream node_exporter reports failure for exactly the
			// same set on an EKS node (verified: bcachefs, bonding, fibrechannel,
			// hwmon, ipvs, nfs, nfsd, rapl, tapestats, zfs). So "zero failures" is
			// the wrong assertion. What matters is that the collectors backing the
			// metrics customers actually consume are healthy.
			required := []string{"cpu", "meminfo", "filesystem", "diskstats", "netdev", "loadavg", "stat", "vmstat"}
			failed := map[string]bool{}
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "node_scrape_collector_success{") && strings.HasSuffix(line, " 0") {
					name := strings.SplitN(strings.SplitN(line, `collector="`, 2)[1], `"`, 2)[0]
					failed[name] = true
				}
			}
			for _, name := range required {
				if failed[name] {
					t.Errorf("core collector %q reported failure; its metrics would be silently missing", name)
				}
			}
			if len(failed) > 0 {
				t.Logf("collectors reporting failure (absent hardware/filesystem, matches upstream): %v", keys(failed))
			}
			return ctx
		}).
		Feature()
}

// MetricsDoNotDisturbNodeConditions asserts the agent still reports its health
// conditions while serving metrics, guarding the shared-fate risk of running a
// scrape-driven workload inside the event-driven agent.
func MetricsDoNotDisturbNodeConditions() features.Feature {
	return features.New("MetricsDoNotDisturbNodeConditions").
		WithLabel("suite", "metrics").
		Assess("node conditions are still reported", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			pod := agentPod(ctx, t, cfg)

			var node corev1.Node
			if err := cfg.Client().Resources().Get(ctx, pod.Spec.NodeName, "", &node); err != nil {
				t.Fatalf("failed to get node %s: %v", pod.Spec.NodeName, err)
			}

			// The agent owns these conditions; if serving metrics broke the
			// monitor loop they would be missing or stale.
			want := []string{"KernelReady", "StorageReady", "NetworkingReady", "ContainerRuntimeReady"}
			found := map[string]bool{}
			for _, c := range node.Status.Conditions {
				found[string(c.Type)] = true
			}
			for _, w := range want {
				if !found[w] {
					t.Errorf("node condition %q missing while metrics endpoint is enabled", w)
				}
			}
			return ctx
		}).
		Feature()
}
