package main

// tsinfo reads what a sender delivers (MPEG-TS) just far enough to say the TV
// norm (1080p50) and how many audio channels arrive. Nothing is decoded: only
// the program tables, the start of each video PES (timestamps, SPS) and the
// first header of each audio PES are looked at. Only the reader goroutine of
// the sender touches a probe; the result is handed over through an atomic pointer.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
)

// MediaInfo is what the interface shows about the content of an input.
type MediaInfo struct {
	Video      string `json:"video"`      // "1080p50", "" while unknown
	VideoCodec string `json:"videoCodec"` // "HEVC", "H.264", ""
	Audio      []int  `json:"audio"`      // channels per audio track, 0 = not known
	AudioCodec string `json:"audioCodec"` // "AAC", "AC-3", "MPEG", mixed with "/"
}

type tsTrack struct {
	codec    string // "H.264", "HEVC", "AAC", "AC-3", "MPEG", "LATM" or "other"
	video    bool
	channels int
	buf      []byte // start of the current video PES, up to pesKeep bytes
	collect  bool
}

type tsProbe struct {
	pmtPID int // -1 = not known yet
	pmtVer int
	tracks map[int]*tsTrack
	order  []int // audio PIDs in the order of the PMT
	width  int
	height int
	interl bool
	first  int64 // DTS of the first picture in the rate window, -1 = none
	last   int64
	frames int
	rate   float64
	out    atomic.Pointer[MediaInfo]
	shown  string // what out says, so it is replaced only on a change
}

const pesKeep = 1024

func newTSProbe() *tsProbe {
	return &tsProbe{pmtPID: -1, pmtVer: -1, tracks: map[int]*tsTrack{}, first: -1}
}

// feed takes one SRT payload (usually 7 TS packets of 188 bytes).
func (p *tsProbe) feed(b []byte) {
	if len(b)%188 != 0 || b[0] != 0x47 {
		return // not aligned TS; some senders pad, a later payload may be fine
	}
	for off := 0; off+188 <= len(b); off += 188 {
		p.packet(b[off : off+188])
	}
}

func (p *tsProbe) packet(pk []byte) {
	if pk[0] != 0x47 {
		return
	}
	pusi := pk[1]&0x40 != 0
	pid := int(pk[1]&0x1f)<<8 | int(pk[2])
	afc := pk[3] >> 4 & 3
	if afc&1 == 0 {
		return // no payload
	}
	pay := pk[4:]
	if afc == 3 {
		n := int(pk[4])
		if 1+n > len(pay) {
			return
		}
		pay = pay[1+n:]
	}
	switch {
	case pid == 0 && pusi:
		p.pat(pay)
	case pid == p.pmtPID && pusi:
		p.pmt(pay)
	default:
		t := p.tracks[pid]
		if t == nil {
			return
		}
		if t.video {
			p.videoData(t, pusi, pay)
		} else if pusi {
			p.audioStart(t, pay)
		}
	}
}

// section returns the section that starts in a payload with pointer_field.
func section(pay []byte) []byte {
	if len(pay) < 1 || 1+int(pay[0]) >= len(pay) {
		return nil
	}
	s := pay[1+int(pay[0]):]
	if len(s) < 3 {
		return nil
	}
	n := 3 + (int(s[1]&0x0f)<<8 | int(s[2]))
	if n > len(s) {
		return nil // longer than one TS packet: rare for PAT/PMT, ignored
	}
	return s[:n]
}

func (p *tsProbe) pat(pay []byte) {
	s := section(pay)
	if len(s) < 12 || s[0] != 0 {
		return
	}
	for i := 8; i+4 <= len(s)-4; i += 4 {
		prog := int(s[i])<<8 | int(s[i+1])
		if prog != 0 { // the first program; 0 is the network PID
			pid := int(s[i+2]&0x1f)<<8 | int(s[i+3])
			if pid != p.pmtPID {
				p.pmtPID, p.pmtVer = pid, -1
			}
			return
		}
	}
}

