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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	ready      chan struct{}
	activate   chan struct{}
	wgReady    chan struct{}
	healthKick chan struct{}
	reconnectWake chan struct{}

	readyOnce    sync.Once
	activateOnce sync.Once
	wgReadyOnce  sync.Once

	mu                 sync.Mutex
	attemptCancel      context.CancelFunc
	reconnectRequested bool
	lastReconnect      time.Time
	networkConfigJSON  string
	stats              *Stats
	wgDev              *device.Device
	wgPeerHex          string

	// Data-plane diagnostics and repair state. Guarded by mu.
	generation        uint64
	configApplied     bool
	wgStarted         bool
	lastRepair        time.Time
	repairAttempts    int
	repairWindowStart time.Time
	physicalAvailable bool // last NWPathMonitor state reported by Swift; true until Swift reports

	// Counters of the previous diagnostic snapshot, used to report deltas.
	snapAt      time.Time
	snapTunUp   int64
	snapTunDown int64
	snapDtlsTx  int64
	snapDtlsRx  int64
}

func (rt *iosRuntime) setWireGuard(dev *device.Device, peerHex string) {
	rt.mu.Lock()
	rt.wgDev = dev
	rt.wgPeerHex = peerHex
	rt.mu.Unlock()
}

func (rt *iosRuntime) clearWireGuard() {
	rt.mu.Lock()
	rt.wgDev = nil
	rt.wgPeerHex = ""
	rt.mu.Unlock()
}

// noteRunGeneration stamps a new transport generation and clears per-run flags.
func (rt *iosRuntime) noteRunGeneration() uint64 {
	rt.mu.Lock()
	rt.generation++
	rt.configApplied = false
	rt.wgStarted = false
	gen := rt.generation
	rt.mu.Unlock()
	return gen
}

func (rt *iosRuntime) setDataPlaneState(configApplied, wgStarted bool) {
	rt.mu.Lock()
	rt.configApplied = configApplied
	rt.wgStarted = wgStarted
	rt.mu.Unlock()
}

func (rt *iosRuntime) dataPlaneState() (configApplied, wgStarted bool, generation uint64) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.configApplied, rt.wgStarted, rt.generation
}

// wgStats reads handshake age and byte counters straight from wireguard-go.
func (rt *iosRuntime) wgStats() (lastHandshake time.Time, rxBytes, txBytes int64, ok bool) {
	rt.mu.Lock()
	dev := rt.wgDev
	rt.mu.Unlock()
	if dev == nil {
		return time.Time{}, 0, 0, false
	}

	var buf bytes.Buffer
	if err := dev.IpcGetOperation(&buf); err != nil {
		return time.Time{}, 0, 0, false
	}
	return parseWGStats(buf.String(), time.Now())
}

// physicalProbe proves only that the underlying network answers. Traffic of the
// extension itself bypasses its own tunnel, so the result is reported as phys
// and never treated as a data-plane success signal.
func (rt *iosRuntime) physicalProbe(timeout time.Duration) string {
	for _, target := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", target, timeout)
		if err != nil {
			continue
		}
		_ = conn.Close()
		return fmt.Sprintf("OK(%s)", time.Since(start).Round(time.Millisecond))
	}
	return "FAIL"
}

