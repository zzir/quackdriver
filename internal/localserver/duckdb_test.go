package localserver

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		input                string
		major, minor, patch  int
		wantErr              bool
	}{
		{"v1.5.3 abcdef1234", 1, 5, 3, false},
		{"v2.0.0\n", 2, 0, 0, false},
		{"v1.5.3", 1, 5, 3, false},
		{"1.6.0", 1, 6, 0, false},
		{"v1.5", 0, 0, 0, true},
		{"garbage", 0, 0, 0, true},
		{"", 0, 0, 0, true},
	}
	for _, c := range cases {
		major, minor, patch, err := parseVersion(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseVersion(%q): expected error", c.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVersion(%q): %v", c.input, err)
			continue
		}
		if major != c.major || minor != c.minor || patch != c.patch {
			t.Errorf("parseVersion(%q) = %d.%d.%d, want %d.%d.%d",
				c.input, major, minor, patch, c.major, c.minor, c.patch)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		major, minor, patch int
		want                bool
	}{
		{1, 5, 3, true},  // exact match
		{1, 5, 4, true},  // patch above
		{1, 6, 0, true},  // minor above
		{2, 0, 0, true},  // major above
		{1, 5, 2, false}, // patch below
		{1, 4, 9, false}, // minor below
		{0, 9, 9, false}, // major below
	}
	for _, c := range cases {
		got := versionAtLeast(c.major, c.minor, c.patch, MinVersion)
		if got != c.want {
			t.Errorf("versionAtLeast(%d.%d.%d) = %v, want %v",
				c.major, c.minor, c.patch, got, c.want)
		}
	}
}
