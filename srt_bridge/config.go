package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Place is one of the fixed signals of the bridge. Its stream ID says which devices talk to
// it: a sender publishes under the bare ID, receivers fetch the original
// under the same bare ID, and "<id>*forward" delivers the input of the port
// that the matrix assigns (Source). A signal without an ID is free.
type Place struct {
	ID      string `json:"id"`      // stream ID; "" = the signal is free
	Name    string `json:"name"`    // label in the interface; "" shows "Line n"
	Source  int    `json:"source"`  // number of the signal whose input goes out on <id>*forward; 0 = nothing
	PassIn  string `json:"passIn"`  // passphrase for the bare ID (sender and receivers of the original); "" = none
	PassFwd string `json:"passFwd"` // passphrase for <id>*forward; "" = none
	Auto    bool   `json:"auto"`    // the ID was set by an incoming connection, not by hand
	RecvID  string `json:"recvId"`  // receiver key: own stream ID for the forward output; "" = <id>*forward
}

const maxSlots = 32

const (
	modeSingle = "single"
	modeSignal = "signal"
)

// perSignal: every signal has its own port, and the port alone says which
// signal a device talks to. Whoever delivers data there is its input; whoever
// only listens gets what the matrix assigns to that signal.
func (c *Config) perSignal() bool { return c.PortMode == modeSignal }

// ports lists the UDP ports the bridge listens on.
func (c *Config) ports() []int {
	if !c.perSignal() {
		return []int{c.SRTPort}
	}
	out := make([]int, c.Slots)
	for i := range out {
		out[i] = c.SRTPort + i
	}
	return out
}

// portRange is "21101" or "21101-21108", as the firewall and the texts need it.
func (c *Config) portRange() string {
	p := c.ports()
	if len(p) == 1 {
		return fmt.Sprint(p[0])
	}
	return fmt.Sprintf("%d-%d", p[0], p[len(p)-1])
}

// signalLabel names a signal in the log when it has its own port.
func (c *Config) signalLabel(i int) string {
	return fmt.Sprintf("signal %d (UDP %d)", i+1, c.SRTPort+i)
}

// checkPorts says why this first port cannot be used, or "".
func checkPorts(first, slots int, mode string) string {
	if first < 1 || first > 65535 {
		return "the port must be between 1 and 65535"
	}
	if mode == modeSignal && first+slots-1 > 65535 {
		return fmt.Sprintf("with %d signals the first port must not be above %d", slots, 65536-slots)
	}
	return ""
}

// Config is everything that is saved in config.json next to the program.
type Config struct {
	SRTPort   int      `json:"srtPort"`   // the one UDP port for sending and receiving; with a port per signal: the port of signal 1
	PortMode  string   `json:"portMode"`  // "single": one port, the stream ID says which signal; "signal": signal n has port SRTPort+n-1, no stream ID needed
	LatencyMs int      `json:"latencyMs"` // lowest latency the bridge accepts; a higher value from the device wins
	Suffixes  []string `json:"suffixes"`  // "<id>*<suffix>" is the forward output; the first one is shown in links
	AcceptAny bool     `json:"acceptAny"` // true: an unknown stream ID takes the next free signal; false: it is turned away
	PublicIP  string   `json:"publicIp"`  // address of this site on the internet, for the links; "" = not set
	WebAddr   string   `json:"webAddr"`   // address of the web interface
	Slots     int      `json:"slots"`     // number of signals; the matrix always shows all of them
	Places    []*Place `json:"places"`    // exactly Slots entries; signal n is Places[n-1]
	// NoOriginal: the sender key only sends. A device that only listens on it
	// gets nothing and is turned away; receivers use the forward output.
	NoOriginal bool `json:"noOriginal"`
	// Remote: the interface over HTTPS on TCP SRTPort, with a login (remote.go).
	Remote RemoteConfig `json:"remote"`
	// Bridge Output (output.go): where the program is ("" = next to the bridge),
	// whether the bridge starts it, and the address of its interface.
	OutputPath string `json:"outputPath"`
	OutputAuto bool   `json:"outputAuto"`
	OutputAddr string `json:"outputAddr"`
}

func defaultConfig() *Config {
	c := &Config{
		SRTPort:   21101,
		PortMode:  modeSingle,
		LatencyMs: 200,
		Suffixes:  []string{"forward", "talkback"},
		AcceptAny: true,
		WebAddr:   "127.0.0.1:8790",
		Slots:     8,
	}
	c.normalize()
	// A new installation starts with the straight matrix: every output gets
	// its own input, so a signal comes out before anybody has set anything.
	for i, p := range c.Places {
		p.Source = i + 1
	}
	return c
}

func loadConfig(path string) (*Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	cfg.Places = nil
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

func cleanSuffixes(in []string) []string {
	var suf []string
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "*")))
		if s != "" && !contains(suf, s) {
			suf = append(suf, s)
		}
	}
	if len(suf) == 0 {
		suf = []string{"forward", "talkback"}
	}
	return suf
}

