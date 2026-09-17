package callaudit

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The broken call and the working call differ in nothing a health check looks
// at: both connected, both ran for minutes, both carry a large non-zero
// port.audio_packets, because the far end always sends audio. The only
// difference is on the LiveKit side, and this is the table that says so.
func TestAudioVerdictSeparatesASilentCallFromAWorkingOne(t *testing.T) {
	tests := []struct {
		name  string
		stats Stats
		want  string
	}{
		{
			// The observed working call.
			name:  "audio both ways",
			stats: Stats{SIPAudioPackets: 956, RoomInputPackets: 51, RoomTrackSubscribes: 1, RoomPublishedFrames: 956},
			want:  AudioBoth,
		},
		{
			// The observed failure: 1163 SIP packets is what makes it look fine.
			name:  "silent call still carries PSTN audio",
			stats: Stats{SIPAudioPackets: 1163, RoomInputPackets: 0, RoomTrackSubscribes: 0, RoomPublishedFrames: 1163},
			want:  AudioSIPOnly,
		},
		{
			// The two counters are not on the same scale: the observed working
			// call carried 51 room packets against 956 SIP ones. A shorter but
			// equally working call must not read as the fault, which is what a
			// per-direction floor of DefaultMinPackets would have made it.
			name:  "a short call with audio both ways is not a fault",
			stats: Stats{SIPAudioPackets: 478, RoomInputPackets: 5, RoomTrackSubscribes: 1, RoomPublishedFrames: 478},
			want:  AudioBoth,
		},
		{
			name:  "matrix audio only",
			stats: Stats{SIPAudioPackets: 0, RoomInputPackets: 800, RoomTrackSubscribes: 1, RoomPublishedFrames: 0},
			want:  AudioMatrixOnly,
		},
		{
			name:  "nothing moved but the call ran",
			stats: Stats{SIPAudioPackets: 0, RoomInputPackets: 0, RoomTrackSubscribes: 1, RoomPublishedFrames: 900},
			want:  AudioNone,
		},
		{
			// A probe that never connected must not read as a pass, and a
			// hangup during ring must not read as a failure.
			name:  "call too short to judge",
			stats: Stats{SIPAudioPackets: 3, RoomInputPackets: 0, RoomPublishedFrames: 3},
			want:  AudioTooShort,
		},
		{
			name:  "empty stats are too short, not passing",
			stats: Stats{},
			want:  AudioTooShort,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.stats.Audio(DefaultMinPackets); got != tt.want {
				t.Errorf("Audio() = %s, want %s", got, tt.want)
			}
		})
	}
}

