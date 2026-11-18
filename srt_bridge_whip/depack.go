package main

// Video from RTP to whole pictures (Annex B), with as little waiting as possible:
// a picture goes out the moment its last packet (marker bit) is there and nothing
// is missing. A hole waits for the retransmission (NACK) for a short time only;
// then the picture is given up, a keyframe is asked for, and pictures are held
// back until it comes (a frozen picture instead of a broken one).

import (
	"errors"
	"time"

	"github.com/pion/rtp"
)

type videoCodec int

const (
	codecH264 videoCodec = iota
	codecH265
)

func (c videoCodec) String() string {
	if c == codecH265 {
		return "H.265"
	}
	return "H.264"
}

var startCode = []byte{0, 0, 0, 1}

// picture is one access unit, ready for the TS muxer.
type picture struct {
	data []byte // Annex B, with access unit delimiter and, on keyframes, the parameter sets
	ts   uint32 // RTP timestamp (90 kHz)
	key  bool
}

// depacketizer turns the payloads of one picture into Annex B.
type depacketizer struct {
	codec videoCodec
	nals  [][]byte // NAL units of the current picture
	fu    []byte   // fragment being put together
	fuOK  bool
	// Parameter sets seen last: put in front of a keyframe that comes without them.
	vps, sps, pps []byte
}

var errBadPayload = errors.New("invalid video payload")

func (d *depacketizer) reset() {
	d.nals = d.nals[:0]
	d.fu, d.fuOK = d.fu[:0], false
}

func (d *depacketizer) add(nal []byte) {
	if len(nal) == 0 {
		return
	}
	cp := append([]byte(nil), nal...)
	d.nals = append(d.nals, cp)
}

// push takes the payload of one RTP packet (in order).
func (d *depacketizer) push(p []byte) error {
	if d.codec == codecH265 {
		return d.push265(p)
	}
	return d.push264(p)
}

func (d *depacketizer) push264(p []byte) error {
	if len(p) < 1 {
		return errBadPayload
	}
	switch t := p[0] & 0x1f; {
	case t >= 1 && t <= 23:
		d.add(p)
	case t == 24: // STAP-A
		for i := 1; i+2 <= len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			i += 2
			if n == 0 || i+n > len(p) {
				return errBadPayload
			}
			d.add(p[i : i+n])
			i += n
		}
	case t == 28: // FU-A
		if len(p) < 2 {
			return errBadPayload
		}
		start, end := p[1]&0x80 != 0, p[1]&0x40 != 0
		if start {
			d.fu = append(d.fu[:0], p[0]&0xe0|p[1]&0x1f)
			d.fuOK = true
		} else if !d.fuOK {
			return errBadPayload
		}
		d.fu = append(d.fu, p[2:]...)
		if end {
			d.add(d.fu)
			d.fu, d.fuOK = d.fu[:0], false
		}
	default:
		return errBadPayload
	}
	return nil
}

func (d *depacketizer) push265(p []byte) error {
	if len(p) < 3 {
		return errBadPayload
	}
	switch t := (p[0] >> 1) & 0x3f; {
	case t < 48:
		d.add(p)
	case t == 48: // aggregation packet
		for i := 2; i+2 <= len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			i += 2
			if n < 2 || i+n > len(p) {
				return errBadPayload
			}
			d.add(p[i : i+n])
			i += n
		}
	case t == 49: // fragmentation unit
		start, end := p[2]&0x80 != 0, p[2]&0x40 != 0
		if start {
			typ := p[2] & 0x3f
			d.fu = append(d.fu[:0], p[0]&0x81|typ<<1, p[1])
			d.fuOK = true
		} else if !d.fuOK {
			return errBadPayload
		}
		d.fu = append(d.fu, p[3:]...)
		if end {
			d.add(d.fu)
			d.fu, d.fuOK = d.fu[:0], false
		}
	default:
		return errBadPayload
	}
	return nil
}

func (d *depacketizer) nalType(n []byte) int {
	if d.codec == codecH265 {
		return int(n[0]>>1) & 0x3f
	}
	return int(n[0] & 0x1f)
}

