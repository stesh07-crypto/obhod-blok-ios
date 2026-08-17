//go:build ios

package main

import (
	"encoding/json"
	"sync"
	"time"
)

type iosLiveStatsPayload struct {
	ActiveConnections int32 `json:"activeConnections"`
	UploadBytes       int64 `json:"uploadBytes"`
	DownloadBytes     int64 `json:"downloadBytes"`
}

const (
	iOSZeroWorkerMinChecks         = 5
	iOSZeroWorkerGracePeriod       = 30 * time.Second
	iOSZeroWorkerReconnectCooldown = 90 * time.Second
)

var (
	iOSStatsWatchMu            sync.Mutex
	iOSStatsWatchRuntime       *iosRuntime
	iOSStatsWatchHadLive       bool
	iOSStatsWatchZeroSince     time.Time
	iOSStatsWatchZeroChecks    int
	iOSStatsWatchLastReconnect time.Time
)

func publishStats(s *Stats) {
	if s == nil {
		return
	}

	active := s.ActiveConnections.Load()
	payload := iosLiveStatsPayload{
		ActiveConnections: active,
		UploadBytes:       s.TotalBytesUp.Load(),
		DownloadBytes:     s.TotalBytesDown.Load(),
	}
	data, err := json.Marshal(payload)
	if err == nil {
		iosStats(string(data))
	}

	watchActiveConnections(active)
}

// iOS can briefly report zero active TURN/DTLS workers during normal recovery,
// sleep/wake, or a physical-network handoff. A single zero sample must never
// rebuild an otherwise healthy VPN session. Once this runtime has demonstrated
// at least one live connection, require a sustained zero window with several
// consecutive checks before rebuilding only the Go transport. A separate
// cooldown prevents a difficult network from entering a periodic restart loop.
func watchActiveConnections(active int32) {
	rt := currentIOSRuntime()
	if rt == nil || rt.ctx.Err() != nil {
		return
	}

	now := time.Now()
	triggerReconnect := false

	iOSStatsWatchMu.Lock()
	if iOSStatsWatchRuntime != rt {
		iOSStatsWatchRuntime = rt
		iOSStatsWatchHadLive = false
		iOSStatsWatchZeroSince = time.Time{}
		iOSStatsWatchZeroChecks = 0
		iOSStatsWatchLastReconnect = time.Time{}
	}

	if active > 0 {
		iOSStatsWatchHadLive = true
		iOSStatsWatchZeroSince = time.Time{}
		iOSStatsWatchZeroChecks = 0
	} else if iOSStatsWatchHadLive {
		if iOSStatsWatchZeroSince.IsZero() {
			iOSStatsWatchZeroSince = now
			iOSStatsWatchZeroChecks = 1
		} else {
			iOSStatsWatchZeroChecks++
		}

		zeroConfirmed := iOSStatsWatchZeroChecks >= iOSZeroWorkerMinChecks &&
			now.Sub(iOSStatsWatchZeroSince) >= iOSZeroWorkerGracePeriod
		cooldownReady := iOSStatsWatchLastReconnect.IsZero() ||
			now.Sub(iOSStatsWatchLastReconnect) >= iOSZeroWorkerReconnectCooldown

		if zeroConfirmed && cooldownReady {
			// Start a new confirmation window now. If requestReconnect is temporarily
			// rejected (for example another handoff repair just won the race), we will
			// wait through another full grace period instead of retrying every 2s.
			iOSStatsWatchZeroSince = now
			iOSStatsWatchZeroChecks = 0
			triggerReconnect = true
		}
	}
	iOSStatsWatchMu.Unlock()

	if triggerReconnect && rt.requestReconnect("watchdog: 0 активных подключений подтверждено 30 секунд") {
		iOSStatsWatchMu.Lock()
		if iOSStatsWatchRuntime == rt {
			iOSStatsWatchLastReconnect = time.Now()
		}
		iOSStatsWatchMu.Unlock()
	}
}