// snapshot logs one secret-free line that localises a stalled data plane:
// physical network, config, WireGuard handshake, per-leg byte counters, worker
// slots and active sessions.
func (rt *iosRuntime) snapshot(reason string) string {
	now := time.Now()

	up, down, _ := rt.statsSnapshot()

	var (
		active   int32
		dtlsTx   int64
		dtlsRx   int64
		workers  int32
		statsRef *Stats
	)
	rt.mu.Lock()
	statsRef = rt.stats
	gen := rt.generation
	configApplied := rt.configApplied
	wgStarted := rt.wgStarted
	repairs := rt.repairAttempts
	previousAt := rt.snapAt
	previousTunUp := rt.snapTunUp
	previousTunDown := rt.snapTunDown
	previousDtlsTx := rt.snapDtlsTx
	previousDtlsRx := rt.snapDtlsRx
	rt.mu.Unlock()

	if statsRef != nil {
		active = statsRef.ActiveConnections.Load()
		dtlsTx = statsRef.DtlsTxBytes.Load()
		dtlsRx = statsRef.DtlsRxBytes.Load()
		workers = statsRef.WorkerSlots.Load()
	}

	var since time.Duration
	var deltaTunUp, deltaTunDown, deltaDtlsTx, deltaDtlsRx int64
	if !previousAt.IsZero() {
		since = now.Sub(previousAt)
		deltaTunUp = up - previousTunUp
		deltaTunDown = down - previousTunDown
		deltaDtlsTx = dtlsTx - previousDtlsTx
		deltaDtlsRx = dtlsRx - previousDtlsRx
	}

	rt.mu.Lock()
	rt.snapAt = now
	rt.snapTunUp = up
	rt.snapTunDown = down
	rt.snapDtlsTx = dtlsTx
	rt.snapDtlsRx = dtlsRx
	rt.mu.Unlock()

	handshake, wgRx, wgTx, _ := rt.wgStats()
	var handshakeAge time.Duration
	if !handshake.IsZero() {
		handshakeAge = now.Sub(handshake)
	}

	line := diagSnapshot{
		Reason:         reason,
		At:             now,
		Build:          BuildNumber,
		Generation:     gen,
		ConfigReady:    configApplied,
		WGReady:        wgStarted,
		WGHandshakeAge: handshakeAge,
		WGRxBytes:      wgRx,
		WGTxBytes:      wgTx,
		TunUpBytes:     up,
		TunDownBytes:   down,
		DtlsTxBytes:    dtlsTx,
		DtlsRxBytes:    dtlsRx,
		Workers:        workers,
		Active:         active,
		Physical:       rt.physicalProbe(2 * time.Second),
		Repairs:        repairs,
		SinceLast:      since,
		TunUpDelta:     deltaTunUp,
		TunDownDelta:   deltaTunDown,
		DtlsTxDelta:    deltaDtlsTx,
		DtlsRxDelta:    deltaDtlsRx,
	}.String()

	iosLog(line, false)
	return line
}

// snapshotAsync keeps diagnostics off the transport hot path: callers get a
// start marker immediately, then the snapshot (including network probes) runs
// in the background and reports its duration.
func (rt *iosRuntime) snapshotAsync(reason string) {
	if rt == nil {
		return
	}
	iosLog(fmt.Sprintf("[ДИАГ] snapshot start reason=%s at=%s", reason, time.Now().Format(time.RFC3339)), false)
	go func() {
		start := time.Now()
		rt.snapshot(reason)
		iosLog(fmt.Sprintf("[ДИАГ] snapshot done reason=%s ms=%d", reason, time.Since(start).Milliseconds()), false)
	}()
}

// dataPlaneSample is the cheap periodic probe of the data path: it reads only
// in-process counters and the WireGuard UAPI, so it can run while the user
// browses without adding latency.
func (rt *iosRuntime) dataPlaneSample() {
	now := time.Now()
	up, down, _ := rt.statsSnapshot()

	var (
		active  int32
		dtlsTx  int64
		dtlsRx  int64
		workers int32
	)
	rt.mu.Lock()
	st := rt.stats
	rt.mu.Unlock()
	if st != nil {
		active = st.ActiveConnections.Load()
		dtlsTx = st.DtlsTxBytes.Load()
		dtlsRx = st.DtlsRxBytes.Load()
		workers = st.WorkerSlots.Load()
	}

	handshake, wgRx, wgTx, _ := rt.wgStats()
	age := "none"
	if !handshake.IsZero() {
		age = now.Sub(handshake).Round(time.Second).String()
	}

	iosLog(fmt.Sprintf(
		"[ДИАГ-ПОТОК] wg_hs=%s wg_rx=%d wg_tx=%d tun_up=%d tun_down=%d dtls_tx=%d dtls_rx=%d workers=%d active=%d",
		age, wgRx, wgTx, up, down, dtlsTx, dtlsRx, workers, active,
	), false)
}

func (rt *iosRuntime) setPhysicalAvailable(available bool) {
	rt.mu.Lock()
	rt.physicalAvailable = available
	rt.mu.Unlock()
}

func (rt *iosRuntime) physicalPathAvailable() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.physicalAvailable
}

