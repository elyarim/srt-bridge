package main

// The WHIP input: one more signal after the last one. srt-bridge-whip.exe
// takes the WHIP sender (OBS, a browser, FastVideo Bridge), repacks the picture
// into MPEG-TS, turns the sound into AAC and sends the stream as plain UDP to
// the bridge on 127.0.0.1, without a second SRT leg. Here it is handed out like
// the input of any other signal: to receivers of its stream ID and to every
// forward output the matrix points at it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	srt "github.com/datarhei/gosrt"
)

const whipExeName = "srt-bridge-whip.exe"

// whipStatusAddr is where srt-bridge-whip answers the bridge.
var whipStatusAddr = "127.0.0.1:8792"

// whipInput is the running state of the WHIP signal inside the hub.
type whipInput struct {
	rt      *placeRT
	conn    *net.UDPConn // 127.0.0.1:<port>, gets the TS stream; nil if it could not be opened
	probe   *tsProbe
	pkts    atomic.Uint64 // TS packets received
	bytes   atomic.Uint64
	last    atomic.Int64  // unix nanoseconds of the last datagram
	mbps    atomic.Uint64 // math.Float64bits of the rate over the last second
	wantKey atomic.Bool   // a receiver joined: ask the sender for a keyframe
	off     atomic.Bool   // switched off on the tile: nothing is passed on
}

func whipSlotLocked(h *Hub) int { return len(h.cfg.Places) + 1 }

// whipFirewallPort is the port the firewall must open for WHIP; 0 when WHIP is off.
func (h *Hub) whipFirewallPort() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.cfg.Whip.Enabled {
		return 0
	}
	return h.cfg.Whip.Port
}

func (h *Hub) whipPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.Whip.Path
}

// openWhipInput opens the local UDP port for the stream of srt-bridge-whip.
func (h *Hub) openWhipInput() {
	h.whip.rt = newPlaceRT()
	h.whip.probe = newTSProbe()
	h.whip.off.Store(h.cfg.Whip.Off)
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		log.Printf("WHIP: local input could not be opened: %v", err)
		return
	}
	c.SetReadBuffer(4 << 20)
	h.whip.conn = c
	go h.whipLoop()
	go h.whipRate()
}

func (h *Hub) whipTapPort() int {
	if h.whip.conn == nil {
		return 0
	}
	return h.whip.conn.LocalAddr().(*net.UDPAddr).Port
}

// whipLoop passes every datagram on, the same way serve does for an SRT input.
func (h *Hub) whipLoop() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := h.whip.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n == 0 || !addr.IP.IsLoopback() || h.whip.off.Load() {
			continue
		}
		now := time.Now().UnixNano()
		h.whip.last.Store(now)
		h.whip.bytes.Add(uint64(n))
		h.whip.pkts.Add(uint64(n / 188))
		pkt := buf[:n]
		h.whip.probe.feed(pkt)
		if t := h.whip.rt.tap.Load(); t != nil && now < t.until && h.tapConn != nil {
			h.tapConn.WriteToUDP(pkt, t.addr)
		}
		// SRT carries at most 1316 bytes per packet; srt-bridge-whip sends no more.
		for _, t := range *h.whip.rt.targets.Load() {
			t.write(pkt)
		}
	}
}

// whipRate works out the input rate once a second.
func (h *Hub) whipRate() {
	var last uint64
	for range time.Tick(time.Second) {
		b := h.whip.bytes.Load()
		h.whip.mbps.Store(uint64((b - last) * 8 / 1000)) // kbit/s
		last = b
	}
}

// whipLive: the stream came in within the last second.
func (h *Hub) whipLive() bool {
	return time.Since(time.Unix(0, h.whip.last.Load())) < time.Second
}

