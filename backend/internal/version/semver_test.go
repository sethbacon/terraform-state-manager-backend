package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"v1.2.3", "1.2.3", 0},         // leading v tolerated on either side
		{"1.2.3", "v1.2.3", 0},         //
		{"1.2.0", "1.3.0", -1},         // minor bump
		{"2.0.0", "1.9.9", 1},          // major dominates
		{"1.2.0-rc1", "1.2.0", -1},     // pre-release sorts below its release
		{"1.2.0", "1.2.0-rc1", 1},      //
		{"1.2.0+build5", "1.2.0", 0},   // build metadata ignored
		{"1.2", "1.2.0", 0},            // x/mod treats 1.2 == 1.2.0
		{"not-a-version", "1.0.0", -1}, // invalid sorts lowest
		{"1.0.0", "not-a-version", 1},
		{"bad", "worse", 0}, // two invalids are equal
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsValid(t *testing.T) {
	for _, v := range []string{"1.2.3", "v1.2.3", "1.2", "1.2.0-rc1", "1.2.0+m"} {
		if !IsValid(v) {
			t.Errorf("IsValid(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "latest", "1.x", "abc"} {
		if IsValid(v) {
			t.Errorf("IsValid(%q) = true, want false", v)
		}
	}
}
