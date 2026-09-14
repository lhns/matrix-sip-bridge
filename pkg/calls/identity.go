// Package calls bridges telephone calls as real media: the SIP server parks the
// far end in a conference, and livekit-sip is told to dial that conference and
// join the LiveKit room that backs the portal room's Element Call. The bridge
// itself never enters either.
package calls

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

// SlotRoom is the MatrixRTC slot identifier for a room-scoped call, which is
// what Element Call's native call button starts.
const SlotRoom = "m.call#ROOM"

// hashIdentifiers implements the MSC4195 hash derivation: unpadded standard
// base64 of the SHA-256 of the compact JSON array of the inputs.
//
// Two details are load-bearing and easy to get wrong in Go:
//   - the JSON must be compact and must not HTML-escape, so the encoder's
//     SetEscapeHTML(false) is required rather than plain json.Marshal;
//   - the alphabet is standard base64 ("+/"), not URL-safe, and unpadded.
//
// Getting either wrong yields a plausible-looking string that silently never
// matches the room lk-jwt-service put the Matrix clients in.
func hashIdentifiers(parts ...string) string {
	sum := sha256.Sum256(marshalStrings(parts))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// marshalStrings renders the compact JSON array that the hash is taken over.
func marshalStrings(parts []string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encode appends exactly one newline; the hash is over the JSON only.
	_ = enc.Encode(parts)
	b := buf.Bytes()
	return b[:len(b)-1]
}

// LiveKitRoomName returns the LiveKit room that backs a MatrixRTC session in
// the given Matrix room. The bridge must compute the same value as
// lk-jwt-service, or livekit-sip joins an empty room of its own.
func LiveKitRoomName(matrixRoomID, slot string) string {
	return hashIdentifiers(matrixRoomID, slot)
}

// LiveKitIdentity returns the LiveKit participant identity for a MatrixRTC
// membership.
//
// This is why the bridge originates the SIP participant instead of letting
// LiveKit accept it via a dispatch rule: a dispatch-rule participant is named
// "sip_<caller>" by LiveKit and can never equal this hash, so Matrix clients
// would see an unattributed stream. Only CreateSIPParticipant lets the caller
// choose the identity.
func LiveKitIdentity(matrixUserID, deviceID, memberID string) string {
	return hashIdentifiers(matrixUserID, deviceID, memberID)
}
