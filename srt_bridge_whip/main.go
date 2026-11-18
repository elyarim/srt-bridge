// srt-bridge-whip: the WHIP input of SRT Bridge.
//
// A WHIP sender (OBS, a browser, FastVideo Bridge) publishes here over HTTP
// (TCP) and sends picture and sound over WebRTC (UDP, the same port number).
// The picture is only repacked (H.264/H.265 into MPEG-TS), the sound goes from
// Opus to AAC. The TS stream goes as plain UDP to the bridge on 127.0.0.1,
// without a second SRT leg. The bridge starts this program and reads its state
// on 127.0.0.1 (-status).
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/webrtc/v4"
)

const version = "0.1.0"

type server struct {
	key      string
	tap      *net.UDPAddr
	tapConn  *net.UDPConn
	api      *webrtc.API
	bitrate  int
	listen   string
	udpPort  int
	publicIP string

	mu       sync.Mutex
	cur      *session
	conv     *aacConv
	convErr  string
	lastErr  string
	rejected int
	fails    map[string][]time.Time // wrong keys per address
	sessions uint64
	tsOut    uint64
}

func main() {
	listen := flag.String("listen", ":21102", "HTTP address for WHIP (TCP)")
	udpPort := flag.Int("udp", 0, "UDP port for WebRTC; 0 = the port of -listen")
	tap := flag.String("tap", "127.0.0.1:0", "where the TS stream goes (the bridge, plain UDP)")
	status := flag.String("status", "127.0.0.1:8792", "status interface for the bridge")
	publicIP := flag.String("public-ip", "", "public IPv4 address, announced to senders on the internet")
	bitrate := flag.Int("aac-bitrate", 192000, "AAC bitrate in bit/s")
	logPath := flag.String("log", "-", "log file; \"-\" writes to the console")
	watchStdin := flag.Bool("stdin", false, "end when stdin closes or says \"stop\" (set by the bridge)")
	flag.Parse()

	if *logPath != "-" {
		if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			log.SetOutput(f)
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	key := os.Getenv("SRT_BRIDGE_WHIP_KEY")
	tapAddr, err := net.ResolveUDPAddr("udp4", *tap)
	if err != nil || tapAddr.Port == 0 {
		log.Fatalf("-tap: a UDP address with a port is needed (%v)", err)
	}
	port := *udpPort
	if port == 0 {
		_, p, _ := net.SplitHostPort(*listen)
		fmt.Sscan(p, &port)
	}
	s := &server{key: key, tap: tapAddr, bitrate: *bitrate, listen: *listen, udpPort: port, publicIP: *publicIP, fails: map[string][]time.Time{}}
	s.tapConn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		log.Fatalf("UDP for the bridge: %v", err)
	}
	if err := s.setupAPI(port); err != nil {
		log.Fatalf("WebRTC: %v", err)
	}
	s.converter()
	log.Printf("SRT Bridge WHIP %s: WHIP on TCP %s, WebRTC on UDP %d, stream to %s, public IP %q", version, *listen, port, tapAddr, *publicIP)
	if key == "" {
		log.Print("no key set: every sender is turned away")
	}

	if *watchStdin {
		go func() { // stdin closed or "stop": the bridge ends us
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				if strings.TrimSpace(sc.Text()) == "stop" {
					break
				}
			}
			s.shutdown()
		}()
	}
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.state()) })
		mux.HandleFunc("POST /api/keyframe", func(w http.ResponseWriter, r *http.Request) {
			if c := s.current(); c != nil {
				c.askKey("asked by the bridge")
			}
			writeJSON(w, map[string]bool{"ok": true})
		})
		mux.HandleFunc("POST /api/disconnect", func(w http.ResponseWriter, r *http.Request) {
			if c := s.current(); c != nil {
				c.close("disconnected by hand")
				s.drop(c)
			}
			writeJSON(w, map[string]bool{"ok": true})
		})
		mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]bool{"ok": true})
			go s.shutdown()
		})
		if err := http.ListenAndServe(*status, mux); err != nil {
			log.Fatalf("status interface %s: %v", *status, err)
		}
	}()
	srv := &http.Server{Addr: *listen, Handler: http.HandlerFunc(s.whip), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("WHIP on TCP %s: %v", *listen, err)
	}
}

func (s *server) shutdown() {
	if c := s.current(); c != nil {
		c.close("program ends")
	}
	s.mu.Lock()
	if s.conv != nil {
		s.conv.close()
	}
	s.mu.Unlock()
	log.Print("ended")
	os.Exit(0)
}

