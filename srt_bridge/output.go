package main

// SRT Bridge Output (the decoder for OMT, NDI and SDI) belongs to the bridge
// since 0.4.0: the tab «Outputs» starts and stops it and holds its settings.
// The program keeps its own interface on 127.0.0.1:8791; the bridge talks to
// it there and hands its state and its settings through, so they work over
// remote access too (with the login of the bridge). Started by the bridge,
// it runs without a window; started by hand it keeps its window.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const outputExeName = "srt-bridge-output.exe"

// OutputStatus is what the tab «Outputs» shows.
type OutputStatus struct {
	Path    string          `json:"path"`    // the program that is started; "" = not found
	Custom  string          `json:"custom"`  // the path set by hand; "" = looked for next to the bridge
	Found   bool            `json:"found"`   // the program is there
	Running bool            `json:"running"` // its interface answers
	Ours    bool            `json:"ours"`    // the bridge started it
	Want    bool            `json:"want"`    // the bridge keeps it running (started here, or «with the bridge»)
	Auto    bool            `json:"auto"`    // start it with the bridge
	Error   string          `json:"error"`
	State   json.RawMessage `json:"state"` // the state of the program itself, as its own interface shows it
}

type Output struct {
	location statusCache[programLocation]
	hub      *Hub
	client   *http.Client

	mu        sync.Mutex
	cmd       *exec.Cmd // started by the bridge and not ended yet
	want      bool      // keep it running: start it again when it ends
	lastStart time.Time
	err       string
	ok        bool
	state     json.RawMessage
}

func newOutput(h *Hub) *Output {
	o := &Output{hub: h, client: &http.Client{Timeout: 1500 * time.Millisecond}}
	h.mu.Lock()
	o.want = h.cfg.OutputAuto
	h.mu.Unlock()
	return o
}

func (o *Output) conf() (path, addr string, auto bool) {
	o.hub.mu.Lock()
	defer o.hub.mu.Unlock()
	return o.hub.cfg.OutputPath, o.hub.cfg.OutputAddr, o.hub.cfg.OutputAuto
}

// outputLocation finds the program: the path set by hand, or next to the bridge in
// "output" (the installation), or the development copy next to this project.
func outputLocation(custom string) programLocation {
	return findProgram(custom, "output", "srt_bridge_output", outputExeName)
}

func (o *Output) base() string {
	_, addr, _ := o.conf()
	return "http://" + addr
}

// loop reads the program once a second and starts it again when it should run.
func (o *Output) loop() {
	for {
		o.poll()
		time.Sleep(time.Second)
	}
}

func (o *Output) poll() {
	var state json.RawMessage
	ok := false
	if resp, err := o.client.Get(o.base() + "/api/state"); err == nil {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		ok = resp.StatusCode == 200 && json.Valid(data)
		if ok {
			state = data
		}
	}
	o.mu.Lock()
	o.ok, o.state = ok, state
	again := o.want && !ok && o.cmd == nil && time.Since(o.lastStart) > 10*time.Second
	o.mu.Unlock()
	if again {
		if err := o.start(); err != nil {
			o.setErr(err.Error())
		}
	}
}

func (o *Output) setErr(msg string) {
	o.mu.Lock()
	changed := msg != o.err
	o.err = msg
	o.mu.Unlock()
	if changed && msg != "" {
		o.hub.note("Bridge Output: %s", msg)
	}
}

// start runs the program without a window. If it runs already (also when
// started by hand), nothing happens.
func (o *Output) start() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.want = true
	if o.ok || o.cmd != nil {
		return nil
	}
	loc := outputLocation(o.hub.outputPath())
	exe, found := loc.path, loc.found
	if !found {
		if exe == "" {
			return errors.New("the program " + outputExeName + " was not found next to the bridge")
		}
		return errors.New("the program was not found: " + exe)
	}
	cmd := exec.Command(exe, "-headless")
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	o.lastStart = time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start %s: %v", exe, err)
	}
	o.cmd, o.err = cmd, ""
	log.Printf("Bridge Output started: %s (pid %d)", exe, cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		o.mu.Lock()
		if o.cmd == cmd {
			o.cmd = nil
		}
		want := o.want
		o.mu.Unlock()
		if want {
			o.hub.note("Bridge Output ended unexpectedly (%v); it is started again", err)
		} else {
			log.Print("Bridge Output ended")
		}
	}()
	return nil
}

// stop ends the program, also one started by hand: it is asked to quit, so
// its decoders end cleanly; one started here is ended by force after 6 s.
func (o *Output) stop() error {
	o.mu.Lock()
	o.want, o.err = false, ""
	cmd := o.cmd
	o.mu.Unlock()
	req, _ := http.NewRequest("POST", o.base()+"/api/quit", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if cmd == nil {
		if err != nil {
			return errors.New("Bridge Output does not answer")
		}
		return nil
	}
	for i := 0; i < 60; i++ {
		o.mu.Lock()
		gone := o.cmd != cmd
		o.mu.Unlock()
		if gone {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	return nil
}

// shutdown ends a program the bridge started, when the bridge ends.
func (o *Output) shutdown() {
	o.mu.Lock()
	ours := o.cmd != nil
	o.mu.Unlock()
	if ours {
		o.stop()
	}
}

func (o *Output) status() *OutputStatus {
	custom, _, auto := o.conf()
	loc := o.location.get(custom, func() programLocation { return outputLocation(custom) })
	o.mu.Lock()
	defer o.mu.Unlock()
	return &OutputStatus{Path: loc.path, Custom: custom, Found: loc.found, Running: o.ok, Ours: o.cmd != nil, Want: o.want, Auto: auto, Error: o.err, State: o.state}
}

// setOut hands a setting of one signal to the program: output and output name.
func (o *Output) setOut(body []byte) error {
	req, _ := http.NewRequest("PUT", o.base()+"/api/out", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return errors.New("Bridge Output does not answer: start it first")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("Bridge Output: %s", strings.TrimSpace(string(data)))
	}
	if json.Valid(data) {
		o.mu.Lock()
		o.state = data
		o.mu.Unlock()
	}
	return nil
}

// OutputSettings: start with the bridge, and where the program is.
type OutputSettings struct {
	Auto bool   `json:"auto"`
	Path string `json:"path"` // "" = look next to the bridge
}

func (h *Hub) updateOutputSettings(u OutputSettings) error {
	u.Path = strings.Trim(strings.TrimSpace(u.Path), `"`)
	if u.Path != "" && !strings.EqualFold(filepath.Ext(u.Path), ".exe") {
		return errors.New("the path must end with .exe")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	oldPath, oldAuto := h.cfg.OutputPath, h.cfg.OutputAuto
	next := *h.cfg
	next.OutputAuto, next.OutputPath = u.Auto, u.Path
	if err := h.commitLocked(next); err != nil {
		return err
	}
	if u.Auto != oldAuto {
		h.eventf("Bridge Output: start with the bridge %s", map[bool]string{true: "on", false: "off"}[u.Auto])
	}
	if u.Path != oldPath {
		h.eventf("Bridge Output: program path %q", u.Path)
	}
	return nil
}

func (h *Hub) outputPath() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.OutputPath
}
