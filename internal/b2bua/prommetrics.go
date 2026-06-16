package b2bua

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// CallSource exposes the live call/leg counts the gauges read on each scrape.
// It is the consumer-side interface PromMetrics needs; *Registry satisfies it.
type CallSource interface {
	ActiveCalls() int
	ActiveLegs() int
}

// PromMetrics is a Prometheus-backed MetricsSink plus an exposition handler.
// Counters and the histogram are pushed at emit points; the active-calls and
// active-legs gauges are pulled live from a CallSource on each scrape, so they
// cannot leak across call teardown. It owns a private registry to stay hermetic.
type PromMetrics struct {
	reg                    *prometheus.Registry
	appInvocations         *prometheus.CounterVec
	appFailures            *prometheus.CounterVec
	terminatingHopFailures prometheus.Counter
	mediaCodecMismatches   *prometheus.CounterVec
	sequencingDuration     prometheus.Histogram
}

// NewPromMetrics builds a PromMetrics with a private registry and all series
// registered against it (never the global DefaultRegisterer).
func NewPromMetrics() *PromMetrics {
	reg := prometheus.NewRegistry()

	appInvocations := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sequencer_app_invocations_total",
		Help: "Successful application-leg completions, by app name.",
	}, []string{"app"})
	appFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sequencer_app_failures_total",
		Help: "Application failures, by app name.",
	}, []string{"app"})
	terminatingHopFailures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "sequencer_terminating_hop_failures_total",
		Help: "Failed terminating-hop (PBX) attempts.",
	})
	mediaCodecMismatches := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sequencer_media_codec_mismatch_total",
		Help: "Established calls whose two anchored legs negotiated different audio codecs (no transcoding, so silent audio), by codec pair.",
	}, []string{"endpoint_codec", "pbx_codec"})
	sequencingDuration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "sequencer_sequencing_duration_seconds",
		Help:    "Per-call setup span from bridge entry to endpoint answer.",
		Buckets: prometheus.DefBuckets,
	})

	reg.MustRegister(appInvocations, appFailures, terminatingHopFailures, mediaCodecMismatches, sequencingDuration)

	return &PromMetrics{
		reg:                    reg,
		appInvocations:         appInvocations,
		appFailures:            appFailures,
		terminatingHopFailures: terminatingHopFailures,
		mediaCodecMismatches:   mediaCodecMismatches,
		sequencingDuration:     sequencingDuration,
	}
}

func (p *PromMetrics) AppInvocation(name string) { p.appInvocations.WithLabelValues(name).Inc() }

func (p *PromMetrics) AppFailure(name string) { p.appFailures.WithLabelValues(name).Inc() }

func (p *PromMetrics) TerminatingHopFailure() { p.terminatingHopFailures.Inc() }

func (p *PromMetrics) MediaCodecMismatch(endpointCodec, pbxCodec string) {
	p.mediaCodecMismatches.WithLabelValues(endpointCodec, pbxCodec).Inc()
}

func (p *PromMetrics) ObserveSequencingLatency(d time.Duration) {
	p.sequencingDuration.Observe(d.Seconds())
}

// BindCallSource registers the active-calls and active-legs GaugeFuncs that read
// src live on each scrape. Call once at startup.
func (p *PromMetrics) BindCallSource(src CallSource) {
	p.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "sequencer_active_calls",
		Help: "Calls currently active in the registry.",
	}, func() float64 { return float64(src.ActiveCalls()) }))
	p.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "sequencer_active_legs",
		Help: "Legs currently active across all calls.",
	}, func() float64 { return float64(src.ActiveLegs()) }))
}

// Handler serves the private registry in the Prometheus text exposition format.
func (p *PromMetrics) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{})
}
