package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestLoad measures how many receivers one forward output carries.
// It runs only when asked:  $env:SRT_LOAD='200'; go test -run TestLoad -v
// Sender, bridge and all receivers share this PC and this process, so the
// figure is a lower bound for the bridge alone.
func TestLoad(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("SRT_LOAD"))
	if n <= 0 {
		t.Skip("set SRT_LOAD to the number of receivers")
	}
	h := testHub(t, true)

	cam, err := dial("cam1", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	// 3 packets of 1316 bytes per tick; 8 Mbit/s unless SRT_LOAD_MBPS says otherwise
	rate, _ := strconv.ParseFloat(os.Getenv("SRT_LOAD_MBPS"), 64)
	if rate <= 0 {
		rate = 8
	}
	// Claim the input before connecting receivers. Zero marks unmeasured packets.
	packet := make([]byte, 1316)
	if _, err := cam.Write(packet); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.state().Places[0].Sender == nil {
		if time.Now().After(deadline) {
			t.Fatal("sender did not become live")
		}
		time.Sleep(10 * time.Millisecond)
	}
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(1)})
	const endMarker = ^uint64(0)
	type result struct {
		index int
		count uint64
		err   error
	}
	results := make(chan result, n)
	var readers sync.WaitGroup
	defer readers.Wait() // connections close first, including on early failure
	for i := 0; i < n; i++ {
		c, err := dial("cam1*forward", "")
		if err != nil {
			t.Fatalf("receiver %d could not connect: %v", i+1, err)
		}
		defer c.Close()
		readers.Add(1)
		go func() {
			defer readers.Done()
			r := result{index: i}
			defer func() { results <- r }()
			buf := make([]byte, 2048)
			for {
				size, err := c.Read(buf)
				if err != nil {
					r.err = fmt.Errorf("connection ended: %w", err)
					return
				}
				if size != len(packet) {
					r.err = fmt.Errorf("payload size %d", size)
					return
				}
				seq := binary.BigEndian.Uint64(buf)
				if seq == 0 {
					continue
				}
				if seq == endMarker {
					return
				}
				if seq != r.count+1 {
					r.err = fmt.Errorf("expected sequence %d, received %d", r.count+1, seq)
					return
				}
				r.count++
			}
		}()
	}
	// gosrt can finish the caller handshake just before the hub installs the route.
	deadline = time.Now().Add(5 * time.Second)
	for len(h.state().Places[0].Forward) != n {
		if time.Now().After(deadline) {
			t.Fatal("receiver routes did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	interval := time.Duration(3 * 1316 * 8 / (rate * 1e6) * float64(time.Second))
	if interval <= 0 {
		t.Fatal("invalid load rate")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	start := time.Now()
	var sent uint64
	for time.Since(start) < 10*time.Second {
		<-ticker.C
		for j := 0; j < 3; j++ {
			sent++
			binary.BigEndian.PutUint64(packet, sent)
			if _, err := cam.Write(packet); err != nil {
				t.Fatalf("sender: %v", err)
			}
		}
	}
	elapsed := time.Since(start).Seconds()
	binary.BigEndian.PutUint64(packet, endMarker)
	if _, err := cam.Write(packet); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var delivered uint64
	for range n {
		select {
		case r := <-results:
			delivered += r.count
			if r.err != nil || r.count != sent {
				t.Errorf("receiver %d: %d of %d packets, error %v", r.index+1, r.count, sent, r.err)
			}
		case <-timer.C:
			t.Fatal("timed out waiting for the final packet at every receiver")
		}
	}
	t.Logf("%d receivers, %d numbered packets sent, %d/%d delivered; input %.2f Mbit/s, aggregate output %.2f Mbit/s",
		n, sent, delivered, sent*uint64(n), float64(sent)*1316*8/elapsed/1e6, float64(delivered)*1316*8/elapsed/1e6)
}
