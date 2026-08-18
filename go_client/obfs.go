// SPDX-License-Identifier: MIT
// obfs.go — WebRTC SRTP-like obfuscation for DTLS traffic
// Each UDP packet is wrapped in an RTP header making it indistinguishable
// from a real WebRTC OPUS audio stream to DPI systems.
//
// Packet format:
//   [RTP Header 12 bytes][ChaCha20-Poly1305 payload+tag][Padding 0-N bytes][PadLen 1 byte]
//
// The RTP header fields (SSRC + SeqNum + Timestamp) form the 12-byte AEAD nonce,
// so no separate nonce prefix is needed.

package main

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

// Cache AEAD instances by the fixed-size 32-byte key. Using [32]byte avoids the
// per-packet []byte -> string conversion that the old sync.Map lookup performed.
var aeadCache sync.Map

func getAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != wrapKeyLen {
		return nil, fmt.Errorf("obfs: key must be %d bytes", wrapKeyLen)
	}
	var keyID [wrapKeyLen]byte
	copy(keyID[:], key)
	if val, ok := aeadCache.Load(keyID); ok {
		return val.(cipher.AEAD), nil
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	aeadCache.Store(keyID, aead)
	return aead, nil
}

// ─── Configuration ───

type ObfsConfig struct {
	SSRC        uint32
	PayloadType uint8
	PaddingMax  int
}

func NewObfsConfig(mode string) *ObfsConfig {
	var buf [4]byte
	_, _ = rand.Read(buf[:])

	pt := uint8(111)
	pad := 24
	if normalizeObfsMode(mode) == "video" {
		pt = 96
		pad = 60
	}

	return &ObfsConfig{
		SSRC:        binary.BigEndian.Uint32(buf[:]),
		PayloadType: pt,
		PaddingMax:  pad,
	}
}

func normalizeObfsMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "video") {
		return "video"
	}
	return "audio"
}

// ─── Per-direction state (sequence + timestamp counters) ───

// ObfsState is per-session/per-direction. The write path is serialized by the
// session goroutine, so its wire buffer can be safely reused after each
// synchronous relay.WriteTo returns. This removes one full packet allocation
// from every wrapped uplink packet — the same class of hot-path allocation that
// caused the reference iOS implementation to hit a GC/jetsam spiral under
// speedtest load.
type ObfsState struct {
	mu      sync.Mutex
	initSeq uint16
	initTs  uint32
	count   uint64

	aead  cipher.AEAD
	nonce [12]byte
	wire  []byte
}

func NewObfsState() *ObfsState {
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	return &ObfsState{
		initSeq: binary.BigEndian.Uint16(buf[0:2]),
		initTs:  binary.BigEndian.Uint32(buf[2:6]),
	}
}

func obfsFillNonce(dst []byte, ssrc uint32, seq uint16, ts uint32) {
	binary.BigEndian.PutUint32(dst[0:4], ssrc)
	binary.BigEndian.PutUint16(dst[4:6], seq)
	dst[6] = 0
	dst[7] = 0
	binary.BigEndian.PutUint32(dst[8:12], ts)
}

// ─── Wrap (encrypt + add RTP header) ───

// obfsWrapPacket wraps a plaintext payload into a reusable RTP-like buffer.
// The returned bytes are valid until the next call using the SAME ObfsState.
func obfsWrapPacket(key, payload []byte, cfg *ObfsConfig, state *ObfsState) ([]byte, error) {
	if len(key) != wrapKeyLen {
		return nil, fmt.Errorf("obfs: key must be %d bytes (got %d)", wrapKeyLen, len(key))
	}
	if len(payload) == 0 {
		return nil, errors.New("obfs: empty payload")
	}
	if state == nil || cfg == nil {
		return nil, errors.New("obfs: nil state/config")
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	c := state.count
	state.count++

	seq := state.initSeq + uint16(c)
	ts := state.initTs + uint32(c)*960 + uint32(c>>16)
	obfsFillNonce(state.nonce[:], cfg.SSRC, seq, ts)

	padRand := 0
	if cfg.PaddingMax > 0 {
		var rndBuf [1]byte
		_, _ = rand.Read(rndBuf[:])
		padRand = int(rndBuf[0]) % cfg.PaddingMax
	}
	padTotal := padRand + 1

	outLen := 12 + len(payload) + chacha20poly1305.Overhead + padTotal
	if cap(state.wire) < outLen {
		// One-time/rare growth per session rather than one allocation per packet.
		state.wire = make([]byte, outLen)
	} else {
		state.wire = state.wire[:outLen]
	}
	out := state.wire

	out[0] = 0x80 | 0x20
	out[1] = cfg.PayloadType & 0x7F
	binary.BigEndian.PutUint16(out[2:4], seq)
	binary.BigEndian.PutUint32(out[4:8], ts)
	binary.BigEndian.PutUint32(out[8:12], cfg.SSRC)

	if state.aead == nil {
		aead, err := getAEAD(key)
		if err != nil {
			return nil, fmt.Errorf("obfs: cipher init: %w", err)
		}
		state.aead = aead
	}
	sealed := state.aead.Seal(out[12:12], state.nonce[:], payload, out[:12])

	padStart := 12 + len(sealed)
	if padRand > 0 {
		_, _ = rand.Read(out[padStart : padStart+padRand])
	}
	out[outLen-1] = byte(padTotal)

	return out, nil
}

// ─── Unwrap (strip RTP header + decrypt) ───

func obfsUnwrapPacket(key, wire, dst []byte) (int, error) {
	if len(key) != wrapKeyLen {
		return 0, fmt.Errorf("obfs: key must be %d bytes (got %d)", wrapKeyLen, len(key))
	}
	if len(wire) < 13 {
		return 0, errors.New("obfs: packet too short")
	}
	if (wire[0] >> 6) != 2 {
		return 0, errors.New("obfs: not RTP v2")
	}

	seq := binary.BigEndian.Uint16(wire[2:4])
	ts := binary.BigEndian.Uint32(wire[4:8])
	ssrc := binary.BigEndian.Uint32(wire[8:12])

	payloadEnd := len(wire)
	if wire[0]&0x20 != 0 {
		padLen := int(wire[len(wire)-1])
		if padLen == 0 || padLen > payloadEnd-12 {
			return 0, fmt.Errorf("obfs: invalid padding length %d", padLen)
		}
		payloadEnd -= padLen
	}

	ciphertextLen := payloadEnd - 12
	if ciphertextLen <= chacha20poly1305.Overhead {
		return 0, errors.New("obfs: no payload after stripping header/padding")
	}
	if ciphertextLen-chacha20poly1305.Overhead > len(dst) {
		return 0, errors.New("obfs: dst buffer too small")
	}

	// Fixed stack nonce: avoids make([]byte, 12) on every received packet.
	var nonce [12]byte
	obfsFillNonce(nonce[:], ssrc, seq, ts)
	aead, err := getAEAD(key)
	if err != nil {
		return 0, fmt.Errorf("obfs: cipher init: %w", err)
	}
	plain, err := aead.Open(dst[:0], nonce[:], wire[12:payloadEnd], wire[:12])
	if err != nil {
		return 0, fmt.Errorf("obfs: auth: %w", err)
	}

	return len(plain), nil
}

func obfsIsRTPPacket(wire []byte) bool {
	if len(wire) < 13 {
		return false
	}
	if (wire[0] >> 6) != 2 {
		return false
	}
	pt := wire[1] & 0x7F
	return pt == 111 || pt == 96
}
