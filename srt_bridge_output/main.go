// SRT Bridge Output: decodes signals of the SRT Bridge on the same PC and
// hands them out as OMT, NDI or SDI (Blackmagic). It asks the bridge which
// signals exist and whether they are live, asks it for a local copy of the
// chosen inputs, and runs one decoder process per signal.
package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.1.6"

//go:embed web
var webFiles embed.FS

type Config struct {
	BridgeURL string   `json:"bridgeUrl"` // interface of the SRT Bridge on this PC
	WebAddr   string   `json:"webAddr"`   // interface of this program
	TapBase   int      `json:"tapBase"`   // signal n arrives on UDP 127.0.0.1:TapBase+n-1
	Outs      []string `json:"outs"`      // per signal: "off", "omt", "ndi" or "sdi:<port key>"
	Names     []string `json:"names"`     // per signal: the name the output has on the network
}

// checkName says why this output name cannot be used for signal i, or "".
func (c *Config) checkName(i int, name string) string {
	if name == "" || len(name) > 60 {
		return "the output name needs 1 to 60 characters"
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '(' || r == ')' {
			return "the output name must not contain brackets or control characters"
		}
	}
	for j, other := range c.Names {
		if j != i && strings.EqualFold(other, name) {
			return fmt.Sprintf("signal %d already uses the name %q", j+1, other)
		}
	}
	return ""
}

const maxSignals = 32

func loadConfig(path string) *Config {
	c := &Config{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, c)
	}
	if c.BridgeURL == "" {
		c.BridgeURL = "http://127.0.0.1:8790"
	}
	if c.WebAddr == "" {
		c.WebAddr = "127.0.0.1:8791"
	}
	if c.TapBase < 1024 || c.TapBase > 65535-maxSignals {
		c.TapBase = 19201
	}
	for len(c.Outs) < maxSignals {
		c.Outs = append(c.Outs, "off")
	}
	for i, o := range c.Outs {
		if !validOut(o) {
			c.Outs[i] = "off"
		}
	}
	for len(c.Names) < maxSignals {
		c.Names = append(c.Names, "")
	}
	for i := range c.Names {
		c.Names[i] = strings.TrimSpace(c.Names[i])
		if c.checkName(i, c.Names[i]) != "" {
			c.Names[i] = fmt.Sprintf("SRT Bridge %d", i+1)
		}
	}
	return c
}

// validOut: off, omt, ndi, or sdi:KEY with the port key the decoder lists (letters, digits, '-').
func validOut(o string) bool {
	if o == "off" || o == "omt" || o == "ndi" {
		return true
	}
	key, ok := strings.CutPrefix(o, "sdi:")
	if !ok || key == "" || len(key) > 40 {
		return false
	}
	for _, r := range key {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// sdiPort is one Blackmagic port as `srt_bridge_decoder --list-sdi` reports it.
type sdiPort struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Playback bool   `json:"playback"`
	Busy     bool   `json:"busy"`
}

// refreshPorts asks the decoder which Blackmagic ports this PC has. Opening no port, it is quick and harmless.
func (a *App) refreshPorts() {
	cmd := exec.Command(filepath.Join(exeDir(), "srt_bridge_decoder.exe"), "--list-sdi")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	var list []sdiPort
	if err != nil || json.Unmarshal(bytes.TrimSpace(out), &list) != nil {
		return
	}
	a.mu.Lock()
	a.ports = list
	a.mu.Unlock()
}

// engine is one running decoder process.
type engine struct {
	out    string
	name   string
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	status json.RawMessage // last status line of the decoder
	gone   bool            // the process has ended
	asked  bool            // stop was requested
}

type App struct {
	mu       sync.Mutex
	cfg      *Config
	cfgPath  string
	bridgeOK bool
	bridgeV  string
	slots    int
	names    [maxSignals]string
	live     [maxSignals]bool
	engines  [maxSignals]*engine
	ports    []sdiPort
	client   *http.Client
	quitting bool // quit is stopping the decoders: start no new ones
}

type bridgeState struct {
	Version string `json:"version"`
	Slots   int    `json:"slots"`
	Places  []struct {
		Slot   int             `json:"slot"`
		ID     string          `json:"id"`
		Name   string          `json:"name"`
		Sender json.RawMessage `json:"sender"`
	} `json:"places"`
}

// poll runs once a second: read the bridge, renew the taps, look after the decoders.
func (a *App) poll() {
	var st bridgeState
	ok := false
	if resp, err := a.client.Get(a.cfg.BridgeURL + "/api/state"); err == nil {
		ok = resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&st) == nil
		resp.Body.Close()
	}
	a.mu.Lock()
	a.bridgeOK = ok
	if ok {
		a.bridgeV = st.Version
		a.slots = min(st.Slots, maxSignals)
		for i := range a.names {
			a.names[i], a.live[i] = "", false
		}
		for _, p := range st.Places {
			if p.Slot < 1 || p.Slot > maxSignals {
				continue
			}
			name := p.Name
			if name == "" {
				name = p.ID
			}
			a.names[p.Slot-1] = name
			a.live[p.Slot-1] = len(p.Sender) > 0 && string(p.Sender) != "null"
		}
	} else {
		a.live = [maxSignals]bool{}
	}
	taps := map[string]int{}
	for i := 0; i < maxSignals; i++ {
		want, name := "off", a.cfg.Names[i]
		if i < a.slots {
			want = a.cfg.Outs[i]
		}
		e := a.engines[i]
		if e != nil && e.gone {
			a.engines[i], e = nil, nil
		}
		if e != nil && (e.out != want || e.name != name) && !e.asked {
			e.asked = true
			go e.stop()
		}
		if e == nil && want != "off" && !a.quitting {
			a.engines[i] = a.start(i, want, name)
		}
		if want != "off" {
			taps[fmt.Sprint(i+1)] = a.cfg.TapBase + i
		}
	}
	a.mu.Unlock()
	if ok {
		body, _ := json.Marshal(map[string]any{"taps": taps})
		if resp, err := a.client.Post(a.cfg.BridgeURL+"/api/taps", "application/json", bytes.NewReader(body)); err == nil {
			resp.Body.Close()
		}
	}
}

