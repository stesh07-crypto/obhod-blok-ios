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
	"log"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"
)

type TunnelParams struct {
	Peer               string
	VkHashes           []string
	ConnectionPassword string
	Port               int
	WorkersPerHash     int
	DeviceID           string
	GoDnsArg           string
	ObfsMode           string
	VkAnonPath         string
	VkAuthMode         string
}

type DnsProbeResult struct {
	Reachable  bool
	StatusText string
}

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
			iosLog(fmt.Sprintf("[СЕТЬ] Предупреждение DNS: %s (используем системный DNS)", dnsMsg), false)
		} else {
			iosLog(fmt.Sprintf("[СЕТЬ] DNS доступен: %s", dnsMsg), false)
		}

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

type uiLogWriter struct{}

func (w uiLogWriter) Write(p []byte) (n int, err error) {
	line := string(p)
	line = strings.TrimSuffix(line, "\n")
	iosLog(line, false)
	return len(p), nil
}

func init() {
	log.SetOutput(uiLogWriter{})
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
			time.Sleep(3 * time.Second)
		} else {
			// If runOnce completes without error (e.g., config changes or manual shutdown)
			// we should also sleep a bit to avoid hot-looping if it exits instantly.
			time.Sleep(1 * time.Second)
		}
	}
}

func parseDnsTarget(dnsArg string) string {
	switch strings.ToLower(strings.TrimSpace(dnsArg)) {
	case "cloudflare", "1.1.1.1":
		return "1.1.1.1:53"
	case "google", "8.8.8.8":
		return "8.8.8.8:53"
	case "yandex", "77.88.8.8":
		return "77.88.8.8:53"
	case "adguard", "94.140.14.14":
		return "94.140.14.14:53"
	default:
		if strings.HasPrefix(strings.ToLower(dnsArg), "doh:") {
			return "1.1.1.1:53"
		}
		if !strings.Contains(dnsArg, ":") {
			return net.JoinHostPort(dnsArg, "53")
		}
		return dnsArg
	}
}

func GoDnsProbeCheck(ctx context.Context, dnsArg string) DnsProbeResult {
	if dnsArg == "" {
		return DnsProbeResult{Reachable: true, StatusText: "OK"}
	}
	host := parseDnsTarget(dnsArg)
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "udp", host)
	if err != nil {
		return DnsProbeResult{Reachable: false, StatusText: err.Error()}
	}
	conn.Close()
	return DnsProbeResult{Reachable: true, StatusText: "OK"}
}

