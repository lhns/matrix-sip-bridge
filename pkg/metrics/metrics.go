// Package metrics is the bridge's Prometheus instrumentation.
//
// # Cardinality
//
// No label ever carries a phone number, a JID, a SIP URI, a room ID, a call ID
// or a SIP status code. Every label value in this file comes from a closed set
// fixed at compile time, and a new value is a new constant here rather than a
// string built at a call site.
//
// That rule is what makes the exposition safe twice over. The metrics store
// keeps twelve months, so a label carrying a phone number is both a permanent
// PII leak and a cardinality problem that outlives the deployment that caused
// it -- one unbounded label has already produced tens of thousands of series
// and broken Grafana's explore view on this cluster. It is also the only
// reason /metrics can be served unauthenticated on the appservice port; see
// registerMetrics in pkg/connector.
//
// The status code of a refused SIP request is therefore in the zerolog line
// and not here: it is chosen by the carrier and is unbounded.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Direction labels. A call or a message is inbound when it came from the SIP
// side and outbound when Matrix started it.
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
)

// Call outcomes. This is the closed set pkg/calls maps a finished call onto,
// and the same switch that decides what the portal room is told; see
// callOutcome there.
const (
	// OutcomeAnswered is a call that reached StateBridged.
	OutcomeAnswered = "answered"
	// OutcomeMissed is a call nobody picked up, in either direction.
	OutcomeMissed = "missed"
	// OutcomeDeclined is a call a Matrix user rejected outright.
	OutcomeDeclined = "declined"
	// OutcomeFailed is a call the bridge could not carry.
	OutcomeFailed = "failed"
	// OutcomeStale is a row ended by bookkeeping long after its call was over.
	// It leaves no record in the room, but a rising rate of it is a bug in the
	// teardown paths, so it is counted rather than folded into "missed".
	OutcomeStale = "stale"
)

// Trunk reconcile results.
const (
	TrunkOK    = "ok"
	TrunkError = "error"
)

// Inbound message outcomes. Everything but MessageBridged is a text the far
// end considers delivered -- the bridge answers 200 to an unusable sender on
// purpose, so the far end does not retry -- which is why they are counted at
// all: nothing else makes those drops visible.
const (
	MessageBridged           = "bridged"
	MessageDroppedBadSender  = "dropped_unparseable_sender"
	MessageDroppedNoLogin    = "dropped_no_login"
	MessageRejectedMediaType = "rejected_content_type"
	MessageRejectedNoHandler = "rejected_no_handler"
)

// Outbound message outcomes. MessageRejected is the far end refusing the
// MESSAGE -- a trunk that takes calls but not text answers 501 -- as opposed
// to MessageError, which is not getting an answer at all.
const (
	MessageSent     = "sent"
	MessageRejected = "rejected"
	MessageTooLong  = "too_long"
	MessageError    = "error"
)

var (
	callOutcomes            = []string{OutcomeAnswered, OutcomeMissed, OutcomeDeclined, OutcomeFailed, OutcomeStale}
	inboundMessageOutcomes  = []string{MessageBridged, MessageDroppedBadSender, MessageDroppedNoLogin, MessageRejectedMediaType, MessageRejectedNoHandler}
	outboundMessageOutcomes = []string{MessageSent, MessageRejected, MessageTooLong, MessageError}
)

// callSetupBuckets span the ring: a call answered instantly, one answered
// after a few rings, and the 45s default ring timeout that bounds the whole
// range.
var callSetupBuckets = []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34, 45, 60}

// Recorder holds the bridge's collectors.
//
// Every method tolerates a nil receiver, so a Subsystem or a Transport built
// without one -- which is every test that does not care about metrics -- works
// unchanged and records nothing.
type Recorder struct {
	calls      *prometheus.CounterVec
	callSetup  *prometheus.HistogramVec
	registered prometheus.Gauge
	trunk      *prometheus.CounterVec
	messages   *prometheus.CounterVec
	start      prometheus.Gauge
	sipListen  prometheus.Gauge
}

