package siptransport

import (
	"fmt"
	"strings"
	"time"
)

// The bridge answers with a real, non-held SDP even though it never carries
// media. Two chan_sip behaviours make anything else fatal:
//
//   - a 200 OK with no SDP sets SIP_PENDINGBYE and Asterisk tears the call
//     down immediately;
//   - c=0.0.0.0, a=inactive or a=sendonly is parsed as hold
//     (change_hold_state), so the caller hears music on hold instead of
//     falling through into the conference.
//
// Only the two static G.711 payload types are handled. The leg exists for a
// few hundred milliseconds, so there is nothing to gain from negotiating
// anything richer, and anything dynamic would need an offer-side rtpmap
// lookup for no benefit.
const (
	payloadPCMU = "0"
	payloadPCMA = "8"
)

// ErrNoCommonCodec is returned when the offer contains neither G.711 codec.
// The caller should reply 488 rather than answer with something it cannot name.
var ErrNoCommonCodec = fmt.Errorf("offer contains neither PCMU nor PCMA")

func rtpmapName(pt string) string {
	if pt == payloadPCMA {
		return "PCMA"
	}
	return "PCMU"
}

// offeredPayloads returns the payload types on the first m=audio line.
func offeredPayloads(sdp []byte) []string {
	for line := range strings.SplitSeq(strings.ReplaceAll(string(sdp), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "m=audio ") {
			continue
		}
		fields := strings.Fields(line)
		// m=<media> <port> <proto> <fmt> ...
		if len(fields) < 4 {
			return nil
		}
		return fields[3:]
	}
	return nil
}

// pickPayload chooses the codec to answer with, preferring PCMU.
func pickPayload(offered []string) (string, error) {
	var hasPCMA bool
	for _, pt := range offered {
		switch pt {
		case payloadPCMU:
			return payloadPCMU, nil
		case payloadPCMA:
			hasPCMA = true
		}
	}
	if hasPCMA {
		return payloadPCMA, nil
	}
	return "", ErrNoCommonCodec
}

// answerSDP builds the SDP answer to an offer.
func answerSDP(offer []byte, host string, port int) ([]byte, error) {
	pt, err := pickPayload(offeredPayloads(offer))
	if err != nil {
		return nil, err
	}
	return sessionSDP(host, port, []string{pt}), nil
}

// offerSDP builds the offer sent on an outbound control leg.
func offerSDP(host string, port int) []byte {
	return sessionSDP(host, port, []string{payloadPCMU, payloadPCMA})
}

func sessionSDP(host string, port int, payloads []string) []byte {
	var b strings.Builder
	id := time.Now().Unix()
	addrType := "IP4"
	if strings.Contains(host, ":") {
		addrType = "IP6"
	}
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=- %d %d IN %s %s\r\n", id, id, addrType, host)
	fmt.Fprintf(&b, "s=matrix-sip-bridge\r\n")
	fmt.Fprintf(&b, "c=IN %s %s\r\n", addrType, host)
	fmt.Fprintf(&b, "t=0 0\r\n")
	fmt.Fprintf(&b, "m=audio %d RTP/AVP %s\r\n", port, strings.Join(payloads, " "))
	for _, pt := range payloads {
		fmt.Fprintf(&b, "a=rtpmap:%s %s/8000\r\n", pt, rtpmapName(pt))
	}
	// sendrecv, not inactive or sendonly: chan_sip reads those as hold.
	fmt.Fprintf(&b, "a=sendrecv\r\n")
	fmt.Fprintf(&b, "a=ptime:20\r\n")
	return []byte(b.String())
}