func (p *tsProbe) pmt(pay []byte) {
	s := section(pay)
	if len(s) < 16 || s[0] != 2 {
		return
	}
	ver := int(s[5] >> 1 & 0x1f)
	if ver == p.pmtVer {
		return
	}
	p.pmtVer = ver
	tracks := map[int]*tsTrack{}
	var order []int
	i := 12 + (int(s[10]&0x0f)<<8 | int(s[11]))
	for i+5 <= len(s)-4 {
		typ := s[i]
		pid := int(s[i+1]&0x1f)<<8 | int(s[i+2])
		n := int(s[i+3]&0x0f)<<8 | int(s[i+4])
		end := i + 5 + n
		if end > len(s)-4 {
			break
		}
		desc := s[i+5 : end]
		t := &tsTrack{}
		switch typ {
		case 0x1b:
			t.codec, t.video = "H.264", true
		case 0x24:
			t.codec, t.video = "HEVC", true
		case 0x0f:
			t.codec = "AAC"
		case 0x11:
			t.codec = "LATM"
		case 0x03, 0x04:
			t.codec = "MPEG"
		case 0x81:
			t.codec = "AC-3"
		case 0x06:
			t.codec = privateAudio(desc)
		}
		if t.codec != "" {
			if old := p.tracks[pid]; old != nil && old.codec == t.codec {
				t = old // same track in a new PMT version: keep what is known
			}
			tracks[pid] = t
			if !t.video {
				order = append(order, pid)
			}
		}
		i = end
	}
	p.tracks, p.order = tracks, order
	p.publish()
}

// privateAudio tells audio in a private stream (type 6) by its descriptor.
func privateAudio(desc []byte) string {
	for i := 0; i+2 <= len(desc); i += 2 + int(desc[i+1]) {
		switch tag := desc[i]; {
		case tag == 0x6a:
			return "AC-3"
		case tag == 0x7a:
			return "E-AC-3"
		case tag == 0x05 && i+6 <= len(desc) && string(desc[i+2:i+6]) == "Opus":
			return "Opus"
		}
	}
	return ""
}

// pesPayload skips the PES header and returns its DTS (or PTS), -1 if none.
func pesPayload(b []byte) ([]byte, int64) {
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return nil, -1
	}
	hl := 9 + int(b[8])
	if hl > len(b) {
		return nil, -1
	}
	ts := int64(-1)
	flags := b[7] >> 6
	read := func(o int) int64 {
		return int64(b[o]>>1&7)<<30 | int64(b[o+1])<<22 | int64(b[o+2]>>1)<<15 | int64(b[o+3])<<7 | int64(b[o+4]>>1)
	}
	if flags == 3 && hl >= 19 {
		ts = read(14) // DTS: rises steadily even with B-frames
	} else if flags >= 2 && hl >= 14 {
		ts = read(9)
	}
	return b[hl:], ts
}

func (p *tsProbe) videoData(t *tsTrack, pusi bool, pay []byte) {
	if pusi {
		if t.collect {
			p.videoPES(t)
		}
		t.buf, t.collect = append(t.buf[:0], pay...), true
	} else if t.collect {
		t.buf = append(t.buf, pay...)
	}
	if t.collect && len(t.buf) >= pesKeep {
		p.videoPES(t)
		t.collect = false
	}
}

func (p *tsProbe) videoPES(t *tsTrack) {
	es, dts := pesPayload(t.buf)
	if es == nil {
		return
	}
	if dts >= 0 {
		p.timing(dts)
	}
	// Look for the SPS in the start of the picture (it comes with every key frame).
	for _, nal := range nalUnits(es) {
		var w, h int
		var il, ok bool
		if t.codec == "H.264" && len(nal) > 1 && nal[0]&0x1f == 7 {
			w, h, il, ok = spsH264(unescape(nal[1:]))
		} else if t.codec == "HEVC" && len(nal) > 2 && nal[0]>>1&0x3f == 33 {
			w, h, il, ok = spsHEVC(unescape(nal[2:]))
		}
		if ok {
			p.width, p.height, p.interl = w, h, il
			p.publish()
			return
		}
	}
}

