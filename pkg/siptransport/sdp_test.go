package siptransport

import (
	"errors"
	"strings"
	"testing"
)

func TestOfferedPayloads(t *testing.T) {
	tests := []struct {
		name string
		sdp  string
		want []string
	}{
		{
			name: "g711 offer",
			sdp:  "v=0\r\nm=audio 40000 RTP/AVP 0 8 101\r\na=rtpmap:0 PCMU/8000\r\n",
			want: []string{"0", "8", "101"},
		},
		{
			name: "lf line endings",
			sdp:  "v=0\nm=audio 40000 RTP/AVP 8\n",
			want: []string{"8"},
		},
		{
			name: "video before audio",
			sdp:  "v=0\r\nm=video 40002 RTP/AVP 96\r\nm=audio 40000 RTP/AVP 0\r\n",
			want: []string{"0"},
		},
		{"no audio stream", "v=0\r\nm=video 40002 RTP/AVP 96\r\n", nil},
		{"truncated m line", "v=0\r\nm=audio 40000\r\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := offeredPayloads([]byte(tt.sdp))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("offeredPayloads = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPickPayloadPrefersPCMU(t *testing.T) {
	tests := []struct {
		name    string
		offered []string
		want    string
		wantErr bool
	}{
		{"both", []string{"8", "0"}, "0", false},
		{"pcma only", []string{"8", "101"}, "8", false},
		{"pcmu only", []string{"0"}, "0", false},
		{"neither", []string{"9", "111"}, "", true},
		{"empty", nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickPayload(tt.offered)
			if tt.wantErr {
				if !errors.Is(err, ErrNoCommonCodec) {
					t.Fatalf("err = %v, want ErrNoCommonCodec", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("pickPayload = %q, want %q", got, tt.want)
			}
		})
	}
}

// chan_sip reads c=0.0.0.0 and a=inactive/a=sendonly as hold, and a zero port
// as a rejected stream. Any of them turns an answered call into music.
func TestAnswerSDPIsNotHold(t *testing.T) {
	answer, err := answerSDP([]byte("v=0\r\nm=audio 40000 RTP/AVP 0 8\r\n"), "192.0.2.10", 40000)
	if err != nil {
		t.Fatalf("answerSDP: %v", err)
	}
	got := string(answer)
	for _, bad := range []string{"0.0.0.0", "a=inactive", "a=sendonly", "m=audio 0 "} {
		if strings.Contains(got, bad) {
			t.Errorf("answer contains %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{"c=IN IP4 192.0.2.10", "m=audio 40000 RTP/AVP 0", "a=rtpmap:0 PCMU/8000", "a=sendrecv"} {
		if !strings.Contains(got, want) {
			t.Errorf("answer is missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("answer must use CRLF line endings:\n%q", got)
	}
}

func TestAnswerSDPRefusesUnknownCodecs(t *testing.T) {
	_, err := answerSDP([]byte("v=0\r\nm=audio 40000 RTP/AVP 111\r\n"), "192.0.2.10", 40000)
	if !errors.Is(err, ErrNoCommonCodec) {
		t.Fatalf("err = %v, want ErrNoCommonCodec", err)
	}
}

func TestOfferSDPOffersBothG711Codecs(t *testing.T) {
	got := string(offerSDP("192.0.2.10", 40000))
	if !strings.Contains(got, "m=audio 40000 RTP/AVP 0 8") {
		t.Errorf("offer does not advertise both G.711 codecs:\n%s", got)
	}
}
