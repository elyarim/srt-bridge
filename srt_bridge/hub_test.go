package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"log"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	srt "github.com/datarhei/gosrt"
)

const testAddr = "127.0.0.1:19001"

func testHub(t *testing.T, acceptAny bool) *Hub {
	t.Helper()
	log.SetOutput(io.Discard)
	cfg := defaultConfig()
	cfg.SRTPort = 19001
	cfg.LatencyMs = 120
	cfg.AcceptAny = acceptAny
	h := newHub(cfg, filepath.Join(t.TempDir(), "config.json"))
	h.start()
	if h.srtErr != "" {
		t.Fatalf("listen: %s", h.srtErr)
	}
	t.Cleanup(func() {
		h.mu.Lock()
		h.stopLocked()
		h.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	})
	return h
}

func dial(streamID, pass string) (srt.Conn, error) {
	c := srt.DefaultConfig()
	c.StreamId = streamID
	c.Passphrase = pass
	return srt.Dial("srt", testAddr, c)
}

// feed sends numbered packets of 1316 bytes with the given tag until stop is closed.
func feed(c srt.Conn, tag byte, stop <-chan struct{}) {
	pkt := bytes.Repeat([]byte{tag}, 1316)
	var n uint32
	t := time.NewTicker(2 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			binary.BigEndian.PutUint32(pkt[1:], n)
			n++
			if _, err := c.Write(pkt); err != nil {
				return
			}
		}
	}
}

// expect reads from c and reports whether packets with the tag arrive in time.
func expect(t *testing.T, c srt.Conn, tag byte, what string) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		for i := 0; i < 50; i++ {
			n, err := c.Read(buf)
			if err != nil {
				got <- err
				return
			}
			if n != 1316 || buf[0] != tag || buf[1315] != tag {
				t.Errorf("%s: wrong packet: %d bytes, tag %q", what, n, buf[0])
				got <- nil
				return
			}
		}
		got <- nil
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no data within 5 s", what)
	}
}

// expectClosed waits until the bridge has closed c.
func expectClosed(t *testing.T, c srt.Conn, what string) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := c.Read(buf); err != nil {
				close(closed)
				return
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s was not closed", what)
	}
}

func set(t *testing.T, h *Hub, u PlaceUpdate) {
	t.Helper()
	if err := h.updatePlace(u); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestBridge(t *testing.T) {
	h := testHub(t, true)
	stop := make(chan struct{})
	defer close(stop)

	// Unknown IDs take the free signals in order: cam1 = 1, cam2 = 2.
	cam1, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam1.Close()
	go feed(cam1, 'A', stop)
	cam2, err := dial("cam2", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam2.Close()
	go feed(cam2, 'B', stop)
	time.Sleep(300 * time.Millisecond)
	if st := h.state(); len(st.Places) != 8 || st.Places[0].ID != "cam1" || st.Places[1].ID != "cam2" || st.Places[2].ID != "" {
		t.Fatalf("signals not as expected: %q %q %q of %d", st.Places[0].ID, st.Places[1].ID, st.Places[2].ID, len(st.Places))
	}

	// The original comes back under the bare ID, on the same port.
	orig, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	expect(t, orig, 'A', "original of cam1")

	// Matrix: the forward of signal 1 gets signal 2; the old name *talkback works as well.
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(2)})
	fwd, err := dial("cam1*forward", "")
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	expect(t, fwd, 'B', "cam1*forward")
	tb, err := dial("cam1*TalkBack", "")
	if err != nil {
		t.Fatal(err)
	}
	defer tb.Close()
	expect(t, tb, 'B', "cam1*talkback")

	// A signal that is only used as an output, set by hand on signal 5.
	set(t, h, PlaceUpdate{Slot: 5, ID: ptr("regie"), Source: ptr(1)})
	regie, err := dial("regie*forward", "")
	if err != nil {
		t.Fatal(err)
	}
	defer regie.Close()
	expect(t, regie, 'A', "regie*forward")

	// A second sender on a signal that has one is closed; the first one stays.
	second, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	go feed(second, 'X', stop)
	time.Sleep(500 * time.Millisecond)
	h.mu.Lock()
	sender := h.rt[0].sender
	h.mu.Unlock()
	if sender == nil || sender.addr != cam1.LocalAddr().String() {
		t.Fatalf("the first sender of cam1 was replaced")
	}
	listed := false
	for _, r := range h.state().Rejected {
		listed = listed || (r.Slot == 1 && r.Short == "second sender turned away")
	}
	if !listed {
		t.Fatal("the second sender is not listed for the tile of signal 1")
	}
	expect(t, orig, 'A', "original of cam1 after a second sender")

	// Changing the matrix closes the forward receivers of that signal.
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(1)})
	expectClosed(t, fwd, "the forward receiver after the matrix change")
	fwd2, err := dial("cam1*forward", "")
	if err != nil {
		t.Fatal(err)
	}
	defer fwd2.Close()
	expect(t, fwd2, 'A', "cam1*forward after the change")

	// Renaming a signal keeps its place in the matrix; the old ID is gone.
	set(t, h, PlaceUpdate{Slot: 5, ID: ptr("studio")})
	expectClosed(t, regie, "the receiver of the renamed signal")
	studio, err := dial("studio*forward", "")
	if err != nil {
		t.Fatal(err)
	}
	defer studio.Close()
	expect(t, studio, 'A', "studio*forward")

	// Clearing a signal frees it for the next unknown ID.
	set(t, h, PlaceUpdate{Slot: 2, ID: ptr("")})
	expectClosed(t, cam2, "the sender of the cleared signal")
	if st := h.state(); st.Places[1].ID != "" || st.Places[0].Source != 1 {
		t.Fatalf("signal 2 not cleared, or the matrix changed")
	}
}

