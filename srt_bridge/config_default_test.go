package main

import "testing"

// A new installation listens on UDP 21101 (Peter's choice, 2026-11-17).
func TestDefaultPort(t *testing.T) {
	if c := defaultConfig(); c.SRTPort != 21101 {
		t.Fatalf("default port %d, want 21101", c.SRTPort)
	}
}

// A new installation turns unknown stream IDs away (Peter's choice, 2026-11-18);
// a config.json that says otherwise keeps its setting.
func TestDefaultTurnsUnknownAway(t *testing.T) {
	if defaultConfig().AcceptAny {
		t.Fatal("a new installation must turn unknown stream IDs away")
	}
	path := t.TempDir() + "/config.json"
	c := defaultConfig()
	c.AcceptAny = true
	if err := c.save(path); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil || !got.AcceptAny {
		t.Fatalf("a saved «every stream ID» must stay: %v %v", got != nil && got.AcceptAny, err)
	}
}
