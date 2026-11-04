package semver

import "testing"

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.7.5", "0.7.5", 0},
		{"0.7.6", "0.7.5", 1},
		{"0.7.10", "0.7.6", 1},
		{"0.10.0", "0.9.0", 1},
		{"0.8.0-rc1", "0.8.0", -1},
		{"0.8.0", "0.8.0-rc1", 1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
	} {
		got, ok := Compare(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d", c.a, c.b, got, ok, c.want)
		}
	}
	for _, s := range []string{"", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-", "1.2.3-a_b", "dev"} {
		if _, ok := Compare(s, "1.2.3"); ok {
			t.Errorf("Compare accepted %q", s)
		}
	}
}
