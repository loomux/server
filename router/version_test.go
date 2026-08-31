package router

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.251", "2.1.251", 0},
		{"2.1.251", "2.1.252", -1},
		{"2.1.252", "2.1.251", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.9.9", "2.0.0", -1},
		{"2.1", "2.1.0", 0}, // missing trailing components treated as 0
		{"2.1.0", "2.1", 0},
		{"2.2", "2.1.999", 1}, // a shorter version can still be greater
	}
	for _, c := range cases {
		got, err := compareVersions(c.a, c.b)
		if err != nil {
			t.Fatalf("compareVersions(%q, %q): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersions_InvalidComponent(t *testing.T) {
	if _, err := compareVersions("2.x.1", "2.0.0"); err == nil {
		t.Fatal("compareVersions with a non-numeric component: want error, got nil")
	}
}

func TestCheckVersionRange(t *testing.T) {
	cases := []struct {
		name              string
		version, min, max string
		wantErr           bool
	}{
		{"within range", "2.1.251", "2.0.0", "3.0.0", false},
		{"equals min (inclusive)", "2.0.0", "2.0.0", "3.0.0", false},
		{"equals max (exclusive)", "3.0.0", "2.0.0", "3.0.0", true},
		{"below min", "1.9.9", "2.0.0", "3.0.0", true},
		{"above max", "3.0.1", "2.0.0", "3.0.0", true},
		{"no min", "0.0.1", "", "3.0.0", false},
		{"no max", "99.0.0", "2.0.0", "", false},
		{"no bounds at all", "0.0.1", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckVersionRange(c.version, c.min, c.max)
			if (err != nil) != c.wantErr {
				t.Fatalf("CheckVersionRange(%q, %q, %q) err = %v, want error=%v", c.version, c.min, c.max, err, c.wantErr)
			}
		})
	}
}

func TestExtractDottedVersion(t *testing.T) {
	cases := []struct {
		output string
		want   string
	}{
		{"2.1.251 (Claude Code)", "2.1.251"},
		{"v1.2.3", "1.2.3"},
		{"tool version 10.0.44-beta", "10.0.44"},
	}
	for _, c := range cases {
		got, err := ExtractDottedVersion(c.output)
		if err != nil {
			t.Fatalf("ExtractDottedVersion(%q): %v", c.output, err)
		}
		if got != c.want {
			t.Errorf("ExtractDottedVersion(%q) = %q, want %q", c.output, got, c.want)
		}
	}
}

func TestExtractDottedVersion_NoVersionFound(t *testing.T) {
	if _, err := ExtractDottedVersion("command not found"); err == nil {
		t.Fatal("ExtractDottedVersion with no version in the output: want error, got nil")
	}
}