// handleWhipLocked takes a device on the stream ID of the WHIP signal: it can
// only receive the original. The input comes over WHIP; there is no forward output.
func (h *Hub) handleWhipLocked(req srt.ConnRequest, forward bool) {
	slot := whipSlotLocked(h)
	if forward {
		h.rejectSlotLocked(req, srt.REJ_PEER, "the WHIP signal has no forward output; use its stream ID to receive it", false, slot, "WHIP: no forward output")
		return
	}
	if h.cfg.Whip.Off {
		h.rejectSlotLocked(req, srt.REJ_PEER, "the WHIP signal is switched off", false, slot, "signal is switched off")
		return
	}
	if !h.passLocked(req, h.cfg.Whip.Pass, "WHIP: ", slot-1, "") {
		return
	}
	sc, err := req.Accept()
	if err != nil {
		h.eventf("%q from %s: handshake failed: %v", req.StreamId(), ipOf(req.RemoteAddr()), err)
		return
	}
	h.nextID++
	quiet := h.turnedAwayLocked(req.StreamId(), ipOf(req.RemoteAddr()))
	c := &conn{id: h.nextID, sc: sc, place: h.cfg.Whip.ID, stream: req.StreamId(), encrypted: h.cfg.Whip.Pass != "",
		addr: req.RemoteAddr().String(), since: time.Now(), rt: h.whip.rt, probe: newTSProbe(), quiet: quiet}
	c.info.Store(&ConnInfo{ID: c.id, Addr: c.addr, Since: c.since.Unix(), Encrypted: c.encrypted})
	h.conns[c] = struct{}{}
	c.role.Store(roleReceiver)
	h.whip.rt.subs[c] = struct{}{}
	h.rebuildLocked()
	if !quiet {
		h.eventf("connected: %q (WHIP signal) from %s", req.StreamId(), c.addr)
	}
	go h.serve(c)
}

// WhipState is the WHIP tile and its settings.
type WhipState struct {
	Slot      int          `json:"slot"`
	Enabled   bool         `json:"enabled"`
	Port      int          `json:"port"`
	Key       string       `json:"key"`
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Pass      string       `json:"pass"`
	Bitrate   int          `json:"aacBitrate"`
	Custom    string       `json:"custom"` // program path set by hand
	Off       bool         `json:"off"`    // switched off on the tile
	Live      bool         `json:"live"`   // the stream comes in
	Kbps      uint64       `json:"kbps"`
	Pkts      uint64       `json:"pkts"`
	Media     *MediaInfo   `json:"media,omitempty"`
	Receivers []*ConnInfo  `json:"receivers"`
	Forward   []int        `json:"forward"` // forward outputs (signal numbers) the matrix points at it
	Program   *WhipProgram `json:"program"` // filled in by the web server
}

func (h *Hub) whipStateLocked() *WhipState {
	w := h.cfg.Whip
	st := &WhipState{Slot: whipSlotLocked(h), Enabled: w.Enabled, Port: w.Port, Key: w.Key, ID: w.ID, Name: w.Name, Pass: w.Pass, Bitrate: w.Bitrate, Custom: w.Path, Off: w.Off,
		Live: h.whipLive(), Kbps: h.whip.mbps.Load(), Pkts: h.whip.pkts.Load(), Receivers: infos(h.whip.rt.subs), Forward: []int{}}
	if st.Live {
		st.Media = h.whip.probe.out.Load()
	}
	for i, p := range h.cfg.Places {
		if p.Source == whipSource {
			st.Forward = append(st.Forward, i+1)
		}
	}
	return st
}

// WhipUpdate carries only the fields that should change.
type WhipUpdate struct {
	Enabled *bool   `json:"enabled"`
	Port    *int    `json:"port"`
	Key     *string `json:"key"`
	ID      *string `json:"id"`
	Name    *string `json:"name"`
	Pass    *string `json:"pass"`
	Bitrate *int    `json:"aacBitrate"`
	Path    *string `json:"path"`
	Off     *bool   `json:"off"`
}