// normalize repairs values that would stop the bridge from starting.
func (c *Config) normalize() {
	if c.SRTPort < 1 || c.SRTPort > 65535 {
		c.SRTPort = 21101
	}
	if c.LatencyMs < 20 || c.LatencyMs > 8000 {
		c.LatencyMs = 200
	}
	c.Suffixes = cleanSuffixes(c.Suffixes)
	c.PublicIP, _ = cleanPublicIPv4(c.PublicIP)
	if c.WebAddr == "" {
		c.WebAddr = "127.0.0.1:8790"
	}
	if c.OutputAddr == "" {
		c.OutputAddr = "127.0.0.1:8791"
	}
	if c.Slots < 1 {
		c.Slots = 8
	}
	if len(c.Places) > c.Slots {
		c.Slots = len(c.Places)
	}
	if c.Slots > maxSlots {
		c.Slots = maxSlots
		if len(c.Places) > maxSlots {
			c.Places = c.Places[:maxSlots]
		}
	}
	for len(c.Places) < c.Slots {
		c.Places = append(c.Places, &Place{})
	}
	if c.PortMode != modeSignal {
		c.PortMode = modeSingle
	}
	if checkPorts(c.SRTPort, c.Slots, c.PortMode) != "" {
		c.SRTPort = 21101
	}
	seen := map[string]bool{}
	for i, p := range c.Places {
		if p == nil {
			p = &Place{}
			c.Places[i] = p
		}
		if p.ID != "" && (seen[p.ID] || checkPlaceID(p.ID, c.Suffixes) != "") {
			p.ID = ""
		}
		seen[p.ID] = true
	}
	// Receiver keys after all stream IDs: none may be taken twice, and a free signal has none.
	for _, p := range c.Places {
		if p.RecvID != "" && (p.ID == "" || seen[p.RecvID] || checkPlaceID(p.RecvID, c.Suffixes) != "") {
			p.RecvID = ""
		}
		if p.RecvID != "" {
			seen[p.RecvID] = true
		}
	}
	for _, p := range c.Places {
		if p.Source < 0 || p.Source > c.Slots {
			p.Source = 0
		}
	}
}

// save writes the file next to itself first and renames it, so a crash in
// the middle never leaves half a configuration behind.
func (c *Config) save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// slot finds the signal with this stream ID; -1 if there is none.
func (c *Config) slot(id string) int {
	if id == "" {
		return -1
	}
	for i, p := range c.Places {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// recvSlot finds the signal with this receiver key; -1 if there is none.
func (c *Config) recvSlot(id string) int {
	if id == "" {
		return -1
	}
	for i, p := range c.Places {
		if p.RecvID == id {
			return i
		}
	}
	return -1
}

// forwardName is the stream ID receivers use for the forward output of p.
func (c *Config) forwardName(p *Place) string {
	if p.RecvID != "" {
		return p.RecvID
	}
	return p.ID + "*" + c.Suffixes[0]
}

// freeSlot is the first signal without an ID; -1 if all are taken.
func (c *Config) freeSlot() int {
	for i, p := range c.Places {
		if p.ID == "" {
			return i
		}
	}
	return -1
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// exeDir is where config.json and the log live: next to the program.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// checkPlaceID says why an ID cannot be used for a port, or "" if it can.
func checkPlaceID(id string, suffixes []string) string {
	if id == "" {
		return "the stream ID is empty"
	}
	if len(id) > 480 {
		return "the stream ID is too long"
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return "the stream ID contains control characters"
		}
	}
	if _, fwd := splitStreamID(id, suffixes); fwd {
		return "the stream ID must not end with a forward suffix"
	}
	return ""
}

// splitStreamID separates "<id>*forward" into the ID of the port and the flag
// "this is the forward output". The suffix is not case sensitive.
func splitStreamID(streamID string, suffixes []string) (id string, forward bool) {
	i := strings.LastIndexByte(streamID, '*')
	if i < 0 {
		return streamID, false
	}
	tail := strings.ToLower(streamID[i+1:])
	for _, s := range suffixes {
		if tail == s {
			return streamID[:i], true
		}
	}
	return streamID, false
}

// cleanPublicIPv4 accepts "" or an IPv4 address that is reachable from the internet.
func cleanPublicIPv4(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	ip := net.ParseIP(v).To4()
	if ip == nil {
		return "", errors.New("public IP: an IPv4 address like 85.1.2.3, or empty")
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return "", errors.New("public IP: this is not a public address (the router's address on the internet is needed)")
	}
	return ip.String(), nil
}

// checkPassphrase follows the SRT rule: empty, or 10 to 79 characters.
func checkPassphrase(p string) string {
	if p == "" {
		return ""
	}
	if len(p) < 10 || len(p) > 79 {
		return "a passphrase needs 10 to 79 characters"
	}
	return ""
}
