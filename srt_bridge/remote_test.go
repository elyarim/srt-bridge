package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword("correct horse battery", h) {
		t.Fatal("the right password was refused")
	}
	if checkPassword("correct horse batterY", h) || checkPassword("", h) || checkPassword("x", "garbage") {
		t.Fatal("a wrong password was accepted")
	}
	if checkNewPassword("short") == nil {
		t.Fatal("a password under 10 characters was accepted")
	}
}

func TestFirewallCommand(t *testing.T) {
	off := firewallCommand("netsh", "21101", 0)
	if strings.Contains(off, "protocol=TCP") || !strings.Contains(off, `delete rule name="SRT Bridge (remote)"`) {
		t.Fatalf("remote off must delete the TCP rule and not add it: %s", off)
	}
	on := firewallCommand("netsh", "21101-21108", 21101)
	if !strings.Contains(on, "protocol=UDP localport=21101-21108") || !strings.Contains(on, "protocol=TCP localport=21101 ") {
		t.Fatalf("remote on must open UDP and TCP: %s", on)
	}
	if !strings.HasPrefix(on, `/c "`) || !strings.HasSuffix(on, `"`) {
		t.Fatalf("the outer quotes for cmd /c are missing: %s", on)
	}
}

func freeTCPPort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// remoteSetup is a bridge without SRT, with remote access on a free TCP port.
func remoteSetup(t *testing.T) (*Hub, *Remote, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.SRTPort = freeTCPPort(t)
	h := newHub(cfg, filepath.Join(dir, "config.json"))
	t.Cleanup(h.stop) // a new latency opens the SRT port
	rm := newRemote(h, dir)
	web := newWebServer(h, rm, nil)
	rm.inner = web
	on, pw := true, "remote-password-1"
	if err := h.updateRemote(RemoteUpdate{Enabled: &on, Password: &pw}); err != nil {
		t.Fatal(err)
	}
	rm.sync()
	t.Cleanup(rm.stop)
	st := rm.status(httptest.NewRequest("GET", "/", nil))
	if !st.Listening || st.Error != "" {
		t.Fatalf("remote access does not listen: %+v", st)
	}
	return h, rm, web, fmt.Sprintf("https://127.0.0.1:%d", cfg.SRTPort)
}

func client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func call(t *testing.T, c *http.Client, method, url string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestRemoteLogin(t *testing.T) {
	h, _, web, base := remoteSetup(t)
	c := client()

	if code, _ := call(t, c, "GET", base+"/api/state", nil); code != http.StatusUnauthorized {
		t.Fatalf("state without login: %d, want 401", code)
	}
	res, err := c.Get(base + "/")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("the page itself must load without login: %v %v", res, err)
	}
	res.Body.Close()
	if code, _ := call(t, c, "POST", base+"/api/login", map[string]string{"password": "wrong-password"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := call(t, c, "POST", base+"/api/login", map[string]string{"password": "remote-password-1"}); code != 200 {
		t.Fatalf("right password: %d", code)
	}
	code, st := call(t, c, "GET", base+"/api/state", nil)
	if code != 200 {
		t.Fatalf("state after login: %d", code)
	}
	r := st["remote"].(map[string]any)
	if r["viewer"] != "remote" || r["apiToken"] != nil {
		t.Fatalf("remote viewer must not see the API token: %v", r)
	}

	// What stays on the bridge PC.
	if code, _ := call(t, c, "POST", base+"/api/firewall", map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("firewall over remote: %d, want 403", code)
	}
	on := false
	if code, _ := call(t, c, "PUT", base+"/api/remote", RemoteUpdate{Enabled: &on}); code != http.StatusForbidden {
		t.Fatalf("remote settings over remote: %d, want 403", code)
	}
	port, _ := h.srtPortAndMode()
	set := SettingsUpdate{SRTPort: port + 1, LatencyMs: 200, Slots: 8, Suffixes: []string{"forward"}, AcceptAny: true}
	if code, _ := call(t, c, "PUT", base+"/api/settings", set); code != http.StatusBadRequest {
		t.Fatalf("new port over remote: %d, want 400", code)
	}
	set.SRTPort, set.LatencyMs = port, 300
	if code, _ := call(t, c, "PUT", base+"/api/settings", set); code != 200 {
		t.Fatalf("latency over remote: %d, want 200", code)
	}

	// The bridge PC sees the token; a program uses it.
	rec := httptest.NewRecorder()
	web.ServeHTTP(rec, httptest.NewRequest("GET", "/api/state", nil))
	var local State
	json.Unmarshal(rec.Body.Bytes(), &local)
	if local.Remote == nil || local.Remote.Viewer != "local" || local.Remote.APIToken == "" {
		t.Fatalf("the bridge PC must see the token: %+v", local.Remote)
	}
	prog := client()
	if code, _ := call(t, prog, "GET", base+"/api/state", nil, "Authorization", "Bearer "+local.Remote.APIToken); code != 200 {
		t.Fatalf("API token: %d", code)
	}
	if code, _ := call(t, prog, "GET", base+"/api/state", nil, "Authorization", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong API token: %d", code)
	}

	// A new password logs everybody out.
	pw := "another-password-2"
	rec = httptest.NewRecorder()
	b, _ := json.Marshal(RemoteUpdate{Password: &pw})
	req := httptest.NewRequest("PUT", "/api/remote", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	web.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("new password on the bridge PC: %d %s", rec.Code, rec.Body)
	}
	if code, _ := call(t, c, "GET", base+"/api/state", nil); code != http.StatusUnauthorized {
		t.Fatalf("session after a new password: %d, want 401", code)
	}
}

func TestRemoteLock(t *testing.T) {
	_, _, _, base := remoteSetup(t)
	c := client()
	for i := 0; i < failsBeforeLock; i++ {
		call(t, c, "POST", base+"/api/login", map[string]string{"password": "wrong-password"})
	}
	// Now even the right password waits.
	if code, _ := call(t, c, "POST", base+"/api/login", map[string]string{"password": "remote-password-1"}); code != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong passwords: %d, want 429", failsBeforeLock, code)
	}
}

func TestRemoteOff(t *testing.T) {
	h, rm, _, base := remoteSetup(t)
	off := false
	if err := h.updateRemote(RemoteUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	rm.sync()
	if _, err := client().Get(base + "/"); err == nil {
		t.Fatal("remote access switched off still answers")
	}
	// Switching on without a password is refused.
	h2 := newHub(defaultConfig(), filepath.Join(t.TempDir(), "config.json"))
	on := true
	if err := h2.updateRemote(RemoteUpdate{Enabled: &on}); err == nil {
		t.Fatal("remote access without a password was switched on")
	}
}
