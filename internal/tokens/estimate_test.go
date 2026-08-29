package tokens

import "testing"

func TestEstimate_Empty(t *testing.T) {
	if got := Estimate(""); got != 0 {
		t.Fatalf("Estimate(\"\") = %d, want 0", got)
	}
}

func TestEstimate_RoundsUp(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"12345678", 2},
	}
	for _, c := range cases {
		if got := Estimate(c.in); got != c.want {
			t.Errorf("Estimate(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEstimate_CountsRunesNotBytes(t *testing.T) {
	// Four multi-byte runes must estimate as one token, not as their byte count.
	if got := Estimate("日本語だ"); got != 1 {
		t.Fatalf("Estimate(multibyte) = %d, want 1", got)
	}
}
