package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// diagSnapshot is a compact, secret-free view of the data plane. It is logged on
// wake, on physical-network changes, on failed connectivity checks and after a
// repair attempt, so a device log alone shows where the exchange stops.
type diagSnapshot struct {
	Reason         string
	At             time.Time
	Build          string
	Generation     uint64
	ConfigReady    bool
	WGReady        bool
	WGHandshakeAge time.Duration
	WGRxBytes      int64
	WGTxBytes      int64
	TunUpBytes     int64
	TunDownBytes   int64
	DtlsTxBytes    int64
	DtlsRxBytes    int64
	Workers        int32
	Active         int32
	Physical       string
	Repairs        int
	// Deltas since the previous snapshot make a frozen data plane obvious even
	// when absolute counters are large.
	SinceLast    time.Duration
	TunUpDelta   int64
	TunDownDelta int64
	DtlsTxDelta  int64
	DtlsRxDelta  int64
}

func (s diagSnapshot) String() string {
	handshake := "none"
	if s.WGHandshakeAge > 0 {
		handshake = s.WGHandshakeAge.Round(time.Second).String() + " ago"
	}
	physical := s.Physical
	if physical == "" {
		physical = "unknown"
	}
	delta := ""
	if s.SinceLast > 0 {
		delta = fmt.Sprintf(
			" delta=%.0fs,tun_up:%+d,tun_down:%+d,dtls_tx:%+d,dtls_rx:%+d",
			s.SinceLast.Seconds(),
			s.TunUpDelta,
			s.TunDownDelta,
			s.DtlsTxDelta,
			s.DtlsRxDelta,
		)
	}
	return fmt.Sprintf(
		"[ДИАГ-СНИМОК] reason=%s at=%s build=%s gen=%d cfg=%s wg=%s wg_hs=%s wg_rx=%d wg_tx=%d tun_up=%d tun_down=%d dtls_tx=%d dtls_rx=%d workers=%d active=%d phys=%s repairs=%d%s",
		s.Reason,
		s.At.Format(time.RFC3339),
		s.Build,
		s.Generation,
		onOff(s.ConfigReady),
		onOff(s.WGReady),
		handshake,
		s.WGRxBytes,
		s.WGTxBytes,
		s.TunUpBytes,
		s.TunDownBytes,
		s.DtlsTxBytes,
		s.DtlsRxBytes,
		s.Workers,
		s.Active,
		physical,
		s.Repairs,
		delta,
	)
}

func onOff(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// parseWGStats extracts the handshake timestamp and byte counters from a
// wireguard-go UAPI dump. Missing or malformed values never panic: callers get
// ok=false when the peer block is absent.
func parseWGStats(uapi string, now time.Time) (lastHandshake time.Time, rxBytes, txBytes int64, ok bool) {
	var handshakeUnix int64
	var handshakeNsec int64

	for _, raw := range strings.Split(uapi, "\n") {
		line := strings.TrimSpace(raw)
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "last_handshake_time_sec":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				handshakeUnix = parsed
				ok = true
			}
		case "last_handshake_time_nsec":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				handshakeNsec = parsed
			}
		case "rx_bytes":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				rxBytes = parsed
				ok = true
			}
		case "tx_bytes":
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				txBytes = parsed
				ok = true
			}
		}
	}

	if handshakeUnix > 0 {
		lastHandshake = time.Unix(handshakeUnix, handshakeNsec)
		if now.Before(lastHandshake) {
			lastHandshake = now
		}
	}
	return lastHandshake, rxBytes, txBytes, ok
}

// repairPolicy bounds automatic transport rebuilds so a broken network cannot
// turn into a restart loop.
type repairPolicy struct {
	cooldown time.Duration
	window   time.Duration
	max      int
}

var defaultRepairPolicy = repairPolicy{
	cooldown: 60 * time.Second,
	window:   10 * time.Minute,
	max:      3,
}

// allow reports whether another repair may start. The returned reason is meant
// for logs; an empty reason means the repair is allowed.
func (p repairPolicy) allow(now, last time.Time, attempts int, windowStart time.Time) (bool, string) {
	if !last.IsZero() && now.Sub(last) < p.cooldown {
		return false, fmt.Sprintf("cooldown %.0fs", (p.cooldown - now.Sub(last)).Seconds())
	}
	if attempts >= p.max && !windowStart.IsZero() && now.Sub(windowStart) < p.window {
		return false, fmt.Sprintf("limit %d/%s", p.max, p.window)
	}
	return true, ""
}

// repairWindowState returns the attempt counter and window start after a new
// repair. Attempts reset once the window expires.
func repairWindowState(now, windowStart time.Time, attempts int, policy repairPolicy) (int, time.Time) {
	if windowStart.IsZero() || now.Sub(windowStart) >= policy.window {
		return 1, now
	}
	return attempts + 1, windowStart
}

// dataPlaneStale reports whether the WireGuard tunnel looks dead while packets
// are still being sent into it: no handshake at all, an old handshake, and TX
// growth since the previous sample.
func dataPlaneStale(now time.Time, lastHandshake time.Time, tunUpNow, tunUpPrevious int64, handshakeMaxAge time.Duration) bool {
	if tunUpNow <= tunUpPrevious {
		return false
	}
	if lastHandshake.IsZero() {
		return true
	}
	return now.Sub(lastHandshake) >= handshakeMaxAge
}

// shouldRestartForMissingConfig decides whether a transport with live DTLS
// sessions but no WireGuard config must be rebuilt. Zero active sessions means
// credentials are still being fetched (for example waiting for a captcha), and
// restarting there only repeats the same authorization attempts.
func shouldRestartForMissingConfig(active int32, configApplied bool, elapsed, limit time.Duration) bool {
	if configApplied || active <= 0 {
		return false
	}
	return elapsed >= limit
}

// shouldRepairStaleDataPlane gates an automatic repair on a live data plane
// attempt: no sessions means there is nothing to repair yet.
func shouldRepairStaleDataPlane(now time.Time, active int32, lastHandshake time.Time, tunUpNow, tunUpPrevious int64, maxAge time.Duration) bool {
	if active <= 0 {
		return false
	}
	return dataPlaneStale(now, lastHandshake, tunUpNow, tunUpPrevious, maxAge)
}
