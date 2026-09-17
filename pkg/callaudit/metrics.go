package callaudit

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Packet streams counted per call. Closed set, like every label here.
const (
	// StreamSIPIn is RTP the SIP leg received. Non-zero in the silent case too.
	StreamSIPIn = "sip_in"
	// StreamRoomIn is audio the LiveKit room delivered. Zero is the fault.
	StreamRoomIn = "room_in"
	// StreamRoomPublished is what the SIP leg published toward Matrix.
	StreamRoomPublished = "room_published"
)

var streams = []string{StreamSIPIn, StreamRoomIn, StreamRoomPublished}

// Line results.
const (
	LineParsed    = "parsed"
	LineMalformed = "malformed"
)

var lineResults = []string{LineParsed, LineMalformed}

// Recorder exports the audit as Prometheus series.
//
// The cardinality rule of pkg/metrics applies unchanged, and is tighter here
// than it looks: the source line carries a phone number in four fields and a
// Matrix room in a fifth, so every label below comes from a compile-time
// constant and nothing is ever derived from the log. callID is deliberately
// not a label.
//
// Every method tolerates a nil receiver, so the one-shot CLI path runs without
// a registry.
type Recorder struct {
	calls      *prometheus.CounterVec
	packets    *prometheus.CounterVec
	subscribes *prometheus.CounterVec
	lines      *prometheus.CounterVec
	lastCall   prometheus.Gauge
}

// NewRecorder registers the collectors and pre-initialises every label
// combination, so that "has this stopped happening?" can be asked of a series
// before its first event -- the whole point here, since the interesting
// verdicts are the rare ones.
func NewRecorder(reg prometheus.Registerer) *Recorder {
	r := &Recorder{
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "livekit_sip_audit_calls_total",
			Help: "Finished SIP calls, by livekit-sip direction and which way audio actually moved.",
		}, []string{"direction", "audio"}),
		packets: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "livekit_sip_audit_packets_total",
			Help: "Audio packets or frames accounted to finished calls, by stream.",
		}, []string{"direction", "stream"}),
		subscribes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "livekit_sip_audit_room_subscribes_total",
			Help: "LiveKit tracks the SIP leg subscribed to, summed over finished calls.",
		}, []string{"direction"}),
		lines: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "livekit_sip_audit_lines_total",
			Help: "Call statistics log lines seen, by whether they could be read.",
		}, []string{"result"}),
		lastCall: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "livekit_sip_audit_last_call_timestamp_seconds",
			Help: "Unix time at which the last call statistics line was read, or 0 if none has been.",
		}),
	}
	reg.MustRegister(r.calls, r.packets, r.subscribes, r.lines, r.lastCall)

	for _, d := range Directions {
		r.subscribes.WithLabelValues(d)
		for _, v := range Verdicts {
			r.calls.WithLabelValues(d, v)
		}
		for _, s := range streams {
			r.packets.WithLabelValues(d, s)
		}
	}
	// malformed at zero is the tripwire for an upstream field rename: without
	// it, a renamed stats field is indistinguishable from no calls at all.
	for _, res := range lineResults {
		r.lines.WithLabelValues(res)
	}
	return r
}

// Observe records one finished call.
func (r *Recorder) Observe(s Stats, minPackets int64) {
	if r == nil {
		return
	}
	r.lines.WithLabelValues(LineParsed).Inc()
	r.calls.WithLabelValues(s.Direction, s.Audio(minPackets)).Inc()
	r.packets.WithLabelValues(s.Direction, StreamSIPIn).Add(float64(s.SIPAudioPackets))
	r.packets.WithLabelValues(s.Direction, StreamRoomIn).Add(float64(s.RoomInputPackets))
	r.packets.WithLabelValues(s.Direction, StreamRoomPublished).Add(float64(s.RoomPublishedFrames))
	r.subscribes.WithLabelValues(s.Direction).Add(float64(s.RoomTrackSubscribes))
	r.lastCall.Set(float64(time.Now().Unix()))
}

// Malformed records a call statistics line that could not be read.
func (r *Recorder) Malformed() {
	if r == nil {
		return
	}
	r.lines.WithLabelValues(LineMalformed).Inc()
}
