package main

import (
	"testing"
	"time"
)

// Receiver key: the forward output has its own stream ID, the sender key
// does not open it any more. With the original switched off, a device that
// only listens on the sender key gets nothing and is closed.
func TestReceiverKeyAndOriginalOff(t *testing.T) {
	old := originalWait
	originalWait = 500 * time.Millisecond
	defer func() { originalWait = old }()

	h := testHub(t, false)
	stop := make(chan struct{})
	defer close(stop)
	set(t, h, PlaceUpdate{Slot: 1, ID: ptr("cam1"), Source: ptr(1), PassFwd: ptr("forwardsecret")})

	// Rules for the key.
	for _, bad := range []string{"cam1", "x*forward"} {
		if err := h.updatePlace(PlaceUpdate{Slot: 1, RecvID: ptr(bad)}); err == nil {
			t.Fatalf("receiver key %q was accepted", bad)
		}
	}
	if err := h.updatePlace(PlaceUpdate{Slot: 2, RecvID: ptr("rx2")}); err == nil {
		t.Fatal("a free signal got a receiver key")
	}
	set(t, h, PlaceUpdate{Slot: 1, RecvID: ptr("rxKey1")})
	if err := h.updatePlace(PlaceUpdate{Slot: 2, ID: ptr("rxKey1")}); err == nil {
		t.Fatal("the receiver key was accepted as a stream ID of another signal")
	}

	cam, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	go feed(cam, 'A', stop)
	time.Sleep(300 * time.Millisecond)

	rx, err := dial("rxKey1", "forwardsecret")
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	expect(t, rx, 'A', "receiver key")
	if c, err := dial("cam1*forward", "forwardsecret"); err == nil {
		c.Close()
		t.Fatal("cam1*forward still opens the forward output")
	}

	// Original allowed: a listener on the sender key gets the picture.
	orig, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	expect(t, orig, 'A', "original while allowed")

	// Switched off: that listener is closed, a new one is turned away after the wait.
	h.setOriginal(false)
	expectClosed(t, orig, "original receiver after switching off")
	late, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	expectClosed(t, late, "listener on the sender key")
	found := false
	for _, r := range h.state().Rejected {
		found = found || (r.Slot == 1 && r.Short == "original is off: receiver turned away")
	}
	if !found {
		t.Fatal("the turned away listener is not listed for signal 1")
	}
	// The sender and the receiver key still work.
	expect(t, rx, 'A', "receiver key after switching the original off")

	// Removing the key: <id>*forward works again.
	set(t, h, PlaceUpdate{Slot: 1, RecvID: ptr("")})
	fwd, err := dial("cam1*forward", "forwardsecret")
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	expect(t, fwd, 'A', "cam1*forward after removing the key")
}
