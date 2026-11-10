package main

import "testing"

// The Unix socket is named after -addr's port (5433 without TCP); a
// port that is not a number cannot name one.
func TestPortOf(t *testing.T) {
	for addr, want := range map[string]int{"": 5433, "127.0.0.1:5433": 5433, ":6000": 6000, "[::1]:7": 7} {
		if got, err := portOf(addr); err != nil || got != want {
			t.Errorf("portOf(%q) = %d, %v; want %d", addr, got, err, want)
		}
	}
	for _, bad := range []string{"localhost", "127.0.0.1:pg", "127.0.0.1:99999"} {
		if _, err := portOf(bad); err == nil {
			t.Errorf("portOf(%q) accepted", bad)
		}
	}
}