func (s *server) setupAPI(port int) error {
	me := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "transport-cc"}}
	pt := webrtc.PayloadType(96)
	for _, prof := range []string{"42e01f", "42001f", "4d001f", "64001f"} {
		if err := me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + prof, RTCPFeedback: fb}, PayloadType: pt}, webrtc.RTPCodecTypeVideo); err != nil {
			return err
		}
		pt++
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000, RTCPFeedback: fb}, PayloadType: pt}, webrtc.RTPCodecTypeVideo); err != nil {
		return err
	}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=1"}, PayloadType: 111}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	reg := &interceptor.Registry{}
	// Retransmissions are asked for quickly: a hole waits only holeWait for them.
	gen, err := nack.NewGeneratorInterceptor(nack.GeneratorInterval(15 * time.Millisecond))
	if err != nil {
		return err
	}
	me.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	me.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	reg.Add(gen)
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return err
	}
	if err := webrtc.ConfigureTWCCSender(me, reg); err != nil {
		return err
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return fmt.Errorf("UDP %d: %v", port, err)
	}
	se := webrtc.SettingEngine{}
	se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, udp))
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true)
	se.SetICETimeouts(4*time.Second, 10*time.Second, 1*time.Second)
	if s.publicIP != "" {
		se.SetNAT1To1IPs([]string{s.publicIP}, webrtc.ICECandidateTypeSrflx)
	}
	s.api = webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	return nil
}

// converter returns the sound converter, started again if it ended.
func (s *server) converter() *aacConv {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conv != nil && s.conv.alive() {
		return s.conv
	}
	if s.conv != nil {
		log.Print("sound converter ended unexpectedly; started again")
	}
	c, err := startAAC(s.bitrate, func(pts int64, adts []byte) {
		if cur := s.current(); cur != nil {
			cur.aacFrame(pts, adts)
		}
	})
	if err != nil {
		if s.convErr != err.Error() {
			log.Printf("sound: %v (picture only)", err)
		}
		s.convErr, s.conv = err.Error(), nil
		return nil
	}
	s.conv, s.convErr = c, ""
	return c
}

