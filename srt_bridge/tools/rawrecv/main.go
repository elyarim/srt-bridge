// rawrecv: receives an SRT stream from the bridge and writes the payload unchanged
// to a file, plus the size of every SRT packet, to look at the TS as a receiver gets it.
//
//	rawrecv -addr 127.0.0.1:21101 -streamid ID -t 10s -out raw.ts
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	srt "github.com/datarhei/gosrt"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:21101", "bridge")
	sid := flag.String("streamid", "", "stream ID")
	dur := flag.Duration("t", 10*time.Second, "how long")
	out := flag.String("out", "raw.ts", "output file")
	flag.Parse()
	c := srt.DefaultConfig()
	c.StreamId = *sid
	conn, err := srt.Dial("srt", *addr, c)
	if err != nil {
		log.Fatal(err)
	}
	f, _ := os.Create(*out)
	sizes := map[int]int{}
	end := time.Now().Add(*dur)
	buf := make([]byte, 2048)
	n := 0
	for time.Now().Before(end) {
		k, err := conn.Read(buf)
		if err != nil {
			break
		}
		f.Write(buf[:k])
		sizes[k]++
		n++
	}
	f.Close()
	conn.Close()
	fmt.Printf("%d packets; sizes: %v\n", n, sizes)
}
