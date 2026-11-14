package main

/*
#cgo LDFLAGS: -lncursesw -ltinfo
#include "curses.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"
)

// Color pair ids used by cursesPrint.
const (
	colMe     = 1 // green
	colPhone  = 2 // cyan
	colSystem = 3 // yellow
	colHeader = 4 // not used directly here
)

// tui is the ncurses front-end. It is created before anything else, so the
// UI is visible from the very first moment - including while scanning.
type tui struct{}

func newTUI() (*tui, error) {
	if C.cursesInit() != 0 {
		return nil, errors.New("ncurses init failed (is this a terminal? try without -tui)")
	}
	return &tui{}, nil
}

func (t *tui) close()        { C.cursesShutdown() }
func (t *tui) resize()       { C.cursesResize() }
func (t *tui) clear()        { C.cursesClear() }
func (t *tui) termCols() int { return int(C.cursesCols()) }

func (t *tui) printLine(s string, color int) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	C.cursesPrint(cs, C.int(color))
}

func (t *tui) header(s string) {
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	C.cursesHeader(cs)
}

// status renders a one-line system message in yellow, so connection events
// are always visible.
func (t *tui) status(s string) { t.printLine("> "+s, colSystem) }

// drawInput shows prompt+text on the input line.
func (t *tui) drawInput(prompt, text string) {
	cp := C.CString(prompt)
	defer C.free(unsafe.Pointer(cp))
	ct := C.CString(text)
	defer C.free(unsafe.Pointer(ct))
	C.cursesDrawInput(cp, ct)
}

// key waits up to ms for a keypress; returns rune or -1 (and ok=false) on
// timeout. Ctrl-C is 3, ESC is 27, Enter is 10/13.
func (t *tui) key(ms int) (int, bool) {
	c := int(C.cursesKey(C.int(ms)))
	if c < 0 {
		return 0, false
	}
	return c, true
}

func (t *tui) prompt(p string) (string, bool) {
	buf := make([]byte, 256)
	cp := C.CString(p)
	defer C.free(unsafe.Pointer(cp))
	rc := C.cursesPrompt(cp, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if rc < 0 {
		return "", false
	}
	s := strings.TrimSpace(C.GoString((*C.char)(unsafe.Pointer(&buf[0]))))
	if s == "" {
		return "", false
	}
	return s, true
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

// wrap splits s into lines of width cols using simple word wrapping.
func wrap(s string, cols int) []string {
	if cols <= 0 {
		cols = 80
	}
	if len(s) == 0 {
		return []string{""}
	}
	var out []string
	runes := []rune(s)
	for len(runes) > cols {
		cut := cols
		if i := lastSpace(runes[:cols]); i > 0 {
			cut = i
		}
		out = append(out, trim(string(runes[:cut])))
		runes = runes[cut:]
	}
	out = append(out, trim(string(runes)))
	return out
}

func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == ' ' {
			return i
		}
	}
	return 0
}

func trim(s string) string {
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// Device menu
// ---------------------------------------------------------------------------

// pickDevice shows a numbered list of devices and lets the user choose one
// with arrow keys / digits / Enter. Returns the chosen device, or false when
// the user quits (q / ESC / Ctrl-C).
func (t *tui) pickDevice(devs []device) (*device, bool) {
	if len(devs) == 0 {
		return nil, false
	}
	sel := 0
	draw := func() {
		t.header("ghostlink - select a device   [UP/DOWN move, Enter connect, q quit]")
		t.clear()
		for i := range devs {
			d := devs[i]
			line := fmt.Sprintf("  %d) %-20s %s", i+1, d.name, d.addr)
			if !hasUUID(d.uuids, sppUUID) {
				line += "  (no GhostLink service)"
			}
			if i == sel {
				t.printLine("> "+line, colMe)
			} else {
				t.printLine("  "+line, colSystem)
			}
		}
		t.drawInput("choose> ", "")
	}
	draw()

	for {
		c, ok := t.key(250)
		if !ok {
			continue
		}
		switch c {
		case 27, 'q', 'Q', 3:
			return nil, false
		case 10, 13:
			d := devs[sel]
			return &d, true
		case 65, 107: // up arrow / k
			if sel > 0 {
				sel--
			}
			draw()
		case 66, 106: // down arrow / j
			if sel < len(devs)-1 {
				sel++
			}
			draw()
		default:
			if c >= '1' && c <= '9' {
				idx := int(c - '1')
				if idx < len(devs) {
					sel = idx
					draw()
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Connection
// ---------------------------------------------------------------------------

// connectDevice opens the RFCOMM channel to d. Progress is printed into the
// TUI so failures never leave a blank terminal.
func (t *tui) connectDevice(ctx context.Context, bz *bluez, d *device) (*os.File, error) {
	t.status(fmt.Sprintf("connecting to %s (%s) ...", d.name, d.addr))

	if err := bz.call(d.path, "org.bluez.Device1.Connect"); err != nil {
		t.status("Device1.Connect: " + err.Error())
		t.status("(on the phone: Bluetooth on, GhostLink in 'Host' mode)")
	}

	// Fast path: older BlueZ publishes SerialPort1 immediately.
	if ch, ok := bz.serialPortChannel(d.path); ok {
		if f, err := os.OpenFile(rfcommDir()+fmt.Sprintf("/rfcomm%d", ch), os.O_RDWR, 0); err == nil {
			t.status(fmt.Sprintf("connected! RFCOMM channel %d", ch))
			return f, nil
		}
	}

	// BlueZ 5.85+: read the channel from SDP, then open a raw socket.
	t.status("looking up GhostLink RFCOMM channel ...")
	channel := getChannelFromSDP(d.addr)
	if channel <= 0 {
		channel = 28
		t.status("SDP lookup failed, trying channel 28")
	} else {
		t.status(fmt.Sprintf("SDP says GhostLink is on RFCOMM channel %d", channel))
	}

	f, err := connectRFCOMMSocket(d.addr, channel)
	if err != nil {
		return nil, err
	}
	t.status("connected!")
	return f, nil
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

// chatTUI is the main entry: it starts the UI, asks the user to pick a device
// from a scan, connects, and then runs the chat. Nothing is printed to the
// raw terminal - every state is rendered inside ncurses.
func chatTUI(ctx context.Context, bz *bluez, preset *device, scanDur time.Duration) error {
	t, err := newTUI()
	if err != nil {
		return err
	}
	defer t.close()
	t.resize()

	target, ok := preset, preset != nil
	if !ok {
		// ---- scanning phase (UI is already up) ----
		t.header("ghostlink - scanning   [q to quit]")
		t.clear()
		t.status("scanning for nearby devices ...")
		t.status("on the phone: Bluetooth on, GhostLink app in 'Host' mode")

		devs, err := t.scanDevices(ctx, bz, scanDur)
		if err != nil {
			t.status("scan failed: " + err.Error())
			t.printLine("press any key to return", colSystem)
			_, _ = t.key(-1)
			return err
		}
		if len(devs) == 0 {
			t.status("no devices found")
			t.printLine("press any key to return", colSystem)
			_, _ = t.key(-1)
			return errors.New("no devices found")
		}

		// ---- picker phase ----
		picked, ok := t.pickDevice(devs)
		if !ok {
			return nil
		}
		target = picked
	}

	// ---- connect phase ----
	f, err := t.connectDevice(ctx, bz, target)
	if err != nil {
		t.status("connection failed: " + err.Error())
		t.printLine("press any key to return", colSystem)
		_, _ = t.key(-1)
		return err
	}
	defer f.Close()

	// ---- chat phase ----
	t.clear()
	t.header(fmt.Sprintf("ghostlink - chatting with %s   [Ctrl-C/ESC quit]", target.name))
	return t.runChat(ctx, target, f)
}

// runChat is the actual conversation loop: incoming bytes from the phone are
// printed in cyan; the user types lines that are sent raw (no newline).
func (t *tui) runChat(ctx context.Context, target *device, f *os.File) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				msg := string(buf[:n])
				for _, ln := range wrap(msg, t.termCols()-8) {
					t.printLine("phone: "+ln, colPhone)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Poll-and-draw input loop so incoming messages appear immediately while
	// the user is typing.
	var input []rune
	t.drawInput("> ", "")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			t.status("connection closed by peer")
			return nil
		default:
		}

		c, ok := t.key(100)
		if !ok {
			continue
		}
		switch {
		case c == 3 || c == 27: // Ctrl-C / ESC
			return nil
		case c == 10 || c == 13: // Enter: send
			msg := strings.TrimSpace(string(input))
			input = input[:0]
			t.drawInput("> ", "")
			if msg == "" {
				continue
			}
			if _, err := f.Write([]byte(msg)); err != nil {
				t.printLine("send failed: "+err.Error(), colSystem)
				return err
			}
			t.printLine("me: "+msg, colMe)
		case c == 127 || c == 8 || c == 263: // Backspace
			if len(input) > 0 {
				input = input[:len(input)-1]
				t.drawInput("> ", string(input))
			}
		case c >= 32 && c < 127:
			input = append(input, rune(c))
			t.drawInput("> ", string(input))
		}
	}
}

// scanDevices runs Bluetooth discovery for scanDur and returns everything
// found, refreshing the list in the UI as results arrive.
func (t *tui) scanDevices(ctx context.Context, bz *bluez, scanDur time.Duration) ([]device, error) {
	adapter := bz.adapterPath()
	if adapter == "" {
		return nil, errors.New("no Bluetooth adapter (is BlueZ running? rfkill list bluetooth)")
	}
	// Make sure the adapter is powered.
	if v, err := bz.getProperty(adapter, "org.bluez.Adapter1", "Powered"); err == nil {
		var powered bool
		if v.Store(&powered) == nil && !powered {
			t.status("adapter off - powering on ...")
			_ = bz.setProperty(adapter, "org.bluez.Adapter1", "Powered", true)
		}
	}

	if err := bz.call(adapter, "org.bluez.Adapter1.StartDiscovery"); err != nil {
		return nil, fmt.Errorf("StartDiscovery: %w", err)
	}
	defer bz.call(adapter, "org.bluez.Adapter1.StopDiscovery")

	deadline := time.Now().Add(scanDur)
	seen := map[string]bool{}
	var devs []device

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return devs, nil
		default:
		}
		changed := false
		for _, d := range bz.findDevices(false) {
			if seen[d.addr] {
				continue
			}
			seen[d.addr] = true
			devs = append(devs, d)
			changed = true
		}
		if changed {
			t.header(fmt.Sprintf("ghostlink - scanning %ds   [q to quit]", int(time.Until(deadline).Seconds())+1))
			t.clear()
			for _, d := range devs {
				tag := ""
				if hasUUID(d.uuids, sppUUID) {
					tag = "  <- GhostLink"
				}
				t.printLine(fmt.Sprintf("  %-20s %s%s", d.name, d.addr, tag), colSystem)
			}
		}
		if c, ok := t.key(300); ok && (c == 'q' || c == 3 || c == 27) {
			return devs, nil
		}
	}
	return devs, nil
}