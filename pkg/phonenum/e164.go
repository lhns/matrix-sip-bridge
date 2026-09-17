// Package phonenum converts between the several spellings of a phone number
// that the bridge has to deal with: E.164 as humans write it, the bare-digit
// form used as a bridgev2 portal/user ID, and SIP URIs as they arrive on the
// wire.
package phonenum

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrEmpty is returned for an input with no digits at all.
	ErrEmpty = errors.New("phone number is empty")
	// ErrNotE164 is returned for an input that cannot be read as E.164.
	ErrNotE164 = errors.New("phone number is not valid E.164")
)

// maxE164Digits is the E.164 limit: country code plus subscriber number.
const maxE164Digits = 15

// Normalize turns a loosely written number into E.164 with a leading "+".
//
// Separators (spaces, dashes, dots, brackets) and a "tel:" or "sip:" scheme are
// stripped. A leading "00" is treated as the international prefix and rewritten
// to "+". A number with no "+" and no "00" is rejected rather than guessed at:
// the bridge has no notion of a local dialling plan, so guessing would silently
// place calls to the wrong country.
func Normalize(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", ErrEmpty
	}
	s = stripURI(s)

	var b strings.Builder
	plus := false
	for i, r := range s {
		switch {
		case r == '+':
			if i != 0 || plus {
				return "", fmt.Errorf("%w: misplaced '+' in %q", ErrNotE164, input)
			}
			plus = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')' || r == '/' || r == '\t':
			// separator, ignore
		default:
			return "", fmt.Errorf("%w: unexpected %q in %q", ErrNotE164, string(r), input)
		}
	}

	digits := b.String()
	if digits == "" {
		return "", ErrEmpty
	}
	if !plus {
		if !strings.HasPrefix(digits, "00") {
			return "", fmt.Errorf("%w: %q has no international prefix", ErrNotE164, input)
		}
		digits = strings.TrimPrefix(digits, "00")
	}
	if digits == "" {
		return "", ErrEmpty
	}
	if digits[0] == '0' {
		return "", fmt.Errorf("%w: %q starts with a zero country code", ErrNotE164, input)
	}
	if len(digits) > maxE164Digits {
		return "", fmt.Errorf("%w: %q has %d digits, max %d", ErrNotE164, input, len(digits), maxE164Digits)
	}
	return "+" + digits, nil
}

// stripURI reduces "sip:+15551234567@pbx.example.com;user=phone" to
// "+15551234567".
func stripURI(s string) string {
	user, _ := splitURI(s)
	return user
}

// splitURI splits a tel:/sip:/sips: URI into its user part and its host,
// dropping the scheme, the angle brackets and any ";param" tail. The host is
// empty for a URI that has none.
func splitURI(s string) (user, host string) {
	s = strings.TrimPrefix(s, "<")
	for _, scheme := range []string{"tel:", "sips:", "sip:"} {
		if len(s) >= len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			s = s[len(scheme):]
			break
		}
	}
	if i := strings.IndexAny(s, ";>"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i >= 0 {
		host = s[i+1:]
		// A port and a "?header" tail are not part of the host. An IPv6
		// literal keeps its brackets and is mangled here; nothing asks this
		// for one.
		if j := strings.IndexAny(host, ":?"); j >= 0 {
			host = host[:j]
		}
		return s[:i], host
	}
	return s, ""
}

// ToID strips the leading "+" to give the bare-digit form used as a portal ID,
// a ghost user ID and the {{.}} of the appservice username_template.
//
// There is no unconditional inverse, and adding one back would reintroduce a
// bug: "+" prepended to whatever a portal ID happens to contain turned a
// composite ID into "+<line>-<digits>" and a short number into a country code
// it does not have. NumberFromID knows which IDs are stripped E.164 and which
// spell their own number.
func ToID(e164 string) string {
	return strings.TrimPrefix(e164, "+")
}

// NormalizeToID normalizes and converts to the ID form in one step.
func NormalizeToID(input string) (string, error) {
	e164, err := Normalize(input)
	if err != nil {
		return "", err
	}
	return ToID(e164), nil
}
