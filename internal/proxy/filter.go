package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Client-to-server RFB message types.
const (
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3
	msgKeyEvent                 = 4
	msgPointerEvent             = 5
	msgClientCutText            = 6
	msgEnableContinuousUpdates  = 150
	msgClientFence              = 151
	msgSetDesktopSize           = 250
	msgQEMU                     = 255
)

// copyClientMessages reads client-to-server RFB messages from src and writes
// them to dst. When viewOnly is set, input messages (keyboard, pointer,
// clipboard) are dropped so a viewer cannot affect the guest even with a
// modified browser client.
func copyClientMessages(dst io.Writer, src io.Reader, viewOnly bool) error {
	r := &countingReader{Reader: src}
	buf := make([]byte, 0, 4096)
	for {
		buf = buf[:0]
		hdr := make([]byte, 1)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return err
		}
		size, err := clientMessageSize(hdr[0], r)
		if err != nil {
			return err
		}
		// clientMessageSize may have consumed a prefix of the body while
		// figuring out the length; it hands that prefix back in r.prefix.
		body := make([]byte, size)
		copy(body, r.prefix)
		if _, err := io.ReadFull(r, body[len(r.prefix):]); err != nil {
			return err
		}
		r.prefix = nil

		if viewOnly {
			switch hdr[0] {
			case msgKeyEvent, msgPointerEvent, msgClientCutText, msgSetDesktopSize:
				continue
			case msgQEMU:
				continue
			}
		}
		buf = append(buf, hdr[0])
		buf = append(buf, body...)
		if _, err := dst.Write(buf); err != nil {
			return err
		}
	}
}

type countingReader struct {
	io.Reader
	prefix []byte
}

// clientMessageSize returns the body length (excluding the type byte) of a
// client message. For variable-length messages it reads the fixed header
// into r.prefix so the caller can splice it back.
func clientMessageSize(t byte, r *countingReader) (int, error) {
	read := func(n int) ([]byte, error) {
		b := make([]byte, n)
		if _, err := io.ReadFull(r.Reader, b); err != nil {
			return nil, err
		}
		r.prefix = append(r.prefix, b...)
		return b, nil
	}
	switch t {
	case msgSetPixelFormat:
		return 19, nil
	case msgSetEncodings:
		b, err := read(3)
		if err != nil {
			return 0, err
		}
		n := int(binary.BigEndian.Uint16(b[1:3]))
		return 3 + 4*n, nil
	case msgFramebufferUpdateRequest:
		return 9, nil
	case msgKeyEvent:
		return 7, nil
	case msgPointerEvent:
		return 5, nil
	case msgClientCutText:
		b, err := read(7)
		if err != nil {
			return 0, err
		}
		n := int32(binary.BigEndian.Uint32(b[3:7]))
		if n < 0 {
			n = -n // extended clipboard pseudo-encoding
		}
		if n > 64<<20 {
			return 0, fmt.Errorf("clipboard message too large: %d", n)
		}
		return 7 + int(n), nil
	case msgEnableContinuousUpdates:
		return 9, nil
	case msgClientFence:
		b, err := read(8)
		if err != nil {
			return 0, err
		}
		return 8 + int(b[7]), nil
	case msgSetDesktopSize:
		b, err := read(7)
		if err != nil {
			return 0, err
		}
		return 7 + 16*int(b[5]), nil
	case msgQEMU:
		b, err := read(1)
		if err != nil {
			return 0, err
		}
		switch b[0] {
		case 0: // extended key event
			return 1 + 10, nil
		case 1: // audio
			sub, err := read(2)
			if err != nil {
				return 0, err
			}
			if binary.BigEndian.Uint16(sub) == 2 { // set format: +6
				return 3 + 6, nil
			}
			return 3, nil
		}
		return 0, fmt.Errorf("unknown QEMU client submessage %d", b[0])
	}
	return 0, fmt.Errorf("unknown client message type %d", t)
}