// updateWhip checks everything first, then changes. A change of port, key or
// bitrate restarts srt-bridge-whip (the sender reconnects); a new stream ID or
// passphrase closes the receivers of the original.
func (h *Hub) updateWhip(u WhipUpdate) (restart bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.cfg.Whip
	if u.Port != nil {
		p := *u.Port
		if p < 1024 || p > 65535 {
			return false, errors.New("the WHIP port must be between 1024 and 65535")
		}
		w.Port = p
	}
	if u.Key != nil {
		k := strings.TrimSpace(*u.Key)
		if k != "" && len(k) < 8 {
			return false, errors.New("the WHIP key needs at least 8 characters")
		}
		if len(k) > 200 || strings.ContainsAny(k, " \t\r\n\"") {
			return false, errors.New("the WHIP key must not contain spaces or quotes")
		}
		w.Key = k
	}
	if u.ID != nil {
		id := strings.TrimSpace(*u.ID)
		if id != "" {
			if why := checkPlaceID(id, h.cfg.Suffixes); why != "" {
				return false, errors.New(why)
			}
			if j := h.cfg.slot(id); j >= 0 {
				return false, fmt.Errorf("signal %d has this stream ID already", j+1)
			}
			if j := h.cfg.recvSlot(id); j >= 0 {
				return false, fmt.Errorf("signal %d uses this as its receiver key", j+1)
			}
		}
		w.ID = id
	}
	if u.Name != nil {
		name := []rune(*u.Name)
		if len(name) > 40 {
			name = name[:40]
		}
		w.Name = string(name)
	}
	if u.Pass != nil {
		if why := checkPassphrase(*u.Pass); why != "" {
			return false, errors.New(why)
		}
		w.Pass = *u.Pass
	}
	if u.Bitrate != nil {
		if *u.Bitrate < 64000 || *u.Bitrate > 320000 {
			return false, errors.New("the AAC bitrate must be between 64 and 320 kbit/s")
		}
		w.Bitrate = *u.Bitrate
	}
	if u.Path != nil {
		p := strings.Trim(strings.TrimSpace(*u.Path), `"`)
		if p != "" && !strings.EqualFold(filepath.Ext(p), ".exe") {
			return false, errors.New("the path must end with .exe")
		}
		w.Path = p
	}
	if u.Enabled != nil {
		w.Enabled = *u.Enabled
	}
	if u.Off != nil {
		w.Off = *u.Off
	}
	if w.Enabled && w.ID == "" {
		return false, errors.New("the WHIP signal needs a stream ID for its receivers")
	}
	if w.Enabled || u.Port != nil {
		if err := h.cfg.checkWhipPort(w.Port); err != nil {
			return false, err
		}
	}
	old := h.cfg.Whip
	if old == w {
		return false, nil
	}
	next := *h.cfg
	next.Whip = w
	if err := h.commitLocked(next); err != nil {
		return false, err
	}
	h.whip.off.Store(w.Off)
	if old.ID != w.ID || old.Pass != w.Pass || (old.Enabled && !w.Enabled) || (w.Off && !old.Off) {
		kickLocked(h.whip.rt.subs)
	}
	restart = old.Port != w.Port || old.Key != w.Key || old.Bitrate != w.Bitrate || old.Path != w.Path || old.Enabled != w.Enabled
	switch {
	case old.Off != w.Off:
		h.eventf("WHIP signal %s", map[bool]string{true: "switched off: nothing is passed on, receivers are turned away", false: "switched on again"}[w.Off])
	case old.Enabled != w.Enabled:
		h.eventf("WHIP input %s", map[bool]string{true: "switched on", false: "switched off"}[w.Enabled])
	case restart:
		h.eventf("WHIP input: settings changed, it starts again")
	default:
		h.eventf("WHIP input: settings changed")
	}
	return restart, nil
}

// ---- the program srt-bridge-whip ----

// WhipProgram is the state of srt-bridge-whip as the tile shows it.
type WhipProgram struct {
	Path    string          `json:"path"`
	Found   bool            `json:"found"`
	Running bool            `json:"running"`
	Error   string          `json:"error"`
	State   json.RawMessage `json:"state"` // its own /api/state
}

type Whip struct {
	location statusCache[programLocation]
	hub      *Hub
	client   *http.Client

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	lastStart time.Time
	err       string
	ok        bool
	state     json.RawMessage
}

func newWhip(h *Hub) *Whip {
	return &Whip{hub: h, client: &http.Client{Timeout: 1500 * time.Millisecond}}
}

func whipLocation(custom string) programLocation {
	return findProgram(custom, "whip", "srt_bridge_whip", whipExeName)
}

