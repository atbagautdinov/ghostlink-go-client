// GhostLink Go client — chats with the GhostLink Android app over Bluetooth.
//
// How it works (mirrors what the Kotlin app does):
//
//  1. The Android app is an RFCOMM *server*: MainActivity starts
//     listenUsingRfcommWithServiceRecord("GhostLinkBT", MY_UUID) and waits for
//     a connection on SPP UUID 8ce255c0-200a-11e0-ac64-0800200c9a66.
//
//  2. This program is the *client*. It talks to BlueZ over D-Bus (system bus):
//     - org.bluez.Adapter1.StartDiscovery()   -> we see the phone in range
//     - org.bluez.Device1.Connect()           -> BlueZ opens the RFCOMM channel
//     - org.bluez.SerialPort1 appears         -> gives us the RFCOMM channel number
//     - /dev/rfcomm<N>                        -> a plain byte stream, like a serial port
//
//  3. Message protocol (from MessageActivity.kt): each message is one raw
//     UTF-8 write() to the socket — no length prefix, no framing, no newline.
//     So here: send = write UTF-8 bytes; receive = treat each read chunk as
//     one message (the app does exactly the same on its side).
//
// Build & run (Linux with BlueZ):
//
//	go build -o ghostlink .
//	./ghostlink                 # scan ~15s for a GhostLink device, connect, chat
//	./ghostlink -list           # just list devices advertising the SPP service
//	./ghostlink -addr AA:BB:... # skip scanning, connect straight to this MAC
//
// Requirements: bluetoothd (BlueZ) running, adapter powered on.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	sppUUID = "8ce255c0-200a-11e0-ac64-0800200c9a66" // must match MY_UUID in the app
	bzSvc   = "org.bluez"
)

// ---------------------------------------------------------------------------
// BlueZ D-Bus plumbing (godbus v5 API: Call takes a Flags arg, Store returns error)
// ---------------------------------------------------------------------------

type bluez struct {
	conn *dbus.Conn
}

type managedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

func (b *bluez) managed() (managedObjects, error) {
	var objs managedObjects
	err := b.conn.Object(bzSvc, "/").
		Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", dbus.Flags(0)).
		Store(&objs)
	return objs, err
}

// call invokes a fire-and-forget BlueZ method on the object at path.
func (b *bluez) call(path dbus.ObjectPath, method string, args ...interface{}) error {
	return b.conn.Object(bzSvc, path).Call(method, dbus.Flags(0), args...).Store()
}

func (b *bluez) getProperty(path dbus.ObjectPath, iface, name string) (dbus.Variant, error) {
	return b.conn.Object(bzSvc, path).GetProperty(iface + "." + name)
}

func (b *bluez) setProperty(path dbus.ObjectPath, iface, name string, val interface{}) error {
	return b.conn.Object(bzSvc, path).SetProperty(iface+"."+name, val)
}

// ---------------------------------------------------------------------------
// Device discovery
// ---------------------------------------------------------------------------

type device struct {
	path  dbus.ObjectPath
	addr  string
	name  string
	uuids []string
}

func strProp(props map[string]dbus.Variant, key string) string {
	v, ok := props[key]
	if !ok {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

func hasUUID(uuids []string, want string) bool {
	for _, u := range uuids {
		if strings.EqualFold(u, want) {
			return true
		}
	}
	return false
}

// findDevices returns all BlueZ devices, sorted by address for stable output,
// optionally filtered to those advertising the GhostLink SPP service UUID.
func (b *bluez) findDevices(wantSPP bool) []device {
	objs, err := b.managed()
	if err != nil {
		fmt.Fprintln(os.Stderr, "GetManagedObjects:", err)
		return nil
	}
	var out []device
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if !ok {
			continue
		}
		d := device{path: path, addr: strProp(props, "Address"), name: strProp(props, "Name")}
		if u, ok := props["UUIDs"].Value().([]string); ok {
			d.uuids = u
		}
		if wantSPP && !hasUUID(d.uuids, sppUUID) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].addr < out[j].addr })
	return out
}

