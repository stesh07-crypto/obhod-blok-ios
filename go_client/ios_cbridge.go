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
	"unsafe"
)

var (
	swiftWritePacketCallback C.write_packet_func
	packetsFromSwift         = make(chan []byte, 1024)
)

//export WDTT_SetWriteCallback
func WDTT_SetWriteCallback(cb C.write_packet_func) {
	swiftWritePacketCallback = cb
}

//export WDTT_WritePacket
func WDTT_WritePacket(data unsafe.Pointer, length C.int) {
	pkt := C.GoBytes(data, length)
	buf := make([]byte, length)
	copy(buf, pkt)
	select {
	case packetsFromSwift <- buf:
	default:
		// drop packet if channel is full
	}
}

func sendPacketToSwift(data []byte) {
	if swiftWritePacketCallback != nil && len(data) > 0 {
		C.call_write_packet(swiftWritePacketCallback, unsafe.Pointer(&data[0]), C.int(len(data)))
	}
}
