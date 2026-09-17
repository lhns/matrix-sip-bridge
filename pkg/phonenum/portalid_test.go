package phonenum

import (
	"errors"
	"testing"
)

func TestSplitID(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		line   string
		number string
	}{
		{"legacy bare digits", "15551234567", "", "15551234567"},
		{"line", "home-+15551234567", "home", "+15551234567"},
		{"hyphenated line name", "office-main-+15551234567", "office-main", "+15551234567"},
		{"line name ending in a digit", "line2-+15551234567", "line2", "+15551234567"},
		{"short number on a line", "office-1001", "office", "1001"},
		{"malformed stays whole", "home-not-a-number", "", "home-not-a-number"},
		{"trailing hyphen stays whole", "home-", "", "home-"},
		{"trailing plus stays whole", "home-+", "", "home-+"},
		{"empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, number := SplitID(tt.id)
			if line != tt.line || number != tt.number {
				t.Errorf("SplitID(%q) = (%q, %q), want (%q, %q)", tt.id, line, number, tt.line, tt.number)
			}
			if got := LineFromID(tt.id); got != tt.line {
				t.Errorf("LineFromID(%q) = %q, want %q", tt.id, got, tt.line)
			}
		})
	}
}

// The asymmetry this covers is the whole point: a legacy ID is E.164 with the
// "+" stripped, a composite one spells its own number.
func TestNumberFromID(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"15551234567", "+15551234567"},
		{"home-+15551234567", "+15551234567"},
		{"office-main-+15551234567", "+15551234567"},
		{"office-1001", "1001"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			if got := NumberFromID(tt.id); got != tt.want {
				t.Errorf("NumberFromID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

// The point of leaving a malformed ID whole: it reaches the wire as something
// no route regex accepts, rather than as a number to some other country.
func TestNumberFromAMalformedIDIsNotDialable(t *testing.T) {
	for _, id := range []string{"home-not-a-number", "nonsense", "home-"} {
		if got := NumberFromID(id); got != "+"+id {
			t.Errorf("NumberFromID(%q) = %q, want it left whole", id, got)
		}
	}
}

func TestIsE164(t *testing.T) {
	tests := []struct {
		number string
		want   bool
	}{
		{"+15551234567", true},
		{"1001", false},
		{"+", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.number, func(t *testing.T) {
			if got := IsE164(tt.number); got != tt.want {
				t.Errorf("IsE164(%q) = %v, want %v", tt.number, got, tt.want)
			}
		})
	}
}

func TestMakeID(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		number  string
		want    string
		wantErr bool
	}{
		{name: "e164 on a line", line: "home", number: "+15551234567", want: "home-+15551234567"},
		{name: "hyphenated line", line: "office-main", number: "+15551234567", want: "office-main-+15551234567"},
		{name: "line ending in a digit", line: "line2", number: "+15551234567", want: "line2-+15551234567"},
		{name: "short number", line: "office", number: "1001", want: "office-1001"},
		{name: "no line", line: "", number: "+15551234567", wantErr: true},
		{name: "space in the line", line: "my line", number: "+15551234567", wantErr: true},
		{name: "at sign in the line", line: "a@b", number: "+15551234567", wantErr: true},
		// A dot is how LineFromURI tells a trunk hostname from a line.
		{name: "dot in the line", line: "sip.example.com", number: "+15551234567", wantErr: true},
		{name: "line starting with a dash", line: "-home", number: "+15551234567", wantErr: true},
		// The round trip, not the charset, is what catches this: the ID would
		// read back as the line "home-not" and the number "+15551234567".
		{name: "number that is not one", line: "home", number: "not-+15551234567", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MakeID(tt.line, tt.number)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("MakeID(%q, %q) = %q, want an error", tt.line, tt.number, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MakeID(%q, %q): %v", tt.line, tt.number, err)
			}
			if got != tt.want {
				t.Fatalf("MakeID(%q, %q) = %q, want %q", tt.line, tt.number, got, tt.want)
			}
			// The ID is only usable if it reads back as what it was built from.
			if line, number := SplitID(got); line != tt.line || number != tt.number {
				t.Errorf("%q reads back as (%q, %q), want (%q, %q)", got, line, number, tt.line, tt.number)
			}
		})
	}
}

func TestIDFor(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "no line keeps the legacy form", input: "+15551234567", want: "15551234567"},
		{name: "line scopes the id", line: "home", input: "+15551234567", want: "home-+15551234567"},
		{name: "loosely written number", line: "home", input: "00 1 555 123 4567", want: "home-+15551234567"},
		{name: "bad number", line: "home", input: "5551234567", wantErr: true},
		{name: "bad line", line: "no spaces please", input: "+15551234567", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IDFor(tt.line, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("IDFor(%q, %q) = %q, want an error", tt.line, tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("IDFor(%q, %q): %v", tt.line, tt.input, err)
			}
			if got != tt.want {
				t.Errorf("IDFor(%q, %q) = %q, want %q", tt.line, tt.input, got, tt.want)
			}
		})
	}
}

// A line the bridge cannot spell must not quietly become a line-less portal:
// that is a different room from the one the line's inbound calls land in.
func TestIDForRefusesABadLineRatherThanDroppingIt(t *testing.T) {
	got, err := IDFor("bad line", "+15551234567")
	if !errors.Is(err, ErrBadLine) {
		t.Fatalf("IDFor with a bad line = (%q, %v), want ErrBadLine", got, err)
	}
	if got != "" {
		t.Errorf("IDFor returned the portal ID %q anyway", got)
	}
}

// The SIP server puts the line where the carrier's host would be, and the
// trunk's own hostname there when it could not resolve one. Only the first is
// a line.
func TestLineFromURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{"line", "sip:+15551234567@home", "home"},
		{"hyphenated line", "sip:+15551234567@office-main", "office-main"},
		{"angle brackets and a parameter", "<sip:+15551234567@home;user=phone>", "home"},
		{"port", "sip:+15551234567@home:5060", "home"},
		{"uppercase scheme", "SIP:+15551234567@home", "home"},
		{"trunk hostname", "sip:+15551234567@sip.example.com", ""},
		{"sbc address", "sip:+15551234567@198.51.100.7", ""},
		{"no host", "+15551234567", ""},
		{"empty host", "sip:+15551234567@", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LineFromURI(tt.uri); got != tt.want {
				t.Errorf("LineFromURI(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

// Reading the host must not change what Normalize makes of the user part.
func TestLineFromURIDoesNotDisturbTheNumber(t *testing.T) {
	for _, uri := range []string{
		"sip:+15551234567@home",
		"<sip:+15551234567@sip.example.com;user=phone>",
		"sips:+15551234567@home:5060",
		"tel:+15551234567",
	} {
		got, err := Normalize(uri)
		if err != nil || got != "+15551234567" {
			t.Errorf("Normalize(%q) = (%q, %v), want +15551234567", uri, got, err)
		}
	}
}
