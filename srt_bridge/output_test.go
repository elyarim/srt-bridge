package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeOutput answers like SRT Bridge Output on its own interface.
func fakeOutput(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var quits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"9.9.9","bridgeOk":true,"signals":[{"slot":1,"out":"off","outName":"SRT Bridge 1"}],"ports":[],"cards":[]}`))
	})
	mux.HandleFunc("PUT /api/out", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Slot int    `json:"slot"`
			Out  string `json:"out"`
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Name == "taken" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"signal 2 already uses the name \"taken\""}`))
			return
		}
		w.Write([]byte(`{"version":"9.9.9","signals":[{"slot":1,"out":"` + in.Out + `","outName":"` + in.Name + `"}]}`))
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		quits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &quits
}

func TestOutputProxy(t *testing.T) {
	srv, quits := fakeOutput(t)
	cfg := defaultConfig()
	cfg.OutputAddr = strings.TrimPrefix(srv.URL, "http://")
	h := newHub(cfg, filepath.Join(t.TempDir(), "config.json"))
	out := newOutput(h)
	web := newWebServer(h, nil, out, nil)
	out.poll()

	do := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		web.ServeHTTP(rec, req)
		m := map[string]any{}
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}

	code, st := do("GET", "/api/state", "")
	o := st["output"].(map[string]any)
	if code != 200 || o["running"] != true || o["state"].(map[string]any)["version"] != "9.9.9" {
		t.Fatalf("the state of Bridge Output must come through: %d %v", code, o)
	}
	code, st = do("PUT", "/api/output/out", `{"slot":1,"out":"ndi","name":"Studio A"}`)
	if code != 200 {
		t.Fatalf("setting an output: %d %v", code, st)
	}
	sig := st["output"].(map[string]any)["state"].(map[string]any)["signals"].([]any)[0].(map[string]any)
	if sig["out"] != "ndi" || sig["outName"] != "Studio A" {
		t.Fatalf("the answer must carry the new setting: %v", sig)
	}
	code, st = do("PUT", "/api/output/out", `{"slot":1,"out":"ndi","name":"taken"}`)
	if code != 400 || !strings.Contains(st["error"].(string), "already uses the name") {
		t.Fatalf("the reason of Bridge Output must come through: %d %v", code, st)
	}
	if code, _ := do("POST", "/api/output/stop", `{}`); code != 200 || quits.Load() != 1 {
		t.Fatalf("stop must ask Bridge Output to quit: %d, %d quits", code, quits.Load())
	}
	// Stopped by hand: the bridge does not start it again.
	if out.status().Want {
		t.Fatal("after stop the bridge must not keep it running")
	}
	code, st = do("PUT", "/api/output/settings", `{"auto":true,"path":"C:\\x\\y.txt"}`)
	if code != 400 {
		t.Fatalf("a path that is not an .exe: %d %v", code, st)
	}
	if code, _ := do("PUT", "/api/output/settings", `{"auto":true,"path":""}`); code != 200 || !out.status().Auto {
		t.Fatalf("start with the bridge: %d", code)
	}
}

func TestOutputNotThere(t *testing.T) {
	cfg := defaultConfig()
	cfg.OutputAddr = "127.0.0.1:1" // nothing answers
	cfg.OutputPath = filepath.Join(t.TempDir(), "missing.exe")
	h := newHub(cfg, filepath.Join(t.TempDir(), "config.json"))
	out := newOutput(h)
	out.poll()
	if err := out.start(); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("starting a missing program: %v", err)
	}
	if err := out.setOut([]byte(`{"slot":1,"out":"omt","name":"x"}`)); err == nil {
		t.Fatal("setting an output without the program must fail")
	}
	st := out.status()
	if st.Running || st.Found {
		t.Fatalf("status of a missing program: %+v", st)
	}
}

func TestOutputPathNotRemote(t *testing.T) {
	h, _, _, base := remoteSetup(t)
	c := client()
	call(t, c, "POST", base+"/api/login", map[string]string{"password": "remote-password-1"})
	code, st := call(t, c, "PUT", base+"/api/output/settings", OutputSettings{Auto: true, Path: `C:\evil.exe`})
	if code != http.StatusBadRequest || h.outputPath() != "" {
		t.Fatalf("a program path from outside must be refused: %d %v", code, st)
	}
	if code, _ := call(t, c, "PUT", base+"/api/output/settings", OutputSettings{Auto: true}); code != 200 {
		t.Fatalf("start with the bridge from outside: %d", code)
	}
}
