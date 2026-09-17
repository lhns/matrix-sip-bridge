// Package callaudit turns livekit-sip's `call statistics` log line into the one
// assertion a call test actually cares about: did audio move in BOTH
// directions.
//
// The bridge is not in the media path (ADR-0005). Audio goes Asterisk <->
// livekit-sip <-> the LiveKit SFU <-> the Element Call client, and every hop of
// that can be healthy while the call is silent. livekit-sip emits one
// `call statistics` line per finished call, and its `stats` blob is the only
// machine-readable evidence that exists anywhere on this path:
//
//   - stats.port.audio_packets     RTP the SIP leg received. NON-ZERO even in
//     the silent case, because the far end does send audio -- this is why "a
//     call happened" reads as success and must never be the assertion.
//   - stats.room.input_packets     audio the LiveKit room delivered to the SIP
//     leg. Zero is the Matrix side being silent.
//   - stats.room.track_subscribes  tracks the SIP leg subscribed to. Zero means
//     it subscribed to nothing, which is the sharper form of the same fault.
//
// # Privacy
//
// The same log line carries the caller's number in `participant`,
// `participantName`, `toUser` and `reqUser`, and the Matrix room in `room`.
// None of those fields are parsed here, so no output of this package can carry
// them; keep it that way. The identifier used instead is `callID`, which
// LiveKit generates per call. See pkg/metrics for the label rule the exported
// metrics obey.
package callaudit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// logMarker is what livekit-sip logs the blob under. Only the console encoder
// puts it in front of the JSON; under a JSON encoder it is the `msg` field, so
// both are accepted.
const logMarker = "call statistics"

// Directions, as livekit-sip reports them.
//
// The trap: this is livekit-sip's own direction and is the INVERSE of the
// bridge's. Every call on this deployment is "outbound" because livekit-sip
// only ever originates (CreateSIPParticipant); a call arriving from the PSTN is
// an outbound call from livekit-sip. Do not join it to sip_bridge_calls_total's
// `direction` label.
const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
	// DirectionUnknown keeps the label set closed when livekit-sip reports
	// something new.
	DirectionUnknown = "unknown"
)

// Audio verdicts. Closed set: these are label values, and a new one is a new
// constant here.
const (
	// AudioBoth is the only passing verdict.
	AudioBoth = "both"
	// AudioSIPOnly is the silent call: the SIP leg received audio and the
	// LiveKit room delivered none, so the Matrix end hears the caller and the
	// caller hears nothing.
	AudioSIPOnly = "sip_only"
	// AudioMatrixOnly is the mirror image.
	AudioMatrixOnly = "matrix_only"
	// AudioNone is a call that moved no audio in either direction.
	AudioNone = "none"
	// AudioTooShort is a call that barely existed. Reported separately because
	// counting it as a failure makes every hangup-during-ring a false alarm,
	// and counting it as a pass makes a probe that never connected report
	// green.
	AudioTooShort = "too_short"
)

// Verdicts is every value Stats.Audio can return, for pre-initialising label
// combinations and for validating a -fail-on list.
var Verdicts = []string{AudioBoth, AudioSIPOnly, AudioMatrixOnly, AudioNone, AudioTooShort}

// Directions is every value Stats.Direction can hold.
var Directions = []string{DirectionInbound, DirectionOutbound, DirectionUnknown}

// DefaultMinPackets is the floor of TOTAL activity below which a call is
// AudioTooShort, in 20ms G.711 packets: half a second. It gates whether a call
// is judged at all; it is not applied per direction. See Audio.
const DefaultMinPackets int64 = 25

// Stats is the audio-bearing subset of one `call statistics` line.
type Stats struct {
	// Direction is livekit-sip's, not the bridge's. See the constants.
	Direction string
	// CallID is LiveKit's per-call identifier. Safe to print: it is generated
	// per call and identifies nobody.
	CallID string

	// SIPAudioPackets is stats.port.audio_packets.
	SIPAudioPackets int64
	// RoomInputPackets is stats.room.input_packets.
	RoomInputPackets int64
	// RoomTrackSubscribes is stats.room.track_subscribes.
	RoomTrackSubscribes int64
	// RoomPublishedFrames is stats.room.published_frames.
	RoomPublishedFrames int64
}

