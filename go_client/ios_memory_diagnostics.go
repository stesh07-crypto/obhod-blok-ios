//go:build ios

package main

/*
#include <stdint.h>
#include <mach/mach.h>
#include <mach/task_info.h>

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

// publishStats currently ticks every ~2s, so a 1s throttle means we capture
// every available stats tick instead of discarding two out of three as before.
const iosMemoryDiagnosticInterval = 1 * time.Second
const iosMemoryAllocSpikeBytes = 5 * 1024 * 1024

var (
    iosMemoryDiagMu              sync.Mutex
    iosMemoryDiagRuntime         *iosRuntime
    iosMemoryDiagLastSample      time.Time
    iosMemoryDiagPreviousHeap    uint64
    iosMemoryDiagPreviousObjects uint64
    iosMemoryDiagPreviousGC      uint32
    iosMemoryDiagPreviousUp      int64
    iosMemoryDiagPreviousDown    int64
    iosMemoryDiagWarned35MB      bool
    iosMemoryDiagWarned40MB      bool
    iosMemoryDiagWarned45MB      bool
)

func memoryMB(bytes uint64) float64 {
    return float64(bytes) / (1024.0 * 1024.0)
}

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
        iosMemoryDiagPreviousObjects = 0
        iosMemoryDiagPreviousGC = 0
        iosMemoryDiagPreviousUp = 0
        iosMemoryDiagPreviousDown = 0
        iosMemoryDiagWarned35MB = false
        iosMemoryDiagWarned40MB = false
        iosMemoryDiagWarned45MB = false
    }
    if !iosMemoryDiagLastSample.IsZero() && now.Sub(iosMemoryDiagLastSample) < iosMemoryDiagnosticInterval {
        iosMemoryDiagMu.Unlock()
        return
    }

    previousAt := iosMemoryDiagLastSample
    previousHeap := iosMemoryDiagPreviousHeap
    previousObjects := iosMemoryDiagPreviousObjects
    previousGC := iosMemoryDiagPreviousGC
    previousUp := iosMemoryDiagPreviousUp
    previousDown := iosMemoryDiagPreviousDown
    iosMemoryDiagLastSample = now
    iosMemoryDiagMu.Unlock()

    var ms runtime.MemStats
    runtime.ReadMemStats(&ms)

    footprint := uint64(C.wdtt_phys_footprint_bytes())
    active := s.ActiveConnections.Load()
    goroutines := runtime.NumGoroutine()
    up := s.TotalBytesUp.Load()
    down := s.TotalBytesDown.Load()

    elapsed := now.Sub(previousAt).Seconds()
    upMbit := 0.0
    downMbit := 0.0
    objectRate := 0.0
    gcRate := 0.0
    if !previousAt.IsZero() && elapsed > 0 {
        upMbit = float64(up-previousUp) * 8.0 / elapsed / 1_000_000.0
        downMbit = float64(down-previousDown) * 8.0 / elapsed / 1_000_000.0
        objectRate = float64(int64(ms.HeapObjects)-int64(previousObjects)) / elapsed
        gcRate = float64(ms.NumGC-previousGC) / elapsed
    }

    iosLog(fmt.Sprintf(
        "[СЕТЬ] [ПАМЯТЬ] footprint=%.1fMB heap=%.1fMB heapInuse=%.1fMB sys=%.1fMB stack=%.1fMB objects=%d objRate=%+.0f/s goroutines=%d workers=%d gc=%d gcRate=%.1f/s traffic=↓%.1f ↑%.1f Mbit/s",
        memoryMB(footprint),
        memoryMB(ms.HeapAlloc),
        memoryMB(ms.HeapInuse),
        memoryMB(ms.Sys),
        memoryMB(ms.StackInuse),
        ms.HeapObjects,
        objectRate,
        goroutines,
        active,
        ms.NumGC,
        gcRate,
        downMbit,
        upMbit,
    ), false)

    if previousHeap > 0 && ms.HeapAlloc > previousHeap && ms.HeapAlloc-previousHeap >= iosMemoryAllocSpikeBytes {
        iosLog(fmt.Sprintf(
            "[СЕТЬ] [ПАМЯТЬ] ALLOC-SPIKE: heap +%.1fMB; footprint=%.1fMB workers=%d goroutines=%d traffic=↓%.1f ↑%.1f Mbit/s objRate=%+.0f/s gcRate=%.1f/s",
            memoryMB(ms.HeapAlloc-previousHeap),
            memoryMB(footprint),
            active,
            goroutines,
            downMbit,
            upMbit,
            objectRate,
            gcRate,
        ), false)
    }

    fpMB := memoryMB(footprint)
    warningThreshold := 0

    iosMemoryDiagMu.Lock()
    iosMemoryDiagPreviousHeap = ms.HeapAlloc
    iosMemoryDiagPreviousObjects = ms.HeapObjects
    iosMemoryDiagPreviousGC = ms.NumGC
    iosMemoryDiagPreviousUp = up
    iosMemoryDiagPreviousDown = down
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
            "[СЕТЬ] [ПАМЯТЬ] ВНИМАНИЕ: phys_footprint=%.1fMB пересёк %dMB; traffic=↓%.1f ↑%.1f Mbit/s objRate=%+.0f/s gcRate=%.1f/s",
            fpMB,
            warningThreshold,
            downMbit,
            upMbit,
            objectRate,
            gcRate,
        ), false)
    }
}
