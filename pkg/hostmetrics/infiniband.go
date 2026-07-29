package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/infiniband_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2017 The Prometheus Authors, Apache-2.0
//
// 63 metrics, and zero of them on a normal EKS node -- InfiniBand appears only on EFA
// instance types (p4d/p5, hpc6a), which is exactly the workload where someone WOULD
// look at these. So "emits nothing here" is not the same as "nobody needs it".
//
// Purely mechanical: 59 counters and 4 gauges, each a name paired with a nullable
// field. Generated from upstream's source and diffed in the test, for the same reason
// as xfs -- 59 hand-transcribed (name, field) pairs across two nested structs with
// names like ReqCqeError / ReqCqeFlushError / RespCqeError / RespCqeFlushError is a
// coin flip, and a mis-wired one reports a plausible number from the wrong counter.
//
// THE ONE NON-MECHANICAL PART: lifespan_seconds is divided by 1000 while every other
// hw counter is raw. The kernel reports it in MILLISECONDS despite the field being
// grouped with the counters. That is the fifth distinct unit convention in this
// package.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/sysfs"
)

const infinibandSubsystem = "infiniband"

func init() {
	register("infiniband", true, newInfiniBandCollector)
}

// infinibandDescriptions is upstream's help-text map, verbatim. Help text is part of
// the exposition output, so it is diffed against upstream in the test.
func infinibandDescriptions() map[string]string {
	return map[string]string{
		"duplicate_requests_packets_total":           "The number of received packets. A duplicate request is a request that had been previously executed.",
		"excessive_buffer_overrun_errors_total":      "Number of times that OverrunErrors consecutive flow control update periods occurred, each having at least one overrun error.",
		"implied_nak_seq_errors_total":               "The number of time the requested decided an ACK. with a PSN larger than the expected PSN for an RDMA read or response.",
		"legacy_data_received_bytes_total":           "Number of data octets received on all links",
		"legacy_data_transmitted_bytes_total":        "Number of data octets transmitted on all links",
		"legacy_multicast_packets_received_total":    "Number of multicast packets received",
		"legacy_multicast_packets_transmitted_total": "Number of multicast packets transmitted",
		"legacy_packets_received_total":              "Number of data packets received on all links",
		"legacy_packets_transmitted_total":           "Number of data packets received on all links",
		"legacy_unicast_packets_received_total":      "Number of unicast packets received",
		"legacy_unicast_packets_transmitted_total":   "Number of unicast packets transmitted",
		"lifespan_seconds":                           "The maximum period in ms which defines the aging of the counter reads. Two consecutive reads within this period might return the same values.",
		"link_downed_total":                          "Number of times the link failed to recover from an error state and went down",
		"link_error_recovery_total":                  "Number of times the link successfully recovered from an error state",
		"local_ack_timeout_errors_total":             "The number of times QP's ack timer expired for RC, XRC, DCT QPs at the sender side. The QP retry limit was not exceed, therefore it is still recoverable error.",
		"local_link_integrity_errors_total":          "Number of times that the count of local physical errors exceeded the threshold specified by LocalPhyErrors.",
		"multicast_packets_received_total":           "Number of multicast packets received (including errors)",
		"multicast_packets_transmitted_total":        "Number of multicast packets transmitted (including errors)",
		"np_cnp_packets_sent_total":                  "The number of CNP packets sent by the Notification Point when it noticed congestion experienced in the RoCEv2 IP header (ECN bits). The counters was added in MLNX_OFED 4.1",
		"np_ecn_marked_roce_packets_received_total":  "The number of RoCEv2 packets received by the notification point which were marked for experiencing the congestion (ECN bits where '11' on the ingress RoCE traffic) . The counters was added in MLNX_OFED 4.1",
		"out_of_buffer_drops_total":                  "The number of drops occurred due to lack of WQE for the associated QPs.",
		"out_of_sequence_packets_received_total":     "The number of out of sequence packets received.",
		"packet_sequence_errors_total":               "The number of received NAK sequence error packets. The QP retry limit was not exceeded.",
		"physical_state_id":                          "Physical state of the InfiniBand port (0: no change, 1: sleep, 2: polling, 3: disable, 4: shift, 5: link up, 6: link error recover, 7: phytest)",
		"port_constraint_errors_received_total":      "Number of packets received on the switch physical port that are discarded",
		"port_constraint_errors_transmitted_total":   "Number of packets not transmitted from the switch physical port",
		"port_data_received_bytes_total":             "Number of data octets received on all links",
		"port_data_transmitted_bytes_total":          "Number of data octets transmitted on all links",
		"port_discards_received_total":               "Number of inbound packets discarded by the port because the port is down or congested",
		"port_discards_transmitted_total":            "Number of outbound packets discarded by the port because the port is down or congested",
		"port_errors_received_total":                 "Number of packets containing an error that were received on this port",
		"port_packets_received_total":                "Number of packets received on all VLs by this port (including errors)",
		"port_packets_transmitted_total":             "Number of packets transmitted on all VLs from this port (including errors)",
		"port_receive_remote_physical_errors_total":  "Number of packets marked with the EBP (End of Bad Packet) delimiter received on the port.",
		"port_receive_switch_relay_errors_total":     "Number of packets that could not be forwarded by the switch.",
		"port_transmit_wait_total":                   "Number of ticks during which the port had data to transmit but no data was sent during the entire tick",
		"rate_bytes_per_second":                      "Maximum signal transfer rate",
		"req_cqes_errors_total":                      "The number of times requester detected CQEs completed with errors. The counters was added in MLNX_OFED 4.1",
		"req_cqes_flush_errors_total":                "The number of times requester detected CQEs completed with flushed errors. The counters was added in MLNX_OFED 4.1",
		"req_remote_access_errors_total":             "The number of times requester detected remote access errors. The counters was added in MLNX_OFED 4.1",
		"req_remote_invalid_request_errors_total":    "The number of times requester detected remote invalid request errors. The counters was added in MLNX_OFED 4.1",
		"resp_cqes_errors_total":                     "The number of times responder detected CQEs completed with errors. The counters was added in MLNX_OFED 4.1",
		"resp_cqes_flush_errors_total":               "The number of times responder detected CQEs completed with flushed errors. The counters was added in MLNX_OFED 4.1",
		"resp_local_length_errors_total":             "The number of times responder detected local length errors. The counters was added in MLNX_OFED 4.1",
		"resp_remote_access_errors_total":            "The number of times responder detected remote access errors. The counters was added in MLNX_OFED 4.1",
		"rnr_nak_retry_packets_received_total":       "The number of received RNR NAK packets. The QP retry limit was not exceeded.",
		"roce_adp_retransmits_timeout_total":         "The number of times RoCE traffic reached timeout due to adaptive retransmission. The counter was added in MLNX_OFED rev 5.0-1.0.0.0 and kernel v5.6.0",
		"roce_adp_retransmits_total":                 "The number of adaptive retransmissions for RoCE traffic. The counter was added in MLNX_OFED rev 5.0-1.0.0.0 and kernel v5.6.0",
		"roce_slow_restart_cnps_total":               "The number of times RoCE slow restart generated CNP packets. The counter was added in MLNX_OFED rev 5.0-1.0.0.0 and kernel v5.6.0",
		"roce_slow_restart_total":                    "The number of times RoCE slow restart changed state to slow restart. The counter was added in MLNX_OFED rev 5.0-1.0.0.0 and kernel v5.6.0",
		"roce_slow_restart_used_total":               "The number of times RoCE slow restart was used. The counter was added in MLNX_OFED rev 5.0-1.0.0.0 and kernel v5.6.0",
		"rp_cnp_ignored_packets_received_total":      "The number of CNP packets received and ignored by the Reaction Point HCA. This counter should not raise if RoCE Congestion Control was enabled in the network. If this counter raise, verify that ECN was enabled on the adapter.",
		"rp_cnp_packets_handled_total":               "The number of CNP packets handled by the Reaction Point HCA to throttle the transmission rate. The counters was added in MLNX_OFED 4.1",
		"rx_atomic_requests_total":                   "The number of received ATOMIC request for the associated QPs.",
		"rx_dct_connect_requests_total":              "The number of received connection requests for the associated DCTs.",
		"rx_icrc_encapsulated_errors_total":          "The number of RoCE packets with ICRC errors. This counter was added in MLNX_OFED 4.4 and kernel 4.19",
		"rx_read_requests_total":                     "The number of received READ requests for the associated QPs.",
		"rx_write_requests_total":                    "The number of received WRITE requests for the associated QPs.",
		"state_id":                                   "State of the InfiniBand port (0: no change, 1: down, 2: init, 3: armed, 4: active, 5: act defer)",
		"symbol_error_total":                         "Number of minor link errors detected on one or more physical lanes.",
		"unicast_packets_received_total":             "Number of unicast packets received (including errors)",
		"unicast_packets_transmitted_total":          "Number of unicast packets transmitted (including errors)",
		"vl15_dropped_total":                         "Number of incoming VL15 packets dropped due to resource limitations."}
}

