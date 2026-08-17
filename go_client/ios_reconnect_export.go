//go:build ios

package main

/*
#include <stdint.h>
*/
import "C"

// WDTT_RequestReconnect asks the existing iOS runtime to rebuild only the
// transport attempt. NetworkExtension and the packet-tunnel session stay alive.
//export WDTT_RequestReconnect
func WDTT_RequestReconnect() C.int {
	rt := currentIOSRuntime()
	if rt == nil || rt.ctx.Err() != nil {
		return 0
	}
	if rt.requestReconnect("ручная команда из приложения") {
		return 1
	}
	return 0
}
