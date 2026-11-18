package main

import "testing"

// Loss and drops on the tile cover the last minute: errors at the start are
// gone 60 samples later, the totals stay.
func TestRecentWindow(t *testing.T) {
	c := &conn{}
	total := counts{}
	got := c.recent(total, roleSender)
	if got != (counts{}) {
		t.Fatalf("first sample: %+v", got)
	}
	// Second 1: 5 drops at the start.
	total = counts{pkts: 1000, loss: 20, drops: 5}
	if got = c.recent(total, roleSender); got.drops != 5 || got.loss != 20 {
		t.Fatalf("start errors not counted: %+v", got)
	}
	// 59 clean seconds: still inside the minute.
	for i := 0; i < 59; i++ {
		total.pkts += 1000
		got = c.recent(total, roleSender)
	}
	if got.drops != 5 {
		t.Fatalf("after 60 s the start drops should still count: %+v", got)
	}
	total.pkts += 1000
	if got = c.recent(total, roleSender); got.drops != 0 || got.loss != 0 || got.pkts != 60000 {
		t.Fatalf("after 61 s the start errors should be gone: %+v", got)
	}
	// A new drop shows at once; a new role starts over.
	total.drops++
	if got = c.recent(total, roleSender); got.drops != 1 {
		t.Fatalf("new drop not shown: %+v", got)
	}
	if got = c.recent(counts{pkts: 7, drops: 3}, roleReceiver); got != (counts{}) {
		t.Fatalf("new role should start at zero: %+v", got)
	}
}