func (s *server) current() *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *server) sendTS(b []byte) {
	s.tapConn.WriteToUDP(b, s.tap)
	s.mu.Lock()
	s.tsOut += uint64(len(b))
	s.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttled: 10 wrong keys in a minute lock an address out for a minute (loopback never).
func (s *server) throttled(ip string, fail bool) bool {
	if ip == "127.0.0.1" || ip == "::1" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	list := s.fails[ip][:0]
	for _, t := range s.fails[ip] {
		if now.Sub(t) < time.Minute {
			list = append(list, t)
		}
	}
	if fail {
		list = append(list, now)
		s.rejected++
	}
	s.fails[ip] = list
	return len(list) >= 10
}

func (s *server) whip(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, DELETE, PATCH, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match")
	h.Set("Access-Control-Expose-Headers", "Location, ETag, Link")
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodOptions:
		h.Set("Accept-Post", "application/sdp")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && (path == "" || path == "/whip"):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "SRT Bridge WHIP input "+version+": POST an SDP offer to /whip with \"Authorization: Bearer <key>\".\n")
	case r.Method == http.MethodPost && (path == "/whip" || strings.HasPrefix(path, "/whip/")):
		s.publish(w, r)
	case strings.HasPrefix(path, "/whip/session/"):
		id := strings.TrimPrefix(path, "/whip/session/")
		c := s.current()
		if c == nil || c.id != id {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		if !s.authorized(r) {
			http.Error(w, "wrong key", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			c.close("the sender said goodbye")
			s.drop(c)
			w.WriteHeader(http.StatusOK)
		case http.MethodPatch:
			// Trickle ICE is not needed: the answer has all candidates.
			http.Error(w, "trickle ICE is not supported", http.StatusMethodNotAllowed)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *server) authorized(r *http.Request) bool {
	if s.key == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	want := "Bearer " + s.key
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *server) setErr(msg string) {
	s.mu.Lock()
	s.lastErr = msg
	s.mu.Unlock()
	log.Print(msg)
}

func (s *server) publish(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.throttled(ip, false) {
		http.Error(w, "too many wrong keys, try again in a minute", http.StatusTooManyRequests)
		return
	}
	if !s.authorized(r) {
		if s.throttled(ip, true) {
			s.setErr("sender " + ip + " locked out for a minute: too many wrong keys")
		} else {
			s.setErr("sender " + ip + " turned away: wrong or missing key")
		}
		http.Error(w, "wrong or missing key", http.StatusUnauthorized)
		return
	}
	offer, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || !strings.Contains(string(offer), "v=0") {
		http.Error(w, "an SDP offer is needed", http.StatusBadRequest)
		return
	}
	low := strings.ToLower(string(offer))
	if !strings.Contains(low, "h264/90000") && !strings.Contains(low, "h265/90000") {
		s.setErr("sender " + ip + " turned away: it offers neither H.264 nor H.265")
		http.Error(w, "the video must be H.264 or H.265", http.StatusNotAcceptable)
		return
	}
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var idb [12]byte
	rand.Read(idb[:])
	sess := &session{id: hex.EncodeToString(idb[:]), srv: s, pc: pc, addr: ip, since: time.Now(), hasAudio: strings.Contains(low, "m=audio") && s.converter() != nil}
	pc.OnTrack(sess.onTrack)
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		switch st {
		case webrtc.ICEConnectionStateConnected:
			sess.logf("connected")
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
			sess.close("connection " + st.String())
			s.drop(sess)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		pc.Close()
		s.setErr("sender " + ip + ": offer not accepted: " + err.Error())
		http.Error(w, "offer not accepted: "+err.Error(), http.StatusBadRequest)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err == nil {
		gathered := webrtc.GatheringCompletePromise(pc)
		if err = pc.SetLocalDescription(answer); err == nil {
			select {
			case <-gathered:
			case <-time.After(3 * time.Second):
			}
		}
	}
	if err != nil {
		pc.Close()
		s.setErr("sender " + ip + ": no answer: " + err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// One sender at a time: the new one takes over (a sender that reconnects after a break).
	s.mu.Lock()
	old := s.cur
	s.cur = sess
	s.sessions++
	s.lastErr = ""
	conv := s.conv
	s.mu.Unlock()
	if old != nil {
		old.close("a new sender took over")
	}
	if conv != nil {
		conv.reset()
	}
	sess.logf("publishing (session %s)", sess.id)
	h := w.Header()
	h.Set("Content-Type", "application/sdp")
	h.Set("Location", "/whip/session/"+sess.id)
	h.Set("ETag", `"`+sess.id+`"`)
	w.WriteHeader(http.StatusCreated)
	io.WriteString(w, pc.LocalDescription().SDP)
}

func (s *server) drop(c *session) {
	s.mu.Lock()
	if s.cur == c {
		s.cur = nil
	}
	s.mu.Unlock()
}

// State is what the bridge shows on the WHIP tile.
type State struct {
	Version  string  `json:"version"`
	Listen   string  `json:"listen"`
	UDPPort  int     `json:"udpPort"`
	PublicIP string  `json:"publicIp"`
	HasKey   bool    `json:"hasKey"`
	Live     bool    `json:"live"`
	Client   string  `json:"client"`
	Since    int64   `json:"since"`
	Video    string  `json:"video"`
	Audio    string  `json:"audio"`
	InBytes  uint64  `json:"inBytes"`
	OutBytes uint64  `json:"outBytes"`
	Pictures uint64  `json:"pictures"`
	Lost     uint64  `json:"lost"`    // video packets not recovered
	Dropped  uint64  `json:"dropped"` // pictures not passed on (broken, or waiting for a keyframe)
	KeyReqs  uint64  `json:"keyRequests"`
	AudioLost uint64 `json:"audioLost"`
	DelayMs  int64   `json:"delayMs"` // PCR to PTS: the time the sound needs
	AudioLagMs int64 `json:"audioLagMs"`
	Rejected int     `json:"rejected"`
	Sessions uint64  `json:"sessions"`
	Error    string  `json:"error"`
	SoundErr string  `json:"soundError"`
}

func (s *server) state() *State {
	s.mu.Lock()
	st := &State{Version: version, Listen: s.listen, UDPPort: s.udpPort, PublicIP: s.publicIP, HasKey: s.key != "", Rejected: s.rejected,
		Sessions: s.sessions, Error: s.lastErr, SoundErr: s.convErr, OutBytes: s.tsOut}
	c := s.cur
	bitrate := s.bitrate
	s.mu.Unlock()
	if c != nil && !c.closed.Load() {
		st.Live, st.Client, st.Since = true, c.addr, c.since.Unix()
		if v := c.videoInfo.Load(); v != nil {
			st.Video = *v
		}
		if c.hasAudio {
			st.Audio = fmt.Sprintf("Opus → AAC %d kbit/s", bitrate/1000)
		}
		st.InBytes, st.Pictures, st.Lost, st.Dropped = c.bytesIn.Load(), c.pictures.Load(), c.lostV.Load(), c.dropped.Load()
		st.KeyReqs, st.AudioLost, st.AudioLagMs = c.keyReqs.Load(), c.audioLost.Load(), c.audioLagMs.Load()
		c.mu.Lock()
		st.DelayMs = c.delay / 90
		c.mu.Unlock()
	}
	return st
}
