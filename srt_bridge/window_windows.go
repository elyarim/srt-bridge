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
	procSysParamInfo  = user32.NewProc("SystemParametersInfoW")
	procSetWindowPos  = user32.NewProc("SetWindowPos")
)

// The window opens at this size, smaller if the screen (without the taskbar)
// is smaller, and in the middle of it.
const (
	windowWidth  = 1500
	windowHeight = 950
)

// workArea is the part of the main screen the taskbar leaves free, in the
// same (scaled) pixels the window is made with.
func workArea() (x, y, w, h int) {
	var r struct{ Left, Top, Right, Bottom int32 }
	const spiGetWorkArea = 0x0030
	if ok, _, _ := procSysParamInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0); ok == 0 {
		return 0, 0, windowWidth, windowHeight
	}
	return int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top)
}

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

func showError(msg string) {
	messageBox(0, msg, "SRT Bridge", mbIconError)
}

// runWindow shows the interface in its own window and returns when the
// window is closed. Closing the window ends the bridge, so it asks first
// while devices are connected.
func runWindow(url string, hub *Hub) {
	data := filepath.Join(os.Getenv("LOCALAPPDATA"), "SRTBridge", "WebView2")
	ax, ay, aw, ah := workArea()
	width, height := min(windowWidth, aw), min(windowHeight, ah)
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  data,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "SRT Bridge " + version,
			IconId: 1, // app.rc
			Width:  uint(width),
			Height: uint(height),
			Center: true,
		},
	})
	if w == nil {
		showError("The window could not be opened. Is the Microsoft Edge WebView2 runtime installed?\n\nThe bridge keeps running; open " + url + " in a browser, or end it in the Task Manager.")
		select {}
	}
	defer w.Destroy()

	hwnd := uintptr(w.Window())
	mainHwnd.Store(hwnd)
	// The library centres on the whole screen; this puts it in the middle of the free part.
	const swpNoSize, swpNoZOrder = 0x0001, 0x0004
	procSetWindowPos.Call(hwnd, 0, uintptr(ax+(aw-width)/2), uintptr(ay+(ah-height)/2), 0, 0, swpNoSize|swpNoZOrder)
	var oldProc uintptr
	newProc := syscall.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		if msg == wmClose {
			if n := hub.connCount(); n > 0 {
				text := fmt.Sprintf("%d connection(s) are active.\n\nClosing this window ends SRT Bridge and stops all streams.\n\nEnd SRT Bridge?", n)
				if messageBox(h, text, "SRT Bridge", mbYesNo|mbIconQuestion|mbDefButton2) != idYes {
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
