package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The test never switches the PC off: shutdown.exe is replaced.
func TestShutdownAndCancel(t *testing.T) {
	var calls [][]string
	old := runShutdown
	runShutdown = func(args ...string) error { calls = append(calls, args); return nil }
	t.Cleanup(func() { cancelShutdown(); runShutdown = old })

	h := testHub(t, false)
	web := newWebServer(h, nil, nil, nil)
	post := func(path string) *State {
		req := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		web.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var st State
		json.Unmarshal(rec.Body.Bytes(), &st)
		return &st
	}
	st := post("/api/shutdown")
	if len(calls) != 1 || calls[0][0] != "/s" || calls[0][2] != "20" {
		t.Fatalf("shutdown.exe called with %v", calls)
	}
	if st.ShutdownAt == 0 {
		t.Fatal("the state must show the pending shutdown")
	}
	st = post("/api/shutdown/cancel")
	if len(calls) != 2 || calls[1][0] != "/a" || st.ShutdownAt != 0 {
		t.Fatalf("cancel: calls %v, shutdownAt %d", calls, st.ShutdownAt)
	}
	// From another web page in a browser on this PC: refused.
	req := httptest.NewRequest("POST", "/api/shutdown", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	web.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(calls) != 2 {
		t.Fatalf("a foreign page must not shut down the PC: %d, calls %v", rec.Code, calls)
	}
}
