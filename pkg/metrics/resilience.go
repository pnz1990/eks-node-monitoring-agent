package metrics

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/node_exporter/collector"
)

// This file contains the resilience boundary between the upstream node_exporter
// collectors and the agent.
//
// The distinction matters more here than it does upstream. In node_exporter, a
// collector that panics or hangs degrades a process whose only job is serving
// metrics. In this agent the same collector shares a process with the health
// monitors that publish NodeConditions, which EKS node auto repair acts on, so a
// collector defect must never be able to take the agent down.
//
// Upstream offers no protection to build on. NodeCollector.Collect fans every
// collector out to its own goroutine and calls Update() directly
// (collector/collector.go:145-157); there is no recover() in that file and no
// timeout. A panic on a spawned goroutine cannot be recovered by the HTTP
// handler because the handler is not on that stack, so the recover has to sit
// directly around the Update call. Upstream issues #2585 and #3649 are open
// requests for collector timeouts that still do not exist.
//
// Known upstream failure modes this contains:
//   - #1007  panic when the supervisord collector cannot reach supervisord
//   - #3346  SIGSEGV during collection on linux/amd64
//   - #1987  crash with the textfile collector enabled
//   - #1841  netclass/bonding causing scrape timeouts
//   - #1353  indefinite hang on a stuck NFS mount

const (
	// defaultCollectorTimeout bounds a single collector's Update call. It is
	// well inside a typical 15s Prometheus scrape interval so a wedged collector
	// yields a partial scrape rather than a request that never returns.
	defaultCollectorTimeout = 5 * time.Second
)

var (
	// collectorPanicsTotal counts contained panics per collector. A non-zero
	// value is actionable: it means a collector is faulty on this node and its
	// metrics are missing, while the agent itself stayed up.
	collectorPanicsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_collector_panics_total",
			Help: "Total number of panics recovered while running a node collector.",
		},
		[]string{"collector"},
	)
	// collectorTimeoutsTotal counts collectors abandoned for exceeding the
	// timeout. Distinct from a failure, because the collector may still be
	// running; the scrape simply stopped waiting for it.
	collectorTimeoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_collector_timeouts_total",
			Help: "Total number of node collectors abandoned for exceeding the per-collector timeout.",
		},
		[]string{"collector"},
	)
)

// resilientCollector wraps the upstream collector set so that a panicking or
// hanging collector degrades only its own metrics.
//
// It replaces upstream's NodeCollector rather than wrapping it, because the
// unrecoverable panic happens inside NodeCollector.Collect's own goroutines and
// cannot be intercepted from outside.
type resilientCollector struct {
	collectors map[string]collector.Collector
	timeout    time.Duration
	logger     *slog.Logger

	// scrapeDurationDesc and scrapeSuccessDesc reproduce upstream's per-collector
	// meta metrics exactly, since dashboards and alerts depend on them.
	scrapeDurationDesc *prometheus.Desc
	scrapeSuccessDesc  *prometheus.Desc
}