// consumeRepairBudget is the single gate for every automatic transport rebuild.
// It returns false when the cooldown or the per-window limit forbids another
// restart, so callers must not cancel the attempt in that case.
func (rt *iosRuntime) consumeRepairBudget(reason string) bool {
	now := time.Now()

	rt.mu.Lock()
	if rt.ctx.Err() != nil {
		rt.mu.Unlock()
		return false
	}
	allowed, why := defaultRepairPolicy.allow(now, rt.lastRepair, rt.repairAttempts, rt.repairWindowStart)
	if !allowed {
		rt.mu.Unlock()
		iosLog(fmt.Sprintf("[ДИАГ] Ремонт отклонён (%s): %s", reason, why), false)
		return false
	}
	rt.repairAttempts, rt.repairWindowStart = repairWindowState(now, rt.repairWindowStart, rt.repairAttempts, defaultRepairPolicy)
	rt.lastRepair = now
	attempts := rt.repairAttempts
	rt.mu.Unlock()

	iosLog(fmt.Sprintf("[ДИАГ] Ремонт #%d: %s", attempts, reason), false)
	return true
}
// requestRepair rebuilds the Go transport after a confirmed failure, bounded by
// defaultRepairPolicy so a broken network cannot cause a restart loop.
func (rt *iosRuntime) requestRepair(reason string) bool {
	// Physical path state comes from NWPathMonitor via Swift. Network probes are
	// diagnostic only: a reachable TURN path must stay repairable even when the
	// public probe addresses are blocked.
	if !rt.physicalPathAvailable() {
		iosLog(fmt.Sprintf("[ДИАГ] Ремонт отложен (%s): физический путь недоступен по NWPathMonitor", reason), false)
		return false
	}

	if rt.activeSessions() == 0 {
		iosLog(fmt.Sprintf("[ДИАГ] Ремонт не требуется (%s): нет активных DTLS-сессий", reason), false)
		return false
	}

	if !rt.consumeRepairBudget(reason) {
		return false
	}

	rt.dataPlaneSample()
	rt.snapshotAsync("repair:" + reason)
	return rt.requestReconnect("health: " + reason)
}

// noteDataPlaneHealthy clears the repair budget after a fresh handshake proves
// that the tunnel carries traffic again.
func (rt *iosRuntime) noteDataPlaneHealthy() {
	rt.mu.Lock()
	rt.repairAttempts = 0
	rt.repairWindowStart = time.Time{}
	rt.mu.Unlock()
}

