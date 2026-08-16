//go:build ios

package main

/*
#include <stdint.h>
#include <stddef.h>

typedef void (*write_packet_func)(const void *data, int length);
static inline void call_write_packet(write_packet_func cb, const void *data, int length) {
    if (cb) {
        cb(data, length);
    }
}
*/
import "C"

import (
	"sync"
	"unsafe"
)

var (
	packetBridgeMu           sync.RWMutex
	swiftWritePacketCallback C.write_packet_func
	packetsFromSwift         = make(chan []byte, 1024)
)

//export WDTT_SetWriteCallback
func WDTT_SetWriteCallback(cb C.write_packet_func) {
	packetBridgeMu.Lock()
	swiftWritePacketCallback = cb
	packetBridgeMu.Unlock()
}

//export WDTT_WritePacket
func WDTT_WritePacket(data unsafe.Pointer, length C.int) {
	if data == nil || length <= 0 {
		return
	}
	pkt := C.GoBytes(data, length)
	buf := make([]byte, len(pkt))
	copy(buf, pkt)
	select {
	case packetsFromSwift <- buf:
	default:
		// Backpressure must never block NEPacketTunnelFlow. Dropping here is
		// preferable to deadlocking the NetworkExtension process.
	}
}

func sendPacketToSwift(data []byte) {
	if len(data) == 0 {
		return
	}
	packetBridgeMu.RLock()
	cb := swiftWritePacketCallback
	packetBridgeMu.RUnlock()
	if cb != nil {
		C.call_write_packet(cb, unsafe.Pointer(&data[0]), C.int(len(data)))
	}
}

// Remove packets captured while WireGuard was being recreated. Feeding those
// stale packets into the new device can otherwise create a burst of obsolete
// TCP traffic immediately after a Wi-Fi/cellular handoff.
func drainPacketsFromSwift() {
	for {
		select {
		case <-packetsFromSwift:
		default:
			return
		}
	}
}