// loop keeps the program running while WHIP is on, reads its state, and asks
// for a keyframe when a receiver joined.
func (w *Whip) loop() {
	for {
		w.poll()
		for i := 0; i < 10; i++ { // keyframe requests faster than the state
			if w.hub.whip.wantKey.Swap(false) {
				w.post("/api/keyframe")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (w *Whip) poll() {
	var state json.RawMessage
	ok := false
	if resp, err := w.client.Get("http://" + whipStatusAddr + "/api/state"); err == nil {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		ok = resp.StatusCode == 200 && json.Valid(data)
		if ok {
			state = data
		}
	}
	w.hub.mu.Lock()
	on := w.hub.cfg.whipOn()
	w.hub.mu.Unlock()
	w.mu.Lock()
	w.ok, w.state = ok, state
	ours := w.cmd != nil
	again := on && !ours && time.Since(w.lastStart) > 5*time.Second
	stop := !on && ours
	w.mu.Unlock()
	switch {
	case stop:
		w.stop()
	case again && ok:
		// Another copy answers on the status port (started by hand, or left over).
		w.setErr("another srt-bridge-whip answers on " + whipStatusAddr + "; it is not the one of this bridge")
	case again:
		if err := w.start(); err != nil {
			w.setErr(err.Error())
		}
	}
}

func (w *Whip) post(path string) {
	resp, err := w.client.Post("http://"+whipStatusAddr+path, "application/json", strings.NewReader("{}"))
	if err == nil {
		resp.Body.Close()
	}
}

func (w *Whip) setErr(msg string) {
	w.mu.Lock()
	changed := msg != w.err
	w.err = msg
	w.mu.Unlock()
	if changed && msg != "" {
		w.hub.note("WHIP input: %s", msg)
	}
}

func (w *Whip) start() error {
	loc := whipLocation(w.hub.whipPath())
	exe, found := loc.path, loc.found
	if !found {
		if exe == "" {
			return errors.New("the program " + whipExeName + " was not found next to the bridge")
		}
		return errors.New("the program was not found: " + exe)
	}
	h := w.hub
	h.mu.Lock()
	cfg := h.cfg.Whip
	public := h.cfg.PublicIP
	h.mu.Unlock()
	port := h.whipTapPort()
	if port == 0 {
		return errors.New("the local input of the bridge is not open")
	}
	logPath := filepath.Join(filepath.Dir(logFile), "srt-bridge-whip.log")
	if logFile == "-" || logFile == "" {
		logPath = "-"
	} else if st, err := os.Stat(logPath); err == nil && st.Size() > 5<<20 {
		os.Remove(logPath + ".old")
		os.Rename(logPath, logPath+".old")
	}
	args := []string{"-stdin", "-listen", fmt.Sprintf(":%d", cfg.Port), "-tap", fmt.Sprintf("127.0.0.1:%d", port),
		"-status", whipStatusAddr, "-aac-bitrate", strconv.Itoa(cfg.Bitrate), "-log", logPath}
	if public != "" {
		args = append(args, "-public-ip", public)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = append(os.Environ(), "SRT_BRIDGE_WHIP_KEY="+cfg.Key)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr, _ := cmd.StderrPipe()
	w.mu.Lock()
	w.lastStart = time.Now()
	w.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start %s: %v", exe, err)
	}
	w.mu.Lock()
	w.cmd, w.stdin, w.err = cmd, stdin, ""
	w.mu.Unlock()
	log.Printf("WHIP input started: %s (pid %d), WHIP on TCP/UDP %d", exe, cmd.Process.Pid, cfg.Port)
	var lastLine atomic.Pointer[string]
	go func() { // a fatal start error (port in use) comes on stderr
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			lastLine.Store(&line)
			log.Printf("srt-bridge-whip: %s", line)
		}
	}()
	go func() {
		err := cmd.Wait()
		w.mu.Lock()
		if w.cmd == cmd {
			w.cmd, w.stdin = nil, nil
		}
		w.mu.Unlock()
		w.hub.mu.Lock()
		on := w.hub.cfg.whipOn()
		w.hub.mu.Unlock()
		if on {
			why := fmt.Sprint(err)
			if l := lastLine.Load(); l != nil {
				why = *l
			}
			w.setErr("the program ended (" + why + "); it is started again")
		} else {
			log.Print("WHIP input ended")
		}
	}()
	return nil
}

// stop ends the program: "stop" on stdin, by force after 3 s.
func (w *Whip) stop() {
	w.mu.Lock()
	cmd, stdin := w.cmd, w.stdin
	w.err = ""
	w.mu.Unlock()
	if cmd == nil {
		return
	}
	if stdin != nil {
		io.WriteString(stdin, "stop\n")
		stdin.Close()
	}
	for i := 0; i < 30; i++ {
		w.mu.Lock()
		gone := w.cmd != cmd
		w.mu.Unlock()
		if gone {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
}

// restart: new settings; the loop starts it again at once.
func (w *Whip) restart() {
	w.stop()
	w.mu.Lock()
	w.lastStart = time.Time{}
	w.mu.Unlock()
	w.poll()
}

func (w *Whip) disconnect() error {
	resp, err := w.client.Post("http://"+whipStatusAddr+"/api/disconnect", "application/json", strings.NewReader("{}"))
	if err != nil {
		return errors.New("the WHIP input does not answer")
	}
	resp.Body.Close()
	return nil
}

func (w *Whip) status() *WhipProgram {
	custom := w.hub.whipPath()
	loc := w.location.get(custom, func() programLocation { return whipLocation(custom) })
	w.mu.Lock()
	defer w.mu.Unlock()
	return &WhipProgram{Path: loc.path, Found: loc.found, Running: w.ok && w.cmd != nil, Error: w.err, State: w.state}
}