// infinibandCounter pairs a metric name with its nullable accessor.
type infinibandCounter struct {
	name  string
	value func(*sysfs.InfiniBandPort) *uint64
}

// infinibandCounters is upstream's pushCounter sequence, field for field.
func infinibandCounters() []infinibandCounter {
	return []infinibandCounter{
		{"legacy_multicast_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortMulticastRcvPackets }},
		{"legacy_multicast_packets_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortMulticastXmitPackets }},
		{"legacy_data_received_bytes_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortRcvData64 }},
		{"legacy_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortRcvPackets64 }},
		{"legacy_unicast_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortUnicastRcvPackets }},
		{"legacy_unicast_packets_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortUnicastXmitPackets }},
		{"legacy_data_transmitted_bytes_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortXmitData64 }},
		{"legacy_packets_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LegacyPortXmitPackets64 }},
		{"excessive_buffer_overrun_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.ExcessiveBufferOverrunErrors }},
		{"link_downed_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LinkDowned }},
		{"link_error_recovery_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LinkErrorRecovery }},
		{"local_link_integrity_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.LocalLinkIntegrityErrors }},
		{"multicast_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.MulticastRcvPackets }},
		{"multicast_packets_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.MulticastXmitPackets }},
		{"port_constraint_errors_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvConstraintErrors }},
		{"port_constraint_errors_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortXmitConstraintErrors }},
		{"port_data_received_bytes_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvData }},
		{"port_data_transmitted_bytes_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortXmitData }},
		{"port_discards_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvDiscards }},
		{"port_discards_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortXmitDiscards }},
		{"port_errors_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvErrors }},
		{"port_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvPackets }},
		{"port_packets_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortXmitPackets }},
		{"port_transmit_wait_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortXmitWait }},
		{"unicast_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.UnicastRcvPackets }},
		{"unicast_packets_transmitted_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.UnicastXmitPackets }},
		{"port_receive_remote_physical_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvRemotePhysicalErrors }},
		{"port_receive_switch_relay_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.PortRcvSwitchRelayErrors }},
		{"symbol_error_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.SymbolError }},
		{"vl15_dropped_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.Counters.VL15Dropped }},
		{"duplicate_requests_packets_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.DuplicateRequest }},
		{"implied_nak_seq_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.ImpliedNakSeqErr }},
		{"local_ack_timeout_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.LocalAckTimeoutErr }},
		{"np_cnp_packets_sent_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.NpCnpSent }},
		{"np_ecn_marked_roce_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.NpEcnMarkedRocePackets }},
		{"out_of_buffer_drops_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.OutOfBuffer }},
		{"out_of_sequence_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.OutOfSequence }},
		{"packet_sequence_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.PacketSeqErr }},
		{"req_cqes_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.ReqCqeError }},
		{"req_cqes_flush_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.ReqCqeFlushError }},
		{"req_remote_access_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.ReqRemoteAccessErrors }},
		{"req_remote_invalid_request_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.ReqRemoteInvalidRequest }},
		{"resp_cqes_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RespCqeError }},
		{"resp_cqes_flush_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RespCqeFlushError }},
		{"resp_local_length_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RespLocalLengthError }},
		{"resp_remote_access_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RespRemoteAccessErrors }},
		{"rnr_nak_retry_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RnrNakRetryErr }},
		{"roce_adp_retransmits_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RoceAdpRetrans }},
		{"roce_adp_retransmits_timeout_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RoceAdpRetransTo }},
		{"roce_slow_restart_used_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RoceSlowRestart }},
		{"roce_slow_restart_cnps_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RoceSlowRestartCnps }},
		{"roce_slow_restart_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RoceSlowRestartTrans }},
		{"rp_cnp_packets_handled_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RpCnpHandled }},
		{"rp_cnp_ignored_packets_received_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RpCnpIgnored }},
		{"rx_atomic_requests_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RxAtomicRequests }},
		{"rx_dct_connect_requests_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RxDctConnect }},
		{"rx_read_requests_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RxReadRequests }},
		{"rx_write_requests_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RxWriteRequests }},
		{"rx_icrc_encapsulated_errors_total", func(p *sysfs.InfiniBandPort) *uint64 { return p.HwCounters.RxIcrcEncapsulated }}}
}

type infinibandCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	deviceFilter deviceFilter
	descs        map[string]*prometheus.Desc
	infoDesc     *prometheus.Desc

	infinibandClass func() (sysfs.InfiniBandClass, error)
}

func newInfiniBandCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newInfiniBandCollectorWithFilter(logger, paths, "", "")
}

// newInfiniBandCollectorWithFilter is split out so the invalid-pattern branches are
// reachable from a test and the filter can be wired to chart values later.
func newInfiniBandCollectorWithFilter(logger *slog.Logger, paths Paths, excludeExpr, includeExpr string) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	if excludeExpr != "" && includeExpr != "" {
		return nil, fmt.Errorf("infiniband device-exclude and device-include are mutually exclusive")
	}
	filter, err := newDeviceFilter(excludeExpr, includeExpr)
	if err != nil {
		return nil, fmt.Errorf("failed to build infiniband device filter: %w", err)
	}

	descriptions := infinibandDescriptions()
	descs := make(map[string]*prometheus.Desc, len(descriptions))
	for name, help := range descriptions {
		descs[name] = prometheus.NewDesc(
			prometheus.BuildFQName(namespace, infinibandSubsystem, name),
			help,
			[]string{"device", "port"}, nil,
		)
	}

	c := &infinibandCollector{
		fs:           fs,
		logger:       logger,
		deviceFilter: filter,
		descs:        descs,
		// Built once. Upstream constructs this Desc inside the per-device loop on every
		// scrape, which allocates one per device per scrape for a descriptor that never
		// changes.
		infoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, infinibandSubsystem, "info"),
			"Non-numeric data from /sys/class/infiniband/<device>, value is always 1.",
			[]string{"device", "board_id", "firmware_version", "hca_type"}, nil,
		),
	}
	c.infinibandClass = c.fs.InfiniBandClass
	return c, nil
}

func (c *infinibandCollector) Update(ch chan<- prometheus.Metric) error {
	devices, err := c.infinibandClass()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.logger.Debug("infiniband statistics not found, skipping")
			return ErrNoData
		}
		return fmt.Errorf("error obtaining InfiniBand class info: %w", err)
	}

	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)

	counters := infinibandCounters()

	for _, name := range names {
		device := devices[name]
		if c.deviceFilter.ignored(device.Name) {
			continue
		}

		ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, 1.0,
			device.Name, device.BoardID, device.FirmwareVersion, device.HCAType)

		portNumbers := make([]uint, 0, len(device.Ports))
		for portNum := range device.Ports {
			portNumbers = append(portNumbers, portNum)
		}
		sort.Slice(portNumbers, func(i, j int) bool { return portNumbers[i] < portNumbers[j] })

		for _, portNum := range portNumbers {
			port := device.Ports[portNum]
			portStr := strconv.FormatUint(uint64(port.Port), 10)

			// NOTE the device label is port.Name, NOT device.Name: procfs sets the port's
			// Name to its parent device, and using device.Name here would be correct by
			// accident today and wrong if that ever changed.
			c.pushGauge(ch, "state_id", uint64(port.StateID), port.Name, portStr)
			c.pushGauge(ch, "physical_state_id", uint64(port.PhysStateID), port.Name, portStr)
			c.pushGauge(ch, "rate_bytes_per_second", port.Rate, port.Name, portStr)

			// THE ONE UNIT CONVERSION. Reported in milliseconds despite living with the
			// raw hw counters; the metric is named _seconds. Integer division, matching
			// upstream -- switching to float would change the value.
			if port.HwCounters.Lifespan != nil {
				c.pushGauge(ch, "lifespan_seconds", *port.HwCounters.Lifespan/1000, port.Name, portStr)
			}

			for _, counter := range counters {
				value := counter.value(&port)
				// nil means the kernel does not expose that counter for this port --
				// most are Mellanox-specific. Emitting zero would claim a reading.
				if value == nil {
					continue
				}
				ch <- prometheus.MustNewConstMetric(c.descs[counter.name],
					prometheus.CounterValue, float64(*value), port.Name, portStr)
			}
		}
	}
	return nil
}

func (c *infinibandCollector) pushGauge(ch chan<- prometheus.Metric, name string, value uint64, device, port string) {
	ch <- prometheus.MustNewConstMetric(c.descs[name], prometheus.GaugeValue,
		float64(value), device, port)
}
