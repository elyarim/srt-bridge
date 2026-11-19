package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalWebBinding(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		ln, err := listenWeb(addr)
		if err != nil {
			t.Fatal(err)
		}
		if !ln.Addr().(*net.TCPAddr).IP.IsLoopback() {
			t.Errorf("bound to %s", ln.Addr())
		}
		ln.Close()
	}
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0"} {
		if ln, err := listenWeb(addr); err == nil {
			ln.Close()
			t.Errorf("accepted %s", addr)
		} else if !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("%s: %v", addr, err)
		}
	}
}

func TestWhipConflictPreservesListener(t *testing.T) {
	h := testHub(t, false)
	port := h.cfg.SRTPort + 1
	if _, err := h.updateWhip(WhipUpdate{Enabled: ptr(true), ID: ptr("whip"), Port: &port}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ln := h.lns[0]
	err = h.updateSettings(SettingsUpdate{SRTPort: port - 1, PortMode: modeSignal, LatencyMs: 120, Slots: 2})
	if err == nil || !strings.Contains(err.Error(), "signal 2") {
		t.Fatalf("expected signal conflict, got %v", err)
	}
	after, err := os.ReadFile(h.cfgPath)
	if err != nil || string(before) != string(after) || ln != h.lns[0] || h.cfg.perSignal() {
		t.Fatal("conflict changed saved configuration or live listener")
	}
	if _, err := h.updateWhip(WhipUpdate{Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := h.updateSettings(SettingsUpdate{SRTPort: port - 1, PortMode: modeSignal, LatencyMs: 120, Slots: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.updateWhip(WhipUpdate{Enabled: ptr(true)}); err == nil {
		t.Fatal("enabled WHIP on a busy SRT port")
	}
}

func TestConcurrentAddPlace(t *testing.T) {
	h := testHub(t, false)
	start := make(chan struct{})
	done := make(chan error, h.cfg.Slots)
	n := h.cfg.Slots
	for i := range n {
		go func() { <-start; done <- h.addPlace(fmt.Sprintf("signal%d", i)) }()
	}
	close(start)
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := loadConfig(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if cfg.slot(fmt.Sprintf("signal%d", i)) < 0 {
			t.Fatalf("lost assignment %d", i)
		}
	}
}

func TestSaveFailurePreservesLiveRoute(t *testing.T) {
	h := testHub(t, true)
	cam, err := dial("cam", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	stop := make(chan struct{})
	defer close(stop)
	go feed(cam, 'A', stop)
	receiver, err := dial("cam*forward", "")
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	expect(t, receiver, 'A', "initial route")
	h.mu.Lock()
	h.cfgPath = filepath.Join(t.TempDir(), "missing", "config.json")
	h.mu.Unlock()
	web := newWebServer(h, nil, nil, nil)
	for _, change := range []struct{ path, body string }{
		{"/api/places", `{"slot":1,"source":0}`},
		{"/api/settings", `{"srtPort":19001,"latencyMs":700,"slots":2}`},
		{"/api/original", `{"allow":false}`},
		{"/api/output/settings", `{"auto":true}`},
		{"/api/whip", `{"enabled":true,"id":"whip"}`},
	} {
		req := httptest.NewRequest(http.MethodPut, change.path, strings.NewReader(change.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		web.ServeHTTP(w, req)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "could not save") {
			t.Fatalf("%s: %d %s", change.path, w.Code, w.Body.String())
		}
	}
	if h.cfg.Places[0].Source != 1 || h.cfg.Slots != 8 || h.cfg.NoOriginal || h.cfg.OutputAuto || h.cfg.Whip.Enabled {
		t.Fatal("failed update changed configuration")
	}
	expect(t, receiver, 'A', "route after write failure")
}

func TestStatusCacheRefresh(t *testing.T) {
	var cache statusCache[int]
	calls := 0
	read := func() int { calls++; return calls }
	if cache.get("first", read) != 1 || cache.get("first", read) != 1 {
		t.Fatal("repeated status request queried the OS again")
	}
	if cache.get("second", read) != 2 {
		t.Fatal("path change returned stale status")
	}
	cache.invalidate()
	if cache.get("second", read) != 3 {
		t.Fatal("explicit refresh returned stale status")
	}
	cache.until = time.Now().Add(-time.Second)
	if cache.get("second", read) != 4 {
		t.Fatal("expired status was not refreshed")
	}
}
