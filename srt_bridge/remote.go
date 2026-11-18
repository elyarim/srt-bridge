package main

// Remote access: the same interface over HTTPS on TCP <SRT port>, from
// anywhere. UDP carries SRT, TCP with the same number carries the interface,
// so the router and the firewall still only need one port number.
//
// The certificate is made by the bridge itself on the first start (self
// signed, 10 years). The browser warns once; the fingerprint shown on the
// bridge PC says whether it is really this bridge. Over this port every
// request to /api needs a login: the password in the browser (a session
// cookie), or the API token for programs (Authorization: Bearer <token>).
// The interface on 127.0.0.1 stays as it was, without a login.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RemoteConfig is saved in config.json.
type RemoteConfig struct {
	Enabled  bool   `json:"enabled"`
	PassHash string `json:"passHash"` // "pbkdf2-sha256$<iterations>$<salt>$<key>"; "" = no password set
	APIToken string `json:"apiToken"` // for programs: Authorization: Bearer <token>
}

// RemoteStatus is what the interface shows about remote access.
type RemoteStatus struct {
	Enabled     bool   `json:"enabled"`
	HasPassword bool   `json:"hasPassword"`
	Port        int    `json:"port"` // TCP port of the interface: the number of the SRT port
	Listening   bool   `json:"listening"`
	Error       string `json:"error"`
	Fingerprint string `json:"fingerprint"`        // SHA-256 of the certificate, as browsers show it
	APIToken    string `json:"apiToken,omitempty"` // only on the bridge PC itself
	Viewer      string `json:"viewer"`             // "local": the bridge PC; "remote": over HTTPS
}

const (
	minPassword     = 10
	maxPassword     = 128
	pbkdf2Rounds    = 300_000
	sessionIdle     = 12 * time.Hour
	failsBeforeLock = 5
	maxLock         = 15 * time.Minute
	certFileName    = "remote-cert.pem"
	keyFileName     = "remote-key.pem"
	sessionCookie   = "srtbridge_session"
)

// ---- password and tokens ----

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func checkNewPassword(pw string) error {
	if len(pw) < minPassword || len(pw) > maxPassword {
		return fmt.Errorf("the password needs %d to %d characters", minPassword, maxPassword)
	}
	return nil
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Rounds, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Rounds, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(pw, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds < 1 || rounds > 10_000_000 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, rounds, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// ---- the remote settings in the hub (they live in config.json) ----

func (h *Hub) note(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.eventf(format, args...)
}

func (h *Hub) remoteConf() (RemoteConfig, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.Remote, h.cfg.SRTPort
}

// ensureAPIToken gives a new installation its token once, and saves it.
func (h *Hub) ensureAPIToken() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg.Remote.APIToken == "" {
		h.cfg.Remote.APIToken = newToken()
		h.saveLocked()
	}
}

// RemoteUpdate changes remote access; a nil field stays as it is.
type RemoteUpdate struct {
	Enabled  *bool   `json:"enabled"`
	Password *string `json:"password"`
	NewToken bool    `json:"newToken"`
}

func (h *Hub) updateRemote(u RemoteUpdate) error {
	var hash string
	if u.Password != nil {
		if err := checkNewPassword(*u.Password); err != nil {
			return err
		}
		var err error
		if hash, err = hashPassword(*u.Password); err != nil {
			return err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := &h.cfg.Remote
	if u.Enabled != nil && *u.Enabled && r.PassHash == "" && hash == "" {
		return errors.New("set a password first")
	}
	if hash != "" {
		r.PassHash = hash
		h.eventf("remote access: new password set")
	}
	if u.NewToken {
		r.APIToken = newToken()
		h.eventf("remote access: new API token, the old one no longer works")
	}
	if u.Enabled != nil && *u.Enabled != r.Enabled {
		r.Enabled = *u.Enabled
		if r.Enabled {
			h.eventf("remote access switched on (HTTPS on TCP %d)", h.cfg.SRTPort)
		} else {
			h.eventf("remote access switched off")
		}
	}
	h.saveLocked()
	return nil
}

// srtPortAndMode is what remote access must not change: it would move the
// port the remote viewer is using.
func (h *Hub) srtPortAndMode() (int, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.SRTPort, h.cfg.PortMode
}

// ---- the HTTPS listener ----

type failCount struct {
	n     int
	last  time.Time
	until time.Time
}

type Remote struct {
	hub   *Hub
	dir   string       // where the certificate is kept: next to config.json
	inner http.Handler // the interface

	mu   sync.Mutex
	srv  *http.Server
	port int
	err  string
	cert *tls.Certificate
	fp   string

	authMu   sync.Mutex
	sessions map[string]time.Time // session token -> expires
	fails    map[string]*failCount
}

func newRemote(h *Hub, dir string) *Remote {
	h.ensureAPIToken()
	return &Remote{hub: h, dir: dir, sessions: map[string]time.Time{}, fails: map[string]*failCount{}}
}

// sync opens, moves or closes the HTTPS port so it matches the settings.
func (rm *Remote) sync() {
	rc, port := rm.hub.remoteConf()
	want := rc.Enabled && rc.PassHash != ""
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.srv != nil && (!want || port != rm.port) {
		rm.srv.Close()
		rm.srv = nil
		log.Printf("remote access: TCP %d closed", rm.port)
	}
	if !want {
		rm.err = ""
		return
	}
	if rm.srv != nil {
		return
	}
	rm.port = port
	if err := rm.loadCertLocked(); err != nil {
		rm.setErrLocked("certificate: " + err.Error())
		return
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		rm.setErrLocked(fmt.Sprintf("TCP %d could not be opened: %v", port, err))
		return
	}
	srv := &http.Server{
		Handler:           rm.wrap(rm.inner),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*rm.cert}},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// Every browser that has not accepted the certificate yet ends its
		// handshake with an error; that is no news for the log.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	rm.srv, rm.err = srv, ""
	log.Printf("remote access: https on TCP %d", port)
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			rm.mu.Lock()
			if rm.srv == srv {
				rm.srv = nil
				rm.setErrLocked("stopped: " + err.Error())
			}
			rm.mu.Unlock()
		}
	}()
}

