package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/lhns/matrix-sip-bridge/pkg/callaudit"
)

func defaults() options {
	return options{
		minPackets:   callaudit.DefaultMinPackets,
		requireCalls: 1,
		failOn:       splitVerdicts(defaultFailOn),
	}
}

// The exit status is the whole assertion, so the cases that must NOT be zero
// are the point: a silent call, and a log with no call in it. The second is the
// one a hand-rolled check gets wrong -- a probe that never placed a call scrapes
// an empty window and reports green.
func TestExitStatus(t *testing.T) {
	captured, err := os.ReadFile("../../pkg/callaudit/testdata/livekit-sip.log")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	working := lastCallLine(t, string(captured))

	tests := []struct {
		name string
		log  string
		opts func(options) options
		want int
	}{
		{
			name: "no call in the log proves nothing",
			log:  "2026-09-16T22:21:10.004Z\tINFO\tsip\tsip/service.go:200\tusing session\n",
			want: exitInvalid,
		},
		{
			name: "empty input proves nothing",
			log:  "",
			want: exitInvalid,
		},
		{
			name: "the captured log contains silent calls",
			log:  string(captured),
			want: exitNoAudio,
		},
		{
			name: "a working call alone passes",
			log:  working,
			want: exitOK,
		},
		{
			name: "two working calls when one was demanded",
			log:  working + working,
			opts: func(o options) options { o.requireCalls = 2; return o },
			want: exitOK,
		},
		{
			name: "fewer calls than demanded",
			log:  working,
			opts: func(o options) options { o.requireCalls = 2; return o },
			want: exitInvalid,
		},
		{
			// A renamed upstream field must not read as a quiet day.
			name: "an unreadable statistics line is not a pass",
			log:  working + "2026-09-16T22:24:32.239Z\tINFO\tsip\tcall statistics\t{\"callID\": \"SCL_a\"}\n",
			want: exitInvalid,
		},
		{
			name: "silent calls tolerated when explicitly not failed on",
			log:  string(captured),
			opts: func(o options) options { o.failOn = nil; return o },
			want: exitOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := defaults()
			if tt.opts != nil {
				o = tt.opts(o)
			}
			var out strings.Builder
			if got := run(strings.NewReader(tt.log), &out, o, nil); got != tt.want {
				t.Errorf("run() = %d, want %d\noutput:\n%s", got, tt.want, out.String())
			}
		})
	}
}

// statsLine builds one `call statistics` line carrying just the counters the
// verdict is made of. The caller and the room are left out of it entirely --
// the point of the fixture is the numbers, and a fixture that carries a number
// is a fixture that can leak one.
func statsLine(callID string, sipAudio, roomIn, subs int64) string {
	return fmt.Sprintf(
		"2026-09-16T22:24:32.239Z\tINFO\tsip\tsip/media.go:119\tcall statistics\t"+
			`{"callID": %q, "direction": "outbound", "stats": {"port":{"audio_packets":%d},`+
			`"room":{"input_packets":%d,"track_subscribes":%d,"published_frames":%d}}}`+"\n",
		callID, sipAudio, roomIn, subs, sipAudio)
}

// The one assertion this whole package exists for, on the two calls that were
// actually observed. They are indistinguishable to everything else: both
// connected, both ran for minutes, both carry a large port.audio_packets
// because the PSTN side always sends. Only room.input_packets and
// room.track_subscribes differ, and the exit status has to follow them.
func TestTheObservedSilentCallFailsAndTheObservedWorkingCallDoesNot(t *testing.T) {
	// subs=1 room_in=51 sip_audio=956
	working := statsLine("SCL_working", 956, 51, 1)
	// subs=0 room_in=0 sip_audio=1163 -- the silent call.
	silent := statsLine("SCL_silent", 1163, 0, 0)

	tests := []struct {
		name string
		log  string
		want int
	}{
		{name: "the working call alone is quiet", log: working, want: exitOK},
		{name: "the silent call alone fires", log: silent, want: exitNoAudio},
		{name: "one silent call among working ones still fires", log: working + silent + working, want: exitNoAudio},
		{name: "working calls only stay quiet", log: working + working, want: exitOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			if got := run(strings.NewReader(tt.log), &out, defaults(), nil); got != tt.want {
				t.Errorf("run() = %d, want %d\noutput:\n%s", got, tt.want, out.String())
			}
			// A verdict nobody can read is not a detector. The summary has to
			// name the counters that decided it.
			if !strings.Contains(out.String(), "room_in=") || !strings.Contains(out.String(), "subs=") {
				t.Errorf("summary does not show the deciding counters:\n%s", out.String())
			}
		})
	}
}

// The -listen path is only exercised when a registry is actually passed, and
// its absence is the default, so the two have to be run against the same input.
func TestRunWithAndWithoutARegistryAgree(t *testing.T) {
	captured, err := os.ReadFile("../../pkg/callaudit/testdata/livekit-sip.log")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var plain, exported strings.Builder
	want := run(strings.NewReader(string(captured)), &plain, defaults(), nil)
	reg := prometheus.NewRegistry()
	if got := run(strings.NewReader(string(captured)), &exported, defaults(), reg); got != want {
		t.Errorf("run() with a registry = %d, without = %d", got, want)
	}
	if plain.String() != exported.String() {
		t.Errorf("output differs with a registry:\n%s\n---\n%s", plain.String(), exported.String())
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) == 0 {
		t.Error("the registry is empty after three calls")
	}
}

func TestValidateRejectsAnUnknownVerdict(t *testing.T) {
	o := defaults()
	o.failOn = []string{callaudit.AudioBoth, "silent"}
	if err := validate(o); err == nil {
		t.Error("validate() accepted a verdict that no call can ever have, which would make -fail-on a no-op")
	}
	o.failOn = callaudit.Verdicts
	if err := validate(o); err != nil {
		t.Errorf("validate() rejected the full verdict set: %v", err)
	}
}

// lastCallLine returns the fixture's working call: audio both ways.
func lastCallLine(t *testing.T, log string) string {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		s, ok, err := callaudit.Parse(line)
		if !ok || err != nil {
			continue
		}
		if s.Audio(callaudit.DefaultMinPackets) == callaudit.AudioBoth {
			return line + "\n"
		}
	}
	t.Fatal("fixture has no call with audio both ways, so the passing cases below prove nothing")
	return ""
}
