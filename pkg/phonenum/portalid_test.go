package phonenum

import "testing"

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
