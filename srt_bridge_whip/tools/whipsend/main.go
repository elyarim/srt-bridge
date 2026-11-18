// whipsend: test sender for the WHIP input. Sends an H.264 (Annex B) file and an
// Ogg Opus file over WHIP, in real time, looping, like a camera would.
//
//	whipsend -url http://127.0.0.1:21102/whip -key KEY -video test.h264 -audio test.ogg -fps 50
package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:21102/whip", "WHIP endpoint")
	key := flag.String("key", "", "bearer key")
	video := flag.String("video", "", "H.264 Annex B file")
	audio := flag.String("audio", "", "Ogg Opus file (optional)")
	fps := flag.Float64("fps", 50, "frame rate of the video file")
	dur := flag.Duration("t", 0, "stop after this long (0 = never)")
	flag.Parse()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Fatal(err)
	}
	vt, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, "video", "whipsend")
	vs, err := pc.AddTrack(vt)
	if err != nil {
		log.Fatal(err)
	}
	go readRTCP(vs)
	var at *webrtc.TrackLocalStaticSample
	if *audio != "" {
		at, _ = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "whipsend")
		as, err := pc.AddTrack(at)
		if err != nil {
			log.Fatal(err)
		}
		go readRTCP(as)
	}
	// Media only once DTLS is up as well (PeerConnection connected): samples written
	// before that are dropped by Pion, and the first keyframe would be lost.
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("connection %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			os.Exit(1)
		}
	})
	offer, _ := pc.CreateOffer(nil)
	gathered := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gathered
	req, _ := http.NewRequest("POST", *url, bytes.NewBufferString(pc.LocalDescription().SDP))
	req.Header.Set("Content-Type", "application/sdp")
	req.Header.Set("Authorization", "Bearer "+*key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		log.Fatalf("WHIP: %s: %s", resp.Status, body)
	}
	log.Printf("WHIP: %s, session %s", resp.Status, resp.Header.Get("Location"))
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}); err != nil {
		log.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		log.Fatal("no connection")
	}
	if *dur > 0 {
		time.AfterFunc(*dur, func() { log.Print("done"); pc.Close(); os.Exit(0) })
	}
	if at != nil {
		go sendAudio(at, *audio)
	}
	sendVideo(vt, *video, *fps)
}

func readRTCP(s *webrtc.RTPSender) {
	buf := make([]byte, 1500)
	for {
		if _, _, err := s.Read(buf); err != nil {
			return
		}
	}
}

// sendVideo sends access units (everything up to the next AUD or slice start) at the frame rate.
func sendVideo(t *webrtc.TrackLocalStaticSample, path string, fps float64) {
	frame := time.Duration(float64(time.Second) / fps)
	next := time.Now()
	for {
		f, err := os.Open(path)
		if err != nil {
			log.Fatal(err)
		}
		r, _ := h264reader.NewReader(f)
		var au []byte
		for {
			nal, err := r.NextNAL()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				log.Fatal(err)
			}
			t0 := nal.UnitType
			if t0 == h264reader.NalUnitTypeAUD && len(au) > 0 {
				time.Sleep(time.Until(next))
				next = next.Add(frame)
				t.WriteSample(media.Sample{Data: au, Duration: frame})
				au = au[:0]
			}
			au = append(au, 0, 0, 0, 1)
			au = append(au, nal.Data...)
		}
		f.Close()
	}
}

func sendAudio(t *webrtc.TrackLocalStaticSample, path string) {
	next := time.Now()
	for {
		f, err := os.Open(path)
		if err != nil {
			log.Fatal(err)
		}
		ogg, _, err := oggreader.NewWith(f)
		if err != nil {
			log.Fatal(err)
		}
		var last uint64
		for {
			page, hdr, err := ogg.ParseNextPage()
			if err != nil {
				break
			}
			if hdr.GranulePosition == 0 || len(page) < 1 || (len(page) >= 4 && string(page[:4]) == "Opus") {
				continue
			}
			samples := hdr.GranulePosition - last
			last = hdr.GranulePosition
			if samples == 0 || samples > 5760 {
				samples = 960
			}
			d := time.Duration(samples) * time.Second / 48000
			time.Sleep(time.Until(next))
			next = next.Add(d)
			t.WriteSample(media.Sample{Data: page, Duration: d})
		}
		f.Close()
	}
}