func runOnce(ctx context.Context, params TunnelParams, logFn func(line string, isErr bool)) error {
	logFn("[ГО-ВОРКЕР] Инициализация сессии...", false)
	if len(params.VkHashes) == 0 {
		return fmt.Errorf("нет VK-хешей")
	}
	setVkAnonPath(params.VkAnonPath)
	setVkAuthMode(params.VkAuthMode)

	// Derive Wrap Key
	wrapKey, err := deriveWrapKey(params.ConnectionPassword)
	if err != nil {
		return fmt.Errorf("WRAP key derive error: %v", err)
	}

	peerStr := params.Peer
	if !strings.Contains(peerStr, ":") {
		peerStr += ":443"
	}
	peer, err := net.ResolveUDPAddr("udp", peerStr)
	if err != nil {
		return fmt.Errorf("Peer resolve error: %v", err)
	}

	tp := &TurnParams{
		Host:     "",
		Port:     "",
		Hashes:   params.VkHashes,
		WrapKey:  wrapKey,
		ObfsMode: normalizeObfsMode(params.ObfsMode),
	}

	listenAddr := fmt.Sprintf("127.0.0.1:%d", params.Port)
	if params.Port == 0 {
		listenAddr = "127.0.0.1:9000"
	}
	localConn, err := listenUDP(listenAddr)
	if err != nil {
		return fmt.Errorf("Listen error: %v", err)
	}
	if uc, ok := localConn.(*net.UDPConn); ok {
		_ = uc.SetReadBuffer(socketBufSize)
		_ = uc.SetWriteBuffer(socketBufSize)
	}
	stopLocalConn := context.AfterFunc(ctx, func() { _ = localConn.Close() })
	defer stopLocalConn()

	_, localPort, _ := net.SplitHostPort(listenAddr)

	stats := NewStats()
	shutdownCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(shutdownCh)
	}()
	go stats.RunLoop(shutdownCh)

	disp := NewDispatcher(ctx, localConn, stats)
	defer disp.Shutdown()

	configCh := make(chan string, 1)
	configDone := make(chan struct{})
	go func() {
		defer close(configDone)
		select {
		case rawConf, ok := <-configCh:
			if !ok || rawConf == "" {
				return
			}
			finalConf := rawConf
			if !strings.Contains(finalConf, "MTU =") {
				lines := strings.Split(finalConf, "\n")
				var newLines []string
				for _, line := range lines {
					newLines = append(newLines, line)
					if strings.TrimSpace(line) == "[Interface]" {
						newLines = append(newLines, "MTU = 1280")
					}
				}
				finalConf = strings.Join(newLines, "\n")
			}
			logFn("[КОНФИГ] Запуск Userspace WireGuard...", false)
			
			dev, tnet, err := startUserspaceWireGuard(finalConf)
			if err != nil {
				logFn(fmt.Sprintf("[SOCKS] Ошибка userspace WG: %v", err), true)
				return
			}
			defer dev.Close()
			socksAddr := "0.0.0.0:1080"
			logFn(fmt.Sprintf("[SOCKS] Запуск SOCKS5 на %s...", socksAddr), false)
			if err := runSocks5Server(ctx, socksAddr, tnet); err != nil {
				logFn(fmt.Sprintf("[SOCKS] Сервер остановлен: %v", err), true)
			}
		case <-ctx.Done():
		}
	}()

	numW := params.WorkersPerHash * len(params.VkHashes)
	// Apply max workers limits
	maxWorkers := 108
	if numW > maxWorkers {
		numW = maxWorkers
	}
	if getVkAuthMode() == "account" {
		const accountMaxWorkers = 4
		if numW > accountMaxWorkers {
			numW = accountMaxWorkers
		}
		if numW < 1 {
			numW = 1
		}
	} else {
		if numW < workersPerGroup {
			numW = workersPerGroup
		}
		numW = (numW / workersPerGroup) * workersPerGroup
	}

	numGroups := (numW + workersPerGroup - 1) / workersPerGroup

	var wg sync.WaitGroup
	workerIDCounter := 1

	var pauseFlag int32
	var prevWaitReady <-chan struct{}

	for g := 0; g < numGroups; g++ {
		isFirst := (g == 0)

		var myWaitReady <-chan struct{}
		var mySignalReady chan<- struct{}

		if g > 0 {
			myWaitReady = prevWaitReady
		}
		if g < numGroups-1 {
			ch := make(chan struct{})
			mySignalReady = ch
			prevWaitReady = ch
		}

		startIdx := g * workersPerGroup
		endIdx := startIdx + workersPerGroup
		if endIdx > numW {
			endIdx = numW
		}
		groupSize := endIdx - startIdx
		if groupSize <= 0 {
			continue
		}

		ids := make([]int, groupSize)
		for i := range ids {
			ids[i] = workerIDCounter
			workerIDCounter++
		}

		gID := g + 1
		var cc chan<- string
		if isFirst {
			cc = configCh
		}

		wg.Add(1)
		go func(groupID int, isFirstGroup bool, configChan chan<- string, workerIds []int, startHashIndex int, waitR <-chan struct{}, sigR chan<- struct{}) {
			defer wg.Done()
			WorkerGroup(ctx, groupID, startHashIndex, tp, peer, disp, localPort,
				isFirstGroup, configChan, workerIds, &pauseFlag, params.DeviceID, params.ConnectionPassword, stats, waitR, sigR)
		}(gID, isFirst, cc, ids, g, myWaitReady, mySignalReady)
	}

	wg.Wait()
	close(configCh)
	<-configDone
	logFn("[ГО-ВОРКЕР] Все воркеры завершены", false)
	return nil
}
