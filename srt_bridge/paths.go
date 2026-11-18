package main

import (
	"os"
	"path/filepath"
)

// Where the configuration and the log are kept, as the interface shows them.
var configFile, logFile string

// dataPaths says where config.json and the log live.
//
// A config.json next to the program (the development copy, or a copy that
// is carried around) keeps both there, as before. Otherwise they go where
// Windows keeps such files, so the program itself can sit in Program Files:
// the settings of this user in %APPDATA%\SRTBridge, the log in
// %LOCALAPPDATA%\SRTBridge (next to the data of the window).
func dataPaths() (cfg, logf string) {
	dir := exeDir()
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		return filepath.Join(dir, "config.json"), filepath.Join(dir, "srt-bridge.log")
	}
	roam, local := os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA")
	if roam == "" || local == "" {
		return filepath.Join(dir, "config.json"), filepath.Join(dir, "srt-bridge.log")
	}
	cdir, ldir := filepath.Join(roam, "SRTBridge"), filepath.Join(local, "SRTBridge")
	os.MkdirAll(cdir, 0o700)
	os.MkdirAll(ldir, 0o700)
	return filepath.Join(cdir, "config.json"), filepath.Join(ldir, "srt-bridge.log")
}
