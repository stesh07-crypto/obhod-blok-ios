//go:build ios

package main

import "runtime/debug"

const (
	// Build 162 on-device diagnostics showed the NetworkExtension disappearing
	// without stopTunnel immediately after phys_footprint reached ~40.9 MiB,
	// while all 108 workers were still alive. Keep the Go-managed working set
	// below that observed kill zone and make GC react earlier during the session
	// ramp. This is a soft runtime policy only: it does not alter worker count,
	// TURN/DTLS, WireGuard, packet routing, reconnects, or wire format.
	iOSGoMemoryLimitBytes int64 = 35 * 1024 * 1024
	iOSGCPercent                = 50
)

func init() {
	// SetMemoryLimit is a soft limit. The runtime can exceed it when memory is
	// genuinely live, but it will collect more aggressively before allowing the
	// heap to grow. GOGC=50 complements it by starting collections earlier than
	// the default during the steep worker startup ramp observed on-device.
	debug.SetMemoryLimit(iOSGoMemoryLimitBytes)
	debug.SetGCPercent(iOSGCPercent)
}
