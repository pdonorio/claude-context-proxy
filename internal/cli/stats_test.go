package cli

import "testing"

func TestFmtWindows(t *testing.T) {
	cases := []struct {
		tokens     int64
		windowSize int64
		want       string
	}{
		{0, 200000, "0.1×"},        // minimum enforced
		{200000, 200000, "1.0×"},   // exactly 1 window
		{284391, 200000, "1.4×"},   // typical session
		{2000000, 200000, "10.0×"}, // >10 windows
		{5, 200000, "0.1×"},        // very small — minimum
		{0, 0, "0.1×"},             // zero window size falls back to 200000
	}
	for _, c := range cases {
		got := FmtWindows(c.tokens, c.windowSize)
		if got != c.want {
			t.Errorf("FmtWindows(%d, %d) = %q, want %q", c.tokens, c.windowSize, got, c.want)
		}
	}
}

func TestFmtWindowPct(t *testing.T) {
	cases := []struct {
		tokens     int64
		windowSize int64
		want       string
	}{
		{0, 200000, "0%"},
		{200000, 200000, "100%"},
		{300000, 200000, "150%"},
		{82341, 200000, "41%"},
		{0, 0, "0%"}, // zero window size falls back to 200000
	}
	for _, c := range cases {
		got := FmtWindowPct(c.tokens, c.windowSize)
		if got != c.want {
			t.Errorf("FmtWindowPct(%d, %d) = %q, want %q", c.tokens, c.windowSize, got, c.want)
		}
	}
}
