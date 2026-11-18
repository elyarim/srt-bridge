package main

// The sound: Opus from the sender goes to srt_bridge_aac.exe (libopus + fdk-aac),
// AAC frames come back with their timestamps. One converter runs for the whole
// time; a new sender resets it.

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

const aacExeName = "srt_bridge_aac.exe"

type aacConv struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	in      io.WriteCloser
	w       *bufio.Writer
	delay   int // samples the encoder holds back (already taken off the timestamps)
	bitrate int
	onFrame func(pts48 int64, adts []byte)
	dead    bool
}

func aacExePath() string {
	exe, err := os.Executable()
	if err != nil {
		return aacExeName
	}
	return filepath.Join(filepath.Dir(exe), aacExeName)
}

func startAAC(bitrate int, onFrame func(int64, []byte)) (*aacConv, error) {
	path := aacExePath()
	cmd := exec.Command(path, "--bitrate", fmt.Sprint(bitrate))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errp, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start %s: %v", path, err)
	}
	a := &aacConv{cmd: cmd, in: in, w: bufio.NewWriter(in), bitrate: bitrate, onFrame: onFrame}
	sc := bufio.NewScanner(errp)
	if !sc.Scan() {
		cmd.Process.Kill()
		return nil, errors.New("the sound converter ended at once")
	}
	var ready struct {
		Ready   bool   `json:"ready"`
		Delay   int    `json:"delay"`
		Bitrate int    `json:"bitrate"`
		Opus    string `json:"opus"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(sc.Bytes(), &ready) != nil || !ready.Ready {
		cmd.Process.Kill()
		return nil, fmt.Errorf("sound converter: %s", sc.Text())
	}
	a.delay, a.bitrate = ready.Delay, ready.Bitrate
	log.Printf("sound converter ready: %s, AAC-LC %d kbit/s, encoder delay %d samples", ready.Opus, ready.Bitrate/1000, ready.Delay)
	go func() {
		for sc.Scan() {
			log.Printf("sound converter: %s", sc.Text())
		}
	}()
	go a.read(bufio.NewReader(out))
	return a, nil
}

func (a *aacConv) read(r *bufio.Reader) {
	var hdr [12]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			break
		}
		pts := int64(binary.LittleEndian.Uint64(hdr[0:]))
		n := binary.LittleEndian.Uint32(hdr[8:])
		if n > 1<<16 {
			break
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		a.onFrame(pts, buf)
	}
	a.mu.Lock()
	a.dead = true
	a.mu.Unlock()
	a.cmd.Wait()
	log.Print("sound converter ended")
}

func (a *aacConv) write(kind byte, pts int64, n uint32, data []byte) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dead {
		return false
	}
	var hdr [13]byte
	hdr[0] = kind
	binary.LittleEndian.PutUint64(hdr[1:], uint64(pts))
	binary.LittleEndian.PutUint32(hdr[9:], n)
	a.w.Write(hdr[:])
	a.w.Write(data)
	if err := a.w.Flush(); err != nil {
		a.dead = true
		return false
	}
	return true
}

func (a *aacConv) packet(pts48 int64, data []byte) bool {
	return a.write(1, pts48, uint32(len(data)), data)
}
func (a *aacConv) lost(pts48 int64, samples int) bool { return a.write(2, pts48, uint32(samples), nil) }
func (a *aacConv) reset() bool                        { return a.write(3, 0, 0, nil) }

func (a *aacConv) alive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.dead
}

func (a *aacConv) close() {
	a.mu.Lock()
	a.dead = true
	a.in.Close()
	a.mu.Unlock()
}