// newResilientCollector builds the wrapper from an upstream collector set.
//
// A zero timeout selects defaultCollectorTimeout. A negative timeout disables
// the timeout entirely, which restores upstream's unbounded behaviour and is
// offered only as an escape hatch.
func newResilientCollector(nc *collector.NodeCollector, timeout time.Duration, logger *slog.Logger) *resilientCollector {
	if timeout == 0 {
		timeout = defaultCollectorTimeout
	}
	return &resilientCollector{
		collectors: nc.Collectors,
		timeout:    timeout,
		logger:     logger,
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
func (r *resilientCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- r.scrapeDurationDesc
	ch <- r.scrapeSuccessDesc
}

// Collect implements prometheus.Collector, running every collector concurrently
// with panic containment and a per-collector timeout.
func (r *resilientCollector) Collect(ch chan<- prometheus.Metric) {
	wg := sync.WaitGroup{}
	wg.Add(len(r.collectors))
	for name, c := range r.collectors {
		go func(name string, c collector.Collector) {
			defer wg.Done()
			r.execute(name, c, ch)
		}(name, c)
	}
	wg.Wait()
}

// execute runs one collector, containing panics and bounding its runtime.
//
// The metrics a collector emits before panicking or timing out are kept: they
// were already sent to ch and are valid. Partial data beats no data when the
// alternative is a failed scrape.
func (r *resilientCollector) execute(name string, c collector.Collector, ch chan<- prometheus.Metric) {
	begin := time.Now()
	success, timedOut := r.runCollector(name, c, ch)
	duration := time.Since(begin)

	if timedOut {
		collectorTimeoutsTotal.WithLabelValues(name).Inc()
		r.logger.Error("collector timed out",
			"name", name,
			"duration_seconds", duration.Seconds(),
			"timeout_seconds", r.timeout.Seconds(),
		)
	}

	var successValue float64
	if success {
		successValue = 1
	}
	ch <- prometheus.MustNewConstMetric(r.scrapeDurationDesc, prometheus.GaugeValue, duration.Seconds(), name)
	ch <- prometheus.MustNewConstMetric(r.scrapeSuccessDesc, prometheus.GaugeValue, successValue, name)
}

// runCollector invokes c.Update with a recover on the same goroutine as the
// call, and abandons the wait if the timeout elapses.
//
// It reports whether the collector succeeded and whether it timed out. A
// timeout leaves the collector goroutine running: it cannot be killed, because
// Update has no context parameter. The goroutine is left to finish on its own so
// the scrape can complete, which is the same trade upstream would have to make.
func (r *resilientCollector) runCollector(name string, c collector.Collector, ch chan<- prometheus.Metric) (success, timedOut bool) {
	done := make(chan error, 1)

	// The collector writes into its own channel rather than directly into ch.
	//
	// This indirection is essential, not stylistic. An abandoned collector may
	// keep emitting metrics after this function has returned, by which time the
	// registry has closed ch. Writing to a closed channel panics, and that panic
	// would be on the abandoned goroutine where nothing can recover it, turning
	// the timeout guard into a new crash source. Forwarding through an
	// intermediate channel means late writes land somewhere harmless.
	relay := make(chan prometheus.Metric)

	go func() {
		// Deferred in reverse order: close(relay) is registered first so it runs
		// LAST, guaranteeing the forwarder is released whether Update returns
		// normally or panics.
		//
		// The recover must be on the goroutine that calls Update. A recover in the
		// HTTP handler or in Collect cannot catch a panic raised here, which is
		// exactly why upstream's structure is fatal.
		defer close(relay)
		defer func() {
			if rec := recover(); rec != nil {
				collectorPanicsTotal.WithLabelValues(name).Inc()
				r.logger.Error("recovered panic in collector",
					"name", name,
					"panic", fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				done <- fmt.Errorf("collector %s panicked: %v", name, rec)
			}
		}()
		done <- c.Update(relay)
	}()

	// forward relays metrics to ch until the collector finishes or we stop
	// waiting. stopForwarding makes the forwarder detach without blocking the
	// collector, which would otherwise deadlock on an unbuffered send.
	stopForwarding := make(chan struct{})
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for {
			select {
			case m, ok := <-relay:
				if !ok {
					return
				}
				select {
				case ch <- m:
				case <-stopForwarding:
					// Detached: drain the rest into oblivion so the collector
					// goroutine can finish instead of blocking forever.
					go drain(relay)
					return
				}
			case <-stopForwarding:
				go drain(relay)
				return
			}
		}
	}()

	// finish waits for the forwarder to drain the relay before returning.
	//
	// It must NOT close stopForwarding on the success path: Update sends to done
	// before the deferred close(relay) runs, so metrics can still be in flight.
	// Cutting the forwarder off here would silently drop a healthy collector's
	// metrics — a bug this originally had, caught by TestPanicIsContained
	// asserting the sibling collector's metric survived.
	finish := func(err error) bool {
		<-forwarded // the forwarder returns when relay closes
		return r.classify(name, err)
	}

	if r.timeout < 0 {
		// Timeout disabled: wait indefinitely, matching upstream.
		return finish(<-done), false
	}

	timer := time.NewTimer(r.timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return finish(err), false
	case <-timer.C:
		// Abandoning: detach the forwarder so this scrape can complete, and let
		// the collector's own goroutine finish into the drain.
		close(stopForwarding)
		<-forwarded
		return false, true
	}
}

// drain discards anything a detached collector still emits, so its goroutine can
// run to completion rather than blocking on a send nobody is reading.
func drain(relay <-chan prometheus.Metric) {
	for range relay {
	}
}

// classify turns a collector's error into a success flag, preserving upstream's
// treatment of ErrNoData as a non-failure.
func (r *resilientCollector) classify(name string, err error) bool {
	if err == nil {
		r.logger.Debug("collector succeeded", "name", name)
		return true
	}
	if collector.IsNoDataError(err) {
		// Upstream logs this at debug and still reports success=0; a collector
		// with nothing to report is not broken.
		r.logger.Debug("collector returned no data", "name", name, "err", err)
		return false
	}
	r.logger.Error("collector failed", "name", name, "err", err)
	return false
}
