package main

// Shut down this computer from the interface (Peter, 2026-11-19): the bridge
// PC is switched on from afar and starts the bridge by itself; over remote
// access it can now be switched off again. Windows' own shutdown.exe does it
// with a countdown, so a click by mistake can still be cancelled.

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// shutdownDelay is the countdown. With a countdown Windows also closes programs
// that would hold the shutdown up (shutdown.exe implies /f): unsaved work in
// other programs on this PC is lost.
const shutdownDelay = 20 * time.Second

// runShutdown calls shutdown.exe; tests replace it so nothing is switched off.
var runShutdown = func(args ...string) error {
	cmd := exec.Command(system32("shutdown.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) > 0 {
		return errors.New(string(out))
	}
	return err
}

var shutdownState struct {
	mu sync.Mutex
	at time.Time // when Windows shuts down; zero = nothing asked for
}

// shutdownAt is the moment of a pending shutdown in unix seconds, 0 = none.
func shutdownAt() int64 {
	shutdownState.mu.Lock()
	defer shutdownState.mu.Unlock()
	if shutdownState.at.IsZero() || time.Now().After(shutdownState.at.Add(time.Minute)) {
		return 0
	}
	return shutdownState.at.Unix()
}

func requestShutdown() error {
	if err := runShutdown("/s", "/t", "20", "/d", "p:0:0", "/c", "SRT Bridge: shutdown asked for in its interface"); err != nil {
		return errors.New("Windows refused the shutdown: " + err.Error())
	}
	shutdownState.mu.Lock()
	shutdownState.at = time.Now().Add(shutdownDelay)
	shutdownState.mu.Unlock()
	return nil
}

func cancelShutdown() error {
	err := runShutdown("/a")
	shutdownState.mu.Lock()
	shutdownState.at = time.Time{}
	shutdownState.mu.Unlock()
	if err != nil {
		return errors.New("no shutdown to cancel, or Windows refused: " + err.Error())
	}
	return nil
}
