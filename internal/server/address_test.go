package server

import "testing"

func TestNormalizeNodeAddress(t *testing.T) {
	ok := []struct {
		in   string
		want string
	}{
		{"http://localhost:9001", "http://localhost:9001"},
		{"https://node-2:9443", "https://node-2:9443"},
		{"  http://10.0.0.5:9001/  ", "http://10.0.0.5:9001"},
	}
	for _, tc := range ok {
		got, err := normalizeNodeAddress(tc.in)
		if err != nil {
			t.Fatalf("normalizeNodeAddress(%q) unexpected error: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeNodeAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"",
		"ftp://node:21",
		"node:9001",                    // no scheme -> no host
		"http://",                      // no host
		"http://user:pass@node:9001",   // credentials
		"http://node:9001/chunks/abc",  // path
		"http://node:9001?x=1",         // query
		"http://169.254.169.254/#frag", // fragment
	}
	for _, in := range bad {
		if got, err := normalizeNodeAddress(in); err == nil {
			t.Fatalf("normalizeNodeAddress(%q) = %q, want error", in, got)
		}
	}
}