// timing measures the picture rate from the timestamps over about two seconds.
func (p *tsProbe) timing(dts int64) {
	if p.first < 0 || dts <= p.last || dts-p.last > 90000 {
		p.first, p.last, p.frames = dts, dts, 0 // start, jump or wrap: begin again
		return
	}
	p.last = dts
	p.frames++
	if span := dts - p.first; span >= 2*90000 {
		p.rate = float64(p.frames) * 90000 / float64(span)
		p.first, p.frames = dts, 0
		p.publish()
	}
}

func (p *tsProbe) audioStart(t *tsTrack, pay []byte) {
	es, _ := pesPayload(pay)
	if len(es) < 7 {
		return
	}
	ch := 0
	switch t.codec {
	case "AAC":
		if es[0] == 0xff && es[1]&0xf0 == 0xf0 {
			cfg := int(es[2]&1)<<2 | int(es[3]>>6)
			ch = []int{0, 1, 2, 3, 4, 5, 6, 8}[cfg]
		}
	case "MPEG":
		if es[0] == 0xff && es[1]&0xe0 == 0xe0 {
			ch = 2
			if es[3]>>6 == 3 {
				ch = 1
			}
		}
	case "AC-3":
		if es[0] == 0x0b && es[1] == 0x77 {
			// bsid 5 bits, bsmod 3, then acmod 3 bits and, depending on it, mix levels, then lfeon.
			r := bitReader{b: es[5:]}
			r.u(5)
			r.u(3)
			acmod := int(r.u(3))
			if acmod&1 != 0 && acmod != 1 {
				r.u(2)
			}
			if acmod&4 != 0 {
				r.u(2)
			}
			if acmod == 2 {
				r.u(2)
			}
			ch = []int{2, 1, 2, 3, 3, 4, 4, 5}[acmod] + int(r.u(1))
		}
	}
	if ch != 0 && ch != t.channels {
		t.channels = ch
		p.publish()
	}
}

// publish builds the result that the statistics ticker picks up.
func (p *tsProbe) publish() {
	m := &MediaInfo{Audio: []int{}}
	var codecs []string
	for _, t := range p.tracks {
		if t.video && m.VideoCodec == "" {
			m.VideoCodec = t.codec
		}
	}
	for _, pid := range p.order {
		t := p.tracks[pid]
		m.Audio = append(m.Audio, t.channels)
		if !contains(codecs, t.codec) {
			codecs = append(codecs, t.codec)
		}
	}
	sort.Strings(codecs)
	m.AudioCodec = strings.Join(codecs, "/")
	if p.height > 0 {
		m.Video = norm(p.height, p.interl, p.rate)
	}
	key := fmt.Sprint(*m)
	if key != p.shown {
		p.shown = key
		p.out.Store(m)
	}
}

// norm writes the TV norm the usual way: lines, p or i, and the picture rate
// (fields per second for interlaced, as in 1080i50).
func norm(height int, interlaced bool, rate float64) string {
	letter := "p"
	if interlaced {
		letter = "i"
		if rate > 0 && rate <= 31 {
			rate *= 2 // one PES per frame of two fields
		}
	}
	s := fmt.Sprintf("%d%s", height, letter)
	if rate <= 0 {
		return s
	}
	for _, std := range []float64{23.976, 24, 25, 29.97, 30, 48, 50, 59.94, 60, 100, 119.88, 120} {
		if math.Abs(rate-std)/std < 0.015 {
			switch std {
			case 23.976:
				return s + "23.98"
			case 29.97, 59.94, 119.88:
				return s + fmt.Sprintf("%.2f", std)
			}
			return s + fmt.Sprintf("%.0f", std)
		}
	}
	return s + fmt.Sprintf("%.1f", rate)
}