// start launches the decoder for signal i (0-based). Call with a.mu held.
func (a *App) start(i int, out, name string) *engine {
	e := &engine{out: out, name: name}
	exe := filepath.Join(exeDir(), "srt_bridge_decoder.exe")
	args := []string{"--udp", fmt.Sprint(a.cfg.TapBase + i), "--name", name, "--out", out}
	if key, ok := strings.CutPrefix(out, "sdi:"); ok {
		args = []string{"--udp", fmt.Sprint(a.cfg.TapBase + i), "--out", "sdi", "--sdi", key}
	}
	e.cmd = exec.Command(exe, args...)
	e.cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	stdout, err1 := e.cmd.StdoutPipe()
	stdin, err2 := e.cmd.StdinPipe()
	if err := errors.Join(err1, err2, e.cmd.Start()); err != nil {
		log.Printf("signal %d: decoder did not start: %v", i+1, err)
		e.status, _ = json.Marshal(map[string]string{"state": "failed", "problem": "decoder did not start: " + err.Error()})
		e.gone = true
		// Keep the message visible for a moment; the next poll tries again.
		return e
	}
	e.stdin = stdin
	log.Printf("signal %d: decoder started, output %s as %q", i+1, out, name)
	go func() {
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			line := append([]byte(nil), lines.Bytes()...)
			if json.Valid(line) {
				a.mu.Lock()
				e.status = line
				a.mu.Unlock()
			}
		}
		e.cmd.Wait()
		time.Sleep(time.Second) // a decoder that fails at once is not restarted in a tight loop
		a.mu.Lock()
		e.gone = true
		a.mu.Unlock()
		log.Printf("signal %d: decoder ended", i+1)
	}()
	return e
}

// stop asks the decoder to end and makes sure it does.
func (e *engine) stop() {
	if e.stdin != nil {
		io.WriteString(e.stdin, "stop\n")
		e.stdin.Close()
	}
	done := make(chan struct{})
	go func() { e.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		e.cmd.Process.Kill()
	}
}

func (a *App) stopAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.engines {
		if e != nil && e.cmd != nil && e.cmd.Process != nil {
			e.cmd.Process.Kill()
		}
	}
}

// quit ends the program on request of the bridge: every decoder is asked to
// stop (an SDI port is released cleanly), then the program ends.
func (a *App) quit() {
	a.mu.Lock()
	a.quitting = true
	var list []*engine
	for _, e := range a.engines {
		if e != nil && e.cmd != nil && e.cmd.Process != nil && !e.gone {
			list = append(list, e)
		}
	}
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range list {
		wg.Add(1)
		go func() { defer wg.Done(); e.stop() }()
	}
	wg.Wait()
	log.Print("ended on request of the bridge")
	os.Exit(0)
}

func (a *App) running() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.engines {
		if e != nil && !e.gone {
			n++
		}
	}
	return n
}

type signalState struct {
	Slot   int             `json:"slot"`
	Name   string          `json:"name"`
	Live   bool            `json:"live"`
	Out     string          `json:"out"`
	OutName string          `json:"outName"`
	Engine  json.RawMessage `json:"engine"`
}