func (rt *iosRuntime) rekeyWireGuard() {
	rt.mu.Lock()
	dev := rt.wgDev
	peerHex := rt.wgPeerHex
	rt.mu.Unlock()

	if dev == nil || peerHex == "" {
		return
	}

	var pk device.NoisePublicKey
	if err := pk.FromHex(peerHex); err != nil {
		log.Printf("[WG-WAKE] Ошибка NoisePublicKey: %v", err)
		return
	}

	peer := dev.LookupPeer(pk)
	if peer == nil {
		log.Printf("[WG-WAKE] Peer не найден в WireGuard для rekey")
		return
	}

	log.Printf("[WG-WAKE] iOS wake: сброс ключей WireGuard и немедленный handshake")
	peer.ExpireCurrentKeypairs()
	if err := peer.SendHandshakeInitiation(false); err != nil {
		log.Printf("[WG-WAKE] SendHandshakeInitiation: %v", err)
	}
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
		reconnectWake: make(chan struct{}, 1),
		physicalAvailable: true,
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
	if !rt.lastReconnect.IsZero() && time.Since(rt.lastReconnect) < 1500*time.Millisecond {
		rt.mu.Unlock()
		return false
	}
	rt.lastReconnect = time.Now()
	rt.reconnectRequested = true
	cancel := rt.attemptCancel
	rt.mu.Unlock()

	select {
	case rt.reconnectWake <- struct{}{}:
	default:
	}

	iosLog(fmt.Sprintf("[СЕТЬ] Перезапуск транспорта: %s", reason), false)
	if cancel != nil {
		cancel()
	}
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

func (rt *iosRuntime) activeSessions() int32 {
	rt.mu.Lock()
	stats := rt.stats
	rt.mu.Unlock()
	if stats == nil {
		return 0
	}
	return stats.ActiveConnections.Load()
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

	turnEndpointScheduler.Reset()

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

		iosLog(fmt.Sprintf("[КЛИЕНТ] Ядро: Build #%s (commit %s) | DTLS handshake timeout: %v, Concurrency: %d", BuildNumber, SourceCommit, CurrentHandshakeTimeout, HandshakeConcurrency), false)
		iosLog(fmt.Sprintf("[КЛИЕНТ] Хешей=%d Потоков=%d", len(hashList), params.WorkersPerHash), false)
		iosLog(fmt.Sprintf("[СЕТЬ] Маскировка: %s", obfsModeDisplay(obfsStr)), false)
		iosLog(fmt.Sprintf("[КЛИЕНТ] Режим VK: %s", anonPath), false)

		iosLog("[СЕТЬ] Настройка DNS...", false)
		setupGlobalResolver(dnsStr)
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
	RotateVKHttpClients("смена физической сети")
	turnEndpointScheduler.Reset()
	rt := currentIOSRuntime()
	if rt != nil {
		rt.snapshotAsync("network-change")
		rt.requestReconnect("смена физической сети")
	}
}

// WDTT_SetPhysicalPath receives the current NWPathMonitor state from Swift so
// repairs consult the real physical path instead of public probe addresses.
//
//export WDTT_SetPhysicalPath
func WDTT_SetPhysicalPath(available C.int) {
	rt := currentIOSRuntime()
	if rt != nil {
		rt.setPhysicalAvailable(available != 0)
	}
}

//export WDTT_WakeHealthCheck
func WDTT_WakeHealthCheck() {
	rt := currentIOSRuntime()
	if rt != nil {
		rt.snapshotAsync("wake")
		rt.rekeyWireGuard()
		rt.kickHealthCheck()
	}
}

// WDTT_RequestRepair lets the app rebuild the Go transport after its own
// end-to-end probe failed. The repair budget rejects restart loops.
//
//export WDTT_RequestRepair
func WDTT_RequestRepair() C.int {
	rt := currentIOSRuntime()
	if rt == nil {
		return 0
	}
	if rt.requestRepair("проверка связи в приложении не прошла") {
		return 1
	}
	return 0
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

	resetClocks := func(now time.Time, up int64, down int64) {
		previousUp = up
		previousDown = down
		lastTX = time.Time{}
		lastRX = now
		bytesUpAtLastRX = up
	}

	evaluate := func(now time.Time) {
		gap := now.Sub(lastTick)
		lastTick = now

		up, down, ok := rt.statsSnapshot()
		if !ok {
			return
		}

		// A ticker gap larger than the 30s interval means iOS suspended the extension.
		// Reset clocks so we never interpret sleep intervals as a dead network.
		if gap > 35*time.Second || up < previousUp || down < previousDown {
			resetClocks(now, up, down)
			return
		}

		if lastRX.IsZero() {
			lastRX = now
			bytesUpAtLastRX = up
		}
		priorUp := previousUp
		if up > previousUp {
			lastTX = now
		}
		if down > previousDown {
			lastRX = now
			bytesUpAtLastRX = up
		}

		previousUp = up
		previousDown = down

		// Data-plane check: a live tunnel rekeys regularly. If TX keeps growing
		// while the last WireGuard handshake is old or missing, the transport can
		// look healthy while nothing reaches the server.
		if handshake, _, _, wgOK := rt.wgStats(); wgOK {
			if !handshake.IsZero() && now.Sub(handshake) < 90*time.Second {
				rt.noteDataPlaneHealthy()
			} else if shouldRepairStaleDataPlane(now, rt.activeSessions(), handshake, up, priorUp, 150*time.Second) {
				rt.snapshotAsync("wg-stale")
				if rt.requestRepair("нет свежего handshake WireGuard при исходящем трафике") {
					resetClocks(now, up, down)
					return
				}
			}
		}

		// Reconnect only when the user is actively sending traffic and receives
		// nothing back for a sustained period. Idle/sleep alone is not failure.
		if !lastTX.IsZero() &&
			now.Sub(lastTX) <= 60*time.Second &&
			now.Sub(lastRX) >= 45*time.Second &&
			up-bytesUpAtLastRX >= 2048 {
			if rt.requestRepair("health watchdog: есть TX, но нет RX") {
				resetClocks(now, up, down)
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
			// Explicit wake kick from iOS: reset clocks immediately to start fresh
			now := time.Now()
			lastTick = now
			if up, down, ok := rt.statsSnapshot(); ok {
				resetClocks(now, up, down)
			}
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
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(checkCtx, "login.vk.com")
	if err != nil || len(addrs) == 0 {
		return DnsProbeResult{Reachable: false, StatusText: fmt.Sprintf("сбой резолва: %v", err)}
	}
	return DnsProbeResult{Reachable: true, StatusText: fmt.Sprintf("OK (%s -> %s)", dnsArg, addrs[0])}
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
			turnEndpointScheduler.Reset()
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
		case <-rt.reconnectWake:
		case <-time.After(delay):
		}
	}
}

func runOnce(ctx context.Context, rt *iosRuntime, params TunnelParams, logFn func(line string, isErr bool)) error {
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	rt.noteRunGeneration()
	logFn(fmt.Sprintf("[ДИАГ] storage_dir_set=%t", profileStorageDirectory() != ""), false)
	logFn(fmt.Sprintf("[ГО-ВОРКЕР] Инициализация сессии (ядро: Build #%s, commit %s)...", BuildNumber, SourceCommit), false)
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

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				rt.dataPlaneSample()
			}
		}
	}()

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
			parsedWgCfg, parseErr := parseWgQuick(finalConf)
			if parseErr != nil {
				logFn(fmt.Sprintf("[КОНФИГ] Некорректный WireGuard-конфиг: %v", parseErr), true)
				runCancel()
				return
			}

			rt.setNetworkConfigJSON(extractIOSNetworkConfigJSON(finalConf))
			rt.setDataPlaneState(true, false)
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
			rt.setWireGuard(dev, parsedWgCfg.peerPublicHex)
			rt.setDataPlaneState(true, true)
			rt.wgReadyOnce.Do(func() { close(rt.wgReady) })
			logFn("[IOS-TUN] WireGuard поднят и готов к трафику", false)
			rt.snapshotAsync("wg-up")

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
		const iosMemoryTestWorkers = 108
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
	cfgCoord := NewConfigCoordinator()

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
				stats, waitR, sigR, cfgCoord)
		}(gID, cc, ids, g, myWaitReady, mySignalReady)
	}

	// A transport with live DTLS sessions but no WireGuard config is a dead
	// tunnel that still reports "active". Never let it sit there: if the first
	// GETCONF does not arrive in time, rebuild the transport. Zero sessions mean
	// credentials are still being fetched (captcha, cooldown); restarting there
	// would only repeat the same authorization attempts.
	var configTimedOut atomic.Bool
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		started := time.Now()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				applied, _, _ := rt.dataPlaneState()
				if !shouldRestartForMissingConfig(stats.ActiveConnections.Load(), applied, time.Since(started), 90*time.Second) {
					continue
				}
				if !rt.consumeRepairBudget("config-timeout: нет ответа GETCONF") {
					return
				}
				configTimedOut.Store(true)
				logFn("[ДИАГ] Есть DTLS-сессии, но конфиг WireGuard не получен за 90с — перезапуск транспорта", true)
				rt.snapshotAsync("config-timeout")
				runCancel()
				return
			}
		}
	}()

	wg.Wait()

	if configTimedOut.Load() {
		runCancel()
		select {
		case <-configDone:
		case <-time.After(2 * time.Second):
		}
		disp.Shutdown()
		if wgDevice != nil {
			rt.clearWireGuard()
			wgDevice.Close()
		}
		return fmt.Errorf("конфиг WireGuard не получен за 90с")
	}

	// Stop transport/dispatcher before closing WireGuard. This avoids the
	// shutdown deadlock where WireGuard waits on I/O that is still owned by the
	// transport side.
	runCancel()
	close(configCh)
	<-configDone
	disp.Shutdown()

	if wgDevice != nil {
		rt.clearWireGuard()
		wgDevice.Close()
	}

	logFn("[ГО-ВОРКЕР] Все воркеры завершены", false)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if stats != nil && stats.ActiveConnections.Load() == 0 {
		return fmt.Errorf("все воркеры завершились или не смогли подключиться")
	}
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
