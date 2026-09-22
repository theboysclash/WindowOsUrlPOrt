// Package proxy bridges browser WebSocket connections to QEMU's VNC server.
// The proxy authenticates to VNC itself so the per-boot password never leaves
// the host, and presents a password-less RFB handshake to the browser, which
// is already authenticated by the HTTP session.
package proxy

import (
	"crypto/des" //nolint:staticcheck // VNC auth is defined in terms of DES.
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const rfbVersion = "RFB 003.008\n"

const (
	secTypeInvalid = 0
	secTypeNone    = 1
	secTypeVNCAuth = 2
)

// authToServer completes the RFB handshake with the real VNC server using
// VNC authentication, leaving conn positioned right before ClientInit.
func authToServer(conn io.ReadWriter, password string) error {
	ver := make([]byte, 12)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return fmt.Errorf("read server version: %w", err)
	}
	if string(ver[:4]) != "RFB " {
		return fmt.Errorf("not a VNC server: %q", ver)
	}
	if _, err := conn.Write([]byte(rfbVersion)); err != nil {
		return err
	}

	var n [1]byte
	if _, err := io.ReadFull(conn, n[:]); err != nil {
		return fmt.Errorf("read security count: %w", err)
	}
	if n[0] == 0 {
		return errors.New("server refused connection: " + readReason(conn))
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(conn, types); err != nil {
		return err
	}
	chosen := byte(secTypeInvalid)
	for _, t := range types {
		if t == secTypeVNCAuth || (t == secTypeNone && chosen == secTypeInvalid) {
			chosen = t
		}
		if t == secTypeVNCAuth {
			break
		}
	}
	if chosen == secTypeInvalid {
		return fmt.Errorf("server offers no supported security type: %v", types)
	}
	if _, err := conn.Write([]byte{chosen}); err != nil {
		return err
	}

	if chosen == secTypeVNCAuth {
		challenge := make([]byte, 16)
		if _, err := io.ReadFull(conn, challenge); err != nil {
			return fmt.Errorf("read challenge: %w", err)
		}
		resp, err := vncEncrypt(password, challenge)
		if err != nil {
			return err
		}
		if _, err := conn.Write(resp); err != nil {
			return err
		}
	}

	var result [4]byte
	if _, err := io.ReadFull(conn, result[:]); err != nil {
		return fmt.Errorf("read security result: %w", err)
	}
	if binary.BigEndian.Uint32(result[:]) != 0 {
		return errors.New("VNC authentication failed: " + readReason(conn))
	}
	return nil
}

// handshakeWithClient performs the server side of an RFB handshake with the
// browser, offering only the "None" security type.
func handshakeWithClient(conn io.ReadWriter) error {
	if _, err := conn.Write([]byte(rfbVersion)); err != nil {
		return err
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(conn, ver); err != nil {
		return fmt.Errorf("read client version: %w", err)
	}
	if string(ver) != rfbVersion && string(ver) != "RFB 003.007\n" {
		return fmt.Errorf("unsupported client version %q", ver)
	}
	if _, err := conn.Write([]byte{1, secTypeNone}); err != nil {
		return err
	}
	var choice [1]byte
	if _, err := io.ReadFull(conn, choice[:]); err != nil {
		return err
	}
	if choice[0] != secTypeNone {
		return fmt.Errorf("client chose unexpected security type %d", choice[0])
	}
	if string(ver) == rfbVersion {
		if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
			return err
		}
	}
	return nil
}

func readReason(r io.Reader) string {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return "unknown"
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > 1024 {
		return "unknown"
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "unknown"
	}
	return string(buf)
}

// vncEncrypt implements the VNC authentication response: the challenge is
// DES-encrypted with the password, where each key byte has its bits reversed.
func vncEncrypt(password string, challenge []byte) ([]byte, error) {
	key := make([]byte, 8)
	copy(key, password)
	for i := range key {
		key[i] = reverseBits(key[i])
	}
	block, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out, nil
}

func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = r<<1 | b&1
		b >>= 1
	}
	return r
}
