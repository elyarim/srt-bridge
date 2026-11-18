package main

import "testing"

// A new installation listens on UDP 21101 (Peter's choice, 2026-11-17).
func TestDefaultPort(t *testing.T) {
	if c := defaultConfig(); c.SRTPort != 21101 {
		t.Fatalf("default port %d, want 21101", c.SRTPort)
	}
}
