package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW   = user32.NewProc("MessageBoxW")
	procSetWindowLong = user32.NewProc("SetWindowLongPtrW")
	procCallWndProc   = user32.NewProc("CallWindowProcW")
)

const (
	wmClose        = 0x0010
	gwlpWndProc    = ^uintptr(3) // -4
	mbYesNo        = 0x00000004
	mbIconQuestion = 0x00000020
	mbIconError    = 0x00000010
	mbDefButton2   = 0x00000100
	idYes          = 6
)

func messageBox(hwnd uintptr, text, title string, flags uintptr) uintptr {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(title)
	r, _, _ := procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
	return r
}

func showError(msg string) { messageBox(0, msg, "SRT Bridge Output", mbIconError) }

// runWindow shows the interface and returns when the window is closed.
// Closing it ends all decoders, so it asks first while some are running.
func runWindow(url string, app *App) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(os.Getenv("LOCALAPPDATA"), "SRTBridgeOutput", "WebView2"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "SRT Bridge Output " + version,
			IconId: 1,
			Width:  1200,
			Height: 640,
			Center: true,
		},
	})
	if w == nil {
		showError("The window could not be opened. Is the Microsoft Edge WebView2 runtime installed?")
		return
	}
	defer w.Destroy()
	hwnd := uintptr(w.Window())
	var oldProc uintptr
	newProc := syscall.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		if msg == wmClose {
			if n := app.running(); n > 0 {
				text := fmt.Sprintf("%d output(s) are running.\n\nClosing this window stops them.\n\nEnd SRT Bridge Output?", n)
				if messageBox(h, text, "SRT Bridge Output", mbYesNo|mbIconQuestion|mbDefButton2) != idYes {
					return 0
				}
			}
		}
		r, _, _ := procCallWndProc.Call(oldProc, h, msg, wp, lp)
		return r
	})
	oldProc, _, _ = procSetWindowLong.Call(hwnd, gwlpWndProc, newProc)
	w.Navigate(url)
	w.Run()
}
