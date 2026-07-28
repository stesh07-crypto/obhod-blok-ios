//go:build ios

package main

/*
#include <stdlib.h>

// Callback type: called from Go to send log lines to Swift
typedef void (*LogCallbackFn)(const char* line, int isError);
// Callback type: for stats updates
typedef void (*StatsCallbackFn)(const char* stats);

static inline void bridge_log(LogCallbackFn fn, const char* line, int isError) {
    if (fn) fn(line, isError);
}

static inline void bridge_stats(StatsCallbackFn fn, const char* stats) {
    if (fn) fn(stats);
}
*/
import "C"
import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unsafe"
)

// ── Global state ────────────────────────────────────────────────────────────

var (
	iosMu          sync.Mutex
	iosCtx         context.Context
	iosCancel      context.CancelFunc
	iosLogCb       C.LogCallbackFn
	iosStatsCb     C.StatsCallbackFn
	iosRunning     bool
)

// ── C exports ───────────────────────────────────────────────────────────────

//export WDTT_SetLogCallback
func WDTT_SetLogCallback(fn C.LogCallbackFn) {
	iosMu.Lock()
	defer iosMu.Unlock()
	iosLogCb = fn
}

//export WDTT_SetStatsCallback
func WDTT_SetStatsCallback(fn C.StatsCallbackFn) {
	iosMu.Lock()
	defer iosMu.Unlock()
	iosStatsCb = fn
}

//export WDTT_Start
func WDTT_Start(
	peer *C.char,
	vkHashes *C.char,
	password *C.char,
	port C.int,
	workers C.int,
	deviceID *C.char,
	goDns *C.char,
	obfsMode *C.char,
	vkAnonPath *C.char,
) C.int {
	iosMu.Lock()
	if iosRunning {
		iosMu.Unlock()
		iosLog("Туннель уже запущен", true)
		return -1
	}

	peerStr := C.GoString(peer)
	hashesStr := C.GoString(vkHashes)
	passStr := C.GoString(password)
	portInt := int(port)
	workersInt := int(workers)
	deviceStr := C.GoString(deviceID)
	dnsStr := C.GoString(goDns)
	obfsStr := C.GoString(obfsMode)
	anonPath := C.GoString(vkAnonPath)

	ctx, cancel := context.WithCancel(context.Background())
	iosCtx = ctx
	iosCancel = cancel
	iosRunning = true
	iosMu.Unlock()

	go func() {
		defer func() {
			iosMu.Lock()
			iosRunning = false
			iosMu.Unlock()
			iosLog("Туннель остановлен", false)
		}()

		hashList := parseHashes(hashesStr)
		if len(hashList) == 0 {
			iosLog("Ошибка: хэш не указан", true)
			return
		}
		if passStr == "" {
			iosLog("Ошибка: пароль подключения не указан", true)
			return
		}

		iosLog(fmt.Sprintf("[КЛИЕНТ] Хешей=%d Потоков=%d", len(hashList), workersInt), false)
		iosLog(fmt.Sprintf("[СЕТЬ] Маскировка: %s", obfsModeDisplay(obfsStr)), false)
		iosLog(fmt.Sprintf("[КЛИЕНТ] Режим VK: %s", anonPath), false)

		// DNS pre-check
		iosLog("[СЕТЬ] Проверка DNS...", false)
		dnsOk, dnsMsg := checkDNS(ctx, dnsStr)
		if !dnsOk {
			iosLog(fmt.Sprintf("[СЕТЬ] DNS недоступен: %s", dnsMsg), true)
			iosLog("[СЕТЬ] Смените DNS в Настройках → Сеть", true)
			return
		}
		iosLog(fmt.Sprintf("[СЕТЬ] DNS доступен: %s", dnsMsg), false)

		// Run tunnel
		runTunnelLoop(ctx, TunnelParams{
			Peer:             peerStr,
			VkHashes:         hashList,
			ConnectionPassword: passStr,
			Port:             portInt,
			WorkersPerHash:   workersInt,
			DeviceID:         deviceStr,
			GoDnsArg:         dnsStr,
			ObfsMode:         obfsStr,
			VkAnonPath:       anonPath,
			VkAuthMode:       "anonymous",
		})
	}()

	return 0
}

//export WDTT_Stop
func WDTT_Stop() {
	iosMu.Lock()
	cancel := iosCancel
	iosMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

//export WDTT_IsRunning
func WDTT_IsRunning() C.int {
	iosMu.Lock()
	defer iosMu.Unlock()
	if iosRunning {
		return 1
	}
	return 0
}

//export WDTT_Free
func WDTT_Free(ptr *C.char) {
	C.free(unsafe.Pointer(ptr))
}

// ── Internal helpers ─────────────────────────────────────────────────────────

func iosLog(line string, isError bool) {
	iosMu.Lock()
	cb := iosLogCb
	iosMu.Unlock()
	if cb == nil {
		return
	}
	cs := C.CString(line)
	defer C.free(unsafe.Pointer(cs))
	errFlag := C.int(0)
	if isError {
		errFlag = 1
	}
	C.bridge_log(cb, cs, errFlag)
}

func iosStats(stats string) {
	iosMu.Lock()
	cb := iosStatsCb
	iosMu.Unlock()
	if cb == nil {
		return
	}
	cs := C.CString(stats)
	defer C.free(unsafe.Pointer(cs))
	C.bridge_stats(cb, cs)
}

func parseHashes(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t'
	})
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func obfsModeDisplay(mode string) string {
	switch strings.ToLower(mode) {
	case "none":
		return "Без маскировки"
	case "tls":
		return "TLS"
	case "http":
		return "HTTP"
	default:
		return mode
	}
}

// checkDNS — lightweight DNS probe for iOS (no subprocess)
func checkDNS(ctx context.Context, dnsArg string) (bool, string) {
	// Use the existing doh/dns probe logic but via direct call
	result := GoDnsProbeCheck(ctx, dnsArg)
	return result.Reachable, result.StatusText
}

// runTunnelLoop — main tunnel loop, mirrors Android Go subprocess behavior
func runTunnelLoop(ctx context.Context, params TunnelParams) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		iosLog("[ВОРКЕР] Запуск туннеля...", false)
		err := runOnce(ctx, params, func(line string, isErr bool) {
			if strings.Contains(line, "[СТАТИСТИКА]") {
				msg := strings.TrimPrefix(line, "[СТАТИСТИКА]")
				iosStats(strings.TrimSpace(msg))
			}
			iosLog(line, isErr)
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			iosLog(fmt.Sprintf("[ВОРКЕР] Ошибка: %v — переподключение...", err), true)
		}
	}
}
