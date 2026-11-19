package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	srt "github.com/datarhei/gosrt"
)

// What a connection is doing on its signal.
const (
	roleReceiver = int32(iota) // bare ID, no data from the device: it gets the original
	roleSender                 // bare ID, the device delivers data: it is the input of the signal
	roleForward                // <id>*forward: it gets what the matrix assigns
)

// ConnInfo is the status of one connection as the web interface shows it.
type ConnInfo struct {
	ID    uint64  `json:"id"`
	Addr  string  `json:"addr"`
	Since int64   `json:"since"` // unix seconds
	Mbps  float64 `json:"mbps"`
	RTTMs float64 `json:"rttMs"`
	Pkts  uint64  `json:"pkts"`  // data packets received (input) or sent (receiver)
	Loss  uint64  `json:"loss"`  // packets lost on the way; SRT asked for them again
	Drops uint64  `json:"drops"` // packets that did not arrive in time and are missing
	// The same over the last minute: errors at the start do not stay on the tile forever.
	RecentPkts  uint64     `json:"recentPkts"`
	RecentLoss  uint64     `json:"recentLoss"`
	RecentDrops uint64     `json:"recentDrops"`
	LatencyMs   uint64     `json:"latencyMs"`
	Encrypted   bool       `json:"encrypted"`
	Media       *MediaInfo `json:"media,omitempty"` // input only: TV norm and audio channels
}

type conn struct {
	id        uint64
	sc        srt.Conn
	place     string // stream ID of the signal when the connection was made
	stream    string // what the device asked for (stream ID, or the signal and port), for the log
	forward   bool
	encrypted bool
	addr      string
	since     time.Time
	role      atomic.Int32
	lastData  atomic.Int64 // unix nanoseconds of the last data packet from the device
	rt        *placeRT
	probe     *tsProbe // looks into the data once the connection is an input
	pending   bool     // unknown stream ID while the original is off: no signal until it sends (h.mu)
	quiet     bool     // the same key from the same address was turned away a moment ago: not logged again

	wmu sync.Mutex             // gosrt allows one writer at a time
	why atomic.Pointer[string] // set when the bridge itself closed the connection

	stats srt.Statistics // touched by the statistics ticker only
	hist  [recentWindow]counts
	histN int   // samples taken since the role was last set
	histR int32 // the role the samples belong to (a sender counts other packets than a receiver)
	info  atomic.Pointer[ConnInfo]
}

var (
	whySendQueue = "closed by the bridge: its send queue was full"
	whySettings  = "closed by the bridge: matrix or settings changed, it reconnects"
	whyReplaced  = "closed by the bridge: the device came back on a new connection"
	whyNoOrig    = "closed by the bridge: receiving the original is switched off"
	whyUnknown   = "closed by the bridge: unknown stream ID, and the device only listens"
)

// quietFor: a device that was turned away keeps trying, often every few
// seconds. For this long its next attempts are counted under «Turned away»
// but not written to the log again.
const quietFor = 60 * time.Second

// originalWait: with the original switched off, a device on the sender key
// has this long to start sending. SRT does not say at the handshake whether
// a device wants to send or to receive; a sender delivers data at once.
var originalWait = 3 * time.Second

// senderSilence: an input that delivered nothing for this long gives way to a
// new connection with the same stream ID. After a break in the network the
// device reconnects while its old connection still waits for its timeout
// (5 s); without this rule the device itself would be turned away as a
// "second sender".
const senderSilence = time.Second

// defaultListenRetry is how often a port that could not be opened is tried again.
const defaultListenRetry = 3 * time.Second

// write passes one packet on. gosrt queues it without waiting; if the queue
// of this receiver is full or the connection is gone, the receiver is closed
// and reconnects on its own. A slow receiver never holds up the others.
func (c *conn) write(b []byte) {
	c.wmu.Lock()
	_, err := c.sc.Write(b)
	c.wmu.Unlock()
	if err != nil {
		c.why.CompareAndSwap(nil, &whySendQueue)
		go c.sc.Close()
	}
}

// placeRT is the running state of a signal: who sends, who listens.
type placeRT struct {
	sender  *conn
	starts  int                // how often an input went live on this signal
	subs    map[*conn]struct{} // receivers of the original
	fwd     map[*conn]struct{} // receivers of <id>*forward
	targets atomic.Pointer[[]*conn]
	tap     atomic.Pointer[tapDest] // local copy of the input for a program on this PC; nil = none
}

// tapDest is where a local program (SRT Bridge Output) wants the input of a
// signal: plain UDP on 127.0.0.1, without a second SRT leg and its latency.
// It asks again every second; a tap nobody renews ends by itself.
type tapDest struct {
	addr  *net.UDPAddr
	until int64 // unix nanoseconds
}

const tapLease = 5 * time.Second

// The SRT version comes from the library (gosrt), its own version from the build.
var srtVersion = fmt.Sprintf("%d.%d.%d", srt.SRT_VERSION>>16&0xff, srt.SRT_VERSION>>8&0xff, srt.SRT_VERSION&0xff)
var srtLibrary = func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/datarhei/gosrt" {
				return "gosrt " + strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return "gosrt"
}()

// setTaps replaces all taps: signal number (1-based) -> UDP port on 127.0.0.1.
func (h *Hub) setTaps(taps map[int]int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	own := h.cfg.ports()
	for _, port := range taps {
		if port < 1024 || port > 65535 {
			return errors.New("a tap port must be between 1024 and 65535")
		}
		for _, p := range own {
			if p == port {
				return fmt.Errorf("port %d is an SRT port of the bridge", port)
			}
		}
	}
	until := time.Now().Add(tapLease).UnixNano()
	for i, rt := range h.rt {
		if port := taps[i+1]; port != 0 {
			rt.tap.Store(&tapDest{addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, until: until})
		} else {
			rt.tap.Store(nil)
		}
	}
	return nil
}

func newPlaceRT() *placeRT {
	rt := &placeRT{subs: map[*conn]struct{}{}, fwd: map[*conn]struct{}{}}
	empty := []*conn{}
	rt.targets.Store(&empty)
	return rt
}

