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
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.zx2c4.com/wireguard/device"
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

// iosRuntime is the single owner of one NetworkExtension tunnel generation.
// It survives transport reconnects; each reconnect only replaces runOnce.
type iosRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	ready     chan struct{}
	activate  chan struct{}
	wgReady   chan struct{}
	healthKick chan struct{}

	readyOnce    sync.Once
	activateOnce sync.Once
	wgReadyOnce  sync.Once

	mu                 sync.Mutex
	attemptCancel      context.CancelFunc
	reconnectRequested bool
	lastReconnect      time.Time
	networkConfigJSON  string
	stats              *Stats
}

func newIOSRuntime() *iosRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &iosRuntime{
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		ready:      make(chan struct{}),
		activate:   make(chan struct{}),
		wgReady:    make(chan struct{}),
		healthKick: make(chan struct{}, 1),
	}
}

func (rt *iosRuntime) setAttemptCancel(cancel context.CancelFunc) {
	rt.mu.Lock()
	rt.attemptCancel = cancel
	rt.mu.Unlock()
}

func (rt *iosRuntime) clearAttemptCancel() {
	rt.mu.Lock()
	rt.attemptCancel = nil
	rt.mu.Unlock()
}

func (rt *iosRuntime) cancelAttempt() {
	rt.mu.Lock()
	cancel := rt.attemptCancel
	rt.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (rt *iosRuntime) requestReconnect(reason string) bool {
	rt.mu.Lock()
	if rt.ctx.Err() != nil {
		rt.mu.Unlock()
		return false
	}
	if !rt.lastReconnect.IsZero() && time.Since(rt.lastReconnect) < 2*time.Second {
		rt.mu.Unlock()
		return false
	}
	rt.lastReconnect = time.Now()
	rt.reconnectRequested = true
	cancel := rt.attemptCancel
	rt.mu.Unlock()

	if cancel == nil {
		return false
	}
	iosLog(fmt.Sprintf("[СЕТЬ] Перезапуск транспорта: %s", reason), false)
	cancel()
	return true
}

func (rt *iosRuntime) consumeReconnectRequest() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	requested := rt.reconnectRequested
	rt.reconnectRequested = false
	return requested
}

func (rt *iosRuntime) setNetworkConfigJSON(value string) {
	rt.mu.Lock()
	rt.networkConfigJSON = value
	rt.mu.Unlock()
}

func (rt *iosRuntime) networkConfigJSONCopy() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.networkConfigJSON
}

func (rt *iosRuntime) setStats(stats *Stats) {
	rt.mu.Lock()
	rt.stats = stats
	rt.mu.Unlock()
}

func (rt *iosRuntime) statsSnapshot() (up, down int64, ok bool) {
	rt.mu.Lock()
	stats := rt.stats
	rt.mu.Unlock()
	if stats == nil {
		return 0, 0, false
	}
	return stats.TotalBytesUp.Load(), stats.TotalBytesDown.Load(), true
}

func (rt *iosRuntime) kickHealthCheck() {
	select {
	case rt.healthKick <- struct{}{}:
	default:
	}
}

// ── Global bridge state ─────────────────────────────────────────────────────

var (
	iosMu      sync.Mutex
	iosLogCb   C.LogCallbackFn
	iosStatsCb C.StatsCallbackFn
	iosCurrent *iosRuntime
)

func currentIOSRuntime() *iosRuntime {
	iosMu.Lock()
	defer iosMu.Unlock()
	return iosCurrent
}

// ── C exports ───────────────────────────────────────────────────────────────

//export WDTT_SetLogCallback
func WDTT_SetLogCallback(fn C.LogCallbackFn) {
	iosMu.Lock()
	iosLogCb = fn
	iosMu.Unlock()
}