// setErrLocked keeps the error for the interface; the log hears it only when it is new.
func (rm *Remote) setErrLocked(msg string) {
	if msg != rm.err {
		rm.hub.note("remote access: %s", msg)
	}
	rm.err = msg
}

// loop tries again when the port was taken, as the SRT port does.
func (rm *Remote) loop() {
	for range time.Tick(10 * time.Second) {
		rm.sync()
		rm.prune()
	}
}

func (rm *Remote) stop() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.srv != nil {
		rm.srv.Close()
		rm.srv = nil
	}
}

func (rm *Remote) status(r *http.Request) *RemoteStatus {
	rc, port := rm.hub.remoteConf()
	rm.mu.Lock()
	st := &RemoteStatus{Enabled: rc.Enabled, HasPassword: rc.PassHash != "", Port: port, Listening: rm.srv != nil, Error: rm.err, Fingerprint: rm.fp, Viewer: "local"}
	if st.Fingerprint == "" {
		// Read the certificate before remote access is on, so the
		// fingerprint can be noted down at the bridge PC beforehand.
		if rm.loadCertLocked() == nil {
			st.Fingerprint = rm.fp
		}
	}
	rm.mu.Unlock()
	if isRemote(r) {
		st.Viewer = "remote"
	} else {
		st.APIToken = rc.APIToken
	}
	return st
}

// firewallPort is the TCP port the firewall rule must open; 0 = none.
func (rm *Remote) firewallPort() int {
	rc, port := rm.hub.remoteConf()
	if rc.Enabled {
		return port
	}
	return 0
}

// ---- the certificate ----

func (rm *Remote) loadCertLocked() error {
	if rm.cert != nil {
		return nil
	}
	certPath, keyPath := filepath.Join(rm.dir, certFileName), filepath.Join(rm.dir, keyFileName)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if errors.Is(err, os.ErrNotExist) {
		if err = makeCert(certPath, keyPath, rm.hub); err == nil {
			cert, err = tls.LoadX509KeyPair(certPath, keyPath)
		}
	}
	if err != nil {
		return err
	}
	sum := sha256.Sum256(cert.Certificate[0])
	rm.cert, rm.fp = &cert, fingerprint(sum[:])
	return nil
}

func fingerprint(sum []byte) string {
	h := strings.ToUpper(hex.EncodeToString(sum))
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String()
}

