package main

// A small MPEG-TS muxer: one program, one video track (H.264 or H.265) with the
// PCR, one AAC track. Every picture and every sound frame is sent the moment it
// is ready, in datagrams of up to 7 TS packets (1316 bytes), as SRT carries them.

const (
	tsPacket  = 188
	pidPMT    = 0x1000
	pidVideo  = 0x0100
	pidAudio  = 0x0101
	perDgram  = 7
	tableEach = 90000 / 10 // PAT and PMT at least every 100 ms (90 kHz units)
)

type tsMuxer struct {
	codec    videoCodec
	audio    bool
	cc       map[uint16]byte
	buf      []byte // TS packets waiting to be sent
	lastTab  int64
	tabSent  bool
	send     func([]byte) // gets one datagram; must copy if it keeps it
	sentPkts uint64
}

func newTSMuxer(codec videoCodec, audio bool, send func([]byte)) *tsMuxer {
	return &tsMuxer{codec: codec, audio: audio, cc: map[uint16]byte{}, send: send}
}

// crc32mpeg is the CRC of the PSI tables (polynomial 0x04C11DB7, not reflected).
var crcTable = func() *[256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return &t
}()

func crc32mpeg(b []byte) uint32 {
	c := uint32(0xffffffff)
	for _, v := range b {
		c = c<<8 ^ crcTable[byte(c>>24)^v]
	}
	return c
}

func (m *tsMuxer) nextCC(pid uint16) byte {
	c := m.cc[pid]
	m.cc[pid] = (c + 1) & 0x0f
	return c
}

// section writes one PSI section (PAT or PMT) as a single TS packet.
func (m *tsMuxer) section(pid uint16, body []byte) {
	crc := crc32mpeg(body)
	sec := append(append([]byte{}, body...), byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	p := make([]byte, tsPacket)
	p[0], p[1], p[2], p[3] = 0x47, 0x40|byte(pid>>8), byte(pid), 0x10|m.nextCC(pid)
	p[4] = 0 // pointer field
	n := copy(p[5:], sec)
	for i := 5 + n; i < tsPacket; i++ {
		p[i] = 0xff
	}
	m.buf = append(m.buf, p...)
}

func (m *tsMuxer) tables() {
	// PAT: program 1 -> PMT
	pat := []byte{0x00, 0xb0, 0x0d, 0x00, 0x01, 0xc1, 0x00, 0x00, 0x00, 0x01, 0xe0 | byte(pidPMT>>8), byte(pidPMT&0xff)}
	m.section(0, pat)
	vtype := byte(0x1b)
	if m.codec == codecH265 {
		vtype = 0x24
	}
	es := []byte{vtype, 0xe0 | byte(pidVideo>>8), byte(pidVideo&0xff), 0xf0, 0x00}
	if m.audio {
		es = append(es, 0x0f, 0xe0|byte(pidAudio>>8), byte(pidAudio&0xff), 0xf0, 0x00)
	}
	length := 9 + len(es) + 4
	pmt := []byte{0x02, 0xb0 | byte(length>>8), byte(length), 0x00, 0x01, 0xc1, 0x00, 0x00,
		0xe0 | byte(pidVideo>>8), byte(pidVideo&0xff), 0xf0, 0x00}
	pmt = append(pmt, es...)
	m.section(pidPMT, pmt)
}

func putPTS(b []byte, marker byte, ts int64) {
	ts &= 0x1ffffffff
	b[0] = marker<<4 | byte(ts>>29)&0x0e | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14) | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1) | 1
}

// pes cuts one PES packet into TS packets. pcr >= 0 puts a PCR into the first one.
func (m *tsMuxer) pes(pid uint16, streamID byte, pts int64, pcr int64, key bool, data []byte) {
	hdr := make([]byte, 14)
	hdr[0], hdr[1], hdr[2], hdr[3] = 0, 0, 1, streamID
	plen := 0
	if streamID != 0xe0 { // video: length 0 = unbounded
		plen = 8 + len(data)
		if plen > 0xffff {
			plen = 0
		}
	}
	hdr[4], hdr[5] = byte(plen>>8), byte(plen)
	hdr[6], hdr[7], hdr[8] = 0x80, 0x80, 5
	putPTS(hdr[9:], 2, pts)
	payload := append(hdr, data...)
	first := true
	for len(payload) > 0 {
		p := make([]byte, tsPacket)
		p[0], p[1], p[2] = 0x47, byte(pid>>8), byte(pid)
		if first {
			p[1] |= 0x40
		}
		var af []byte
		if first && (pcr >= 0 || key) {
			af = []byte{0, 0}
			if key {
				af[1] |= 0x40 // random access
			}
			if pcr >= 0 {
				af[1] |= 0x10
				base := pcr & 0x1ffffffff
				af = append(af, byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1), byte(base<<7)|0x7e, 0)
			}
			af[0] = byte(len(af) - 1)
		}
		room := tsPacket - 4 - len(af)
		if len(payload) < room {
			// Stuffing in the adaptation field fills the packet.
			need := room - len(payload)
			if af == nil {
				if need == 1 {
					af = []byte{0}
				} else {
					af = append([]byte{byte(need - 1), 0}, make([]byte, need-2)...)
					for i := 2; i < len(af); i++ {
						af[i] = 0xff
					}
				}
			} else {
				for i := 0; i < need; i++ {
					af = append(af, 0xff)
				}
				af[0] = byte(len(af) - 1)
			}
			room = tsPacket - 4 - len(af)
		}
		ctrl := byte(0x10)
		if af != nil {
			ctrl = 0x30
		}
		p[3] = ctrl | m.nextCC(pid)
		copy(p[4:], af)
		n := copy(p[4+len(af):], payload[:room])
		payload = payload[n:]
		m.buf = append(m.buf, p...)
		first = false
	}
}

// video writes one picture; pcr is the time the decoder clock should show now.
func (m *tsMuxer) video(pic []byte, pts, pcr int64, key bool) {
	if !m.tabSent || key || pcr-m.lastTab >= tableEach || pcr < m.lastTab {
		m.tables()
		m.lastTab, m.tabSent = pcr, true
	}
	m.pes(pidVideo, 0xe0, pts, pcr, key, pic)
	m.flush()
}

func (m *tsMuxer) audioFrame(adts []byte, pts int64) {
	if !m.audio || !m.tabSent {
		return
	}
	m.pes(pidAudio, 0xc0, pts, -1, false, adts)
	m.flush()
}

// flush sends everything waiting: full datagrams of 7 packets, the rest in a shorter one.
func (m *tsMuxer) flush() {
	for len(m.buf) > 0 {
		n := perDgram * tsPacket
		if n > len(m.buf) {
			n = len(m.buf)
		}
		m.send(m.buf[:n])
		m.sentPkts += uint64(n / tsPacket)
		m.buf = m.buf[n:]
	}
	m.buf = m.buf[:0]
}