// Audio classifies the call. minPackets is the floor of total activity below
// which the call is not judged; pass DefaultMinPackets unless a test has a
// reason not to.
//
// The trap: minPackets gates the call, not each direction. The two counters are
// not on the same scale -- an observed working call carried 956 SIP packets
// against 51 room input ones -- so a floor big enough to dismiss a hangup
// during ring is within a factor of two of a real call's room side, and
// applying it per direction turns a short working call into a reported fault.
// Each direction is therefore judged on zero versus non-zero, which is the
// whole of the observed signal.
func (s Stats) Audio(minPackets int64) string {
	if minPackets < 1 {
		minPackets = 1
	}
	if s.SIPAudioPackets+s.RoomInputPackets+s.RoomPublishedFrames < minPackets {
		return AudioTooShort
	}
	haveSIP := s.SIPAudioPackets > 0
	haveRoom := s.RoomInputPackets > 0
	switch {
	case haveSIP && haveRoom:
		return AudioBoth
	case haveSIP:
		return AudioSIPOnly
	case haveRoom:
		return AudioMatrixOnly
	default:
		return AudioNone
	}
}

// Summary is a one-line, PII-free rendering for a human reading a probe run.
func (s Stats) Summary(minPackets int64) string {
	return fmt.Sprintf("call=%s direction=%s audio=%s subs=%d room_in=%d sip_audio=%d published=%d",
		s.CallID, s.Direction, s.Audio(minPackets),
		s.RoomTrackSubscribes, s.RoomInputPackets, s.SIPAudioPackets, s.RoomPublishedFrames)
}

// payload is the shape parsed out of the line. Only these fields: adding one
// that carries a number or a room would put it in the output.
type payload struct {
	Msg       string `json:"msg"`
	Direction string `json:"direction"`
	CallID    string `json:"callID"`
	Stats     *struct {
		Port *struct {
			AudioPackets int64 `json:"audio_packets"`
		} `json:"port"`
		Room *struct {
			InputPackets    int64 `json:"input_packets"`
			TrackSubscribes int64 `json:"track_subscribes"`
			PublishedFrames int64 `json:"published_frames"`
		} `json:"room"`
	} `json:"stats"`
}

// ErrNoStats is returned for a line that announces itself as call statistics
// and then carries no usable `stats` object. It is an error rather than a skip
// because the upstream field names are the whole contract here: a rename would
// otherwise turn every call into a silent zero, which reads exactly like the
// fault being looked for.
var ErrNoStats = fmt.Errorf("%q line carries no stats object", logMarker)

// Parse reads one log line. ok is false for any line that is not a call
// statistics line, which is almost all of them; err is non-nil only for a line
// that is one and could not be read.
func Parse(line string) (Stats, bool, error) {
	trimmed := strings.TrimSpace(line)

	// Two encoders. Under the JSON one the whole line is the object and the
	// marker is its msg, so it can only be rejected after decoding; under the
	// console one the blob starts at the first brace after the marker.
	jsonEncoder := strings.HasPrefix(trimmed, "{")
	body := trimmed
	if !jsonEncoder {
		i := strings.Index(trimmed, logMarker)
		if i < 0 {
			return Stats{}, false, nil
		}
		j := strings.Index(trimmed[i:], "{")
		if j < 0 {
			return Stats{}, true, ErrNoStats
		}
		body = trimmed[i+j:]
	}

	var p payload
	// A Decoder rather than Unmarshal: the console encoder appends nothing
	// after the blob today, but trailing text must not fail the parse.
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&p); err != nil {
		if jsonEncoder {
			// Any other JSON log line this failed to fit. Not ours.
			return Stats{}, false, nil
		}
		return Stats{}, true, fmt.Errorf("decode %q line: %w", logMarker, err)
	}
	if jsonEncoder && p.Msg != logMarker {
		return Stats{}, false, nil
	}
	if p.Stats == nil || p.Stats.Port == nil || p.Stats.Room == nil {
		return Stats{}, true, ErrNoStats
	}

	return Stats{
		Direction:           direction(p.Direction),
		CallID:              p.CallID,
		SIPAudioPackets:     p.Stats.Port.AudioPackets,
		RoomInputPackets:    p.Stats.Room.InputPackets,
		RoomTrackSubscribes: p.Stats.Room.TrackSubscribes,
		RoomPublishedFrames: p.Stats.Room.PublishedFrames,
	}, true, nil
}

func direction(v string) string {
	switch v {
	case DirectionInbound, DirectionOutbound:
		return v
	default:
		return DirectionUnknown
	}
}

// Scan reads a log stream and calls fn for each call statistics line.
// Malformed ones go to bad, so a caller can fail on them rather than see zero
// calls; every other line is ignored. One bad line does not stop the scan,
// because when the stream is a tail it has to keep going.
func Scan(r io.Reader, fn func(Stats), bad func(error)) error {
	sc := bufio.NewScanner(r)
	// The blob is ~2KB and the default 64KB token limit is close enough to a
	// long line to be worth raising: an over-long line is dropped silently.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		s, ok, err := Parse(sc.Text())
		if !ok {
			continue
		}
		if err != nil {
			if bad != nil {
				bad(err)
			}
			continue
		}
		fn(s)
	}
	return sc.Err()
}
