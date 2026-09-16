package phonenum

import "regexp"

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
