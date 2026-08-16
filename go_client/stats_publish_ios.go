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

var (
	iOSStatsWatchMu       sync.Mutex
	iOSStatsWatchRuntime  *iosRuntime
	iOSStatsWatchHadLive  bool
	iOSStatsWatchZeroSince time.Time
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

// Android already has a "0 workers for a long time" zombie watchdog. iOS used
// to rely mostly on TX-without-RX detection, which misses the case where every
// TURN/DTLS worker silently disappears while the phone is idle. Once this
// runtime has demonstrated at least one live connection, keep a zero-worker
// window and rebuild only the transport if it remains empty for 60 seconds.
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
	}

	if active > 0 {
		iOSStatsWatchHadLive = true
		iOSStatsWatchZeroSince = time.Time{}
	} else if iOSStatsWatchHadLive {
		if iOSStatsWatchZeroSince.IsZero() {
			iOSStatsWatchZeroSince = now
		} else if now.Sub(iOSStatsWatchZeroSince) >= 60*time.Second {
			// Start a fresh window immediately. If a difficult network still cannot
			// recover, another bounded repair may happen after another full minute,
			// never in a tight reconnect loop.
			iOSStatsWatchZeroSince = now
			triggerReconnect = true
		}
	}
	iOSStatsWatchMu.Unlock()

	if triggerReconnect {
		rt.requestReconnect("watchdog: 0 активных подключений 60 секунд")
	}
}
