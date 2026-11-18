// aactest: feeds an Ogg Opus file through srt_bridge_aac.exe and writes the AAC
// frames to a raw ADTS file, plus the timestamp of the first frame, to check the
// delay compensation of the converter against a direct decode.
//
//	aactest -conv bin\srt_bridge_aac.exe -in sync.ogg -out sync.aac
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

func main() {
	conv := flag.String("conv", "srt_bridge_aac.exe", "converter")
	in := flag.String("in", "", "Ogg Opus file")
	outPath := flag.String("out", "out.aac", "ADTS output")
	flag.Parse()
	cmd := exec.Command(*conv)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	out, _ := os.Create(*outPath)
	done := make(chan struct{})
	go func() {
		r := bufio.NewReader(stdout)
		var hdr [12]byte
		first := true
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				break
			}
			pts := int64(binary.LittleEndian.Uint64(hdr[:]))
			n := binary.LittleEndian.Uint32(hdr[8:])
			buf := make([]byte, n)
			io.ReadFull(r, buf)
			if first {
				fmt.Printf("first AAC frame pts %d samples\n", pts)
				first = false
			}
			out.Write(buf)
		}
		close(done)
	}()
	f, _ := os.Open(*in)
	ogg, _, err := oggreader.NewWith(f)
	if err != nil {
		log.Fatal(err)
	}
	var last uint64
	w := bufio.NewWriter(stdin)
	pts := int64(0)
	for {
		page, hdr, err := ogg.ParseNextPage()
		if err != nil {
			break
		}
		if hdr.GranulePosition == 0 || len(page) < 1 || (len(page) >= 4 && string(page[:4]) == "Opus") {
			continue
		}
		samples := int64(hdr.GranulePosition - last)
		last = hdr.GranulePosition
		var h [13]byte
		h[0] = 1
		binary.LittleEndian.PutUint64(h[1:], uint64(pts))
		binary.LittleEndian.PutUint32(h[9:], uint32(len(page)))
		w.Write(h[:])
		w.Write(page)
		pts += samples
	}
	w.Flush()
	time.Sleep(200 * time.Millisecond)
	stdin.Close()
	<-done
	out.Close()
	cmd.Wait()
}
