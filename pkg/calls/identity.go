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

// LiveKitIdentity returns the MSC4195 hashed LiveKit participant identity for
// a MatrixRTC membership.
//
// This is why the bridge originates the SIP participant instead of letting
// LiveKit accept it via a dispatch rule: a dispatch-rule participant is named
// "sip_<caller>" by LiveKit and can never equal a chosen value, so Matrix
// clients would see an unattributed stream. Only CreateSIPParticipant lets the
// caller choose the identity.
func LiveKitIdentity(matrixUserID, deviceID, memberID string) string {
	return hashIdentifiers(matrixUserID, deviceID, memberID)
}

// IdentityScheme selects how a MatrixRTC membership is turned into a LiveKit
// participant identity. The two schemes are not interchangeable and there is
// no way to serve both at once; see ADR-0012.
type IdentityScheme string

const (
	// IdentityUserDevice is "<user id>:<device id>", used verbatim as both the
	// member ID and the LiveKit identity.
	IdentityUserDevice IdentityScheme = "user_device"
	// IdentityHashed is the MSC4195 hash of the user, device and member IDs,
	// with an opaque member ID.
	IdentityHashed IdentityScheme = "hashed"
)

// MemberIDFor returns the member ID to publish in the RTC membership.
//
// Under the hashed scheme it is an opaque string that only has to be stable;
// under the user/device scheme it is itself the identity, so it is not free.
func MemberIDFor(scheme IdentityScheme, matrixUserID, deviceID, opaque string) string {
	if scheme == IdentityHashed {
		return opaque
	}
	return matrixUserID + ":" + deviceID
}

// ParticipantIdentityFor returns the LiveKit participant identity that matches
// a membership published with the given member ID.
//
// Getting this wrong is the quietest failure in the bridge: the participant
// joins the right room, the server reports the tracks subscribed and healthy,
// packets flow with no loss, and the human hears nothing at all. See ADR-0010.
func ParticipantIdentityFor(scheme IdentityScheme, matrixUserID, deviceID, memberID string) string {
	if scheme == IdentityHashed {
		return LiveKitIdentity(matrixUserID, deviceID, memberID)
	}
	return memberID
}