// kickAllLocked closes every connection of the signal; the readers tidy up.
func (rt *placeRT) kickAllLocked() {
	if rt.sender != nil {
		go rt.sender.sc.Close()
	}
	kickLocked(rt.subs)
	kickLocked(rt.fwd)
}

func kickLocked(set map[*conn]struct{}) {
	for c := range set {
		c.why.CompareAndSwap(nil, &whySettings)
		go c.sc.Close()
	}
}

// Rejected is a connection attempt that the bridge turned away.
type Rejected struct {
	Time     int64  `json:"time"`
	StreamID string `json:"streamId"`
	Addr     string `json:"addr"`
	Reason   string `json:"reason"`
	Count    int    `json:"count"`
	CanAdd   bool   `json:"canAdd"` // an unknown ID that could take a free signal with one click
	Slot     int    `json:"slot"`   // signal the attempt was meant for, 0 = none
	Short    string `json:"short"`  // the reason in a few words, for the tile of that signal
}

type Event struct {
	Time int64  `json:"time"`
	Text string `json:"text"`
}

// Hub owns the SRT listener, the signals and the matrix.
// h.rt[i] belongs to h.cfg.Places[i]; both always have cfg.Slots entries.
type Hub struct {
	mu          sync.Mutex
	cfg         *Config
	cfgPath     string
	rt          []*placeRT
	conns       map[*conn]struct{}
	rejected    []*Rejected
	events      []Event
	lns         []srt.Listener // one per port of cfg.ports(); nil = that port is not open
	lnNote      string         // what the log said last about the ports
	srtErr      string
	addresses   statusCache[[]string]
	listenRetry time.Duration
	retrying    bool // retryListen is running
	closed      bool // the bridge is ending: do not open the port again
	nextID      uint64
	tapConn     *net.UDPConn // sends the taps; nil if it could not be opened
	whip        whipInput    // the WHIP signal (whip.go)
}

func newHub(cfg *Config, cfgPath string) *Hub {
	h := &Hub{listenRetry: defaultListenRetry, cfg: cfg, cfgPath: cfgPath, conns: map[*conn]struct{}{}}
	h.tapConn, _ = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	for range cfg.Places {
		h.rt = append(h.rt, newPlaceRT())
	}
	h.openWhipInput()
	return h
}

// eventf notes something for the log file and for the list in the interface. Call with h.mu held.
func (h *Hub) eventf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	log.Print(text)
	h.events = append(h.events, Event{Time: time.Now().Unix(), Text: text})
	if len(h.events) > 200 {
		h.events = h.events[len(h.events)-200:]
	}
}

var errConfigSave = errors.New("could not save the configuration")

// commitLocked persists the proposed configuration before publishing it.
func (h *Hub) commitLocked(next Config) error {
	if err := next.save(h.cfgPath); err != nil {
		err = fmt.Errorf("%w: %w", errConfigSave, err)
		h.eventf("%v", err)
		return err
	}
	*h.cfg = next
	return nil
}

func (h *Hub) storePlaceLocked(i int, place Place) error {
	next := *h.cfg
	next.Places = append([]*Place(nil), next.Places...)
	next.Places[i] = &place
	return h.commitLocked(next)
}

// start opens the UDP port. An error (port in use) is kept for the interface.
func (h *Hub) start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.startLocked()
}

func (h *Hub) startLocked() {
	sc := srt.DefaultConfig()
	lat := time.Duration(h.cfg.LatencyMs) * time.Millisecond
	sc.ReceiverLatency = lat
	sc.PeerLatency = lat
	ports := h.cfg.ports()
	if len(h.lns) != len(ports) { // only after stopLocked
		h.lns = make([]srt.Listener, len(ports))
	}
	errText := ""
	for i, port := range ports {
		if h.lns[i] != nil {
			continue
		}
		ln, err := srt.Listen("srt", ":"+strconv.Itoa(port), sc)
		if err != nil {
			if errText == "" {
				errText = fmt.Sprintf("UDP %d: %v", port, err)
			}
			continue
		}
		h.lns[i] = ln
		slot := -1 // one port for all: the stream ID says which signal
		if h.cfg.perSignal() {
			slot = i
		}
		go h.acceptLoop(ln, i, slot)
	}
	note := fmt.Sprintf("SRT listening on UDP %s, latency at least %d ms", h.cfg.portRange(), h.cfg.LatencyMs)
	if errText != "" {
		note = fmt.Sprintf("SRT port could not be opened: %s; trying again every %s", errText, h.listenRetry)
	}
	if note != h.lnNote { // said once, not on every retry
		h.lnNote = note
		h.eventf("%s", note)
	}
	h.srtErr = errText
	if errText != "" && !h.retrying {
		h.retrying = true
		go h.retryListen()
	}
}

// allOpenLocked: every port the configuration asks for is open.
func (h *Hub) allOpenLocked() bool {
	for _, ln := range h.lns {
		if ln == nil {
			return false
		}
	}
	return len(h.lns) > 0
}

// retryListen opens the port again as soon as that works, so the bridge
// recovers on its own when the port was busy or the network was gone.
func (h *Hub) retryListen() {
	time.Sleep(h.listenRetry)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retrying = false
	if !h.closed && !h.allOpenLocked() {
		h.startLocked() // schedules the next retry only if a port is still unavailable
	}
}

// stop ends the bridge: every device is told, so it notices at once instead
// of waiting for its timeout.
func (h *Hub) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	h.stopLocked()
}

// stopLocked closes the port and with it every connection.
func (h *Hub) stopLocked() {
	lns := h.lns
	h.lns, h.lnNote = nil, ""
	for _, ln := range lns {
		if ln != nil {
			// Close must be finished before the port is opened again. It needs
			// nothing we hold; the readers that end now wait for h.mu and tidy up later.
			ln.Close()
		}
	}
}

