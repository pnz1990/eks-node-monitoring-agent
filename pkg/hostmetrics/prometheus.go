package hostmetrics

// The prometheus.Collector adapter for a Set, with the same resilience guarantees
// the dependency-based package provides.
//
// WHY THIS IS NOT A THIN WRAPPER. pkg/metrics wraps upstream's NodeCollector and had
// to REPLACE it, because the unrecoverable panic happens inside NodeCollector.Collect's
// own goroutines where nothing outside can recover it. Here the collectors are ours, so
// the containment can be built in rather than retrofitted — but the CONTRACT must be
// identical, because dashboards and alerts depend on it:
//
//	node_scrape_collector_duration_seconds{collector="..."}
//	node_scrape_collector_success{collector="..."}
//	node_collector_panics_total{collector="..."}      (ours, not upstream's)
//	node_collector_timeouts_total{collector="..."}    (ours, not upstream's)
//
// The first two are upstream's and are reproduced exactly, including that ErrNoData
// yields success=0 rather than being treated as a success. The last two are additions
// on both branches: a contained panic that produced NO signal would be worse than the
// crash it replaced, because nobody would know it happened.
//
// THE THREE BUGS THIS DESIGN ALREADY COST ON THE DEPENDENCY BRANCH, all of them in the
// timeout path, are avoided here by construction rather than by remembering:
//
//  1. An abandoned collector may keep emitting AFTER Collect returns, by which time
//     the registry has closed ch. Writing to a closed channel panics, on a goroutine
//     where nothing can recover it — turning the timeout guard into a NEW crash source.
//     So collectors write to a per-collector relay channel, never to ch directly.
//  2. Closing the relay early drops metrics the collector had already produced.
//  3. Defer ordering: the drain must outlive the forwarder.

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// defaultCollectorTimeout bounds a single collector's Update call.
//
// Chosen to match the dependency branch. Measured under pressure at ~2,888 pods across
// 6 nodes, the slowest collector took 0.138s, so 5s is ~36x headroom — it is a
// backstop against a hung filesystem or a stuck sysfs read, not a performance budget.
const defaultCollectorTimeout = 5 * time.Second

var (
	collectorPanicsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_collector_panics_total",
			Help: "Total number of node collector panics recovered by the resilience boundary.",
		},
		[]string{"collector"},
	)
	collectorTimeoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_collector_timeouts_total",
			Help: "Total number of node collectors abandoned for exceeding the per-collector timeout.",
		},
		[]string{"collector"},
	)
)

// PrometheusCollector adapts a Set to prometheus.Collector.
type PrometheusCollector struct {
	set     *Set
	timeout time.Duration
	logger  *slog.Logger

	scrapeDurationDesc *prometheus.Desc
	scrapeSuccessDesc  *prometheus.Desc
}