// nalUnits splits an Annex B byte stream at its start codes (the last unit
// may be cut off, which is fine for an SPS near the start).
func nalUnits(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				end := i
				if end > start && b[end-1] == 0 {
					end--
				}
				out = append(out, b[start:end])
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 && start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

// unescape removes the emulation prevention bytes (00 00 03 -> 00 00).
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

type bitReader struct {
	b   []byte
	pos int
	bad bool
}

func (r *bitReader) u(n int) uint32 {
	var v uint32
	for ; n > 0; n-- {
		if r.pos >= len(r.b)*8 {
			r.bad = true
			return 0
		}
		v = v<<1 | uint32(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if r.bad || zeros > 31 {
			r.bad = true
			return 0
		}
		zeros++
	}
	return 1<<zeros - 1 + r.u(zeros)
}

func (r *bitReader) se() int32 {
	v := r.ue()
	if v&1 != 0 {
		return int32(v+1) / 2
	}
	return -int32(v / 2)
}

// spsH264 reads the picture size from an H.264 SPS (without the NAL header byte).
func spsH264(b []byte) (w, h int, interlaced, ok bool) {
	r := bitReader{b: b}
	profile := r.u(8)
	r.u(16) // constraint flags, level
	r.ue()  // seq_parameter_set_id
	chroma := uint32(1)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = r.ue()
		if chroma == 3 {
			r.u(1)
		}
		r.ue()
		r.ue()
		r.u(1)
		if r.u(1) == 1 { // scaling matrices
			lists := 8
			if chroma == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.u(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					last, next := int32(8), int32(8)
					for j := 0; j < size; j++ {
						if next != 0 {
							next = (last + r.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue()
	case 1:
		r.u(1)
		r.se()
		r.se()
		n := r.ue()
		for i := uint32(0); i < n && !r.bad; i++ {
			r.se()
		}
	}
	r.ue()
	r.u(1)
	wMbs := int(r.ue()) + 1
	hMap := int(r.ue()) + 1
	frameOnly := int(r.u(1))
	if frameOnly == 0 {
		r.u(1)
	}
	r.u(1)
	w, h = wMbs*16, (2-frameOnly)*hMap*16
	if r.u(1) == 1 { // cropping
		cx, cy := 1, 2-frameOnly
		if chroma == 1 {
			cx, cy = 2, 2*(2-frameOnly)
		} else if chroma == 2 {
			cx = 2
		}
		left, right, top, bottom := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		w -= cx * (left + right)
		h -= cy * (top + bottom)
	}
	if r.bad || w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return 0, 0, false, false
	}
	return w, h, frameOnly == 0, true
}

// spsHEVC reads the picture size from an HEVC SPS (without the two NAL header bytes).
func spsHEVC(b []byte) (w, h int, interlaced, ok bool) {
	r := bitReader{b: b}
	r.u(4)
	maxSub := int(r.u(3))
	r.u(1)
	r.u(8)  // profile space, tier, profile
	r.u(32) // compatibility flags
	progressive := r.u(1) == 1
	interlacedSrc := r.u(1) == 1
	r.u(2)
	r.u(32) // 43 reserved bits plus one
	r.u(12)
	r.u(8) // level
	sub := make([][2]bool, maxSub)
	for i := 0; i < maxSub; i++ {
		sub[i] = [2]bool{r.u(1) == 1, r.u(1) == 1}
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.u(2)
		}
	}
	for i := 0; i < maxSub; i++ {
		if sub[i][0] {
			r.u(32)
			r.u(32)
			r.u(24)
		}
		if sub[i][1] {
			r.u(8)
		}
	}
	r.ue()
	chroma := r.ue()
	if chroma == 3 {
		r.u(1)
	}
	w, h = int(r.ue()), int(r.ue())
	if r.u(1) == 1 { // conformance window, in chroma samples
		cx, cy := 1, 1
		if chroma == 1 {
			cx, cy = 2, 2
		} else if chroma == 2 {
			cx = 2
		}
		left, right, top, bottom := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		w -= cx * (left + right)
		h -= cy * (top + bottom)
	}
	if r.bad || w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return 0, 0, false, false
	}
	interlaced = interlacedSrc && !progressive
	if interlaced && h < 720 && w >= 1280 {
		h *= 2 // HEVC codes the fields as pictures of half height
	}
	return w, h, interlaced, true
}