//export WDTT_SetStatsCallback
func WDTT_SetStatsCallback(fn C.StatsCallbackFn) {
	iosMu.Lock()
	iosStatsCb = fn
	iosMu.Unlock()
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
	peerStr := C.GoString(peer)
	hashesStr := C.GoString(vkHashes)
	passStr := C.GoString(password)
	deviceStr := C.GoString(deviceID)
	dnsStr := C.GoString(goDns)
	obfsStr := C.GoString(obfsMode)
	anonPath := C.GoString(vkAnonPath)

	hashList := parseHashes(hashesStr)
	if len(hashList) == 0 {
		iosLog("Ошибка: хэш не указан", true)
		return -2
	}
	if passStr == "" {
		iosLog("Ошибка: пароль подключения не указан", true)
		return -3
	}

	iosMu.Lock()
	if iosCurrent != nil && iosCurrent.ctx.Err() == nil {
		iosMu.Unlock()
		iosLog("Туннель уже запущен", true)
		return -1
	}
	rt := newIOSRuntime()
	iosCurrent = rt
	iosMu.Unlock()

	params := TunnelParams{
		Peer:               peerStr,
		VkHashes:           hashList,
		ConnectionPassword: passStr,
		Port:               int(port),
		WorkersPerHash:     int(workers),
		DeviceID:           deviceStr,
		GoDnsArg:           dnsStr,
		ObfsMode:           obfsStr,
		VkAnonPath:         anonPath,
		VkAuthMode:         "anonymous",
	}

	go func() {
		defer close(rt.done)
		defer func() {
			iosMu.Lock()
			if iosCurrent == rt {
				iosCurrent = nil
			}
			iosMu.Unlock()
			iosLog("Туннель остановлен", false)
		}()

		iosLog(fmt.Sprintf("[КЛИЕНТ] Хешей=%d Потоков=%d", len(hashList), params.WorkersPerHash), false)
		iosLog(fmt.Sprintf("[СЕТЬ] Маскировка: %s", obfsModeDisplay(obfsStr)), false)
		iosLog(fmt.Sprintf("[КЛИЕНТ] Режим VK: %s", anonPath), false)

		iosLog("[СЕТЬ] Проверка DNS...", false)
		dnsOK, dnsMsg := checkDNS(rt.ctx, dnsStr)
		if !dnsOK {
			iosLog(fmt.Sprintf("[СЕТЬ] Предупреждение DNS: %s (используем системный DNS)", dnsMsg), false)
		} else {
			iosLog(fmt.Sprintf("[СЕТЬ] DNS доступен: %s", dnsMsg), false)
		}

		go runIOSHealthWatchdog(rt)
		runTunnelLoop(rt, params)
	}()

	return 0
}

//export WDTT_WaitReady
func WDTT_WaitReady(timeoutMilliseconds C.int) C.int {
	rt := currentIOSRuntime()
	if rt == nil {
		return 0
	}
	return waitIOSSignal(rt.ctx, rt.ready, time.Duration(timeoutMilliseconds)*time.Millisecond)
}

//export WDTT_CopyNetworkConfig
func WDTT_CopyNetworkConfig() *C.char {
	rt := currentIOSRuntime()
	if rt == nil {
		return nil
	}
	value := rt.networkConfigJSONCopy()
	if value == "" {
		return nil
	}
	return C.CString(value)
}

//export WDTT_ActivateWireGuard
func WDTT_ActivateWireGuard() C.int {
	rt := currentIOSRuntime()
	if rt == nil || rt.ctx.Err() != nil {
		return -1
	}
	rt.activateOnce.Do(func() { close(rt.activate) })
	return 0
}

//export WDTT_WaitWireGuardReady
func WDTT_WaitWireGuardReady(timeoutMilliseconds C.int) C.int {
	rt := currentIOSRuntime()
	if rt == nil {
		return 0
	}
	return waitIOSSignal(rt.ctx, rt.wgReady, time.Duration(timeoutMilliseconds)*time.Millisecond)
}

//export WDTT_NotifyNetworkChange
func WDTT_NotifyNetworkChange() {
	rt := currentIOSRuntime()
	if rt != nil {
		rt.requestReconnect("смена физической сети")
	}
}

