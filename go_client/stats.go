package main

import (
	"log"
	"sync/atomic"
	"time"
)

type Stats struct {
	TotalBytesUp      atomic.Int64
	TotalBytesDown    atomic.Int64
	ActiveConnections atomic.Int32
	// DtlsTxBytes/DtlsRxBytes isolate the dispatcher->DTLS and DTLS->dispatcher
	// legs, so a stalled data plane can be localised without a packet capture.
	DtlsTxBytes atomic.Int64
	DtlsRxBytes atomic.Int64
	// WorkerSlots is the number of DTLS sessions currently registered in the
	// dispatcher. It can differ from ActiveConnections while sessions rotate.
	WorkerSlots atomic.Int32
}

func NewStats() *Stats {
	return &Stats{}
}

func (s *Stats) RunLoop(shutdown <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
			active := s.ActiveConnections.Load()
			up := s.TotalBytesUp.Load()
			down := s.TotalBytesDown.Load()
			upMB := float64(up) / (1024.0 * 1024.0)
			downMB := float64(down) / (1024.0 * 1024.0)
			totalMB := upMB + downMB

			publishStats(s)
			log.Printf("[СТАТИСТИКА] Активных: %d | ↓ %.2f МБ | ↑ %.2f МБ | Всего: %.2f МБ", active, downMB, upMB, totalMB)
		}
	}
}
