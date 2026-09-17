package connector

import "testing"

func TestSplitDialArgs(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		number string
		line   string
	}{
		{"number only", "+15551234567", "+15551234567", ""},
		{"number and line", "+15551234567 home", "+15551234567", "home"},
		{"surrounding space", "  +15551234567   home  ", "+15551234567", "home"},
		{"hyphenated line", "+15551234567 office-main", "+15551234567", "office-main"},
		// Normalize accepts a number written with spaces, so the last group of
		// digits is part of the number and not a line.
		{"spaced number", "+1 555 123 4567", "+15551234567", ""},
		{"spaced number and line", "+1 555 123 4567 home", "+15551234567", "home"},
		{"international prefix", "001 555 123 4567 home", "0015551234567", "home"},
		// Left for DialNumber to reject, with the number it could not read.
		{"not a number", "nonsense", "nonsense", ""},
		{"not a number with a line", "nonsense home", "nonsense", "home"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			number, line := splitDialArgs(tt.raw)
			if number != tt.number || line != tt.line {
				t.Errorf("splitDialArgs(%q) = (%q, %q), want (%q, %q)",
					tt.raw, number, line, tt.number, tt.line)
			}
		})
	}
}
