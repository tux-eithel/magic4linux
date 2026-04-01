package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/bendahl/uinput"
	"github.com/gorilla/websocket"
)

// MagicRemoteService binary message types (byte 0 of every frame).
const (
	msgPositionRelative byte = 0x00
	msgPositionAbsolute byte = 0x01
	msgWheel            byte = 0x02
	msgVisible          byte = 0x03
	msgKey              byte = 0x04
	msgUnicode          byte = 0x05
	msgShutdown         byte = 0x06
)

// keyMap maps JavaScript keyCode values (sent by the TV app) to Linux uinput key codes.
// See PROTOCOL.md for the full key-code table.
var keyMap = map[uint16]int{
	0x0008: uinput.KeyBackspace,
	0x000D: uinput.KeyEnter,
	0x0025: uinput.KeyLeft,
	0x0026: uinput.KeyUp,
	0x0027: uinput.KeyRight,
	0x0028: uinput.KeyDown,
	0x0030: uinput.Key0,
	0x0031: uinput.Key1,
	0x0032: uinput.Key2,
	0x0033: uinput.Key3,
	0x0034: uinput.Key4,
	0x0035: uinput.Key5,
	0x0036: uinput.Key6,
	0x0037: uinput.Key7,
	0x0038: uinput.Key8,
	0x0039: uinput.Key9,
	0x0021: uinput.KeyPageup,    // Page Up
	0x0022: uinput.KeyPagedown,  // Page Down
	0x0193: uinput.KeyStop,      // Red button
	0x0194: uinput.KeyPlaypause, // Green button
	0x019D: uinput.KeyStop,      // Stop
	0x019F: uinput.KeyPlaypause, // Play
	0x01CD: uinput.KeyEsc,       // Back
}

var upgrader = websocket.Upgrader{
	// Accept connections from any origin (the TV app does not send an Origin header).
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	port := flag.Int("port", 41230, "TCP port to listen on (must match the port set in the MagicRemoteService TV app)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, *port); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, port int) error {
	kbd, err := uinput.CreateKeyboard("/dev/uinput", []byte("magic4linux-keyboard"))
	if err != nil {
		return fmt.Errorf("create keyboard: %w\nTip: sudo usermod -aG input $USER && newgrp input", err)
	}
	defer kbd.Close()

	mouse, err := uinput.CreateMouse("/dev/uinput", []byte("magic4linux-mouse"))
	if err != nil {
		return fmt.Errorf("create mouse: %w", err)
	}
	defer mouse.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", makeWSHandler(kbd, mouse))

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background()) //nolint:errcheck
	}()

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}

	log.Printf("Listening on TCP port %d — open MagicRemoteService on your LG TV and point it at this machine's IP.", port)

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func makeWSHandler(kbd uinput.Keyboard, mouse uinput.Mouse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		log.Printf("TV connected from %s", r.RemoteAddr)
		defer log.Printf("TV disconnected from %s", r.RemoteAddr)

		// lastX/lastY track the previous absolute pointer position so we can
		// convert PositionAbsolute frames into relative mouse deltas.
		// hasAbsPos is reset on Visible=false to avoid jumps on re-entry.
		var (
			lastX, lastY int16
			hasAbsPos    bool
		)

		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("connection error: %v", err)
				}
				return
			}
			if msgType != websocket.BinaryMessage || len(data) == 0 {
				continue
			}

			switch data[0] {
			case msgPositionRelative: // [0x00][dx int16 LE][dy int16 LE]
				if len(data) < 5 {
					continue
				}
				dx := int16(binary.LittleEndian.Uint16(data[1:3]))
				dy := int16(binary.LittleEndian.Uint16(data[3:5]))
				moveMouse(mouse, dx, dy)

			case msgPositionAbsolute: // [0x01][x uint16 LE 0–1920][y uint16 LE 0–1080]
				if len(data) < 5 {
					continue
				}
				x := int16(binary.LittleEndian.Uint16(data[1:3]))
				y := int16(binary.LittleEndian.Uint16(data[3:5]))
				if hasAbsPos {
					moveMouse(mouse, x-lastX, y-lastY)
				}
				hasAbsPos = true
				lastX, lastY = x, y

			case msgWheel: // [0x02][sY int16 LE]
				if len(data) < 3 {
					continue
				}
				delta := int16(binary.LittleEndian.Uint16(data[1:3]))
				mouse.Wheel(false, int32(delta/20)) //nolint:errcheck

			case msgVisible: // [0x03][visible uint8]
				// Reset absolute tracking when the pointer leaves the screen.
				hasAbsPos = false

			case msgKey: // [0x04][keyCode uint16 LE][state uint8: 0x01=down, 0x00=up]
				if len(data) < 4 {
					continue
				}
				code := binary.LittleEndian.Uint16(data[1:3])
				down := data[3] == 0x01
				handleKey(kbd, mouse, code, down)

			case msgUnicode: // [0x05][codePoint uint16 LE]
				if len(data) >= 3 {
					log.Printf("unicode input U+%04X (not supported via uinput)", binary.LittleEndian.Uint16(data[1:3]))
				}

			case msgShutdown:
				log.Printf("TV requested system shutdown")

			default:
				log.Printf("unknown message 0x%02X: %X", data[0], data)
			}
		}
	}
}

func moveMouse(mouse uinput.Mouse, dx, dy int16) {
	if dx < 0 {
		mouse.MoveLeft(int32(-dx)) //nolint:errcheck
	} else if dx > 0 {
		mouse.MoveRight(int32(dx)) //nolint:errcheck
	}
	if dy < 0 {
		mouse.MoveUp(int32(-dy)) //nolint:errcheck
	} else if dy > 0 {
		mouse.MoveDown(int32(dy)) //nolint:errcheck
	}
}

func handleKey(kbd uinput.Keyboard, mouse uinput.Mouse, code uint16, down bool) {
	switch code {
	case 0x0001: // pointer click → left mouse button
		if down {
			mouse.LeftPress() //nolint:errcheck
		} else {
			mouse.LeftRelease() //nolint:errcheck
		}
	case 0x0002: // long-press on touchpad → right mouse button
		if down {
			mouse.RightPress() //nolint:errcheck
		} else {
			mouse.RightRelease() //nolint:errcheck
		}
	case 0x0195: // Yellow button → right mouse button
		if down {
			mouse.RightPress() //nolint:errcheck
		} else {
			mouse.RightRelease() //nolint:errcheck
		}
	case 0x0196: // Blue button → middle mouse button
		if down {
			mouse.MiddlePress() //nolint:errcheck
		} else {
			mouse.MiddleRelease() //nolint:errcheck
		}
	default:
		if key, ok := keyMap[code]; ok {
			if down {
				kbd.KeyDown(key) //nolint:errcheck
			} else {
				kbd.KeyUp(key) //nolint:errcheck
			}
		} else {
			log.Printf("unmapped key 0x%04X (down=%v) — add it to keyMap if needed", code, down)
		}
	}
}