// finish builds the access unit. ok=false: nothing usable (no picture data, or a
// keyframe without parameter sets ever seen).
func (d *depacketizer) finish(ts uint32) (picture, bool) {
	defer d.reset()
	key, slices := false, false
	var hasVPS, hasSPS, hasPPS bool
	for _, n := range d.nals {
		t := d.nalType(n)
		if d.codec == codecH265 {
			switch {
			case t == 32:
				d.vps, hasVPS = n, true
			case t == 33:
				d.sps, hasSPS = n, true
			case t == 34:
				d.pps, hasPPS = n, true
			case t >= 16 && t <= 21:
				key, slices = true, true
			case t < 32:
				slices = true
			}
		} else {
			switch {
			case t == 7:
				d.sps, hasSPS = n, true
			case t == 8:
				d.pps, hasPPS = n, true
			case t == 5:
				key, slices = true, true
			case t >= 1 && t <= 4:
				slices = true
			}
		}
	}
	if !slices {
		return picture{}, false
	}
	var out []byte
	if d.codec == codecH265 {
		out = append(out, 0, 0, 0, 1, 0x46, 0x01, 0x50) // AUD
	} else {
		out = append(out, 0, 0, 0, 1, 0x09, 0xf0) // AUD
	}
	if key {
		if d.sps == nil || d.pps == nil || (d.codec == codecH265 && d.vps == nil) {
			return picture{}, false
		}
		if d.codec == codecH265 && !hasVPS {
			out = append(append(out, startCode...), d.vps...)
		}
		if !hasSPS {
			out = append(append(out, startCode...), d.sps...)
		}
		if !hasPPS {
			out = append(append(out, startCode...), d.pps...)
		}
	}
	for _, n := range d.nals {
		t := d.nalType(n)
		if (d.codec == codecH264 && t == 9) || (d.codec == codecH265 && t == 35) {
			continue // our own AUD is in front already
		}
		out = append(append(out, startCode...), n...)
	}
	return picture{data: out, ts: ts, key: key}, true
}

// assembler puts the packets of the video track in order and hands out pictures.
type assembler struct {
	dep      depacketizer
	maxWait  time.Duration // how long a hole waits for the retransmission
	slots    [1024]*rtp.Packet
	started  bool
	expect   uint16 // next sequence number to use
	curTS    uint32
	curHas   bool // packets of the current picture were taken
	broken   bool // a packet of the current picture is missing for good
	needKey  bool // pictures are held back until a keyframe
	holeAt   time.Time
	maxSeen  uint16
	onPic    func(picture)
	askKey   func(reason string)
	lost     uint64 // packets given up
	dropped  uint64 // pictures not handed out
	pictures uint64
}

func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

func (a *assembler) push(p *rtp.Packet, now time.Time) {
	if !a.started {
		a.started, a.expect, a.maxSeen, a.needKey = true, p.SequenceNumber, p.SequenceNumber, true
	}
	if seqLess(p.SequenceNumber, a.expect) {
		return // too late or a second copy
	}
	if int(p.SequenceNumber-a.expect) >= len(a.slots) {
		// Far ahead (a jump in the stream): start again from this packet.
		a.restart(p.SequenceNumber)
	}
	a.slots[p.SequenceNumber%uint16(len(a.slots))] = p
	if seqLess(a.maxSeen, p.SequenceNumber) {
		a.maxSeen = p.SequenceNumber
	}
	a.drain(now)
}

func (a *assembler) restart(seq uint16) {
	for i := range a.slots {
		a.slots[i] = nil
	}
	a.dep.reset()
	a.expect, a.maxSeen, a.curHas, a.broken, a.needKey = seq, seq, false, false, true
	a.holeAt = time.Time{}
	a.askKey("stream jumped")
}

// drain takes every packet that is in order now.
func (a *assembler) drain(now time.Time) {
	for {
		i := a.expect % uint16(len(a.slots))
		p := a.slots[i]
		if p == nil {
			if seqLess(a.expect, a.maxSeen) && a.holeAt.IsZero() {
				a.holeAt = now // a hole with later packets already here
			}
			return
		}
		a.slots[i] = nil
		a.holeAt = time.Time{}
		a.expect++
		a.take(p)
	}
}

func (a *assembler) take(p *rtp.Packet) {
	if a.curHas && p.Timestamp != a.curTS {
		// The marker of the last picture was lost or missing: close it by the change of timestamp.
		a.close()
	}
	if !a.curHas {
		a.curTS, a.curHas = p.Timestamp, true
	}
	if !a.broken {
		if err := a.dep.push(p.Payload); err != nil {
			a.broken = true
		}
	}
	if p.Marker {
		a.close()
	}
}

func (a *assembler) close() {
	ts, broken := a.curTS, a.broken
	a.curHas, a.broken = false, false
	if broken {
		a.dep.reset()
		a.dropped++
		a.needKey = true
		return
	}
	pic, ok := a.dep.finish(ts)
	if !ok {
		if a.dep.sps == nil {
			a.needKey = true
		}
		a.dropped++
		return
	}
	if a.needKey && !pic.key {
		a.dropped++
		return
	}
	a.needKey = false
	a.pictures++
	a.onPic(pic)
}

// tick gives up a hole that waited too long: the packets until the next one that
// is there are lost; the picture they belonged to is broken.
func (a *assembler) tick(now time.Time) {
	if a.holeAt.IsZero() || now.Sub(a.holeAt) < a.maxWait {
		return
	}
	for a.slots[a.expect%uint16(len(a.slots))] == nil && seqLess(a.expect, a.maxSeen) {
		a.expect++
		a.lost++
	}
	// The picture being put together is broken; so is the next one if the hole was
	// its beginning. Either way pictures are held back until the keyframe asked for.
	a.broken = true
	a.dep.reset()
	a.needKey = true
	a.holeAt = time.Time{}
	a.askKey("packets lost")
	if p := a.slots[a.expect%uint16(len(a.slots))]; p != nil && !a.curHas {
		a.curTS, a.curHas = p.Timestamp, true
	}
	a.drain(now)
}