func (b *bluez) adapterPath() dbus.ObjectPath {
	objs, err := b.managed()
	if err != nil {
		return ""
	}
	for path, ifaces := range objs {
		if _, ok := ifaces["org.bluez.Adapter1"]; ok {
			return path
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Connection steps
// ---------------------------------------------------------------------------

// scanForGhostlink starts discovery on the local adapter and polls until a
// device advertising our SPP UUID shows up (or timeout).
func (b *bluez) scanForGhostlink(ctx context.Context, timeout time.Duration) (*device, error) {
	adapter := b.adapterPath()
	if adapter == "" {
		return nil, errors.New("no Bluetooth adapter found (is BlueZ running? try: bluetoothctl)")
	}

	// Make sure the adapter is on.
	if v, err := b.getProperty(adapter, "org.bluez.Adapter1", "Powered"); err == nil {
		var powered bool
		if v.Store(&powered) == nil && !powered {
			fmt.Println("adapter off — powering on...")
			if err := b.setProperty(adapter, "org.bluez.Adapter1", "Powered", true); err != nil {
				fmt.Fprintf(os.Stderr, "power on: %v\n", err)
			}
		}
	}

	fmt.Println("scanning for GhostLink devices (keep the app open in 'Host' mode)...")
	if err := b.call(adapter, "org.bluez.Adapter1.StartDiscovery"); err != nil {
		return nil, fmt.Errorf("StartDiscovery: %w", err)
	}
	defer b.call(adapter, "org.bluez.Adapter1.StopDiscovery") // always stop, even on cancel

	deadline := time.Now().Add(timeout)
	for {
		for _, d := range b.findDevices(true) {
			fmt.Printf("found: %s (%s)\n", d.name, d.addr)
			return &d, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("no GhostLink device found (did you press 'Host' on the phone?)")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// rfcommDir is where BlueZ's RFCOMM device nodes live. Overridable via env
// for testing (e.g. point it at a dir with a fake /rfcomm5 socket).
func rfcommDir() string {
	if d := os.Getenv("GHOSTLINK_RFCOMM_DIR"); d != "" {
		return d
	}
	return "/dev"
}

// serialPortChannel looks for a SerialPort1 object under dev and returns its
// RFCOMM channel number (the "Path" property).
func (b *bluez) serialPortChannel(dev dbus.ObjectPath) (uint16, bool) {
	objs, err := b.managed()
	if err != nil {
		return 0, false
	}
	for path, ifaces := range objs {
		sp, ok := ifaces["org.bluez.SerialPort1"]
		if !ok || !strings.HasPrefix(string(path), string(dev)) {
			continue
		}
		ch, ok := sp["Path"].Value().(uint16)
		return ch, ok
	}
	return 0, false
}

// connect opens the RFCOMM channel via BlueZ and waits for the SerialPort1
// object to appear. Returns the /dev/rfcomm<N> file to read/write.
func (b *bluez) connect(ctx context.Context, d *device) (*os.File, error) {
	fmt.Printf("connecting to %s (%s)...\n", d.name, d.addr)
	if err := b.call(d.path, "org.bluez.Device1.Connect"); err != nil {
		return nil, fmt.Errorf("Device1.Connect: %w (on the phone: Bluetooth on, app in Host mode)", err)
	}

	// BlueZ publishes org.bluez.SerialPort1 under the device path once the
	// SPP channel is up; its "Path" property is the RFCOMM channel number.
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if ch, ok := b.serialPortChannel(d.path); ok {
			// Re-check the channel is still live: if it closed between the two
			// lookups, the node would be stale.
			if ch2, ok := b.serialPortChannel(d.path); ok && ch2 == ch {
				f, err := os.OpenFile(rfcommDir()+fmt.Sprintf("/rfcomm%d", ch), os.O_RDWR, 0)
				if err != nil {
					return nil, fmt.Errorf("open %s/rfcomm%d: %w (on real hardware try adding your user to the 'dialout' group)", rfcommDir(), ch, err)
				}
				fmt.Printf("connected! RFCOMM channel %d -> %s\n", ch, f.Name())
				return f, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for SerialPort1 (channel never appeared)")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Chat loop — mirrors MessageActivity.kt exactly: raw UTF-8 bytes both ways
// ---------------------------------------------------------------------------

func chat(ctx context.Context, f *os.File) {
	defer f.Close()

	// A blocked Read only returns when data arrives or the file closes, so
	// close the socket as soon as the context is cancelled (Ctrl-C).
	go func() {
		<-ctx.Done()
		f.Close()
	}()

	var wg sync.WaitGroup

	// Incoming: each read chunk == one message (the app does one write per message).
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := f.Read(buf)
			if n > 0 {
				fmt.Printf("\n\033[1;34mphone:\033[0m %s", string(buf[:n]))
			}
			if err != nil || ctx.Err() != nil {
				return
			}
		}
	}()

	// Outgoing: one line per message, written as raw UTF-8 (no newline added —
	// same as the app's outputStream.write(message.toByteArray(UTF_8))).
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("type a message and press Enter (Ctrl-D to exit)")
	for scanner.Scan() {
		msg := strings.TrimSpace(scanner.Text())
		if msg == "" {
			continue
		}
		if _, err := f.Write([]byte(msg)); err != nil {
			fmt.Fprintf(os.Stderr, "send failed: %v\n", err)
			return
		}
		fmt.Printf("\033[1;32mme:\033[0m %s\n", msg)
	}

	// stdin closed (Ctrl-D / pipe ended): give the reader a moment to display
	// any reply that is already in flight, then close the socket so it exits.
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(500 * time.Millisecond):
	}
	f.Close()
	wg.Wait()
	fmt.Println("\nconnection closed")
}

// ---------------------------------------------------------------------------

func main() {
	listOnly := flag.Bool("list", false, "list devices advertising the GhostLink SPP service and exit")
	addr := flag.String("addr", "", "connect directly to this MAC address (skips scanning)")
	scanDur := flag.Duration("scan", 15*time.Second, "how long to scan when -addr is not given")
	flag.Parse()

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		fmt.Fprintln(os.Stderr, "system D-Bus:", err)
		os.Exit(1)
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	bz := &bluez{conn: conn}

	// -list: just show devices advertising the GhostLink SPP service, no scan.
	if *listOnly {
		for _, d := range bz.findDevices(true) {
			fmt.Printf("%s  %s\n", d.addr, d.name)
		}
		return
	}

	var target *device
	switch {
	case *addr != "":
		for _, d := range bz.findDevices(false) {
			if strings.EqualFold(d.addr, *addr) {
				target = &d
				break
			}
		}
		if target == nil {
			fmt.Fprintln(os.Stderr, "device not found in BlueZ (pair it first: bluetoothctl -> pair <MAC>)")
			os.Exit(1)
		}
	default:
		target, err = bz.scanForGhostlink(ctx, *scanDur)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	f, err := bz.connect(ctx, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	chat(ctx, f)
}
