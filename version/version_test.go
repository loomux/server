package version_test

import (
	"testing"

	"github.com/Loomux/server/version"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]version.Semver{
		"0.3.74":               {Major: 0, Minor: 3, Patch: 74},
		"0.3.74 (web 0.3.12)":  {Major: 0, Minor: 3, Patch: 74},
		"1.0.0-rc.1":           {Major: 1, Minor: 0, Patch: 0, Pre: "rc.1"},
		"10.20.30 (web 0.1.0)": {Major: 10, Minor: 20, Patch: 30},
		"  0.4.0 ":             {Major: 0, Minor: 4, Patch: 0},
	} {
		got, ok := version.Parse(in)
		if !ok || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	for _, in := range []string{"dev", "", "abc1234 (web 0.3.1)", "v0.3.74", "0.3", "0.3.74+build.1", "01.2.3", "0.3.x"} {
		if got, ok := version.Parse(in); ok {
			t.Errorf("Parse(%q) = %+v; want not a version", in, got)
		}
	}
}

func TestCompare(t *testing.T) {
	v := func(s string) version.Semver {
		t.Helper()
		p, ok := version.Parse(s)
		if !ok {
			t.Fatalf("bad test version %q", s)
		}
		return p
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.3.74", "0.3.74", 0},
		{"0.3.74", "0.3.75", -1},
		{"0.4.0", "0.3.99", 1},
		{"1.0.0", "0.99.99", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
	} {
		if got := v(tc.a).Compare(v(tc.b)); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// Satisfies is a caret range: the same major, and under 0.x the same
// minor, at or above the minimum.
func TestSatisfies(t *testing.T) {
	v := func(s string) version.Semver {
		t.Helper()
		p, ok := version.Parse(s)
		if !ok {
			t.Fatalf("bad test version %q", s)
		}
		return p
	}
	for _, tc := range []struct {
		host, min string
		want      bool
	}{
		{"0.3.80", "0.3.0", true},
		{"0.3.0", "0.3.0", true},
		{"0.3.80", "0.3.81", false},
		{"0.3.80", "0.4.0", false},
		{"0.4.0", "0.3.0", false},
		{"1.2.0", "1.0.0", true},
		{"1.0.0", "1.2.0", false},
		{"2.0.0", "1.0.0", false},
		{"0.3.0-rc.1", "0.3.0", false},
		{"0.3.1", "0.3.0-rc.1", true},
	} {
		if got := v(tc.host).Satisfies(v(tc.min)); got != tc.want {
			t.Errorf("%s satisfies %s = %v, want %v", tc.host, tc.min, got, tc.want)
		}
	}
}

func TestString(t *testing.T) {
	for in, want := range map[string]string{"0.3.74 (web 0.3.12)": "0.3.74", "1.0.0-rc.1": "1.0.0-rc.1"} {
		p, _ := version.Parse(in)
		if got := p.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
		}
	}
}