func (a *App) state() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.slots
	if n < 8 {
		n = 8
	}
	list := make([]signalState, n)
	for i := range list {
		list[i] = signalState{Slot: i + 1, Name: a.names[i], Live: a.live[i], Out: a.cfg.Outs[i], OutName: a.cfg.Names[i]}
		if e := a.engines[i]; e != nil {
			list[i].Engine = e.status
		}
	}
	// Ports for the choice: playback ports only; "busy" only when no signal of this program holds the port.
	type portState struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Busy bool   `json:"busy"`
	}
	ports := []portState{}
	// The cards for the header: model name and number of output ports, in the order found.
	type card struct {
		Model string `json:"model"`
		Ports int    `json:"ports"`
	}
	cards := []card{}
	for _, p := range a.ports {
		if !p.Playback {
			continue
		}
		model := p.Model
		if model == "" {
			model = "Blackmagic card"
		}
		if n := len(cards); n > 0 && cards[n-1].Model == model {
			cards[n-1].Ports++
		} else {
			cards = append(cards, card{model, 1})
		}
	}
	for _, p := range a.ports {
		if !p.Playback {
			continue
		}
		ours := false
		for i, o := range a.cfg.Outs {
			if o == "sdi:"+p.Key && a.engines[i] != nil {
				ours = true
			}
		}
		ports = append(ports, portState{p.Key, p.Name, p.Busy && !ours})
	}
	return map[string]any{"version": version, "bridgeOk": a.bridgeOK, "bridgeVersion": a.bridgeV, "bridgeUrl": a.cfg.BridgeURL, "signals": list, "ports": ports, "cards": cards}
}

func (a *App) setOut(slot int, out, name string) error {
	if slot < 1 || slot > maxSignals || !validOut(out) {
		return errors.New("unknown signal or output")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	name = strings.TrimSpace(name)
	if why := a.cfg.checkName(slot-1, name); why != "" {
		return errors.New(why)
	}
	if strings.HasPrefix(out, "sdi:") {
		for j, other := range a.cfg.Outs {
			if j != slot-1 && other == out {
				return fmt.Errorf("signal %d already uses this SDI port", j+1)
			}
		}
	}
	a.cfg.Outs[slot-1] = out
	a.cfg.Names[slot-1] = name
	data, _ := json.MarshalIndent(a.cfg, "", "  ")
	tmp := a.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.cfgPath)
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// dataPaths: an output-config.json next to the program (the development copy
// in bin) keeps config and log there. Installed with the bridge in Program
// Files, they go next to the bridge's own: %APPDATA%\SRTBridge and
// %LOCALAPPDATA%\SRTBridge.
func dataPaths() (cfg, logf string) {
	dir := exeDir()
	here := func() (string, string) {
		return filepath.Join(dir, "output-config.json"), filepath.Join(dir, "srt-bridge-output.log")
	}
	if _, err := os.Stat(filepath.Join(dir, "output-config.json")); err == nil {
		return here()
	}
	roam, local := os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")
	if roam == "" || local == "" {
		return here()
	}
	cdir, ldir := filepath.Join(roam, "SRTBridge"), filepath.Join(local, "SRTBridge")
	os.MkdirAll(cdir, 0o700)
	os.MkdirAll(ldir, 0o700)
	return filepath.Join(cdir, "output-config.json"), filepath.Join(ldir, "srt-bridge-output.log")
}

func main() {
	headless := flag.Bool("headless", false, "run without the window")
	defCfg, defLog := dataPaths()
	cfgPath := flag.String("config", defCfg, "configuration file")
	logPath := flag.String("log", defLog, "log file")
	flag.Parse()
	if st, err := os.Stat(*logPath); err == nil && st.Size() > 5<<20 {
		os.Remove(*logPath + ".old")
		os.Rename(*logPath, *logPath+".old")
	}
	if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}
	app := &App{cfg: loadConfig(*cfgPath), cfgPath: *cfgPath, client: &http.Client{Timeout: 800 * time.Millisecond}}
	log.Printf("SRT Bridge Output %s starting, bridge at %s", version, app.cfg.BridgeURL)

	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(app.state())
	})
	mux.HandleFunc("PUT /api/out", func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); (o != "" && o != "http://"+r.Host) || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var in struct {
			Slot int    `json:"slot"`
			Out  string `json:"out"`
			Name string `json:"name"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := app.setOut(in.Slot, in.Out, in.Name); err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		app.poll()
		json.NewEncoder(w).Encode(app.state())
	})
	// The bridge ends this program with it (its tab «Outputs», button Stop).
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); (o != "" && o != "http://"+r.Host) || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"ok":true}`))
		go app.quit()
	})
	ln, err := net.Listen("tcp", app.cfg.WebAddr)
	if err != nil {
		showError("SRT Bridge Output is already running, or " + app.cfg.WebAddr + " is in use.")
		os.Exit(1)
	}
	go http.Serve(ln, mux)
	go func() {
		for {
			app.poll()
			time.Sleep(time.Second)
		}
	}()
	go func() {
		for {
			app.refreshPorts()
			time.Sleep(10 * time.Second)
		}
	}()
	defer app.stopAll()
	if *headless {
		select {}
	}
	runWindow("http://"+app.cfg.WebAddr+"/", app)
}
