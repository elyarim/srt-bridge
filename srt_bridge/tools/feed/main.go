// feed is a test helper: it sends dummy packets to a stream ID, or receives
// from one and counts. The data is not a video; it only shows that the path works.
//
//	go run ./tools/feed -id cam1 -send
//	go run ./tools/feed -id "cam1*forward"
package main

import (
	"flag"
	"log"
	"time"

	srt "github.com/datarhei/gosrt"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:21101", "bridge address")
	id := flag.String("id", "cam1", "stream ID")
	pass := flag.String("pass", "", "passphrase")
	send := flag.Bool("send", false, "send instead of receive")
	rate := flag.Float64("mbps", 4, "bitrate when sending")
	flag.Parse()

	cfg := srt.DefaultConfig()
	cfg.StreamId = *id
	cfg.Passphrase = *pass
	c, err := srt.Dial("srt", *addr, cfg)
	if err != nil {
		log.Fatalf("%s: %v", *id, err)
	}
	log.Printf("connected to %s as %q", *addr, *id)

	if *send {
		pkt := make([]byte, 1316)
		tk := time.NewTicker(time.Duration(3 * 1316 * 8 / (*rate * 1e6) * float64(time.Second)))
		for range tk.C {
			for i := 0; i < 3; i++ {
				if _, err := c.Write(pkt); err != nil {
					log.Fatalf("connection ended: %v", err)
				}
			}
		}
	}
	buf := make([]byte, 2048)
	var n, last uint64
	go func() {
		for range time.Tick(time.Second) {
			log.Printf("%q: %.2f Mbit/s", *id, float64(n-last)*8/1e6)
			last = n
		}
	}()
	for {
		k, err := c.Read(buf)
		if err != nil {
			log.Fatalf("connection ended: %v", err)
		}
		n += uint64(k)
	}
}
