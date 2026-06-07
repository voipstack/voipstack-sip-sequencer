package b2bua

import "time"

// MetricsSink is the observability event seam. The bridge and engine emit through
// this narrow interface; backend specifics (Prometheus, HTTP) live in PromMetrics.
// The default is noopMetrics, so the engine runs without an observability backend.
//
// All methods are fire-and-forget: safe for concurrent calls and never return errors.
type MetricsSink interface {
	// AppInvocation counts one successful app-leg completion, labelled by app name.
	AppInvocation(name string)
	// AppFailure counts one per-application failure, labelled by app name.
	AppFailure(name string)
	// TerminatingHopFailure counts one failed PBX (terminating-hop) attempt.
	TerminatingHopFailure()
	// ObserveSequencingLatency records the setup span of one established call.
	ObserveSequencingLatency(d time.Duration)
}

type noopMetrics struct{}

func (noopMetrics) AppInvocation(string)                   {}
func (noopMetrics) AppFailure(string)                      {}
func (noopMetrics) TerminatingHopFailure()                 {}
func (noopMetrics) ObserveSequencingLatency(time.Duration) {}
