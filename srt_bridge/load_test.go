package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	stop := make(chan struct{})

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
	var sent atomic.Uint64
	go func() {
		pkt := make([]byte, 1316)
		tk := time.NewTicker(time.Duration(3 * 1316 * 8 / (rate * 1e6) * float64(time.Second)))
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				for i := 0; i < 3; i++ {
					if _, err := cam.Write(pkt); err != nil {
						return
					}
					sent.Add(1)
				}
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	set(t, h, PlaceUpdate{Slot: 1, Source: ptr(1)})

	got := make([]atomic.Uint64, n)
	var failed, ended atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		c, err := dial("cam1*forward", "")
		if err != nil {
			failed.Add(1)
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, 2048)
			for {
				if _, err := c.Read(buf); err != nil {
					select {
					case <-stop:
					default:
						ended.Add(1)
					}
					return
				}
				got[i].Add(1)
			}
		}(i)
		defer c.Close()
	}
	time.Sleep(time.Second)
	for i := range got {
		got[i].Store(0)
	}
	start := sent.Load()
	time.Sleep(10 * time.Second)
	total := sent.Load() - start
	var min, sum uint64 = ^uint64(0), 0
	for i := range got {
		v := got[i].Load()
		sum += v
		if v < min {
			min = v
		}
	}
	close(stop)
	full := 0
	for _, e := range h.state().Events {
		if strings.Contains(e.Text, "send queue was full") {
			full++
		}
	}
	t.Logf("closed by the bridge for a full send queue (of the last 200 events): %d", full)
	t.Logf("%d receivers asked, %d could not connect, %d were closed during the run", n, failed.Load(), ended.Load())
	t.Logf("sender: %d packets in 10 s (%.1f Mbit/s)", total, float64(total)*1316*8/10/1e6)
	t.Logf("delivered on average %.1f %%, worst receiver %.1f %%, total out %.0f Mbit/s",
		float64(sum)/float64(n)/float64(total)*100, float64(min)/float64(total)*100, float64(sum)*1316*8/10/1e6)
}