// NewRegistry is the registry the bridge exposes. It is its own rather than
// the global default one, so that nothing a dependency registers behind the
// bridge's back can appear on /metrics without passing the cardinality rule.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// New registers the collectors and pre-initialises every label combination.
//
// The pre-initialisation is not cosmetic: a counter that only appears after
// its first event is absent rather than zero, and "has this stopped
// happening?" cannot be asked of a series that does not exist yet. The start
// timestamp is set here because New is called during startup.
func New(reg prometheus.Registerer) *Recorder {
	r := &Recorder{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sip_bridge_calls_total",
			Help: "Calls that have finished, by direction and outcome.",
		}, []string{"direction", "outcome"}),
		callSetup: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sip_bridge_call_setup_seconds",
			Help:    "Seconds from the call row being created to the call being bridged.",
			Buckets: callSetupBuckets,
		}, []string{"direction"}),
		registered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sip_bridge_sip_registered",
			Help: "1 when the SIP endpoint is listening and, if configured, registered.",
		}),
		trunk: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sip_bridge_trunk_reconcile_total",
			Help: "LiveKit outbound trunk reconciles, by result.",
		}, []string{"result"}),
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sip_bridge_messages_total",
			Help: "SIP MESSAGEs handled, by direction and outcome.",
		}, []string{"direction", "outcome"}),
		start: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sip_bridge_start_timestamp_seconds",
			Help: "Unix time at which this process started.",
		}),
		sipListen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sip_bridge_sip_listen_timestamp_seconds",
			Help: "Unix time at which the SIP listener bound, or 0 if it never has.",
		}),
	}
	reg.MustRegister(r.calls, r.callSetup, r.registered, r.trunk, r.messages, r.start, r.sipListen)

	for _, direction := range []string{DirectionInbound, DirectionOutbound} {
		r.callSetup.WithLabelValues(direction)
		for _, outcome := range callOutcomes {
			r.calls.WithLabelValues(direction, outcome)
		}
	}
	// The two message directions have different outcome sets on purpose: a
	// cross product would publish series such as inbound/too_long that no code
	// path can ever increment.
	for _, outcome := range inboundMessageOutcomes {
		r.messages.WithLabelValues(DirectionInbound, outcome)
	}
	for _, outcome := range outboundMessageOutcomes {
		r.messages.WithLabelValues(DirectionOutbound, outcome)
	}
	for _, result := range []string{TrunkOK, TrunkError} {
		r.trunk.WithLabelValues(result)
	}
	r.start.Set(float64(time.Now().Unix()))
	return r
}

// Handler serves the exposition format.
func Handler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{})
}

// Call counts a finished call. outcome is one of the Outcome constants.
func (r *Recorder) Call(direction, outcome string) {
	if r == nil {
		return
	}
	r.calls.WithLabelValues(direction, outcome).Inc()
}

// CallSetup records how long a call took to become bridged.
func (r *Recorder) CallSetup(direction string, d time.Duration) {
	if r == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	r.callSetup.WithLabelValues(direction).Observe(d.Seconds())
}

// SIPRegistered publishes whether the SIP endpoint is usable.
//
// It does not latch, unlike the readiness endpoint: readiness latches so that
// a dropped registration does not take the pod out of the Service, and this
// gauge is where that tolerated fault is still meant to be visible.
func (r *Recorder) SIPRegistered(ready bool) {
	if r == nil {
		return
	}
	var v float64
	if ready {
		v = 1
	}
	r.registered.Set(v)
}

// TrunkReconcile counts one reconcile of the LiveKit outbound trunk.
func (r *Recorder) TrunkReconcile(err error) {
	if r == nil {
		return
	}
	result := TrunkOK
	if err != nil {
		result = TrunkError
	}
	r.trunk.WithLabelValues(result).Inc()
}

// Message counts one SIP MESSAGE in either direction.
func (r *Recorder) Message(direction, outcome string) {
	if r == nil {
		return
	}
	r.messages.WithLabelValues(direction, outcome).Inc()
}

// SIPListening records that the SIP listener bound.
//
// Together with the start timestamp this is the startup window the readiness
// endpoint exists to close -- now minus start, while this is still 0 -- and a
// listener that never binds leaves it at 0 forever, which is what a pod that
// is 1/1 Running with a dead SIP side looks like from outside.
func (r *Recorder) SIPListening() {
	if r == nil {
		return
	}
	r.sipListen.Set(float64(time.Now().Unix()))
}
