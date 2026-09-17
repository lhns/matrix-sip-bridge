package phonenum

import (
	"errors"
	"fmt"
	"regexp"
)

// portalIDLine splits a composite portal ID into its line and its number.
//
// The number is anchored as the trailing run of digits, optionally carrying
// the "+" that says it is E.164, so everything before the last hyphen that
// leaves only a number behind is the line name: a line may itself contain
// hyphens ("office-main-+15551234567") or end in a digit
// ("line2-+15551234567"). A line name cannot end in "+", so there is nothing
// to disambiguate. Nothing here knows which lines exist -- that is the SIP
// server's routing, not the bridge's.
var portalIDLine = regexp.MustCompile(`^(.*)-(\+?[0-9]+)$`)

// SplitID splits a portal ID into the line it belongs to and the number as the
// ID spells it, the two halves the conference header
// "<prefix><line>-<number>" carries.
//
// A portal ID that is not composite -- a bare-digit ID from before lines
// existed, or anything malformed -- is returned whole as the number with no
// line. Use NumberFromID rather than this number directly: the two spellings
// are not the same, see there.
func SplitID(id string) (line, number string) {
	if m := portalIDLine.FindStringSubmatch(id); m != nil {
		return m[1], m[2]
	}
	return "", id
}

// NumberFromID returns the number to dial, to address a SIP MESSAGE to, or to
// show a human, from a portal ID.
//
// The two ID forms spell a number differently, and the asymmetry is
// deliberate. A composite ID is self-describing: its number carries a "+" when
// it is E.164 and none when it is a short number that only means anything to
// its line, so it is used exactly as written. A legacy ID predates lines and
// is E.164 with the "+" stripped (ADR-0003), so the "+" goes back on.
//
// Nothing here repairs an ID it cannot read: a malformed one keeps its shape
// all the way to the wire, where the SIP server's route regex refuses it. It
// must never be trimmed into something dialable.
func NumberFromID(id string) string {
	if m := portalIDLine.FindStringSubmatch(id); m != nil {
		return m[2]
	}
	if id == "" {
		return ""
	}
	return "+" + id
}

// LineFromID returns the line half of a portal ID, empty for an ID that has
// none.
func LineFromID(id string) string {
	line, _ := SplitID(id)
	return line
}

// IsE164 reports whether a number NumberFromID returned is a global number
// rather than one that only means something on its own line. Only a global
// number can be published as a "tel:" identifier or dialled from anywhere
// else.
func IsE164(number string) bool {
	return len(number) > 1 && number[0] == '+'
}

// lineName is what a line may be spelled with. Nothing here knows which lines
// exist -- that stays the SIP server's routing (ADR-0014) -- but the name has
// to survive being written into a portal ID, a conference header, a SIP URI
// and an MXID localpart unchanged.
//
// No dot, which is what separates a line from a trunk in LineFromURI: they
// arrive in the same slot and nothing else tells them apart.
var lineName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ErrBadLine is returned for a line name that cannot be spelled into a portal
// ID.
var ErrBadLine = errors.New("line name is not usable")

// MakeID builds the composite portal ID "<line>-<number>" that scopes a
// conversation to a line.
//
// An unknown line is not an error here: the bridge has no list to check it
// against and the SIP server refuses what it cannot route. What is refused is
// a name SplitID would not read back as the same line -- there the ID would
// quietly belong to a different line, or to none.
func MakeID(line, number string) (string, error) {
	if !lineName.MatchString(line) {
		return "", fmt.Errorf("%w: %q", ErrBadLine, line)
	}
	id := line + "-" + number
	if gotLine, gotNumber := SplitID(id); gotLine != line || gotNumber != number {
		return "", fmt.Errorf("%w: %q and %q spell the portal ID %q, which reads back as %q and %q",
			ErrBadLine, line, number, id, gotLine, gotNumber)
	}
	return id, nil
}

// IDFor is the portal ID for a number and the line it belongs to, whether the
// line came off the wire or from a user.
//
// An empty line gives the line-less ID of ADR-0003, which is all a number with
// no line can key: there is no default line to fall back on and inventing one
// would be the routing knowledge ADR-0014 keeps out of the bridge.
func IDFor(line, input string) (string, error) {
	e164, err := Normalize(input)
	if err != nil {
		return "", err
	}
	if line == "" {
		return ToID(e164), nil
	}
	return MakeID(line, e164)
}

// LineFromURI returns the line an inbound SIP URI names, or "" for none.
//
// The SIP server rewrites an inbound sender's host to the line it resolved the
// message or call to, and falls back to the trunk's own hostname when it could
// not resolve one. The two arrive in the same slot, so they are told apart by
// shape: a line is a bare label, a trunk is a hostname. Nothing here knows
// which lines exist.
//
// A host this rejects keys the line-less portal, which is what the bridge did
// before lines. It must never key a portal named after a trunk: that is a room
// no call would ever land in.
func LineFromURI(uri string) string {
	_, host := splitURI(uri)
	if !lineName.MatchString(host) {
		return ""
	}
	return host
}