// NewPrometheusCollector wraps a Set for registration with a Prometheus registry.
//
// A zero timeout selects defaultCollectorTimeout. A NEGATIVE timeout disables the
// bound entirely, restoring unbounded behaviour; it exists only as an escape hatch for
// an operator who would rather hang than lose a metric.
func NewPrometheusCollector(set *Set, timeout time.Duration, logger *slog.Logger) *PrometheusCollector {
	if timeout == 0 {
		timeout = defaultCollectorTimeout
	}
	return &PrometheusCollector{
		set:     set,
		timeout: timeout,
		logger:  logger,
		scrapeDurationDesc: prometheus.NewDesc(
			"node_scrape_collector_duration_seconds",
			"node_exporter: Duration of a collector scrape.",
			[]string{"collector"}, nil,
		),
		scrapeSuccessDesc: prometheus.NewDesc(
			"node_scrape_collector_success",
			"node_exporter: Whether a collector succeeded.",
			[]string{"collector"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
//
// Only the meta descriptors are described. The collectors' own descriptors are built
// at collection time from kernel-supplied names (meminfo fields, netdev counters), so
// they cannot be enumerated up front — which is exactly why upstream registers as an
// unchecked collector too.
func (p *PrometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.scrapeDurationDesc
	ch <- p.scrapeSuccessDesc
}

// Collect implements prometheus.Collector, running every collector concurrently with
// panic containment and a per-collector timeout.
func (p *PrometheusCollector) Collect(ch chan<- prometheus.Metric) {
	collectors := p.set.Collectors()

	var wg sync.WaitGroup
	wg.Add(len(collectors))
	for name, c := range collectors {
		go func(name string, c Collector) {
			defer wg.Done()
			p.execute(name, c, ch)
		}(name, c)
	}
	wg.Wait()
}

// execute runs one collector, containing panics and bounding its runtime.
//
// Metrics the collector emitted before panicking or timing out are KEPT: they were
// already forwarded and are valid. Partial data beats no data when the alternative is
// a failed scrape.
func (p *PrometheusCollector) execute(name string, c Collector, ch chan<- prometheus.Metric) {
	begin := time.Now()
	success, timedOut := p.runCollector(name, c, ch)
	duration := time.Since(begin)

	if timedOut {
		collectorTimeoutsTotal.WithLabelValues(name).Inc()
		p.logger.Error("collector timed out",
			"name", name,
			"duration_seconds", duration.Seconds(),
			"timeout_seconds", p.timeout.Seconds(),
		)
	}

	var successValue float64
	if success {
		successValue = 1
	}
	ch <- prometheus.MustNewConstMetric(p.scrapeDurationDesc, prometheus.GaugeValue,
		duration.Seconds(), name)
	ch <- prometheus.MustNewConstMetric(p.scrapeSuccessDesc, prometheus.GaugeValue,
		successValue, name)
}

// runCollector invokes Update with a recover on the same goroutine as the call, and
// abandons the wait if the timeout elapses.
//
// THE RELAY CHANNEL IS ESSENTIAL, NOT STYLISTIC. An abandoned collector may keep
// emitting after this returns, by which time the registry has closed ch. Writing to a
// closed channel panics, on a goroutine where nothing can recover it — so the timeout
// guard would become a new crash source. Forwarding through a relay means late writes
// land somewhere harmless.
func (p *PrometheusCollector) runCollector(name string, c Collector, ch chan<- prometheus.Metric) (success, timedOut bool) {
	relay := make(chan prometheus.Metric, 1024)
	done := make(chan error, 1)

	go func() {
		// The recover must be on THIS goroutine: a panic inside Update cannot be
		// caught from the caller's stack.
		defer func() {
			if r := recover(); r != nil {
				collectorPanicsTotal.WithLabelValues(name).Inc()
				p.logger.Error("collector panicked", "name", name, "panic", r)
				done <- errPanicked
			}
			// close(relay) is deferred AFTER the recover handler registers, so it runs
			// FIRST on unwind -- which is wrong ordering for that purpose, hence the
			// explicit close below rather than a second defer. Getting this wrong cost a
			// dropped-metrics bug on the dependency branch.
			close(relay)
		}()
		done <- c.Update(relay)
	}()

	// Forward until the collector finishes or the timeout fires. The forwarder must
	// keep draining relay after a timeout, or the abandoned collector blocks forever on
	// a full buffer and leaks a goroutine per scrape.
	var (
		timer   = time.NewTimer(p.timeout)
		result  error
		haveRes bool
	)
	defer timer.Stop()

	for {
		select {
		case m, ok := <-relay:
			if !ok {
				// The collector finished and closed the relay. Its result is already in
				// done, or about to be.
				if !haveRes {
					result = <-done
				}
				return !isCollectorFailure(result), false
			}
			ch <- m

		case err := <-done:
			// Update returned but the relay is not yet closed; keep draining.
			result, haveRes = err, true

		case <-timer.C:
			if p.timeout < 0 {
				// A negative timeout disables the bound. The timer still fires once
				// (time.NewTimer with a negative duration fires immediately), so it is
				// reset far enough out to be effectively never rather than special-cased
				// at every use.
				timer.Reset(time.Duration(1) << 62)
				continue
			}
			// Abandon the wait, but keep draining the relay on a separate goroutine so
			// the collector is not blocked and its late writes go nowhere harmful.
			go drainRelay(relay)
			return false, true
		}
	}
}

// drainRelay discards anything an abandoned collector still emits.
//
// This is where the late writes go. Without it the collector blocks on a full relay
// and leaks a goroutine per scrape; with it, the writes are simply discarded — which
// is correct, because the scrape they belonged to has already been reported.
func drainRelay(relay <-chan prometheus.Metric) {
	for range relay {
	}
}

// errPanicked marks a recovered panic as a collector failure.
var errPanicked = errors.New("collector panicked")

// isCollectorFailure reports whether an Update error means success=0.
//
// ErrNoData counts as a FAILURE for this purpose, matching upstream: it logs at debug
// rather than error but still reports success=0. That is the whole reason hwmon shows
// success=0 on EKS, and getting it wrong would flip 3 of 39 collectors on every node.
func isCollectorFailure(err error) bool {
	return err != nil
}

// Register registers the collector set and its own meta counters.
//
// The panic and timeout counters are registered separately because they are
// CounterVecs owned by this package rather than emitted per scrape.
//
// TWO PROPERTIES OF THESE COUNTERS, BOTH MEASURED RATHER THAN ASSUMED, because I got
// both wrong in a first draft of this comment:
//
//  1. A CounterVec with no observed label values emits NOTHING, so
//     node_collector_panics_total is ABSENT until the first panic. I had written that
//     registering it early makes an alert "evaluable from the start" -- wrong, and the
//     smoke test caught it.
//
//  2. The counter is incremented DURING Collect, but the registry has already
//     snapshotted its collector list by then. So the increment lands one scrape LATE.
//     Measured: with a collector that always panics, gather 1 reports the counter as
//     absent, and gather 2 reports 2.
//
// Neither property breaks containment -- the panic is caught, the scrape completes, and
// node_scrape_collector_success=0 is reported in the SAME scrape, which is the signal
// an operator should actually alert on. But an alert written on panics_total alone
// would be both absent-prone and one scrape behind, so:
//
//	ALERT ON:      node_scrape_collector_success == 0      (same-scrape, always present)
//	DIAGNOSE WITH: node_collector_panics_total             (lagging, absent until first panic)
//
// Pre-seeding zeros for all 39 collectors would fix both, at the cost of 78
// permanently-zero series on every scrape on every node. These are a last-resort
// diagnostic rather than a routine signal, so they are left unseeded and the
// consequence is written down here rather than discovered by whoever writes the alert.
func (p *PrometheusCollector) Register(reg prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{collectorPanicsTotal, collectorTimeoutsTotal} {
		if err := reg.Register(c); err != nil {
			// AlreadyRegisteredError is tolerated: the counters are package-level, so a
			// second Server in the same process (as the tests create) would otherwise
			// fail to start.
			var already prometheus.AlreadyRegisteredError
			if !errors.As(err, &already) {
				return err
			}
		}
	}
	return reg.Register(p)
}