// acceptLoop takes the requests of one port. idx is its place in h.lns; slot
// is the signal the port belongs to, or -1 when one port serves all signals.
func (h *Hub) acceptLoop(ln srt.Listener, idx, slot int) {
	for {
		req, err := ln.Accept2()
		if err != nil {
			if !errors.Is(err, srt.ErrListenerClosed) {
				h.mu.Lock()
				mine := idx < len(h.lns) && h.lns[idx] == ln
				if mine {
					h.lns[idx], h.lnNote = nil, ""
					h.eventf("SRT listener stopped: %v; opening the port again", err)
				}
				h.mu.Unlock()
				if mine {
					// The port must be free before it can be opened again; this
					// also closes the connections that hung on the dead listener.
					ln.Close()
					h.mu.Lock()
					if !h.closed && idx < len(h.lns) && h.lns[idx] == nil {
						h.startLocked()
					}
					h.mu.Unlock()
				}
			}
			return
		}
		h.handle(req, slot)
	}
}

func ipOf(a net.Addr) string {
	if u, ok := a.(*net.UDPAddr); ok {
		return u.IP.String()
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// rejectLocked turns the request away and remembers it. Devices retry every
// second or so, so the same attempt is counted instead of listed again.
func (h *Hub) rejectLocked(req srt.ConnRequest, code srt.RejectionReason, reason string, canAdd bool) {
	h.rejectSlotLocked(req, code, reason, canAdd, 0, "")
}

// rejectSlotLocked also says which signal the attempt was for, so its tile can show why.
func (h *Hub) rejectSlotLocked(req srt.ConnRequest, code srt.RejectionReason, reason string, canAdd bool, slot int, short string) {
	req.Reject(code)
	h.noteRejectedLocked(req.StreamId(), ipOf(req.RemoteAddr()), reason, canAdd, slot, short)
}

// noteRejectedLocked lists an attempt that was turned away, during or after the handshake.
func (h *Hub) noteRejectedLocked(id, addr, reason string, canAdd bool, slot int, short string) {
	now := time.Now().Unix()
	for _, r := range h.rejected {
		if r.StreamID == id && r.Addr == addr && r.Reason == reason {
			r.Time, r.Count, r.CanAdd, r.Slot, r.Short = now, r.Count+1, canAdd, slot, short
			return
		}
	}
	h.rejected = append(h.rejected, &Rejected{Time: now, StreamID: id, Addr: addr, Reason: reason, Count: 1, CanAdd: canAdd, Slot: slot, Short: short})
	if len(h.rejected) > 50 {
		h.rejected = h.rejected[len(h.rejected)-50:]
	}
	log.Printf("rejected %q from %s: %s", id, addr, reason)
}

// passLocked checks the request against the passphrase of the signal and
// says whether it may go on. where is put in front of the reason, part in
// front of the short text on the tile of signal i (0-based).
func (h *Hub) passLocked(req srt.ConnRequest, pass, where string, i int, part string) bool {
	switch {
	case pass == "" && req.IsEncrypted():
		h.rejectSlotLocked(req, srt.REJ_UNSECURE, where+"the device uses a passphrase, the signal has none", false, i+1, part+"device sends a passphrase, none is set")
	case pass != "" && !req.IsEncrypted():
		h.rejectSlotLocked(req, srt.REJ_UNSECURE, where+"the signal needs a passphrase, the device sent none", false, i+1, part+"passphrase missing")
	case pass != "" && req.SetPassphrase(pass) != nil:
		h.rejectSlotLocked(req, srt.REJ_BADSECRET, where+"wrong passphrase", false, i+1, part+"wrong passphrase")
	default:
		return true
	}
	return false
}

// handlePortLocked takes a device on the port of signal i. No stream ID is
// needed: it starts as a receiver of what the matrix assigns to this signal;
// as soon as it delivers data it is the input of the signal instead.
func (h *Hub) handlePortLocked(req srt.ConnRequest, i int) {
	if i >= len(h.cfg.Places) {
		req.Reject(srt.REJ_PEER)
		return
	}
	label := h.cfg.signalLabel(i)
	if h.cfg.Places[i].Off {
		h.rejectSlotLocked(req, srt.REJ_PEER, "the signal is switched off", false, i+1, "signal is switched off")
		return
	}
	if !h.passLocked(req, h.cfg.Places[i].PassIn, label+": ", i, "") {
		return
	}
	sc, err := req.Accept()
	if err != nil {
		h.eventf("%s from %s: handshake failed: %v", label, ipOf(req.RemoteAddr()), err)
		return
	}
	h.nextID++
	rt := h.rt[i]
	c := &conn{id: h.nextID, sc: sc, place: label, stream: label, encrypted: h.cfg.Places[i].PassIn != "",
		addr: req.RemoteAddr().String(), since: time.Now(), rt: rt, probe: newTSProbe()}
	c.info.Store(&ConnInfo{ID: c.id, Addr: c.addr, Since: c.since.Unix(), Encrypted: c.encrypted})
	h.conns[c] = struct{}{}
	c.role.Store(roleForward)
	rt.fwd[c] = struct{}{}
	h.rebuildLocked()
	h.eventf("connected: %s from %s", label, c.addr)
	go h.serve(c)
}

// handle decides about one connection request. slot is the signal of the
// port it came in on, or -1 when the stream ID has to say it.
func (h *Hub) handle(req srt.ConnRequest, slot int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slot >= 0 {
		h.handlePortLocked(req, slot)
		return
	}
	id, forward := splitStreamID(req.StreamId(), h.cfg.Suffixes)
	if why := checkPlaceID(id, h.cfg.Suffixes); why != "" {
		h.rejectLocked(req, srt.REJ_PEER, why, false)
		return
	}
	if h.cfg.whipOn() && id == h.cfg.Whip.ID {
		h.handleWhipLocked(req, forward)
		return
	}
	i := h.cfg.slot(id)
	byKey := false // came in with the receiver key of signal i
	if j := h.cfg.recvSlot(id); !forward && j >= 0 {
		i, forward, byKey = j, true, true
	} else if i >= 0 && forward && h.cfg.Places[i].RecvID != "" {
		// The signal has its own receiver key: the sender key must not open the forward output.
		h.rejectSlotLocked(req, srt.REJ_PEER, "this signal has its own receiver key; <stream ID>*forward is not accepted", false, i+1, "forward: receiver key needed")
		return
	}
	pending := false // unknown key while the original is off: a signal only once the device sends
	if i < 0 {
		free := h.cfg.freeSlot()
		switch {
		case free < 0:
			h.rejectLocked(req, srt.REJ_PEER, fmt.Sprintf("unknown stream ID, and all %d signals are taken", h.cfg.Slots), false)
			return
		case !h.cfg.AcceptAny:
			h.rejectLocked(req, srt.REJ_PEER, "unknown stream ID", true)
			return
		case req.IsEncrypted():
			// A new signal has no passphrase yet: do not take a place for an attempt that cannot succeed.
			h.rejectLocked(req, srt.REJ_UNSECURE, "unknown stream ID, and the device uses a passphrase", true)
			return
		}
		if h.cfg.NoOriginal && !forward {
			// A device that only listens could get nothing on a new signal, and it
			// must not take one (a receiver with an old receiver key would).
			// It gets a signal when it starts sending: see promote.
			pending = true
		} else {
			i = free
			old := h.cfg.Places[i]
			if err := h.storePlaceLocked(i, Place{ID: id, Name: old.Name, Source: old.Source, Auto: true}); err != nil {
				req.Reject(srt.REJ_PEER)
				return
			}
			h.eventf("signal %d is now %q, set by a connection from %s", i+1, id, ipOf(req.RemoteAddr()))

		}
	}
	var p *Place
	pass, part := "", ""
	if !pending && h.cfg.Places[i].Off {
		// Switched off on its tile: sender, original and forward are all turned away.
		h.rejectSlotLocked(req, srt.REJ_PEER, "the signal is switched off", false, i+1, "signal is switched off")
		return
	}
	if !pending {
		p = h.cfg.Places[i]
		pass = p.PassIn
		if forward {
			pass, part = p.PassFwd, "forward: "
		}
	}
	if !h.passLocked(req, pass, "", i, part) {
		return
	}
	sc, err := req.Accept()
	if err != nil {
		h.eventf("%q from %s: handshake failed: %v", req.StreamId(), ipOf(req.RemoteAddr()), err)
		return
	}
	h.nextID++
	rt, place := newPlaceRT(), id // a pending connection belongs to no signal yet
	if !pending {
		rt = h.rt[i]
		if byKey {
			place = p.ID
		}
	}
	quiet := h.turnedAwayLocked(req.StreamId(), ipOf(req.RemoteAddr()))
	c := &conn{id: h.nextID, sc: sc, place: place, stream: req.StreamId(), forward: forward, encrypted: pass != "",
		addr: req.RemoteAddr().String(), since: time.Now(), rt: rt, probe: newTSProbe(), pending: pending, quiet: quiet}
	c.info.Store(&ConnInfo{ID: c.id, Addr: c.addr, Since: c.since.Unix(), Encrypted: c.encrypted})
	h.conns[c] = struct{}{}
	if forward {
		c.role.Store(roleForward)
		rt.fwd[c] = struct{}{}
	} else {
		// Until the device delivers data it counts as a receiver of the original.
		c.role.Store(roleReceiver)
		if h.cfg.NoOriginal {
			go h.waitForSender(c, i)
		} else {
			rt.subs[c] = struct{}{}
		}
	}
	h.rebuildLocked()
	if !quiet {
		h.eventf("connected: %q from %s", req.StreamId(), c.addr)
	}
	go h.serve(c)
}

// turnedAwayLocked: this stream ID from this address was turned away a moment ago.
func (h *Hub) turnedAwayLocked(streamID, ip string) bool {
	since := time.Now().Add(-quietFor).Unix()
	for _, r := range h.rejected {
		if r.StreamID == streamID && r.Addr == ip && r.Time >= since {
			return true
		}
	}
	return false
}

// rebuildLocked works out for every signal who gets the packets of its
// sender: the receivers of the original plus the forward receivers of every
// signal that the matrix points here. The readers pick the list up without a lock.
func (h *Hub) rebuildLocked() {
	lists := make([][]*conn, len(h.rt))
	for i, rt := range h.rt {
		for c := range rt.subs {
			lists[i] = append(lists[i], c)
		}
	}
	whip := []*conn{}
	for c := range h.whip.rt.subs {
		whip = append(whip, c)
	}
	for i, p := range h.cfg.Places {
		if p.Source == whipSource {
			for c := range h.rt[i].fwd {
				whip = append(whip, c)
			}
			continue
		}
		if p.Source < 1 || p.Source > len(h.rt) {
			continue
		}
		for c := range h.rt[i].fwd {
			lists[p.Source-1] = append(lists[p.Source-1], c)
		}
	}
	for i, rt := range h.rt {
		l := lists[i]
		if l == nil {
			l = []*conn{}
		}
		rt.targets.Store(&l)
	}
	// A new receiver of the WHIP signal needs a keyframe to start: WebRTC
	// senders send one only when asked.
	if old := len(*h.whip.rt.targets.Load()); len(whip) > old {
		h.whip.wantKey.Store(true)
	}
	h.whip.rt.targets.Store(&whip)
}

// serve reads from one connection until it ends. Whoever delivers data on a
// bare ID becomes the sender of the signal; a second sender is turned away.
func (h *Hub) serve(c *conn) {
	defer h.drop(c)
	buf := make([]byte, 4096)
	for {
		n, err := c.sc.Read(buf)
		if err != nil {
			return
		}
		if n == 0 || c.forward {
			continue // a forward output only receives; data from it is ignored
		}
		now := time.Now().UnixNano()
		c.lastData.Store(now)
		if c.role.Load() != roleSender && !h.promote(c) {
			return
		}
		pkt := buf[:n] // gosrt copies the bytes in Write
		c.probe.feed(pkt)
		if t := c.rt.tap.Load(); t != nil && now < t.until && h.tapConn != nil {
			// Plain UDP on this PC: if SRT Bridge Output is not listening, the packet is simply lost.
			h.tapConn.WriteToUDP(pkt, t.addr)
		}
		for _, t := range *c.rt.targets.Load() {
			t.write(pkt)
		}
	}
}

// waitForSender turns a device on the sender key away if it has not started
// sending in time while the original is switched off. Until then it gets nothing.
func (h *Hub) waitForSender(c *conn, i int) {
	time.Sleep(originalWait)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.conns[c]; !ok || c.role.Load() == roleSender {
		return
	}
	if c.pending { // an unknown key that only listens: it took no signal and gets none
		h.noteRejectedLocked(c.stream, ipOf(c.sc.RemoteAddr()), "unknown stream ID, and the device only listens while receiving the original is off: an old receiver key?", false, 0, "")
		c.why.CompareAndSwap(nil, &whyUnknown)
		go c.sc.Close()
		return
	}
	if !h.cfg.NoOriginal { // switched on again meanwhile: it is a receiver of the original after all
		c.rt.subs[c] = struct{}{}
		h.rebuildLocked()
		return
	}
	h.noteRejectedLocked(c.stream, ipOf(c.sc.RemoteAddr()), "receiving the original is switched off: the device sent nothing", false, i+1, "original is off: receiver turned away")
	c.why.CompareAndSwap(nil, &whyNoOrig)
	go c.sc.Close()
}

// setOriginal switches receiving the original with the sender key on or off.
func (h *Hub) setOriginal(allow bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.NoOriginal == !allow {
		return nil
	}
	next := *h.cfg
	next.NoOriginal = !allow
	if err := h.commitLocked(next); err != nil {
		return err
	}
	if !allow {
		for _, rt := range h.rt {
			for c := range rt.subs {
				c.why.CompareAndSwap(nil, &whyNoOrig)
				go c.sc.Close()
			}
		}
		h.eventf("receiving the original is switched off: the sender key only sends")
	} else {
		h.eventf("receiving the original with the sender key is allowed again")
	}
	return nil
}

func (h *Hub) promote(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.conns[c]; !ok {
		return false // the signal was cleared or renamed meanwhile
	}
	if c.rt == h.whip.rt {
		h.noteRejectedLocked(c.stream, ipOf(c.sc.RemoteAddr()), "the WHIP signal gets its input over WHIP only; this stream ID is for receivers", false, whipSlotLocked(h), "SRT sender on the WHIP signal turned away")
		return false
	}
	if c.pending && !h.claimLocked(c) {
		return false
	}
	if old := c.rt.sender; old != nil && old != c {
		if time.Since(time.Unix(0, old.lastData.Load())) < senderSilence {
			if !c.quiet {
				h.eventf("second sender for %q from %s turned away: %s is already sending", c.place, c.addr, old.addr)
			}
			// Also on the tile: a device that keeps trying shows up there as an error.
			for i, rt := range h.rt {
				if rt == c.rt {
					h.noteRejectedLocked(c.stream, ipOf(c.sc.RemoteAddr()), "second sender: "+old.addr+" is already sending on this signal", false, i+1, "second sender turned away")
				}
			}
			return false
		}
		h.eventf("%q: input from %s was silent, the new connection from %s takes over", c.place, old.addr, c.addr)
		old.why.CompareAndSwap(nil, &whyReplaced)
		go old.sc.Close()
	}
	delete(c.rt.subs, c)
	delete(c.rt.fwd, c) // with a port per signal it listened to the matrix output until now
	c.rt.sender = c
	c.rt.starts++
	c.role.Store(roleSender)
	h.rebuildLocked()
	h.eventf("%q: input is live from %s", c.place, c.addr)
	return true
}

// claimLocked gives a pending connection (unknown key, original off) its
// signal now that it sends: the next free one, as for every new stream ID.
func (h *Hub) claimLocked(c *conn) bool {
	ip := ipOf(c.sc.RemoteAddr())
	i := h.cfg.slot(c.place)
	switch {
	case i >= 0 && h.cfg.Places[i].Off:
		h.noteRejectedLocked(c.stream, ip, "the signal is switched off", false, i+1, "signal is switched off")
		return false
	case i >= 0 && h.cfg.Places[i].PassIn != "":
		// Set by hand meanwhile, with a passphrase this device did not use.
		h.noteRejectedLocked(c.stream, ip, "the signal needs a passphrase, the device sent none", false, i+1, "passphrase missing")
		return false
	case i < 0:
		if i = h.cfg.freeSlot(); i < 0 {
			h.noteRejectedLocked(c.stream, ip, fmt.Sprintf("unknown stream ID, and all %d signals are taken", h.cfg.Slots), false, 0, "")
			return false
		}
		old := h.cfg.Places[i]
		if err := h.storePlaceLocked(i, Place{ID: c.place, Name: old.Name, Source: old.Source, Auto: true}); err != nil {
			return false
		}
		h.eventf("signal %d is now %q, set by a connection from %s", i+1, c.place, ip)

	}
	c.rt, c.pending = h.rt[i], false
	return true
}

func (h *Hub) drop(c *conn) {
	c.sc.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.conns[c]; !ok {
		return
	}
	delete(h.conns, c)
	delete(c.rt.subs, c)
	delete(c.rt.fwd, c)
	if c.rt.sender == c {
		c.rt.sender = nil
		h.eventf("%q: input from %s ended", c.place, c.addr)
	} else if c.role.Load() == roleSender {
		h.eventf("%q: old input connection from %s closed", c.place, c.addr)
	} else if c.quiet && time.Since(c.since) < 10*time.Second {
		// Another short try of a device that is being turned away: counted under «Turned away», not logged.
	} else {
		if why := c.why.Load(); why != nil {
			h.eventf("disconnected: %q receiver %s (%s)", c.stream, c.addr, *why)
		} else {
			h.eventf("disconnected: %q receiver %s", c.stream, c.addr)
		}
	}
	h.rebuildLocked()
}

// counts are the running totals of one connection at one moment.
type counts struct{ pkts, loss, drops uint64 }

// recentWindow: the tile shows loss and drops of the last minute (one sample per second).
const recentWindow = 60

// recent returns the totals now minus those a minute ago (or at the start,
// or when the role changed), and keeps the sample for later.
func (c *conn) recent(now counts, role int32) counts {
	if role != c.histR {
		c.histR, c.histN = role, 0
	}
	var base counts
	if c.histN >= recentWindow {
		base = c.hist[c.histN%recentWindow]
	} else if c.histN > 0 {
		base = c.hist[0]
	} else {
		base = now
	}
	c.hist[c.histN%recentWindow] = now
	c.histN++
	if c.histN > 1<<30 {
		c.histN = recentWindow + c.histN%recentWindow // never overflows, same place in the ring
	}
	return counts{now.pkts - base.pkts, now.loss - base.loss, now.drops - base.drops}
}

// statsLoop refreshes the figures of every connection once a second.
func (h *Hub) statsLoop() {
	for range time.Tick(time.Second) {
		h.mu.Lock()
		list := make([]*conn, 0, len(h.conns))
		for c := range h.conns {
			list = append(list, c)
		}
		h.mu.Unlock()
		for _, c := range list {
			c.sc.Stats(&c.stats)
			s := &c.stats
			info := &ConnInfo{ID: c.id, Addr: c.addr, Since: c.since.Unix(), RTTMs: s.Instantaneous.MsRTT, Encrypted: c.encrypted}
			role := c.role.Load()
			if role == roleSender {
				info.Mbps = s.Interval.MbpsRecvRate
				info.Pkts = s.Accumulated.PktRecv
				info.Loss = s.Accumulated.PktRecvLoss
				info.Drops = s.Accumulated.PktRecvDrop
				info.LatencyMs = s.Instantaneous.MsRecvTsbPdDelay
				info.Media = c.probe.out.Load()
			} else {
				info.Mbps = s.Interval.MbpsSendRate
				info.Pkts = s.Accumulated.PktSent
				info.Loss = s.Accumulated.PktRetrans
				info.Drops = s.Accumulated.PktSendDrop
				info.LatencyMs = s.Instantaneous.MsSendTsbPdDelay
			}
			r := c.recent(counts{info.Pkts, info.Loss, info.Drops}, role)
			info.RecentPkts, info.RecentLoss, info.RecentDrops = r.pkts, r.loss, r.drops
			c.info.Store(info)
		}
	}
}

// ---- what the web interface sees and changes ----

type PlaceState struct {
	Slot int `json:"slot"` // 1-based number of the signal
	Port int `json:"port"` // UDP port devices use for this signal
	Place
	Starts    int         `json:"starts"`
	Sender    *ConnInfo   `json:"sender"`
	Receivers []*ConnInfo `json:"receivers"`
	Forward   []*ConnInfo `json:"forward"`
}

type State struct {
	Version    string          `json:"version"`
	SRTVer     string          `json:"srtVersion"` // SRT version the library speaks and expects at least
	SRTLib     string          `json:"srtLibrary"` // the library and its version
	Sys        SysState        `json:"sys"`
	SRTPort    int             `json:"srtPort"`
	PortMode   string          `json:"portMode"`
	PortRange  string          `json:"portRange"` // "21101" or "21101-21108"
	LatencyMs  int             `json:"latencyMs"`
	Suffixes   []string        `json:"suffixes"`
	AcceptAny  bool            `json:"acceptAny"`
	NoOrig     bool            `json:"noOriginal"`
	Slots      int             `json:"slots"`
	PublicIP   string          `json:"publicIp"`
	SRTError   string          `json:"srtError"`
	Addresses  []string        `json:"addresses"`
	ConfigFile string          `json:"configFile"` // where config.json is kept
	LogFile    string          `json:"logFile"`    // where the log is kept, "-" = console
	Places     []*PlaceState   `json:"places"`
	Rejected   []*Rejected     `json:"rejected"`
	Events     []Event         `json:"events"`
	Remote     *RemoteStatus   `json:"remote"` // filled in by the web server
	Output     *OutputStatus   `json:"output"` // Bridge Output; filled in by the web server
	Autostart  *AutostartState `json:"autostart"`
	Whip       *WhipState      `json:"whip"` // the WHIP signal; the program part is filled in by the web server
}

func infos(set map[*conn]struct{}) []*ConnInfo {
	out := make([]*ConnInfo, 0, len(set))
	for c := range set {
		out = append(out, c.info.Load())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) state() *State {
	addresses := h.addresses.get("", localAddresses)
	h.mu.Lock()
	defer h.mu.Unlock()
	st := &State{Version: version, SRTVer: srtVersion, SRTLib: srtLibrary, Sys: sysSnapshot(), SRTPort: h.cfg.SRTPort, PortMode: h.cfg.PortMode, PortRange: h.cfg.portRange(), LatencyMs: h.cfg.LatencyMs,
		Suffixes: append([]string{}, h.cfg.Suffixes...), AcceptAny: h.cfg.AcceptAny, NoOrig: h.cfg.NoOriginal, Slots: h.cfg.Slots, PublicIP: h.cfg.PublicIP,
		SRTError: h.srtErr, Addresses: addresses, ConfigFile: configFile, LogFile: logFile, Places: []*PlaceState{}}
	for i, p := range h.cfg.Places {
		rt := h.rt[i]
		port := h.cfg.SRTPort
		if h.cfg.perSignal() {
			port += i
		}
		ps := &PlaceState{Slot: i + 1, Port: port, Place: *p, Starts: rt.starts, Receivers: infos(rt.subs), Forward: infos(rt.fwd)}
		if rt.sender != nil {
			ps.Sender = rt.sender.info.Load()
		}
		st.Places = append(st.Places, ps)
	}
	st.Rejected = make([]*Rejected, 0, len(h.rejected))
	for i := len(h.rejected) - 1; i >= 0; i-- {
		r := *h.rejected[i]
		st.Rejected = append(st.Rejected, &r)
	}
	st.Events = make([]Event, 0, len(h.events))
	for i := len(h.events) - 1; i >= 0; i-- {
		st.Events = append(st.Events, h.events[i])
	}
	st.Whip = h.whipStateLocked()
	return st
}

// detachLocked closes every connection of a signal and forgets them at once,
// so the signal can be given a new ID without waiting for the readers.
func (h *Hub) detachLocked(i int) {
	old := h.rt[i]
	old.kickAllLocked()
	for c := range h.conns {
		if c.rt == old {
			delete(h.conns, c)
		}
	}
	h.rt[i] = newPlaceRT()
}

// disconnectInput closes the sender of a signal; the device reconnects on its own.
func (h *Hub) disconnectInput(slot int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slot < 1 || slot > len(h.rt) {
		return errors.New("unknown signal")
	}
	if c := h.rt[slot-1].sender; c != nil {
		h.eventf("signal %d: input %s disconnected by hand", slot, c.addr)
		go c.sc.Close()
	}
	return nil
}

// addPlace gives the first free signal this stream ID.
func (h *Hub) addPlace(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	free := h.cfg.freeSlot()
	if free < 0 {
		return errors.New("all signals are taken; clear one first")
	}
	return h.updatePlaceLocked(PlaceUpdate{Slot: free + 1, ID: &id})
}

// PlaceUpdate carries only the fields that should change.
type PlaceUpdate struct {
	Slot    int     `json:"slot"`
	ID      *string `json:"id"` // "" clears the signal
	Name    *string `json:"name"`
	Source  *int    `json:"source"`
	PassIn  *string `json:"passIn"`
	PassFwd *string `json:"passFwd"`
	RecvID  *string `json:"recvId"` // receiver key; "" = <id>*forward
	Off     *bool   `json:"off"`    // switch the signal off (true) or on again (false)
}

// updatePlace changes the stream ID, the matrix source or a passphrase of a
// signal. Everything is checked before anything changes. The devices that are
// affected are closed once; they reconnect and start clean.
func (h *Hub) updatePlace(u PlaceUpdate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.updatePlaceLocked(u)
}

func (h *Hub) updatePlaceLocked(u PlaceUpdate) error {
	if u.Slot < 1 || u.Slot > len(h.cfg.Places) {
		return errors.New("unknown signal")
	}
	i := u.Slot - 1
	p := h.cfg.Places[i]
	if u.ID != nil && *u.ID != "" && *u.ID != p.ID {
		if why := checkPlaceID(*u.ID, h.cfg.Suffixes); why != "" {
			return errors.New(why)
		}
		if j := h.cfg.slot(*u.ID); j >= 0 {
			return fmt.Errorf("signal %d has this stream ID already", j+1)
		}
		if j := h.cfg.recvSlot(*u.ID); j >= 0 {
			return fmt.Errorf("signal %d uses this as its receiver key", j+1)
		}
		if *u.ID == h.cfg.Whip.ID {
			return errors.New("the WHIP signal uses this stream ID")
		}
	}
	newID := p.ID
	if u.ID != nil {
		newID = *u.ID
	}
	if u.RecvID != nil && *u.RecvID != "" {
		r := *u.RecvID
		if why := checkPlaceID(r, h.cfg.Suffixes); why != "" {
			return errors.New("receiver key: " + strings.TrimPrefix(why, "the stream ID "))
		}
		switch j, k := h.cfg.slot(r), h.cfg.recvSlot(r); {
		case newID == "":
			return errors.New("the signal needs a sender key before it can get a receiver key")
		case r == newID:
			return errors.New("the receiver key must differ from the sender key")
		case j >= 0 && j != i:
			return fmt.Errorf("signal %d uses this as its stream ID", j+1)
		case k >= 0 && k != i:
			return fmt.Errorf("signal %d uses this receiver key already", k+1)
		case r == h.cfg.Whip.ID:
			return errors.New("the WHIP signal uses this stream ID")
		}
	}
	if u.Source != nil && !h.cfg.validSource(*u.Source) {
		return errors.New("unknown input")
	}
	for _, s := range []*string{u.PassIn, u.PassFwd} {
		if s != nil {
			if why := checkPassphrase(*s); why != "" {
				return errors.New(why)
			}
		}
	}

	next := *p
	if newID != p.ID {
		if newID == "" {
			next.PassIn, next.PassFwd, next.RecvID, next.Off = "", "", "", false
		}
		next.ID, next.Auto = newID, false
	}
	if u.Name != nil {
		name := []rune(*u.Name)
		if len(name) > 40 {
			name = name[:40]
		}
		next.Name = string(name)
	}
	if u.Source != nil {
		next.Source = *u.Source
	}
	if u.PassIn != nil {
		next.PassIn = *u.PassIn
	}
	if u.PassFwd != nil {
		next.PassFwd = *u.PassFwd
	}
	if u.RecvID != nil && next.ID != "" {
		next.RecvID = *u.RecvID
	}
	if u.Off != nil && (!*u.Off || next.ID != "" || h.cfg.perSignal()) {
		next.Off = *u.Off
	}
	if next.ID != "" && (u.Source != nil || u.PassIn != nil || u.PassFwd != nil || u.RecvID != nil || u.Off != nil) {
		next.Auto = false
	}
	if err := h.storePlaceLocked(i, next); err != nil {
		return err
	}
	if next.ID != p.ID {
		h.detachLocked(i)
		if *u.ID == "" {
			h.eventf("signal %d cleared (was %q)", u.Slot, p.ID)
		} else {
			h.eventf("signal %d is now %q", u.Slot, *u.ID)
			kept := h.rejected[:0]
			for _, r := range h.rejected {
				if rid, _ := splitStreamID(r.StreamID, h.cfg.Suffixes); rid != *u.ID {
					kept = append(kept, r)
				}
			}
			h.rejected = kept
		}
	}
	rt := h.rt[i]
	if next.Source != p.Source {
		kickLocked(rt.fwd)
		if next.Source == 0 {
			h.eventf("matrix: forward of signal %d gets nothing", u.Slot)
		} else if next.Source == whipSource {
			h.eventf("matrix: forward of signal %d gets the WHIP input", u.Slot)
		} else {
			h.eventf("matrix: forward of signal %d gets signal %d", u.Slot, next.Source)
		}
	}
	if next.PassIn != p.PassIn {
		if rt.sender != nil {
			go rt.sender.sc.Close()
		}
		kickLocked(rt.subs)
		if h.cfg.perSignal() { // there the one passphrase covers the receivers as well
			kickLocked(rt.fwd)
		}
		h.eventf("signal %d: passphrase of the input changed", u.Slot)
	}
	if next.PassFwd != p.PassFwd {
		kickLocked(rt.fwd)
		h.eventf("signal %d: passphrase of the forward output changed", u.Slot)
	}
	if next.RecvID != p.RecvID {
		kickLocked(rt.fwd) // they reconnect and must use the new key
		if next.RecvID == "" {
			h.eventf("signal %d: receiver key removed, receivers use %q", u.Slot, h.cfg.forwardName(&next))
		} else {
			h.eventf("signal %d: new receiver key %q", u.Slot, next.RecvID)
		}
	}
	if next.Off != p.Off {
		if next.Off {
			// Everybody on this signal goes; they keep calling and are turned away.
			h.rt[i].kickAllLocked()
			h.eventf("signal %d switched off: its devices are turned away", u.Slot)
		} else {
			h.eventf("signal %d switched on again", u.Slot)
		}
	}
	h.rebuildLocked()

	return nil
}

type SettingsUpdate struct {
	SRTPort   int      `json:"srtPort"`
	PortMode  string   `json:"portMode"` // "" keeps the mode
	LatencyMs int      `json:"latencyMs"`
	Suffixes  []string `json:"suffixes"`
	AcceptAny bool     `json:"acceptAny"`
	Slots     int      `json:"slots"`
}

// updateSettings saves the settings. A new port or latency restarts the
// listener, which closes every connection; the devices reconnect.
func (h *Hub) updateSettings(u SettingsUpdate) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u.PortMode == "" {
		u.PortMode = h.cfg.PortMode
	}
	if u.PortMode != modeSingle && u.PortMode != modeSignal {
		return errors.New("unknown port mode")
	}
	if u.LatencyMs < 20 || u.LatencyMs > 8000 {
		return errors.New("the latency must be between 20 and 8000 ms")
	}
	if u.Slots < 1 || u.Slots > maxSlots {
		return fmt.Errorf("the number of signals must be between 1 and %d", maxSlots)
	}
	if why := checkPorts(u.SRTPort, u.Slots, u.PortMode); why != "" {
		return errors.New(why)
	}
	suffixes := cleanSuffixes(u.Suffixes)
	if _, fwd := splitStreamID(h.cfg.Whip.ID, suffixes); fwd {
		return fmt.Errorf("the stream ID of the WHIP signal %q ends with one of these suffixes", h.cfg.Whip.ID)
	}
	for i, p := range h.cfg.Places {
		if _, fwd := splitStreamID(p.ID, suffixes); fwd {
			return fmt.Errorf("the stream ID %q ends with one of these suffixes", p.ID)
		}
		if _, fwd := splitStreamID(p.RecvID, suffixes); fwd {
			return fmt.Errorf("the receiver key %q ends with one of these suffixes", p.RecvID)
		}
		// With a port per signal the stream IDs are not in use, so they do not hold a signal.
		if i >= u.Slots && p.ID != "" && u.PortMode == modeSingle {
			return fmt.Errorf("signal %d (%q) is in use; clear it before reducing the number", i+1, p.ID)
		}
	}
	oldSlots, oldMode := h.cfg.Slots, h.cfg.PortMode
	next := *h.cfg
	next.SRTPort, next.LatencyMs, next.PortMode = u.SRTPort, u.LatencyMs, u.PortMode
	next.Suffixes, next.AcceptAny, next.Slots = suffixes, u.AcceptAny, u.Slots
	if next.whipOn() {
		if err := next.checkWhipPort(next.Whip.Port); err != nil {
			return err
		}
	}
	if u.Slots != oldSlots {
		next.Places = make([]*Place, u.Slots)
		for i := range next.Places {
			var p Place
			if i < oldSlots {
				p = *h.cfg.Places[i]
			}
			if !next.validSource(p.Source) {
				p.Source = 0
			}
			next.Places[i] = &p
		}
	}
	restart := u.SRTPort != h.cfg.SRTPort || u.LatencyMs != h.cfg.LatencyMs || u.PortMode != oldMode ||
		len(h.lns) != len(next.ports()) || !h.allOpenLocked()
	oldPlaces := h.cfg.Places
	if err := h.commitLocked(next); err != nil {
		return err
	}
	if u.Slots != oldSlots {
		for i := u.Slots; i < oldSlots; i++ {
			h.detachLocked(i)
		}
		for len(h.rt) < u.Slots {
			h.rt = append(h.rt, newPlaceRT())
		}
		h.rt = h.rt[:u.Slots]
		for i := 0; i < min(oldSlots, u.Slots); i++ {
			if oldPlaces[i].Source != next.Places[i].Source {
				kickLocked(h.rt[i].fwd)
			}
		}
		h.rebuildLocked()
		h.eventf("number of signals is now %d", u.Slots)
	}
	if u.PortMode != oldMode {
		if next.perSignal() {
			h.eventf("ports: one port per signal, the port says which signal")
		} else {
			h.eventf("ports: one port for all signals, the stream ID says which signal")
		}
	}
	if restart {
		h.stopLocked()
		h.startLocked()
	}
	return nil
}

// setPublicIP saves the address of this site on the internet; it is only used for the links.
func (h *Hub) setPublicIP(v string) error {
	ip, err := cleanPublicIPv4(v)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ip != h.cfg.PublicIP {
		next := *h.cfg
		next.PublicIP = ip
		if err := h.commitLocked(next); err != nil {
			return err
		}
		h.eventf("public IP saved: %q", ip)

	}
	return nil
}

// portRange is what the firewall rule has to open: "21101" or "21101-21108".
func (h *Hub) portRange() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.portRange()
}

func (h *Hub) clearRejected() {
	h.mu.Lock()
	h.rejected = nil
	h.mu.Unlock()
}

func (h *Hub) connCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// localAddresses lists the IPv4 addresses of this PC for the links.
func localAddresses() []string {
	out := []string{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				out = append(out, ip.String())
			}
		}
	}
	return out
}