// makeCert writes a self-signed certificate. The browser checks nothing in it
// except the fingerprint the user compares; the addresses are only a courtesy.
func makeCert(certPath, keyPath string, h *Hub) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "SRT Bridge"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	h.mu.Lock()
	if ip := net.ParseIP(h.cfg.PublicIP); ip != nil {
		tpl.IPAddresses = append(tpl.IPAddresses, ip)
	}
	h.mu.Unlock()
	for _, a := range localAddresses() {
		if ip := net.ParseIP(a); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	h.note("remote access: new certificate made, fingerprint %s", fingerprint(func() []byte { s := sha256.Sum256(der); return s[:] }()))
	return nil
}

// ---- login ----

type ctxKey int

const remoteKey ctxKey = 0

// isRemote says whether a request came over the HTTPS port.
func isRemote(r *http.Request) bool { return r.Context().Value(remoteKey) != nil }

// localOnlyAPI is what stays on the bridge PC: the firewall asks Windows there,
// taps go to programs on that PC, and the remote settings guard this port.
func localOnlyAPI(r *http.Request) bool {
	p := r.URL.Path
	return (r.Method == http.MethodPost && (p == "/api/firewall" || p == "/api/taps")) || p == "/api/remote"
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": msg, "login": code == http.StatusUnauthorized})
}

func (rm *Remote) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r = r.WithContext(context.WithValue(r.Context(), remoteKey, true))
		// The page itself holds nothing secret; it asks for the login.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/login":
			if r.Method == http.MethodPost {
				rm.login(w, r)
			} else {
				jsonError(w, http.StatusMethodNotAllowed, "POST only")
			}
			return
		case "/api/logout":
			rm.logout(w, r)
			return
		}
		if !rm.authorized(r) {
			jsonError(w, http.StatusUnauthorized, "login needed")
			return
		}
		if localOnlyAPI(r) {
			jsonError(w, http.StatusForbidden, "this can only be changed on the bridge PC itself")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (rm *Remote) authorized(r *http.Request) bool {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		rc, _ := rm.hub.remoteConf()
		return rc.APIToken != "" && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(bearer)), []byte(rc.APIToken)) == 1
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	rm.authMu.Lock()
	defer rm.authMu.Unlock()
	exp, ok := rm.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(rm.sessions, c.Value)
		return false
	}
	rm.sessions[c.Value] = time.Now().Add(sessionIdle)
	return true
}

func (rm *Remote) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()
	rm.authMu.Lock()
	f := rm.fails[ip]
	if f != nil && now.Before(f.until) {
		wait := int(f.until.Sub(now).Seconds()) + 1
		rm.authMu.Unlock()
		jsonError(w, http.StatusTooManyRequests, fmt.Sprintf("too many wrong passwords, try again in %d s", wait))
		return
	}
	rm.authMu.Unlock()

	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "bad request")
		return
	}
	rc, _ := rm.hub.remoteConf()
	if rc.PassHash == "" || !checkPassword(in.Password, rc.PassHash) {
		rm.authMu.Lock()
		f = rm.fails[ip]
		if f == nil {
			f = &failCount{}
			rm.fails[ip] = f
		}
		f.n++
		f.last = now
		var lock time.Duration
		if f.n >= failsBeforeLock {
			lock = min(time.Minute<<min(f.n-failsBeforeLock, 4), maxLock)
			f.until = now.Add(lock)
		}
		n := f.n
		rm.authMu.Unlock()
		if lock > 0 {
			rm.hub.note("remote access: %d wrong passwords from %s, login locked for %s", n, ip, lock)
		}
		jsonError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	token := newToken()
	rm.authMu.Lock()
	delete(rm.fails, ip)
	rm.sessions[token] = now.Add(sessionIdle)
	rm.authMu.Unlock()
	rm.hub.note("remote access: login from %s", ip)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]bool{"ok": true})
}

func (rm *Remote) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		rm.authMu.Lock()
		delete(rm.sessions, c.Value)
		rm.authMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]bool{"ok": true})
}

// endSessions logs everybody out, after a new password.
func (rm *Remote) endSessions() {
	rm.authMu.Lock()
	clear(rm.sessions)
	rm.authMu.Unlock()
}

func (rm *Remote) prune() {
	now := time.Now()
	rm.authMu.Lock()
	defer rm.authMu.Unlock()
	for k, exp := range rm.sessions {
		if now.After(exp) {
			delete(rm.sessions, k)
		}
	}
	for ip, f := range rm.fails {
		if now.Sub(f.last) > time.Hour && now.After(f.until) {
			delete(rm.fails, ip)
		}
	}
}
