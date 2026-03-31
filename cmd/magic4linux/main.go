package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bendahl/uinput"

	"github.com/mafredri/magic4linux/m4p"
)

const broadcastPort = 42830

func main() {
	interval := flag.Int("interval", 16, "remote update interval in milliseconds (16 ≈ 60fps, lower = smoother)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, *interval); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, interval int) error {
	kbd, err := uinput.CreateKeyboard("/dev/uinput", []byte("magic4linux-keyboard"))
	if err != nil {
		return fmt.Errorf("create keyboard: %w\nTip: add yourself to the input group: sudo usermod -aG input $USER && newgrp input", err)
	}
	defer kbd.Close()

	mouse, err := uinput.CreateMouse("/dev/uinput", []byte("magic4linux-mouse"))
	if err != nil {
		return fmt.Errorf("create mouse: %w", err)
	}
	defer mouse.Close()

	d, err := m4p.NewDiscoverer(broadcastPort)
	if err != nil {
		return err
	}
	defer d.Close()

	log.Printf("Listening for magic4pc TV app on UDP broadcast port %d ...", broadcastPort)
	log.Printf("Open the magic4pc app on your LG TV and connect it to this machine.")

	for {
		select {
		case <-ctx.Done():
			return nil
		case dev := <-d.NextDevice():
			if err := connect(ctx, dev, kbd, mouse, interval); err != nil {
				log.Printf("connection lost: %v", err)
			}
		}
	}
}

func connect(ctx context.Context, dev m4p.DeviceInfo, kbd uinput.Keyboard, mouse uinput.Mouse, interval int) error {
	addr := fmt.Sprintf("%s:%d", dev.IPAddr, dev.Port)
	log.Printf("Connecting to %s (model: %s)...", addr, dev.Model)

	client, err := m4p.Dial(ctx, addr, m4p.WithUpdateFrequency(interval))
	if err != nil {
		return err
	}
	defer client.Close()

	log.Printf("Connected. Use your LG Magic Remote to control the mouse.")

	// lastX/lastY track the previous absolute IR pointer position so we can
	// compute relative deltas for the mouse. hasPointer is reset whenever the
	// pointer leaves the screen to avoid a big jump on re-entry.
	var (
		lastX, lastY int32
		hasPointer   bool
	)

	for {
		m, err := client.Recv(ctx)
		if err != nil {
			return err
		}

		switch m.Type {
		case m4p.RemoteUpdateMessage:
			// Binary payload layout (little-endian):
			//   1 byte  returnValue  — 0 = pointer not on screen
			//   1 byte  deviceID
			//   4 bytes coordinate X (int32)
			//   4 bytes coordinate Y (int32)
			//   ... gyroscope/acceleration/quaternion (unused)
			r := bytes.NewReader(m.RemoteUpdate.Payload)
			var returnValue, deviceID uint8
			var coordinate [2]int32
			if err := binary.Read(r, binary.LittleEndian, &returnValue); err != nil {
				continue
			}
			if err := binary.Read(r, binary.LittleEndian, &deviceID); err != nil {
				continue
			}
			if err := binary.Read(r, binary.LittleEndian, coordinate[:]); err != nil {
				continue
			}

			// returnValue == 0 means the IR pointer is off-screen.
			// Reset tracking so the cursor doesn't jump when it comes back.
			if returnValue == 0 {
				hasPointer = false
				continue
			}

			x, y := coordinate[0], coordinate[1]
			if hasPointer {
				dx := x - lastX
				dy := y - lastY
				if dx < 0 {
					mouse.MoveLeft(-dx) //nolint:errcheck
				} else if dx > 0 {
					mouse.MoveRight(dx) //nolint:errcheck
				}
				if dy < 0 {
					mouse.MoveUp(-dy) //nolint:errcheck
				} else if dy > 0 {
					mouse.MoveDown(dy) //nolint:errcheck
				}
			}
			hasPointer = true
			lastX, lastY = x, y

		case m4p.MouseMessage:
			// Fired when the user clicks the Magic Remote's scroll-wheel button
			// while the IR pointer is active.
			switch m.Mouse.Type {
			case "mousedown":
				mouse.LeftPress() //nolint:errcheck
			case "mouseup":
				mouse.LeftRelease() //nolint:errcheck
			}

		case m4p.WheelMessage:
			mouse.Wheel(false, m.Wheel.Delta) //nolint:errcheck

		case m4p.InputMessage:
			key := m.Input.Parameters.KeyCode
			switch key {
			case m4p.KeyWheelPressed:
				key = uinput.KeyEnter
			case m4p.KeyChannelUp:
				key = uinput.KeyPageup
			case m4p.KeyChannelDown:
				key = uinput.KeyPagedown
			case m4p.KeyLeft:
				key = uinput.KeyLeft
			case m4p.KeyUp:
				key = uinput.KeyUp
			case m4p.KeyRight:
				key = uinput.KeyRight
			case m4p.KeyDown:
				key = uinput.KeyDown
			case m4p.Key0:
				key = uinput.Key0
			case m4p.Key1:
				key = uinput.Key1
			case m4p.Key2:
				key = uinput.Key2
			case m4p.Key3:
				key = uinput.Key3
			case m4p.Key4:
				key = uinput.Key4
			case m4p.Key5:
				key = uinput.Key5
			case m4p.Key6:
				key = uinput.Key6
			case m4p.Key7:
				key = uinput.Key7
			case m4p.Key8:
				key = uinput.Key8
			case m4p.Key9:
				key = uinput.Key9
			case m4p.KeyRed:
				key = uinput.KeyStop
			case m4p.KeyGreen:
				key = uinput.KeyPlaypause
			case m4p.KeyYellow:
				key = uinput.KeyZ
			case m4p.KeyBlue:
				key = uinput.KeyC
			case m4p.KeyBack:
				key = uinput.KeyBackspace
			}

			if m.Input.Parameters.IsDown {
				kbd.KeyDown(key) //nolint:errcheck
			} else {
				kbd.KeyUp(key) //nolint:errcheck
			}
		}
	}
}
