package main

// One WHIP sender: its PeerConnection, the order of its packets, the shared
// clock of picture and sound, and the TS stream that goes to the bridge.

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	ptsStart     = 10 * 90000 // first video PTS: well away from 0
	startDelay   = 60         // ms between PCR and PTS at the start (sound needs time for AAC)
	maxDelay     = 400        // ms; the delay never grows beyond this
	keyEvery     = 300 * time.Millisecond
	holeWait     = 60 * time.Millisecond // how long a missing video packet is waited for
	videoQueue   = 4096
	audioMaxLost = 5 // more missing Opus packets in a row are not concealed one by one
)

// unwrapper makes a 64-bit timeline of a 32-bit RTP timestamp.
type unwrapper struct {
	last    uint32
	acc     int64
	started bool
}

func (u *unwrapper) unwrap(ts uint32) int64 {
	if !u.started {
		u.started, u.last, u.acc = true, ts, int64(ts)
		return u.acc
	}
	u.acc += int64(int32(ts - u.last))
	u.last = ts
	return u.acc
}

type srInfo struct {
	ntp uint64
	rtp uint32
	ok  bool
}

func ntpSeconds(ntp uint64) float64 {
	return float64(ntp>>32) + float64(ntp&0xffffffff)/4294967296.0
}

type session struct {
	id    string
	srv   *server
	pc    *webrtc.PeerConnection
	addr  string
	since time.Time

	hasAudio bool
	closed   atomic.Bool

	mu        sync.Mutex
	mux       *tsMuxer
	codec     videoCodec
	videoSSRC uint32
	vUnwrap   unwrapper
	v0        int64 // first video timestamp (unwrapped)
	vStarted  bool
	lastVPTS  int64
	lastVWall time.Time
	aUnwrap   unwrapper
	a0        int64 // audio sample at which aBase holds
	aBase     int64 // video PTS of audio sample a0
	aMapped   bool  // audio is on the video clock
	aBySR     bool  // the mapping comes from the sender reports
	srV, srA  srInfo
	delay     int64 // 90 kHz: PCR = video PTS - delay
	lastKey   time.Time

	// figures for the status
	bytesIn    atomic.Uint64
	rtt        atomic.Int64 // microseconds
	keyReqs    atomic.Uint64
	lostV      atomic.Uint64
	dropped    atomic.Uint64
	pictures   atomic.Uint64
	audioLost  atomic.Uint64
	audioLagMs atomic.Int64
	videoInfo  atomic.Pointer[string]
}

func (s *session) logf(format string, args ...any) {
	log.Printf("sender %s: %s", s.addr, fmt.Sprintf(format, args...))
}

// askKey sends a PLI to the sender, at most every keyEvery.
func (s *session) askKey(reason string) {
	s.mu.Lock()
	ssrc := s.videoSSRC
	if ssrc == 0 || time.Since(s.lastKey) < keyEvery {
		s.mu.Unlock()
		return
	}
	s.lastKey = time.Now()
	s.mu.Unlock()
	s.keyReqs.Add(1)
	if err := s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}); err != nil && !s.closed.Load() {
		s.logf("keyframe request (%s) failed: %v", reason, err)
	}
}

func (s *session) onTrack(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	mime := strings.ToLower(track.Codec().MimeType)
	go s.readRTCP(recv, track.Kind())
	switch {
	case track.Kind() == webrtc.RTPCodecTypeVideo:
		codec := codecH264
		if mime == strings.ToLower(webrtc.MimeTypeH265) {
			codec = codecH265
		} else if mime != strings.ToLower(webrtc.MimeTypeH264) {
			s.logf("video codec %s is not supported (H.264 or H.265 only)", track.Codec().MimeType)
			return
		}
		s.mu.Lock()
		s.codec, s.videoSSRC = codec, uint32(track.SSRC())
		s.mux = newTSMuxer(codec, s.hasAudio, s.srv.sendTS)
		s.delay = startDelay * 90
		s.mu.Unlock()
		info := codec.String()
		s.videoInfo.Store(&info)
		s.logf("video %s (payload type %d)", codec, track.PayloadType())
		s.readVideo(track, codec)
	case mime == strings.ToLower(webrtc.MimeTypeOpus):
		s.logf("sound Opus %d Hz, %d channels", track.Codec().ClockRate, track.Codec().Channels)
		s.readAudio(track)
	default:
		s.logf("track %s ignored", track.Codec().MimeType)
	}
}

func (s *session) readRTCP(recv *webrtc.RTPReceiver, kind webrtc.RTPCodecType) {
	for {
		pkts, _, err := recv.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok {
				s.mu.Lock()
				if kind == webrtc.RTPCodecTypeVideo {
					s.srV = srInfo{sr.NTPTime, sr.RTPTime, true}
				} else {
					s.srA = srInfo{sr.NTPTime, sr.RTPTime, true}
				}
				s.syncBySRLocked()
				s.mu.Unlock()
			}
		}
	}
}