//export WDTT_WakeHealthCheck
func WDTT_WakeHealthCheck() {
	rt := currentIOSRuntime()
	if rt != nil {
		rt.kickHealthCheck()
	}
}

//export WDTT_Stop
func WDTT_Stop() {
	rt := currentIOSRuntime()
	if rt == nil {
		return
	}

	rt.cancel()
	rt.cancelAttempt()

	// Bound shutdown. NetworkExtension must never wait indefinitely for Go.
	select {
	case <-rt.done:
	case <-time.After(2500 * time.Millisecond):
		iosLog("[СЕТЬ] Остановка Go превысила 2.5 сек; NetworkExtension продолжит завершение", true)
	}
}

//export WDTT_IsRunning
func WDTT_IsRunning() C.int {
	rt := currentIOSRuntime()
	if rt != nil && rt.ctx.Err() == nil {
		return 1
	}
	return 0
}

//export WDTT_Free
func WDTT_Free(ptr *C.char) {
	C.free(unsafe.Pointer(ptr))
}

func waitIOSSignal(ctx context.Context, signal <-chan struct{}, timeout time.Duration) C.int {
	if timeout <= 0 {
		select {
		case <-signal:
			return 1
		default:
			return 0
		}
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-signal:
		return 1
	case <-ctx.Done():
		return 0
	case <-timer.C:
		return 0
	}
}

// ── Logging / callbacks ─────────────────────────────────────────────────────

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
	line := strings.TrimSuffix(string(p), "\n")
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

// ── Health / reconnect ──────────────────────────────────────────────────────

func runIOSHealthWatchdog(rt *iosRuntime) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var previousUp int64
	var previousDown int64
	var lastTX time.Time
	var lastRX time.Time
	var bytesUpAtLastRX int64
	lastTick := time.Now()

	evaluate := func(now time.Time) {
		gap := now.Sub(lastTick)
		lastTick = now

		up, down, ok := rt.statsSnapshot()
		if !ok {
			return
		}

		// A large ticker gap normally means iOS suspended the extension. Do not
		// interpret stale timestamps as a dead network immediately after wake.
		if gap > 75*time.Second || up < previousUp || down < previousDown {
			previousUp = up
			previousDown = down
			lastTX = time.Time{}
			lastRX = now
			bytesUpAtLastRX = up
			return
		}

		if lastRX.IsZero() {
			lastRX = now
			bytesUpAtLastRX = up
		}
		if up > previousUp {
			lastTX = now
		}
		if down > previousDown {
			lastRX = now
			bytesUpAtLastRX = up
		}

		previousUp = up
		previousDown = down

		// Reconnect only when the user is actively sending traffic and receives
		// nothing back for a sustained period. Idle/sleep alone is not failure.
		if !lastTX.IsZero() &&
			now.Sub(lastTX) <= 45*time.Second &&
			now.Sub(lastRX) >= 90*time.Second &&
			up-bytesUpAtLastRX >= 4096 {
			if rt.requestReconnect("health watchdog: есть TX, но нет RX") {
				lastRX = now
				bytesUpAtLastRX = up
			}
		}
	}

	for {
		select {
		case <-rt.ctx.Done():
			return
		case now := <-ticker.C:
			evaluate(now)
		case <-rt.healthKick:
			evaluate(time.Now())
		}
	}
}

// ── DNS probe ───────────────────────────────────────────────────────────────

func checkDNS(ctx context.Context, dnsArg string) (bool, string) {
	result := GoDnsProbeCheck(ctx, dnsArg)
	return result.Reachable, result.StatusText
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
	_ = conn.Close()
	return DnsProbeResult{Reachable: true, StatusText: "OK"}
}

// ── Main transport loop ─────────────────────────────────────────────────────

