package phonenum

import (
	"errors"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr error
	}{
		{"already e164", "+15551234567", "+15551234567", nil},
		{"spaces", "+1 555 123 4567", "+15551234567", nil},
		{"dashes and brackets", "+1 (555) 123-4567", "+15551234567", nil},
		{"dots", "+1.555.123.4567", "+15551234567", nil},
		{"double zero prefix", "0015551234567", "+15551234567", nil},
		{"double zero with spaces", "00 1 555 123 4567", "+15551234567", nil},
		{"tel uri", "tel:+15551234567", "+15551234567", nil},
		{"sip uri", "sip:+15551234567@pbx.example.com", "+15551234567", nil},
		{"sips uri with params", "sips:+15551234567@pbx.example.com;user=phone", "+15551234567", nil},
		{"angle bracket uri", "<sip:+15551234567@example.com>", "+15551234567", nil},
		{"uppercase scheme", "SIP:+15551234567@example.com", "+15551234567", nil},
		{"surrounding whitespace", "  +15551234567 ", "+15551234567", nil},
		{"max length", "+123456789012345", "+123456789012345", nil},

		{"empty", "", "", ErrEmpty},
		{"only separators", "  - - ", "", ErrEmpty},
		{"no international prefix", "5551234567", "", ErrNotE164},
		{"national trunk zero", "05551234567", "", ErrNotE164},
		{"letters", "+1555CALLNOW", "", ErrNotE164},
		{"plus in the middle", "1+5551234567", "", ErrNotE164},
		{"two pluses", "++15551234567", "", ErrNotE164},
		{"zero country code", "+05551234567", "", ErrNotE164},
		{"too long", "+1234567890123456", "", ErrNotE164},
		{"extension only", "sip:1001@pbx.example.com", "", ErrNotE164},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Normalize(tt.input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Normalize(%q) error = %v, want %v", tt.input, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIDRoundTrip(t *testing.T) {
	tests := []struct {
		e164 string
		id   string
	}{
		{"+15551234567", "15551234567"},
		{"+442071234567", "442071234567"},
		{"+33123456789", "33123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.e164, func(t *testing.T) {
			if got := ToID(tt.e164); got != tt.id {
				t.Errorf("ToID(%q) = %q, want %q", tt.e164, got, tt.id)
			}
			if got := NumberFromID(ToID(tt.e164)); got != tt.e164 {
				t.Errorf("round trip of %q gave %q", tt.e164, got)
			}
		})
	}
}

func TestNormalizeToID(t *testing.T) {
	got, err := NormalizeToID("+1 (555) 123-4567")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "15551234567" {
		t.Errorf("NormalizeToID = %q, want %q", got, "15551234567")
	}
	if _, err := NormalizeToID("nonsense"); err == nil {
		t.Error("expected an error for a non-number")
	}
}
