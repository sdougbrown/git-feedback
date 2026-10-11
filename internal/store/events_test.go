package store

import "testing"

func TestValidEventID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"e1", true},
		{"e123", true},
		{"e0", true},
		{"e", false},
		{"e-3", false},
		{"e1x", false},
		{"x1", false},
		{"", false},
		{"E1", false},
	}
	for _, tc := range tests {
		if got := ValidEventID(tc.id); got != tc.want {
			t.Errorf("ValidEventID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestParseEventID(t *testing.T) {
	tests := []struct {
		id      string
		wantSeq int64
		wantOK  bool
	}{
		{"e1", 1, true},
		{"e123", 123, true},
		{"e0", 0, true},
		{"e", 0, false},
		// Sscanf %d accepts a leading '-', but the documented form is
		// unsigned digits, so signed IDs are rejected.
		{"e-3", 0, false},
		{"e1x", 0, false},
		{"x1", 0, false},
		{"e0001", 1, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		seq, ok := parseEventID(tc.id)
		if ok != tc.wantOK || (ok && seq != tc.wantSeq) {
			t.Errorf("parseEventID(%q) = (%d, %v), want (%d, %v)", tc.id, seq, ok, tc.wantSeq, tc.wantOK)
		}
	}
}
