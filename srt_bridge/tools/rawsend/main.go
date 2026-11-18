// rawsend: sends a recorded TS file unchanged to the bridge as an SRT caller, always in
// full packets of 7 x 188 bytes, paced by the PCR in the file (real time).
//
//	rawsend -addr 127.0.0.1:21101 -streamid ID -in raw.ts
package main

import (
	"flag"
	"log"
	"os"
	"time"

	srt "github.com/datarhei/gosrt"
)

func pcrOf(p []byte) (int64, bool) {
	if p[3]&0x20 == 0 || p[4] == 0 || p[5]&0x10 == 0 {
		return 0, false
	}
	b := p[6:12]
	return int64(b[0])<<25 | int64(b[1])<<17 | int64(b[2])<<9 | int64(b[3])<<1 | int64(b[4]>>7), true
}

func main() {
	addr := flag.String("addr", "127.0.0.1:21101", "bridge")
	sid := flag.String("streamid", "", "stream ID")
	in := flag.String("in", "raw.ts", "TS file")
	flag.Parse()
	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	c := srt.DefaultConfig()
	c.StreamId = *sid
	conn, err := srt.Dial("srt", *addr, c)
	if err != nil {
		log.Fatal(err)
	}
	var start time.Time
	var pcr0 int64 = -1
	for off := 0; off+7*188 <= len(data); off += 7 * 188 {
		chunk := data[off : off+7*188]
		for i := 0; i < 7; i++ {
			if v, ok := pcrOf(chunk[i*188:]); ok {
				if pcr0 < 0 {
					pcr0, start = v, time.Now()
				}
				time.Sleep(time.Until(start.Add(time.Duration(v-pcr0) * time.Second / 90000)))
				break
			}
		}
		if _, err := conn.Write(chunk); err != nil {
			log.Fatal(err)
		}
	}
	conn.Close()
	log.Print("done")
}
