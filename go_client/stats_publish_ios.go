//go:build ios

package main

import (
	"encoding/json"
	"fmt"
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
	iOSStatsWatchLastActive    int32 = -1
	iOSStatsWatchLastBand      int   = -1
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

func activeWorkerBand(active int32) int {
	switch {
	case active <= 0:
		return 0
	case active < 18:
		return 1
	case active < 54:
		return 2
	case active < 90:
		return 3
	default:
		return 4
	}
}

// iOS can briefly report zero active TURN/DTLS workers during normal recovery,
// sleep/wake, or a physical-network handoff. A single zero sample must never
// rebuild an otherwise healthy VPN session. Once this runtime has demonstrated
// at least one live connection, require a sustained zero window with several
// consecutive checks before rebuilding only the Go transport. A separate
// cooldown prevents a difficult network from entering a periodic restart loop.
//
// Diagnostic logging here is deliberately event-based rather than every 2s:
// we record meaningful worker-band changes, the start/end of a zero-worker
// window, and the exact moment the zero-worker watchdog asks for a reconnect.
func watchActiveConnections(active int32) {
	rt := currentIOSRuntime()
	if rt == nil || rt.ctx.Err() != nil {
		return
	}

	now := time.Now()
	triggerReconnect := false
	var diagnosticLines []string

	iOSStatsWatchMu.Lock()
	if iOSStatsWatchRuntime != rt {
		iOSStatsWatchRuntime = rt
		iOSStatsWatchHadLive = false
		iOSStatsWatchZeroSince = time.Time{}
		iOSStatsWatchZeroChecks = 0
		iOSStatsWatchLastReconnect = time.Time{}
		iOSStatsWatchLastActive = -1
		iOSStatsWatchLastBand = -1
	}

	band := activeWorkerBand(active)
	if iOSStatsWatchLastActive < 0 {
		iOSStatsWatchLastActive = active
		iOSStatsWatchLastBand = band
	} else if band != iOSStatsWatchLastBand {
		direction := "восстановление"
		if band < iOSStatsWatchLastBand {
			direction = "деградация"
		}
		diagnosticLines = append(diagnosticLines,
			fmt.Sprintf("[ДИАГ] Workers: %d → %d (%s, band %d→%d)",
				iOSStatsWatchLastActive, active, direction, iOSStatsWatchLastBand, band))
		iOSStatsWatchLastBand = band
		iOSStatsWatchLastActive = active
	} else {
		iOSStatsWatchLastActive = active
	}

	if active > 0 {
		if !iOSStatsWatchZeroSince.IsZero() {
			diagnosticLines = append(diagnosticLines,
				fmt.Sprintf("[ДИАГ] Workers восстановились: active=%d после %.1fс на нуле (%d проверок)",
					active, now.Sub(iOSStatsWatchZeroSince).Seconds(), iOSStatsWatchZeroChecks))
		}
		iOSStatsWatchHadLive = true
		iOSStatsWatchZeroSince = time.Time{}
		iOSStatsWatchZeroChecks = 0
	} else if iOSStatsWatchHadLive {
		if iOSStatsWatchZeroSince.IsZero() {
			iOSStatsWatchZeroSince = now
			iOSStatsWatchZeroChecks = 1
			diagnosticLines = append(diagnosticLines,
				"[ДИАГ] Workers упали до 0: запускаем 30с окно подтверждения, VPN пока не перезапускаем")
		} else {
			iOSStatsWatchZeroChecks++
		}

		zeroConfirmed := iOSStatsWatchZeroChecks >= iOSZeroWorkerMinChecks &&
			now.Sub(iOSStatsWatchZeroSince) >= iOSZeroWorkerGracePeriod
		cooldownReady := iOSStatsWatchLastReconnect.IsZero() ||
			now.Sub(iOSStatsWatchLastReconnect) >= iOSZeroWorkerReconnectCooldown

		if zeroConfirmed && cooldownReady {
			diagnosticLines = append(diagnosticLines,
				fmt.Sprintf("[ДИАГ] Zero-worker watchdog подтверждён: 0 workers %.1fс, checks=%d, cooldown=OK → запрос reconnect",
					now.Sub(iOSStatsWatchZeroSince).Seconds(), iOSStatsWatchZeroChecks))
			// Start a new confirmation window now. If requestReconnect is temporarily
			// rejected (for example another repair just won the race), we wait through
			// another full grace period instead of retrying every 2s.
			iOSStatsWatchZeroSince = now
			iOSStatsWatchZeroChecks = 0
			triggerReconnect = true
		}
	}
	iOSStatsWatchMu.Unlock()

	for _, line := range diagnosticLines {
		iosLog(line, false)
	}

	if triggerReconnect {
		accepted := rt.requestReconnect("watchdog: 0 активных подключений подтверждено 30 секунд")
		if accepted {
			iOSStatsWatchMu.Lock()
			if iOSStatsWatchRuntime == rt {
				iOSStatsWatchLastReconnect = time.Now()
			}
			iOSStatsWatchMu.Unlock()
		} else {
			iosLog("[ДИАГ] Zero-worker watchdog запросил reconnect, но runtime его не принял (busy/stopping/no attempt)", false)
		}
	}
}