// expectSoon reads from c until packets with the tag arrive; older packets may come first.
func expectSoon(t *testing.T, c srt.Conn, tag byte, what string) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := c.Read(buf)
			if err != nil || (n > 0 && buf[0] == tag) {
				got <- err
				return
			}
		}
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no data within 5 s", what)
	}
}

// After a break in the network the device reconnects while its old connection
// still exists on the bridge. The new connection must take over; the receiver stays.
func TestSenderComesBack(t *testing.T) {
	h := testHub(t, true)
	stopOld := make(chan struct{})
	old, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	go feed(old, 'A', stopOld)
	orig, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	expect(t, orig, 'A', "original before the break")

	close(stopOld) // the old connection stays open but delivers nothing any more
	time.Sleep(senderSilence + 300*time.Millisecond)

	stop := make(chan struct{})
	defer close(stop)
	back, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	go feed(back, 'B', stop)
	expectSoon(t, orig, 'B', "original after the device came back")
	h.mu.Lock()
	sender, starts := h.rt[0].sender, h.rt[0].starts
	h.mu.Unlock()
	if sender == nil || sender.addr != back.LocalAddr().String() || starts != 2 {
		t.Fatalf("the new connection did not take over (starts %d)", starts)
	}
}

// A port that is busy at the start is opened as soon as it is free.
func TestPortOpensLater(t *testing.T) {
	log.SetOutput(io.Discard)
	busy, err := net.ListenPacket("udp", ":19002")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.SRTPort = 19002
	h := newHub(cfg, filepath.Join(t.TempDir(), "config.json"))
	h.listenRetry = 300 * time.Millisecond
	h.start()
	defer h.stop()
	if h.state().SRTError == "" {
		t.Fatal("the busy port was opened")
	}
	busy.Close()
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if h.state().SRTError == "" {
			return
		}
	}
	t.Fatal("the port was not opened after it became free")
}

