//go:build ios

package main

import "encoding/json"

type iosLiveStatsPayload struct {
	ActiveConnections int32 `json:"activeConnections"`
	UploadBytes       int64 `json:"uploadBytes"`
	DownloadBytes     int64 `json:"downloadBytes"`
}

func publishStats(s *Stats) {
	if s == nil {
		return
	}
	payload := iosLiveStatsPayload{
		ActiveConnections: s.ActiveConnections.Load(),
		UploadBytes:       s.TotalBytesUp.Load(),
		DownloadBytes:     s.TotalBytesDown.Load(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	iosStats(string(data))
}
