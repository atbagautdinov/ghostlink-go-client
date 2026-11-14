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
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/godbus/dbus/v5"
)

// tui is the ncurses front-end: a scrollable chat window plus a single-line
// input at the bottom.
//
// The TUI starts first (before the connection), so connection progress and
// failures render inside ncurses instead of aborting to a raw terminal.
type tui struct{}

func newTUI() (*tui, error) {
	if C.cursesInit() != 0 {
		return nil, errors.New("ncurses init failed (is this a terminal? try without -tui)")
	}
	return &tui{}, nil
}

func (t *tui) close()      { C.cursesShutdown() }
func (t *tui) resize()     { C.cursesResize() }
func (t *tui) clear()      { C.cursesClear() }
func (t *tui) termCols() int { return int(C.cursesCols()) }
func (t *tui) chatRows() int { return int(C.cursesChatRows()) }

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

// status renders a one-line system message in yellow at the bottom of the
// chat window, so connection events are always visible.
func (t *tui) status(s string) {
	t.printLine("> "+s, 3)
}

// prompt shows the prompt and reads one line; returns ("", false) on cancel
// (ESC / Ctrl-C / empty). Polls keystrokes so callers can stay responsive.
func (t *tui) prompt(p string) (string, bool) {
	buf := make([]byte, 256)
	cp := C.CString(p)
	defer C.free(unsafe.Pointer(cp))
	rc := C.cursesPrompt(cp, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if rc < 0 {
		return "", false
	}
	s := C.GoString((*C.char)(unsafe.Pointer(&buf[0])))
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	return s, true
}

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
		line := trim(string(runes[:cut]))
		out = append(out, line)
		runes = runes[cut:]
	}
	line := trim(string(runes))
	out = append(out, line)
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
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\r' || s[j-1] == '\n') {
		j--
	}
	return s[i:j]
}

// chatTUI drives the whole ncurses session: connect (if f == nil), then run
// the read/write chat loop. Every error surfaces inside ncurses so the UI
// stays up even when things fail.
func chatTUI(ctx context.Context, bz *bluez, target *device, f *os.File) error {
	t, err := newTUI()
	if err != nil {
		return err
	}
	defer t.close()

	t.header("ghostlink TUI — Ctrl-C/ESC or empty line to quit")
	t.resize()

	// --- connection phase (inside the UI) ---
	if f == nil {
		t.status("connecting to " + target.addr + " ...")
		if err := bz.conn.Object(bzSvc, target.path).Call("org.bluez.Device1.Connect", dbus.Flags(0)).Store(); err != nil {
			t.status("Device1.Connect failed: " + err.Error())
			t.status("waiting for rfcomm channel ...")
			if err := waitSerialPort(ctx, bz, target, t); err != nil {
				t.status("connect failed: " + err.Error())
				t.printLine("press any key to return", 3)
				_, _ = t.prompt("")
				return err
			}
		}
		channel, chErr := ghostLinkChannel(ctx, bz, target, t)
		if chErr != nil {
			t.status("rfcomm channel not found: " + chErr.Error())
			t.printLine("press any key to return", 3)
			_, _ = t.prompt("")
			return chErr
		}
		t.status("rfcomm channel " + channel + " — connecting ...")
		ch, _ := strconv.Atoi(channel)
		var ferr error
		f, ferr = connectRFCOMMSocket(target.addr, ch)
		if ferr != nil {
			t.status("rfcomm open failed: " + ferr.Error())
			t.printLine("press any key to return", 3)
			_, _ = t.prompt("")
			return ferr
		}
		t.status("connected!")
	}

	defer f.Close()

	// --- chat phase ---
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				msg := string(buf[:n])
				for _, ln := range wrap(msg, t.termCols()) {
					t.printLine("phone: "+ln, 2) // 2 = cyan
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		s, ok := t.prompt("> ")
		if !ok {
			break
		}
		if _, err := f.Write([]byte(s)); err != nil {
			t.printLine("send failed: "+err.Error(), 3) // 3 = yellow
			return err
		}
		f.Sync()
		t.printLine("me: "+s, 1) // 1 = green
	}

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
	return nil
}

// ghostLinkChannel waits for org.bluez.SerialPort1 (older BlueZ) under the
// device path and returns its RFCOMM channel as a string.
func ghostLinkChannel(ctx context.Context, bz *bluez, target *device, t *tui) (string, error) {
	deadline := time.Now().Add(8 * time.Second)
	for {
		objs, err := bz.managed()
		if err == nil {
			for path, ifaces := range objs {
				sp, ok := ifaces["org.bluez.SerialPort1"]
				if !ok || !strings.HasPrefix(string(path), string(target.path)) {
					continue
				}
				if ch, ok := sp["Path"].Value().(uint16); ok {
					return fmt.Sprintf("%d", ch), nil
				}
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", errors.New("SerialPort1 never appeared")
}

// waitSerialPort waits for SerialPort1 (older BlueZ path) and opens the rfcomm
// tty node.
func waitSerialPort(ctx context.Context, bz *bluez, target *device, t *tui) error {
	deadline := time.Now().Add(8 * time.Second)
	for {
		objs, err := bz.managed()
		if err == nil {
			for path, ifaces := range objs {
				sp, ok := ifaces["org.bluez.SerialPort1"]
				if !ok || !strings.HasPrefix(string(path), string(target.path)) {
					continue
				}
				if ch, ok := sp["Path"].Value().(uint16); ok {
					f, err := os.OpenFile(rfcommDir()+fmt.Sprintf("/rfcomm%d", ch), os.O_RDWR, 0)
					if err != nil {
						return err
					}
					f.Close()
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.New("SerialPort1 never appeared")
}