// syncBySRLocked puts the sound on the clock of the picture with the sender
// reports (both say what time of the sender their timestamps are). Done once.
func (s *session) syncBySRLocked() {
	if s.aBySR || !s.srV.ok || !s.srA.ok || !s.vStarted || !s.aMapped {
		return
	}
	// The video timestamp at the time of the audio report, on our unwrapped timeline.
	dt := ntpSeconds(s.srA.ntp) - ntpSeconds(s.srV.ntp)
	vAtA := s.vUnwrapNear(s.srV.rtp) + int64(dt*90000)
	aAt := s.aUnwrapNear(s.srA.rtp)
	newBase := vAtA - s.v0 + ptsStart - (aAt-s.a0)*15/8
	shift := (newBase - s.aBase) / 90
	s.aBase, s.aBySR = newBase, true
	s.logf("sound and picture aligned by the sender reports (moved %d ms)", shift)
}

// vUnwrapNear places a 32-bit video timestamp on the unwrapped timeline next to the last one.
func (s *session) vUnwrapNear(ts uint32) int64 {
	return s.vUnwrap.acc + int64(int32(ts-s.vUnwrap.last))
}
func (s *session) aUnwrapNear(ts uint32) int64 {
	return s.aUnwrap.acc + int64(int32(ts-s.aUnwrap.last))
}

func (s *session) readVideo(track *webrtc.TrackRemote, codec videoCodec) {
	queue := make(chan *rtp.Packet, videoQueue)
	go func() {
		defer close(queue)
		for {
			p, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			s.bytesIn.Add(uint64(len(p.Payload) + 12))
			select {
			case queue <- p:
			default: // the worker is stuck: the packet is lost, the hole asks for a keyframe
			}
		}
	}()
	a := &assembler{dep: depacketizer{codec: codec}, maxWait: holeWait, onPic: s.picture, askKey: s.askKey}
	a.askKey("start")
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case p, ok := <-queue:
			if !ok {
				return
			}
			a.push(p, time.Now())
		case now := <-tick.C:
			a.tick(now)
			s.lostV.Store(a.lost)
			s.dropped.Store(a.dropped)
		}
	}
}

func (s *session) picture(pic picture) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mux == nil || s.closed.Load() {
		return
	}
	v := s.vUnwrap.unwrap(pic.ts)
	if !s.vStarted {
		s.vStarted, s.v0 = true, v
	}
	pts := v - s.v0 + ptsStart
	if s.vStarted && s.lastVPTS != 0 && (pts < s.lastVPTS-90000 || pts > s.lastVPTS+5*90000) {
		// The sender's clock jumped: start the timeline again here.
		s.logf("video timestamps jumped, timeline restarted")
		s.v0 = v - (s.lastVPTS - ptsStart) - 3600
		pts = s.lastVPTS + 3600
		s.aMapped, s.aBySR = false, false
	}
	s.lastVPTS, s.lastVWall = pts, now
	s.pictures.Add(1)
	s.mux.video(pic.data, pts, pts-s.delay, pic.key)
}

// videoNowLocked: the video PTS that belongs to this moment.
func (s *session) videoNowLocked(now time.Time) int64 {
	return s.lastVPTS + int64(now.Sub(s.lastVWall).Seconds()*90000)
}

func (s *session) readAudio(track *webrtc.TrackRemote) {
	var lastSeq uint16
	var lastTS int64
	started := false
	for {
		p, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		s.bytesIn.Add(uint64(len(p.Payload) + 12))
		conv := s.srv.converter()
		if conv == nil || len(p.Payload) == 0 {
			continue
		}
		s.mu.Lock()
		ts := s.aUnwrap.unwrap(p.Timestamp)
		if !s.aMapped {
			if !s.vStarted {
				s.mu.Unlock()
				continue // the sound waits for the first picture
			}
			s.a0, s.aBase, s.aMapped = ts, s.videoNowLocked(time.Now()), true
		}
		s.mu.Unlock()
		if started {
			gap := int(p.SequenceNumber - lastSeq)
			switch {
			case gap <= 0 || gap > 1000:
				continue // late or a copy
			case gap > 1 && gap <= audioMaxLost+1:
				// Missing packets: concealed by the decoder, on their place on the timeline.
				step := (ts - lastTS) / int64(gap)
				for i := 1; i < gap; i++ {
					conv.lost(lastTS+step*int64(i), int(step))
				}
				s.audioLost.Add(uint64(gap - 1))
			case gap > 1:
				s.audioLost.Add(uint64(gap - 1))
			}
		}
		started, lastSeq, lastTS = true, p.SequenceNumber, ts
		conv.packet(ts, p.Payload)
	}
}

// aacFrame comes from the converter: it goes on the video clock and into the stream.
func (s *session) aacFrame(pts48 int64, adts []byte) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mux == nil || !s.aMapped || s.closed.Load() {
		return
	}
	pts := s.aBase + (pts48-s.a0)*15/8
	// How late the sound is against the picture; the delay of the stream grows to cover it.
	lag := s.videoNowLocked(now) - pts
	s.audioLagMs.Store(lag / 90)
	if need := lag + 15*90; need > s.delay && s.delay < maxDelay*90 {
		old := s.delay
		s.delay = min(need+10*90, maxDelay*90)
		s.logf("sound needs more time: delay %d -> %d ms", old/90, s.delay/90)
	}
	s.mux.audioFrame(adts, pts)
}

func (s *session) close(why string) {
	if s.closed.Swap(true) {
		return
	}
	s.pc.Close()
	s.logf("ended (%s)", why)
}
