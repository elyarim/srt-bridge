package main

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

// whipFeed sends tagged datagrams of 1316 bytes to the local WHIP input, as srt-bridge-whip does.
func whipFeed(t *testing.T, h *Hub, tag byte, stop <-chan struct{}) {
	t.Helper()
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: h.whipTapPort()})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer c.Close()
		pkt := bytes.Repeat([]byte{tag}, 1316)
		tk := time.NewTicker(2 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				c.Write(pkt)
			}
		}
	}()
}

func whipOnHub(t *testing.T) *Hub {
	t.Helper()
	h := testHub(t, false)
	on, id := true, "whipcam"
	if _, err := h.updateWhip(WhipUpdate{Enabled: &on, ID: &id}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestWhipReceiverAndMatrix(t *testing.T) {
	h := whipOnHub(t)
	set(t, h, PlaceUpdate{Slot: 1, ID: ptr("venue")})
	src := whipSource
	set(t, h, PlaceUpdate{Slot: 1, Source: &src})
	stop := make(chan struct{})
	defer close(stop)
	whipFeed(t, h, 'W', stop)

	rx, err := dial("whipcam", "")
	if err != nil {
		t.Fatalf("receiver of the WHIP signal: %v", err)
	}
	defer rx.Close()
	expect(t, rx, 'W', "receiver of the WHIP signal")

	fwd, err := dial("venue*forward", "")
	if err != nil {
		t.Fatalf("forward output: %v", err)
	}
	defer fwd.Close()
	expect(t, fwd, 'W', "forward output with the WHIP input in the matrix")

	st := h.state()
	if st.Whip == nil || !st.Whip.Live || st.Whip.Slot != len(st.Places)+1 || len(st.Whip.Receivers) != 1 || len(st.Whip.Forward) != 1 || st.Whip.Forward[0] != 1 {
		t.Fatalf("WHIP state: %+v", st.Whip)
	}
	if !h.whip.wantKey.Load() {
		t.Fatal("a new receiver must ask the WHIP sender for a keyframe")
	}
}

func TestWhipRefusesSRTSenderAndForward(t *testing.T) {
	h := whipOnHub(t)
	if _, err := dial("whipcam*forward", ""); err == nil {
		t.Fatal("the WHIP signal has no forward output")
	}
	c, err := dial("whipcam", "")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go feed(c, 'S', stop)
	expectClosed(t, c, "an SRT sender on the WHIP stream ID")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.rejected) == 0 || !strings.Contains(h.rejected[len(h.rejected)-1].Reason, "WHIP") {
		t.Fatalf("the SRT sender must be listed as turned away: %+v", h.rejected)
	}
}

func TestWhipSettingsChecks(t *testing.T) {
	h := testHub(t, false)
	set(t, h, PlaceUpdate{Slot: 2, ID: ptr("taken")})
	on := true
	if _, err := h.updateWhip(WhipUpdate{Enabled: &on}); err == nil {
		t.Fatal("switching on without a stream ID must fail")
	}
	if _, err := h.updateWhip(WhipUpdate{ID: ptr("taken")}); err == nil {
		t.Fatal("the stream ID of signal 2 must not be taken")
	}
	port := 19001
	if _, err := h.updateWhip(WhipUpdate{Port: &port}); err == nil {
		t.Fatal("the SRT port must not be the WHIP port")
	}
	if _, err := h.updateWhip(WhipUpdate{Key: ptr("short")}); err == nil {
		t.Fatal("a key of 5 characters must be refused")
	}
	restart, err := h.updateWhip(WhipUpdate{Enabled: &on, ID: ptr("w1"), Key: ptr("0123456789abcdef")})
	if err != nil || !restart {
		t.Fatalf("switching on: restart %v, %v", restart, err)
	}
	if err := h.updatePlace(PlaceUpdate{Slot: 3, ID: ptr("w1")}); err == nil {
		t.Fatal("a signal must not take the stream ID of the WHIP signal")
	}
	// The WHIP source stays in the matrix when the number of signals changes.
	src := whipSource
	set(t, h, PlaceUpdate{Slot: 2, Source: &src})
	if err := h.updateSettings(SettingsUpdate{SRTPort: 19001, LatencyMs: 120, Slots: 4}); err != nil {
		t.Fatal(err)
	}
	if h.cfg.Places[1].Source != whipSource {
		t.Fatalf("matrix source after fewer signals: %d", h.cfg.Places[1].Source)
	}
}

// Switched off on its tile, a signal closes its devices and turns them away while
// they keep calling; switched on again, they get in.
func TestSignalOff(t *testing.T) {
	h := testHub(t, false)
	set(t, h, PlaceUpdate{Slot: 1, ID: ptr("cam")})
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(1)})
	tx, err := dial("cam", "")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go feed(tx, 'C', stop)
	time.Sleep(300 * time.Millisecond)
	set(t, h, PlaceUpdate{Slot: 1, Off: ptr(true)})
	expectClosed(t, tx, "the sender of a signal switched off")
	if _, err := dial("cam", ""); err == nil {
		t.Fatal("a signal switched off must turn the sender away")
	}
	if _, err := dial("cam*forward", ""); err == nil {
		t.Fatal("a signal switched off must turn the forward receiver away")
	}
	set(t, h, PlaceUpdate{Slot: 1, Off: ptr(false)})
	tx2, err := dial("cam", "")
	if err != nil {
		t.Fatalf("switched on again: %v", err)
	}
	tx2.Close()
	// Clearing the signal (Reset) also switches it on again.
	set(t, h, PlaceUpdate{Slot: 1, Off: ptr(true)})
	set(t, h, PlaceUpdate{Slot: 1, ID: ptr(""), Name: ptr("")})
	if p := h.cfg.Places[0]; p.Off || p.ID != "" || p.Name != "" {
		t.Fatalf("after reset: %+v", p)
	}
}

func TestWhipOff(t *testing.T) {
	h := whipOnHub(t)
	if _, err := h.updateWhip(WhipUpdate{Off: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := dial("whipcam", ""); err == nil {
		t.Fatal("the WHIP signal switched off must turn receivers away")
	}
	if _, err := h.updateWhip(WhipUpdate{Off: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	whipFeed(t, h, 'W', stop)
	rx, err := dial("whipcam", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	expect(t, rx, 'W', "WHIP signal switched on again")
}

func TestFirewallCommandWhip(t *testing.T) {
	cmd := firewallCommand("netsh", "21101", 0, 21102)
	if !strings.Contains(cmd, `name="SRT Bridge (WHIP)" dir=in action=allow protocol=TCP localport=21102`) ||
		!strings.Contains(cmd, `name="SRT Bridge (WHIP)" dir=in action=allow protocol=UDP localport=21102`) {
		t.Fatalf("WHIP must open TCP and UDP: %s", cmd)
	}
	if off := firewallCommand("netsh", "21101", 0, 0); strings.Contains(off, "add rule name=\"SRT Bridge (WHIP)\"") || !strings.Contains(off, `delete rule name="SRT Bridge (WHIP)"`) {
		t.Fatalf("WHIP off must delete its rule and not add it: %s", off)
	}
}
