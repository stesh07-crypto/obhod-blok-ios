//go:build ios

package main

/*
#include <stdint.h>
#include <mach/mach.h>
#include <mach/task_info.h>

// task_vm_info.phys_footprint is the closest process-level number to what iOS
// uses for memory-pressure / jetsam decisions. Return 0 if the kernel query is
// unavailable so diagnostics never affect tunnel correctness.
static uint64_t wdtt_phys_footprint_bytes(void) {
    task_vm_info_data_t info;
    mach_msg_type_number_t count = TASK_VM_INFO_COUNT;
    kern_return_t kr = task_info(
        mach_task_self(),
        TASK_VM_INFO,
        (task_info_t)&info,
        &count
    );
    if (kr != KERN_SUCCESS) {
        return 0;
    }
    return (uint64_t)info.phys_footprint;
}
*/
import "C"

import (
    "fmt"
    "runtime"
    "sync"
    "time"
)

const iosMemoryDiagnosticInterval = 5 * time.Second
const iosMemoryAllocSpikeBytes = 5 * 1024 * 1024

var (
    iosMemoryDiagMu              sync.Mutex
    iosMemoryDiagRuntime         *iosRuntime
    iosMemoryDiagLastSample      time.Time
    iosMemoryDiagPreviousHeap    uint64
    iosMemoryDiagWarned35MB      bool
    iosMemoryDiagWarned40MB      bool
    iosMemoryDiagWarned45MB      bool
)

func memoryMB(bytes uint64) float64 {
    return float64(bytes) / (1024.0 * 1024.0)
}

// maybeLogIOSMemoryDiagnostics is observation-only. It never runs GC, changes
// GOMEMLIMIT, reconnects transport, alters workers, or touches WireGuard.
// publishStats calls it every ~2s; this function throttles expensive snapshots
// to at most once per 5s.
func maybeLogIOSMemoryDiagnostics(s *Stats) {
    rt := currentIOSRuntime()
    if rt == nil || rt.ctx.Err() != nil || s == nil {
        return
    }

    now := time.Now()
    iosMemoryDiagMu.Lock()
    if iosMemoryDiagRuntime != rt {
        iosMemoryDiagRuntime = rt
        iosMemoryDiagLastSample = time.Time{}
        iosMemoryDiagPreviousHeap = 0
        iosMemoryDiagWarned35MB = false
        iosMemoryDiagWarned40MB = false
        iosMemoryDiagWarned45MB = false
    }
    if !iosMemoryDiagLastSample.IsZero() && now.Sub(iosMemoryDiagLastSample) < iosMemoryDiagnosticInterval {
        iosMemoryDiagMu.Unlock()
        return
    }
    iosMemoryDiagLastSample = now
    previousHeap := iosMemoryDiagPreviousHeap
    iosMemoryDiagMu.Unlock()

    var ms runtime.MemStats
    runtime.ReadMemStats(&ms)

    footprint := uint64(C.wdtt_phys_footprint_bytes())
    active := s.ActiveConnections.Load()
    goroutines := runtime.NumGoroutine()

    iosLog(fmt.Sprintf(
        "[СЕТЬ] [ПАМЯТЬ] footprint=%.1fMB heap=%.1fMB heapInuse=%.1fMB sys=%.1fMB stack=%.1fMB objects=%d goroutines=%d workers=%d gc=%d",
        memoryMB(footprint),
        memoryMB(ms.HeapAlloc),
        memoryMB(ms.HeapInuse),
        memoryMB(ms.Sys),
        memoryMB(ms.StackInuse),
        ms.HeapObjects,
        goroutines,
        active,
        ms.NumGC,
    ), false)

    if previousHeap > 0 && ms.HeapAlloc > previousHeap && ms.HeapAlloc-previousHeap >= iosMemoryAllocSpikeBytes {
        iosLog(fmt.Sprintf(
            "[СЕТЬ] [ПАМЯТЬ] ALLOC-SPIKE: heap +%.1fMB за ~5с; footprint=%.1fMB workers=%d goroutines=%d",
            memoryMB(ms.HeapAlloc-previousHeap),
            memoryMB(footprint),
            active,
            goroutines,
        ), false)
    }

    fpMB := memoryMB(footprint)
    warningThreshold := 0

    iosMemoryDiagMu.Lock()
    iosMemoryDiagPreviousHeap = ms.HeapAlloc
    if footprint > 0 {
        switch {
        case fpMB >= 45 && !iosMemoryDiagWarned45MB:
            iosMemoryDiagWarned35MB = true
            iosMemoryDiagWarned40MB = true
            iosMemoryDiagWarned45MB = true
            warningThreshold = 45
        case fpMB >= 40 && !iosMemoryDiagWarned40MB:
            iosMemoryDiagWarned35MB = true
            iosMemoryDiagWarned40MB = true
            warningThreshold = 40
        case fpMB >= 35 && !iosMemoryDiagWarned35MB:
            iosMemoryDiagWarned35MB = true
            warningThreshold = 35
        }
    }
    iosMemoryDiagMu.Unlock()

    if warningThreshold > 0 {
        iosLog(fmt.Sprintf(
            "[СЕТЬ] [ПАМЯТЬ] ВНИМАНИЕ: phys_footprint=%.1fMB пересёк %dMB; фиксируем возможное приближение к memory-pressure/jetsam",
            fpMB,
            warningThreshold,
        ), false)
    }
}
