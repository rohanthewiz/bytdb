package main

import "testing"

// TestParseMemCeiling pins the -memory-ceiling vocabulary onto
// Engine.SetMemoryCeiling's encoding (0 auto, -1 off, >0 bytes).
func TestParseMemCeiling(t *testing.T) {
	good := map[string]int64{
		"auto": 0, "": 0, "AUTO": 0, "off": -1,
		"6GiB": 6 << 30, "512MiB": 512 << 20, "64KiB": 64 << 10, "1TiB": 1 << 40,
		"1000B": 1000, "1000": 1000,
	}
	for in, want := range good {
		got, err := parseMemCeiling(in)
		if err != nil || got != want {
			t.Errorf("parseMemCeiling(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "-5", "6GB", "lots", "9999999TiB"} {
		if _, err := parseMemCeiling(in); err == nil {
			t.Errorf("parseMemCeiling(%q): want an error", in)
		}
	}
}
