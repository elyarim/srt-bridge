package main

import (
	"strings"
	"testing"
	"time"
)

// With the original off, an unknown key that only listens (an old receiver
// key) takes no signal, and its repeated tries are logged once. An unknown
// key that sends still gets the next free signal.
func TestUnknownListenerTakesNoSignal(t *testing.T) {
	old := originalWait
	originalWait = 400 * time.Millisecond
	defer func() { originalWait = old }()

	h := testHub(t, true)
	h.setOriginal(false)
	stop := make(chan struct{})
	defer close(stop)

	for try := 0; try < 3; try++ {
		c, err := dial("staleKey", "")
		if err != nil {
			t.Fatal(err)
		}
		expectClosed(t, c, "listener with an unknown key")
		c.Close()
	}
	st := h.state()
	for _, p := range st.Places {
		if p.ID == "staleKey" {
			t.Fatalf("signal %d was taken by a key that only listens", p.Slot)
		}
	}
	var listed *Rejected
	for _, r := range st.Rejected {
		if r.StreamID == "staleKey" {
			listed = r
		}
	}
	if listed == nil || listed.Slot != 0 || !strings.Contains(listed.Reason, "old receiver key") || listed.Count != 3 {
		t.Fatalf("not listed as expected under «Turned away»: %+v", listed)
	}
	connected := 0
	for _, e := range st.Events {
		if strings.Contains(e.Text, `connected: "staleKey"`) && !strings.Contains(e.Text, "disconnected") {
			connected++
		}
	}
	if connected != 1 {
		t.Fatalf("the repeated tries were logged %d times, want once", connected)
	}

	// A new key that sends gets the first free signal, as before.
	cam, err := dial("newCam", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	go feed(cam, 'N', stop)
	time.Sleep(500 * time.Millisecond)
	st = h.state()
	if st.Places[0].ID != "newCam" || st.Places[0].Sender == nil || !st.Places[0].Auto {
		t.Fatalf("the sending device did not get signal 1: %q sender %v", st.Places[0].ID, st.Places[0].Sender != nil)
	}
	set(t, h, PlaceUpdate{Slot: 1, RecvID: ptr("rxNew")})
	rx, err := dial("rxNew", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	expect(t, rx, 'N', "receiver key of the new signal")
}