// One port per signal: no stream ID, the port says which signal. Whoever
// sends is the input, whoever listens gets what the matrix assigns.
func TestPortPerSignal(t *testing.T) {
	log.SetOutput(io.Discard)
	cfg := defaultConfig()
	cfg.SRTPort, cfg.PortMode, cfg.Slots, cfg.LatencyMs = 19011, modeSignal, 3, 120
	cfg.Places = nil
	cfg.normalize()
	h := newHub(cfg, filepath.Join(t.TempDir(), "config.json"))
	h.start()
	defer func() { h.stop(); time.Sleep(100 * time.Millisecond) }()
	if st := h.state(); st.SRTError != "" || st.PortRange != "19011-19013" || st.Places[2].Port != 19013 {
		t.Fatalf("ports not open as expected: %q %q", st.SRTError, st.PortRange)
	}
	on := func(port int) srt.Conn {
		t.Helper()
		c, err := srt.Dial("srt", "127.0.0.1:"+strconv.Itoa(port), srt.DefaultConfig())
		if err != nil {
			t.Fatalf("port %d: %v", port, err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	stop := make(chan struct{})
	defer close(stop)

	go feed(on(19011), 'A', stop)
	go feed(on(19012), 'B', stop)
	time.Sleep(300 * time.Millisecond)
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(1)})
	set(t, h, PlaceUpdate{Slot: 2, Source: ptr(1)})
	set(t, h, PlaceUpdate{Slot: 3, Source: ptr(2)})
	expect(t, on(19011), 'A', "port of signal 1, matrix on input 1")
	expect(t, on(19012), 'A', "port of signal 2, matrix on input 1")
	out3 := on(19013)
	expect(t, out3, 'B', "port of signal 3, matrix on input 2")

	// A matrix change closes the listeners of that row, not the inputs.
	set(t, h, PlaceUpdate{Slot: 3, Source: ptr(1)})
	expectClosed(t, out3, "the receiver after the matrix change")
	expect(t, on(19013), 'A', "port of signal 3 after the change")
	st := h.state()
	if st.Places[0].Sender == nil || st.Places[1].Sender == nil || st.Places[2].Sender != nil {
		t.Fatalf("inputs not as expected after the matrix change")
	}

	// Back to one port: the range is closed, the single port opens.
	if err := h.updateSettings(SettingsUpdate{SRTPort: 19011, PortMode: modeSingle, LatencyMs: 120, Suffixes: []string{"forward"}, AcceptAny: true, Slots: 3}); err != nil {
		t.Fatal(err)
	}
	if st := h.state(); st.SRTError != "" || st.PortRange != "19011" {
		t.Fatalf("single port not open: %q %q", st.SRTError, st.PortRange)
	}
	if _, err := srt.Dial("srt", "127.0.0.1:19012", srt.DefaultConfig()); err == nil {
		t.Fatal("port 19012 still answers in single-port mode")
	}
}

func TestFull(t *testing.T) {
	h := testHub(t, true)
	if err := h.updateSettings(SettingsUpdate{SRTPort: 19001, LatencyMs: 120, Suffixes: []string{"forward"}, AcceptAny: true, Slots: 2}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	for _, id := range []string{"a", "b"} {
		c, err := dial(id, "")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		go feed(c, 'A', stop)
	}
	if _, err := dial("c", ""); err == nil {
		t.Fatal("a third stream ID was accepted with two signals")
	}
	if err := h.updateSettings(SettingsUpdate{SRTPort: 19001, LatencyMs: 120, Suffixes: []string{"forward"}, AcceptAny: true, Slots: 1}); err == nil {
		t.Fatal("the number of signals was reduced below a signal in use")
	}
}

func TestPassphraseAndListedOnly(t *testing.T) {
	h := testHub(t, false)
	stop := make(chan struct{})
	defer close(stop)

	if _, err := dial("stranger", ""); err == nil {
		t.Fatal("an unknown stream ID was accepted")
	}
	st := h.state()
	if len(st.Rejected) != 1 || !st.Rejected[0].CanAdd || st.Rejected[0].StreamID != "stranger" {
		t.Fatalf("the attempt was not listed: %+v", st.Rejected)
	}

	if err := h.addPlace("cam1"); err != nil {
		t.Fatal(err)
	}
	if err := h.addPlace("bad*forward"); err == nil {
		t.Fatal("a stream ID ending in *forward was accepted")
	}
	if err := h.updatePlace(PlaceUpdate{Slot: 2, ID: ptr("cam1")}); err == nil {
		t.Fatal("the same stream ID was accepted twice")
	}
	pass := "geheim-geheim-123"
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(1), PassFwd: &pass})
	if err := h.updatePlace(PlaceUpdate{Slot: 1, PassFwd: ptr("kurz")}); err == nil {
		t.Fatal("a passphrase of 4 characters was accepted")
	}

	// The sender stays unencrypted, only the forward output asks for the passphrase.
	cam1, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam1.Close()
	go feed(cam1, 'A', stop)
	time.Sleep(300 * time.Millisecond)

	if _, err := dial("cam1*forward", ""); err == nil {
		t.Fatal("the forward output was given out without the passphrase")
	}
	if _, err := dial("cam1*forward", "falsch-falsch-123"); err == nil {
		t.Fatal("the forward output was given out with a wrong passphrase")
	}
	fwd, err := dial("cam1*forward", pass)
	if err != nil {
		t.Fatal(err)
	}
	defer fwd.Close()
	expect(t, fwd, 'A', "encrypted cam1*forward")
}