func runTunnelLoop(rt *iosRuntime, params TunnelParams) {
	backoff := time.Second

	for rt.ctx.Err() == nil {
		attemptCtx, attemptCancel := context.WithCancel(rt.ctx)
		rt.setAttemptCancel(attemptCancel)

		iosLog("[ВОРКЕР] Запуск транспортной сессии...", false)
		err := runOnce(attemptCtx, rt, params, func(line string, isErr bool) {
			if strings.Contains(line, "[СТАТИСТИКА]") {
				msg := strings.TrimPrefix(line, "[СТАТИСТИКА]")
				iosStats(strings.TrimSpace(msg))
			}
			iosLog(line, isErr)
		})

		attemptCancel()
		rt.clearAttemptCancel()

		if rt.ctx.Err() != nil {
			return
		}

		forced := rt.consumeReconnectRequest()
		delay := time.Second
		if forced {
			delay = 350 * time.Millisecond
			backoff = time.Second
		} else if err != nil {
			iosLog(fmt.Sprintf("[ВОРКЕР] Ошибка: %v — повторяем транспорт", err), true)
			delay = backoff
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		} else {
			backoff = time.Second
		}

		select {
		case <-rt.ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func runOnce(ctx context.Context, rt *iosRuntime, params TunnelParams, logFn func(line string, isErr bool)) error {
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	logFn("[ГО-ВОРКЕР] Инициализация сессии...", false)
	if len(params.VkHashes) == 0 {
		return fmt.Errorf("нет VK-хешей")
	}
	setVkAnonPath(params.VkAnonPath)
	setVkAuthMode(params.VkAuthMode)

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
	stopLocalConn := context.AfterFunc(runCtx, func() { _ = localConn.Close() })
	defer stopLocalConn()

	_, localPort, _ := net.SplitHostPort(listenAddr)

	stats := NewStats()
	rt.setStats(stats)
	defer rt.setStats(nil)

	shutdownCh := make(chan struct{})
	go func() {
		<-runCtx.Done()
		close(shutdownCh)
	}()
	go stats.RunLoop(shutdownCh)

	disp := NewDispatcher(runCtx, localConn, stats)

	configCh := make(chan string, 1)
	configDone := make(chan struct{})
	var wgDevice *device.Device

	go func() {
		defer close(configDone)
		select {
		case rawConf, ok := <-configCh:
			if !ok || rawConf == "" {
				return
			}

			finalConf := ensureIOSMTU(rawConf)
			if _, parseErr := parseWgQuick(finalConf); parseErr != nil {
				logFn(fmt.Sprintf("[КОНФИГ] Некорректный WireGuard-конфиг: %v", parseErr), true)
				runCancel()
				return
			}

			rt.setNetworkConfigJSON(extractIOSNetworkConfigJSON(finalConf))
			rt.readyOnce.Do(func() { close(rt.ready) })
			logFn("[СЕТЬ] TURN/DTLS готов; ждём установки маршрутов iOS", false)

			select {
			case <-rt.activate:
			case <-runCtx.Done():
				return
			}

			drainPacketsFromSwift()
			logFn("[КОНФИГ] Запуск Userspace WireGuard...", false)
			dev, _, startErr := startUserspaceWireGuard(finalConf)
			if startErr != nil {
				logFn(fmt.Sprintf("[IOS-TUN] Ошибка userspace WG: %v", startErr), true)
				runCancel()
				return
			}

			// Ownership intentionally stays in runOnce. The previous implementation
			// used defer dev.Close() inside this short-lived goroutine and killed
			// WireGuard immediately after a successful Up().
			wgDevice = dev
			rt.wgReadyOnce.Do(func() { close(rt.wgReady) })
			logFn("[IOS-TUN] WireGuard поднят и готов к трафику", false)

		case <-runCtx.Done():
			return
		}
	}()

	numW := params.WorkersPerHash * len(params.VkHashes)
	const maxWorkers = 108
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
		const iosMemoryTestWorkers = 72
		if numW > iosMemoryTestWorkers {
			numW = iosMemoryTestWorkers
		}
		if numW < workersPerGroup {
			numW = workersPerGroup
		}
		numW = (numW / workersPerGroup) * workersPerGroup
		logFn(fmt.Sprintf("[КЛИЕНТ] iOS активных workers=%d (hard cap=%d)", numW, maxWorkers), false)
	}

	numGroups := (numW + workersPerGroup - 1) / workersPerGroup
	var wg sync.WaitGroup
	workerIDCounter := 1
	var pauseFlag int32
	var prevWaitReady <-chan struct{}
	var configSent int32
	var configRequestInFlight int32

	for g := 0; g < numGroups; g++ {
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
		cc := (chan<- string)(configCh)

		wg.Add(1)
		go func(groupID int, configChan chan<- string, workerIDs []int, startHashIndex int, waitR <-chan struct{}, sigR chan<- struct{}) {
			defer wg.Done()
			WorkerGroup(runCtx, groupID, startHashIndex, tp, peer, disp, localPort,
				configChan, workerIDs, &pauseFlag, params.DeviceID, params.ConnectionPassword,
				stats, waitR, sigR, &configSent, &configRequestInFlight)
		}(gID, cc, ids, g, myWaitReady, mySignalReady)
	}

	wg.Wait()

	// Stop transport/dispatcher before closing WireGuard. This avoids the
	// shutdown deadlock where WireGuard waits on I/O that is still owned by the
	// transport side.
	runCancel()
	close(configCh)
	<-configDone
	disp.Shutdown()

	if wgDevice != nil {
		wgDevice.Close()
	}

	logFn("[ГО-ВОРКЕР] Все воркеры завершены", false)
	return nil
}

func ensureIOSMTU(conf string) string {
	if strings.Contains(strings.ToLower(conf), "mtu =") {
		return conf
	}
	lines := strings.Split(conf, "\n")
	var result []string
	for _, line := range lines {
		result = append(result, line)
		if strings.EqualFold(strings.TrimSpace(line), "[Interface]") {
			result = append(result, "MTU = 1280")
		}
	}
	return strings.Join(result, "\n")
}

type iosNetworkConfig struct {
	IPv4Address string   `json:"ipv4Address,omitempty"`
	IPv4Prefix  int      `json:"ipv4Prefix,omitempty"`
	IPv6Address string   `json:"ipv6Address,omitempty"`
	IPv6Prefix  int      `json:"ipv6Prefix,omitempty"`
	DNSServers  []string `json:"dnsServers"`
	MTU         int      `json:"mtu"`
}

func extractIOSNetworkConfigJSON(conf string) string {
	cfg := iosNetworkConfig{MTU: 1280}
	section := ""

	for _, raw := range strings.Split(conf, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		if section != "interface" {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "address":
			for _, item := range strings.Split(value, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				prefix, err := netip.ParsePrefix(item)
				if err == nil {
					if prefix.Addr().Is4() && cfg.IPv4Address == "" {
						cfg.IPv4Address = prefix.Addr().String()
						cfg.IPv4Prefix = prefix.Bits()
					} else if prefix.Addr().Is6() && cfg.IPv6Address == "" {
						cfg.IPv6Address = prefix.Addr().String()
						cfg.IPv6Prefix = prefix.Bits()
					}
					continue
				}
				addr, addrErr := netip.ParseAddr(item)
				if addrErr == nil {
					if addr.Is4() && cfg.IPv4Address == "" {
						cfg.IPv4Address = addr.String()
						cfg.IPv4Prefix = 32
					} else if addr.Is6() && cfg.IPv6Address == "" {
						cfg.IPv6Address = addr.String()
						cfg.IPv6Prefix = 128
					}
				}
			}

		case "dns":
			for _, item := range strings.Split(value, ",") {
				item = strings.TrimSpace(item)
				if item != "" {
					cfg.DNSServers = append(cfg.DNSServers, item)
				}
			}

		case "mtu":
			if mtu, err := strconv.Atoi(value); err == nil && mtu >= 576 && mtu <= 9000 {
				cfg.MTU = mtu
			}
		}
	}

	if len(cfg.DNSServers) == 0 {
		cfg.DNSServers = []string{"1.1.1.1", "8.8.8.8"}
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		return `{"dnsServers":["1.1.1.1","8.8.8.8"],"mtu":1280}`
	}
	return string(encoded)
}