// Parsing runs against a real livekit-sip log rather than a hand-written blob,
// so a field the upstream renames shows up here rather than as a silent zero
// that reads exactly like the fault being looked for. The two silent calls are
// verbatim captures; the third carries a working call's observed figures in a
// line reassembled by hand, because no capture of a working call survived.
func TestParseCapturedLog(t *testing.T) {
	f, err := os.Open("testdata/livekit-sip.log")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	var got []Stats
	var bad []error
	if err := Scan(f, func(s Stats) { got = append(got, s) }, func(e error) { bad = append(bad, e) }); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("malformed lines: %v", bad)
	}
	want := []Stats{
		{Direction: DirectionOutbound, CallID: "SCL_uAV5enTMgCku", SIPAudioPackets: 1163, RoomInputPackets: 0, RoomTrackSubscribes: 0, RoomPublishedFrames: 1163},
		{Direction: DirectionOutbound, CallID: "SCL_9cfEBoddSQic", SIPAudioPackets: 50, RoomInputPackets: 0, RoomTrackSubscribes: 0, RoomPublishedFrames: 50},
		{Direction: DirectionOutbound, CallID: "SCL_WORKINGCALL001", SIPAudioPackets: 956, RoomInputPackets: 51, RoomTrackSubscribes: 1, RoomPublishedFrames: 956},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d call statistics lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Lines that are not call statistics -- which is nearly all of a livekit-sip
// log -- must be skipped silently, and a line that IS one but cannot be read
// must not be.
func TestParseClassifiesLines(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantOK  bool
		wantErr bool
	}{
		{name: "empty", line: ""},
		{name: "unrelated console line", line: "2026-09-16T22:21:10.004Z\tINFO\tsip\tsip/service.go:200\tusing session"},
		{name: "unrelated json line", line: `{"level":"info","msg":"using session","nodeID":"NE_x"}`},
		{name: "json encoder", line: `{"level":"info","msg":"call statistics","direction":"inbound","callID":"SCL_a","stats":{"port":{"audio_packets":10},"room":{"input_packets":10,"track_subscribes":1,"published_frames":10}}}`, wantOK: true},
		{name: "marker with no blob", line: "2026-09-16T22:24:32.239Z\tINFO\tsip\tcall statistics", wantOK: true, wantErr: true},
		{name: "marker with truncated blob", line: `2026-09-16T22:24:32.239Z	INFO	sip	call statistics	{"callID": "SCL_a", "stats": {"port"`, wantOK: true, wantErr: true},
		{name: "blob without stats", line: `2026-09-16T22:24:32.239Z	INFO	sip	call statistics	{"callID": "SCL_a", "direction": "outbound"}`, wantOK: true, wantErr: true},
		{name: "stats with a renamed room block", line: `2026-09-16T22:24:32.239Z	INFO	sip	call statistics	{"callID": "SCL_a", "stats": {"port": {"audio_packets": 10}}}`, wantOK: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, err := Parse(tt.line)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

// livekit-sip's direction is the inverse of the bridge's and is not a closed
// set upstream, so anything unrecognised has to collapse to one constant rather
// than become a new label value.
func TestDirectionIsAClosedSet(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"inbound", DirectionInbound},
		{"outbound", DirectionOutbound},
		{"", DirectionUnknown},
		{"Outbound", DirectionUnknown},
		{"transfer", DirectionUnknown},
	} {
		if got := direction(tt.in); got != tt.want {
			t.Errorf("direction(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The source line carries the caller's number in four fields and the Matrix
// room in a fifth. Nothing this package produces may repeat them, and the
// captured log is the fixture that would notice if a field were added to the
// parsed struct.
func TestNothingParsedOrPrintedCarriesTheCallerOrTheRoom(t *testing.T) {
	raw, err := os.ReadFile("testdata/livekit-sip.log")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The fixture's own placeholders. If the number in it is ever a real one
	// this test is not what catches it -- see the package doc.
	forbidden := []string{"+15550100", "ROOMHASHPLACEHOLDER", "sip-home-", "@sip_home-"}
	for _, f := range forbidden {
		if !strings.Contains(string(raw), f) {
			t.Fatalf("fixture no longer contains %q, so this test proves nothing", f)
		}
	}

	var out strings.Builder
	if err := Scan(strings.NewReader(string(raw)), func(s Stats) {
		out.WriteString(s.Summary(DefaultMinPackets))
		out.WriteString("\n")
	}, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, f := range forbidden {
		if strings.Contains(out.String(), f) {
			t.Errorf("Summary() leaked %q", f)
		}
	}
	// Belt and braces: no E.164-shaped run of digits at all.
	if m := regexp.MustCompile(`\+\d{6,}`).FindString(out.String()); m != "" {
		t.Errorf("Summary() carries what looks like a number: %q", m)
	}
}

// families is what /metrics has to carry before a single call has been seen.
var families = map[string]int{
	"livekit_sip_audit_calls_total":                 15, // 3 directions x 5 verdicts
	"livekit_sip_audit_packets_total":               9,  // 3 directions x 3 streams
	"livekit_sip_audit_room_subscribes_total":       3,
	"livekit_sip_audit_lines_total":                 2,
	"livekit_sip_audit_last_call_timestamp_seconds": 1,
}

// An exporter that has seen no call and one whose parser broke look
// identical unless every series exists at zero from the start -- including the
// malformed counter, which is the only thing that distinguishes an upstream
// field rename from a quiet day.
func TestEveryFamilyIsPresentAtZeroBeforeAnyCall(t *testing.T) {
	reg := prometheus.NewRegistry()
	r := NewRecorder(reg)

	got, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	seen := map[string]int{}
	for _, mf := range got {
		seen[mf.GetName()] = len(mf.GetMetric())
	}
	for name, want := range families {
		if seen[name] != want {
			t.Errorf("%s has %d series, want %d", name, seen[name], want)
		}
	}
	if len(seen) != len(families) {
		t.Errorf("registry has %d families, want %d: %v", len(seen), len(families), seen)
	}
	if v := testutil.ToFloat64(r.lastCall); v != 0 {
		t.Errorf("last_call_timestamp = %v before any call, want 0", v)
	}
}

func TestRecorderCountsTheCapturedLog(t *testing.T) {
	f, err := os.Open("testdata/livekit-sip.log")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	reg := prometheus.NewRegistry()
	r := NewRecorder(reg)
	if err := Scan(f, func(s Stats) { r.Observe(s, DefaultMinPackets) }, func(error) { r.Malformed() }); err != nil {
		t.Fatalf("scan: %v", err)
	}

	for _, tt := range []struct {
		metric prometheus.Collector
		labels []string
		want   float64
	}{
		{r.calls, []string{DirectionOutbound, AudioBoth}, 1},
		{r.calls, []string{DirectionOutbound, AudioSIPOnly}, 2},
		{r.calls, []string{DirectionOutbound, AudioNone}, 0},
		{r.packets, []string{DirectionOutbound, StreamSIPIn}, 1163 + 50 + 956},
		{r.packets, []string{DirectionOutbound, StreamRoomIn}, 51},
		{r.subscribes, []string{DirectionOutbound}, 1},
		{r.lines, []string{LineParsed}, 3},
		{r.lines, []string{LineMalformed}, 0},
	} {
		var c prometheus.Counter
		switch m := tt.metric.(type) {
		case *prometheus.CounterVec:
			c = m.WithLabelValues(tt.labels...)
		default:
			t.Fatalf("unexpected collector %T", m)
		}
		if got := testutil.ToFloat64(c); got != tt.want {
			t.Errorf("%v = %v, want %v", tt.labels, got, tt.want)
		}
	}
	if v := testutil.ToFloat64(r.lastCall); v <= 0 {
		t.Errorf("last_call_timestamp = %v after three calls, want a timestamp", v)
	}
}
