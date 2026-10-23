package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/logging"
	"github.com/pion/stun/v3"
	"github.com/pion/transport/v4/stdnet"
	"github.com/pion/turn/v5"
)

const (
	workerSendBuf         = 8
	sessionReadTimeout    = 30 * time.Minute // Increased from 60s to 30min
	readBufSize           = 1600
	socketBufSize         = 128 * 1024
	keepaliveByte         = 0xFF // DTLS-level keepalive marker
	keepaliveInterval     = 15 * time.Second
	keepaliveStaggerSlots = 2000 // 0..1999 ms deterministic phase spread
	seatCoolFor           = 1200 * time.Millisecond // VK TURN relay seat retention window (build 430)
	// Ограничение на DNS/Dial/Allocate, чтобы зависание не переехало из DTLS
	// в подготовку TURN. Сам DTLS handshake по-прежнему ограничен 50 секундами.
	turnPreparationTimeout = 20 * time.Second
)

// Handshake semaphore: limit to 3 concurrent DTLS handshakes
const (
	HandshakeConcurrency = 3
)

var (
	SourceCommit            = "dev"
	BuildNumber             = "0"
	HandshakeTimeoutDefault = 50 * time.Second
	HandshakeTimeoutControl = 50 * time.Second
	CurrentHandshakeTimeout = HandshakeTimeoutControl
	handshakeSem            = make(chan struct{}, HandshakeConcurrency)
	HandshakesInFlight      atomic.Int32
	HandshakesWaiting       atomic.Int32
	HandshakeSuccess        atomic.Int64
	HandshakeTimeout        atomic.Int64
	HandshakeCanceled       atomic.Int64
	HandshakeErrors         atomic.Int64
)

func init() {
	log.Printf("[DTLS] Режим КОНТРОЛЬНОЙ сборки: DTLS handshake timeout: %v Handshake concurrency: %d Build: #%s Source commit: %s (контрольная сборка: проверка на устройстве и сопоставление с дампом)",
		CurrentHandshakeTimeout, HandshakeConcurrency, BuildNumber, SourceCommit)
}

// NullLoggerFactory подавляет логи pion
type NullLoggerFactory struct{}

func (n *NullLoggerFactory) NewLogger(_ string) logging.LeveledLogger { return &NullLogger{} }

type NullLogger struct{}

func (n *NullLogger) Trace(_ string)                    {}
func (n *NullLogger) Tracef(_ string, _ ...interface{}) {}
func (n *NullLogger) Debug(_ string)                    {}
func (n *NullLogger) Debugf(_ string, _ ...interface{}) {}
func (n *NullLogger) Info(_ string)                     {}
func (n *NullLogger) Infof(_ string, _ ...interface{})  {}
func (n *NullLogger) Warn(_ string)                     {}
func (n *NullLogger) Warnf(_ string, _ ...interface{})  {}
func (n *NullLogger) Error(_ string)                    {}
func (n *NullLogger) Errorf(_ string, _ ...interface{}) {}

// connectedUDPConn — обёртка для connected UDP socket → PacketConn
type connectedUDPConn struct{ *net.UDPConn }

func (c *connectedUDPConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.Write(p) }

// keepaliveStagger returns a reproducible per-worker phase offset in the
// 0..2s window. Build 170 uses separate salts for TURN and DTLS so dozens of
// periodic keepalives do not fire as one allocation/CPU burst. Deterministic
// staggering makes A/B runs comparable while preserving the 10s/15s cadence.
func keepaliveStagger(sessionID, salt int) time.Duration {
	v := (sessionID*137 + salt*503) % keepaliveStaggerSlots
	if v < 0 {
		v = -v
	}
	return time.Duration(v) * time.Millisecond
}

func waitKeepalivePhase(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func RunSession(
	ctx context.Context,
	tp *TurnParams,
	peer *net.UDPAddr,
	d *Dispatcher,
	localPort string,
	configOp *ConfigOperation,
	configCh chan<- string,
	sessionID int,
	creds *Credentials,
	deviceID, password string,
	stats *Stats,
) (bool, time.Duration, error) {
	// Страховочное освобождение операции получения конфига устанавливается сразу при входе,
	// гарантируя освобождение до любых ранних возвратов из RunSession (ошибки Allocate, DTLS и т.д.)
	if configOp != nil {
		defer configOp.ReleaseWithCooldown(3 * time.Second)
	}

	// Контекст нужен планировщику TURN и ограниченной сетевой подготовке.
	// cleanup ниже отменяет его ещё раз, это безопасно.
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	configDelivered := false
	var firstWrapUp uint32
	var firstWrapDown uint32
	var firstDtlsWrite uint32
	var firstDtlsRead uint32

	candidates, err := buildTurnEndpointCandidates(creds.TurnURLs, tp)
	if err != nil {
		return false, 0, err
	}

	// Первая попытка сохраняет прежнее распределение sessionID % len(urls).
	// PreferredTurnURL задаётся воркером только после DTLS_TIMEOUT.
	preferredURL := creds.PreferredTurnURL
	if preferredURL == "" {
		preferredURL = creds.TurnURLs[sessionID%len(creds.TurnURLs)]
	}

	HandshakesWaiting.Add(1)
	semStart := time.Now()
	permit, err := turnEndpointScheduler.Acquire(sessCtx, preferredURL, candidates)
	HandshakesWaiting.Add(-1)
	if err != nil {
		return false, 0, fmt.Errorf("ожидание TURN endpoint: %w", err)
	}
	semWait := time.Since(semStart)

	HandshakesInFlight.Add(1)
	outcome := turnOutcomeOther
	var permitOnce sync.Once
	releasePermit := func() {
		permitOnce.Do(func() {
			// Переписывать исход можно только по внешней отмене; внутренний
			// тирдаун не должен маскировать timeout/teardown.
			permit.Finish(permitOutcome(ctx, outcome))
			HandshakesInFlight.Add(-1)
		})
	}
	defer releasePermit()

	selectedURL := permit.URL()
	turnAddr := permit.Endpoint()

	log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [SEM] Ожидание разрешения TURN/handshake: %v (hs_активно=%d, в_очереди=%d, endpoint=%s, занято=%d/%d)",
		creds.GroupID, sessionID, creds.AttemptID, semWait.Round(time.Millisecond), HandshakesInFlight.Load(), HandshakesWaiting.Load(), turnAddr, len(handshakeSem), cap(handshakeSem))

	// Вся сетевая подготовка идёт уже под разрешением планировщика.
	// DialContext ограничен тем же prepCtx, что и Allocate.
	prepCtx, prepCancel := context.WithTimeout(sessCtx, turnPreparationTimeout)
	defer prepCancel()

	var dialer net.Dialer
	rawConn, err := dialer.DialContext(prepCtx, "udp", turnAddr)
	if err != nil {
		return false, 0, fmt.Errorf("подключение TURN UDP: %w", err)
	}
	c, ok := rawConn.(*net.UDPConn)
	if !ok {
		_ = rawConn.Close()
		return false, 0, fmt.Errorf("TURN UDP: неожиданный тип соединения %T", rawConn)
	}
	defer c.Close()
	_ = c.SetReadBuffer(socketBufSize)
	_ = c.SetWriteBuffer(socketBufSize)
	var turnConn net.PacketConn = &connectedUDPConn{c}

	// Pion повторно резолвит адрес в NewClient. Передаём уже разрешённый
	// IP:port, чтобы DNS-задержка не обходила prepCtx.
	turnClientAddr := turnAddr
	if remote := rawConn.RemoteAddr(); remote != nil {
		turnClientAddr = remote.String()
	}

	log.Printf("[СЕССИЯ #%d] TURN UDP (%s)", sessionID, turnAddr)

	// RequestedAddressFamily
	var addrFamily turn.RequestedAddressFamily
	if peer.IP.To4() != nil {
		addrFamily = turn.RequestedAddressFamilyIPv4
	} else {
		addrFamily = turn.RequestedAddressFamilyIPv6
	}

	// Pion's default stdnet.NewNet enumerates network interfaces through
	// NETLINK_ROUTE. Some Huawei/Honor ROMs deny that operation to apps.
	// The zero-value stdnet.Net provides everything this UDP client uses
	// without performing the unnecessary interface enumeration.
	tc, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr:         turnClientAddr,
		TURNServerAddr:         turnClientAddr,
		Conn:                   turnConn,
		Net:                    new(stdnet.Net),
		Username:               creds.User,
		Password:               creds.Pass,
		RequestedAddressFamily: addrFamily,
		LoggerFactory:          &NullLoggerFactory{},
	})
	if err != nil {
		return false, 0, fmt.Errorf("TURN клиент: %w", err)
	}
	defer tc.Close()

	if err = tc.Listen(); err != nil {
		return false, 0, fmt.Errorf("TURN Listen: %w", err)
	}

	// Allocate не принимает context, поэтому отмена/таймаут закрывают транзакцию
	// и UDP-сокет; ожидание ограничено turnPreparationTimeout.
	var prepAborted atomic.Bool
	prepAbortDone := make(chan struct{})
	stopPrepAbort := context.AfterFunc(prepCtx, func() {
		prepAborted.Store(true)
		tc.Close()
		_ = c.Close()
		close(prepAbortDone)
	})
	relay, err := tc.Allocate()
	if !stopPrepAbort() {
		<-prepAbortDone
	}
	prepErr := prepCtx.Err()
	allocTimedOut := prepAborted.Load() || prepErr != nil
	prepCancel()
	if allocTimedOut {
		if relay != nil {
			_ = relay.Close()
		}
		if err != nil {
			return false, 0, fmt.Errorf("TURN подготовка: %w", err)
		}
		return false, 0, fmt.Errorf("TURN подготовка: %w", prepErr)
	}
	if err != nil {
		var turnErr *stun.TurnError
		if errors.As(err, &turnErr) {
			switch turnErr.ErrorCodeAttr.Code {
			case stun.CodeUnauthorized, stun.CodeWrongCredentials:
				return false, 0, fmt.Errorf("TURN_AUTH_REJECTED (код %d): %w", turnErr.ErrorCodeAttr.Code, err)
			case stun.CodeForbidden:
				return false, 0, fmt.Errorf("TURN_FORBIDDEN (код 403): %w", err)
			case stun.CodeStaleNonce:
				return false, 0, fmt.Errorf("TURN_STALE_NONCE (код 438): %w", err)
			case stun.CodeAllocQuotaReached:
				return false, 0, fmt.Errorf("TURN_QUOTA (код 486): %w", err)
			case stun.CodeInsufficientCapacity:
				return false, 0, fmt.Errorf("TURN_CAPACITY (код 508): %w", err)
			default:
				return false, 0, fmt.Errorf("TURN_ERROR (код %d): %w", turnErr.ErrorCodeAttr.Code, err)
			}
		}
		if errors.Is(err, stun.ErrAttributeNotFound) || strings.Contains(strings.ToLower(err.Error()), "attribute not found") {
			return false, 0, fmt.Errorf("TURN_ATTR_NOT_FOUND (неполный ответ или отсутствие обязательного атрибута STUN/TURN): %w", err)
		}
		errStr := err.Error()
		errStrLower := strings.ToLower(errStr)
		if strings.Contains(errStr, "401") || strings.Contains(errStr, "441") || strings.Contains(errStrLower, "unauthorized") || strings.Contains(errStrLower, "wrong credentials") {
			return false, 0, fmt.Errorf("TURN_AUTH_REJECTED: %w", err)
		}
		if strings.Contains(errStr, "403") || strings.Contains(errStrLower, "forbidden") {
			return false, 0, fmt.Errorf("TURN_FORBIDDEN: %w", err)
		}
		if strings.Contains(errStr, "438") || strings.Contains(errStrLower, "stale nonce") {
			return false, 0, fmt.Errorf("TURN_STALE_NONCE: %w", err)
		}
		if strings.Contains(errStr, "486") || strings.Contains(errStrLower, "quota") {
			return false, 0, fmt.Errorf("TURN_QUOTA: %w", err)
		}
		if strings.Contains(errStr, "508") || strings.Contains(errStrLower, "capacity") {
			return false, 0, fmt.Errorf("TURN_CAPACITY: %w", err)
		}
		return false, 0, fmt.Errorf("TURN Allocate (%s): %w", turnAddr, err)
	}
	defer func() {
		if relay != nil {
			_ = relay.Close()
		}
		select {
		case <-time.After(seatCoolFor):
		case <-ctx.Done():
		}
	}()

	// Reset error count on successful allocation
	getStreamCache(creds.CacheStreamID).errorCount.Store(0)

	log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [ALLOCATE] Успешно выделен relay: time=%s, turn=%s, allocated=%s, vps=%s",
		creds.GroupID, sessionID, creds.AttemptID,
		time.Now().UTC().Format(time.RFC3339Nano),
		turnAddr,
		relay.LocalAddr().String(),
		peer.String(),
	)
	log.Printf("[СЕССИЯ #%d] Relay: %s", sessionID, relay.LocalAddr())

	// Pipe для DTLS ↔ TURN relay
	pipeA, pipeB := connutil.AsyncPacketPipe()

	var (
		cleanupOnce sync.Once
		sessionWg   sync.WaitGroup
		relayWg     sync.WaitGroup
		proxyWg     sync.WaitGroup
		dtlsConn    *dtls.Conn
	)

	cleanup := func() {
		cleanupOnce.Do(func() {
			// 1. Отменяем контекст сессии — горутины получают <-sessCtx.Done()
			sessCancel()

			// 2. Немедленно устанавливаем дедлайны и закрываем I/O для гарантированной разблокировки сетевых вызовов
			now := time.Now()
			if dtlsConn != nil {
				_ = dtlsConn.SetDeadline(now)
				_ = dtlsConn.Close()
			}
			if pipeA != nil {
				_ = pipeA.SetDeadline(now)
				_ = pipeA.Close()
			}
			if pipeB != nil {
				_ = pipeB.SetDeadline(now)
				_ = pipeB.Close()
			}
			if relay != nil {
				_ = relay.SetDeadline(now)
				_ = relay.Close()
			}

			// 3. Ждём завершения запущенных горутин ТОЛЬКО ПОСЛЕ разблокировки I/O
			proxyWg.Wait()
			relayWg.Wait()
			sessionWg.Wait()
		})
	}
	defer cleanup()

	// Keepalive goroutine (TURN binding request). Build 170 staggers only the
	// initial phase; after that the existing 10s cadence is unchanged.
	sessionWg.Add(1)
	go func() {
		defer sessionWg.Done()
		if !waitKeepalivePhase(sessCtx, keepaliveStagger(sessionID, 0)) {
			return
		}
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sessCtx.Done():
				return
			case <-t.C:
				tc.SendBindingRequest()
			}
		}
	}()

	// Relay ↔ Pipe proxy (with RTP obfuscation)
	relayWg.Add(2)

	useWrap := len(tp.WrapKey) == wrapKeyLen

	// Initialize obfs config per session
	var obfsCfg *ObfsConfig
	var obfsWriteState *ObfsState
	if useWrap {
		obfsCfg = NewObfsConfig(tp.ObfsMode)
		obfsWriteState = NewObfsState()
	}

	stopRelay := context.AfterFunc(sessCtx, func() {
		_ = relay.SetDeadline(time.Now())
		_ = pipeA.SetDeadline(time.Now())
	})
	defer stopRelay()

	// Первичная причина внутреннего тирдауна: handshake в этом случае вернёт
	// только context.Canceled, поэтому ошибку relay/pipe запоминаем до отмены
	// сессии и передаём в SessionTeardownError.
	var teardown teardownCause

	// relay → pipeA (UNWRAP: strip RTP header + decrypt)
	go func() {
		defer relayWg.Done()
		defer sessCancel()
		// Max incoming: RTP header (12) + AEAD tag (16) + padding.
		readBufLen := readBufSize + 80
		buf := make([]byte, readBufLen)
		plain := make([]byte, readBufSize)
		for {
			n, _, readErr := relay.ReadFrom(buf)
			if readErr != nil {
				teardown.record(fmt.Errorf("чтение TURN relay: %w", readErr))
				return
			}
			payload := buf[:n]
			if useWrap {
				if !obfsIsRTPPacket(payload) {
					log.Printf("[СЕССИЯ #%d] OBFS unwrap: unexpected packet (n=%d)", sessionID, n)
					continue
				}
				m, wrapErr := obfsUnwrapPacket(tp.WrapKey, payload, plain)
				if wrapErr != nil {
					log.Printf("[СЕССИЯ #%d] OBFS unwrap: %v (n=%d)", sessionID, wrapErr, n)
					continue
				}
				payload = plain[:m]
			}
			if atomic.CompareAndSwapUint32(&firstWrapUp, 0, 1) {
				log.Printf("[СЕССИЯ #%d] [ДЕБАГ] Успешно расшифрован/получен ПЕРВЫЙ пакет от TURN Relay (%d байт)", sessionID, len(payload))
			}
			if _, writeErr := pipeA.WriteTo(payload, peer); writeErr != nil {
				teardown.record(fmt.Errorf("запись в DTLS pipe: %w", writeErr))
				return
			}
		}
	}()

	// pipeA → relay (WRAP: add RTP header + encrypt)
	go func() {
		defer relayWg.Done()
		defer sessCancel()
		b := make([]byte, readBufSize)
		for {
			n, _, readErr := pipeA.ReadFrom(b)
			if readErr != nil {
				teardown.record(fmt.Errorf("чтение DTLS pipe: %w", readErr))
				return
			}
			out := b[:n]
			if useWrap {
				if obfsCfg != nil && obfsWriteState != nil {
					wrapped, wrapErr := obfsWrapPacket(tp.WrapKey, out, obfsCfg, obfsWriteState)
					if wrapErr != nil {
						log.Printf("[СЕССИЯ #%d] OBFS wrap: %v", sessionID, wrapErr)
						return
					}
					out = wrapped
				}
			}
			if _, writeErr := relay.WriteTo(out, peer); writeErr != nil {
				teardown.record(fmt.Errorf("запись в TURN relay: %w", writeErr))
				return
			}
			if atomic.CompareAndSwapUint32(&firstWrapDown, 0, 1) {
				log.Printf("[СЕССИЯ #%d] [ДЕБАГ] Успешно зашифрован/отправлен ПЕРВЫЙ пакет на TURN Relay (%d байт)", sessionID, len(out))
			}
		}
	}()

	// DTLS с поддержкой Connection ID (без SNI)
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return false, 0, fmt.Errorf("генерация сертификата: %w", err)
	}

	dtlsCfg := &dtls.Config{
		Certificates:          []tls.Certificate{cert},
		InsecureSkipVerify:    true,
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(),
		MTU:                   1100,
		// No ServerName (SNI) — less detectable by DPI
	}

	rawDtlsConn, err := dtls.Client(pipeB, peer, dtlsCfg)
	if err != nil {
		outcome = turnOutcomeOther
		releasePermit()
		HandshakeErrors.Add(1)
		return false, 0, fmt.Errorf("DTLS клиент (%s): %w", turnAddr, err)
	}
	dtlsConn = rawDtlsConn
	defer dtlsConn.Close()

	hsTimeout := CurrentHandshakeTimeout
	if tp != nil && tp.HandshakeTimeout > 0 {
		hsTimeout = tp.HandshakeTimeout
	}
	hctx, hcancel := context.WithTimeout(sessCtx, hsTimeout)
	hsStart := time.Now()
	log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [DTLS] Рукопожатие начато... (relay=%s, gen=%d, очередь=%v, hs_активно=%d, в_очереди=%d)",
		creds.GroupID, sessionID, creds.AttemptID, turnAddr, creds.Generation, semWait.Round(time.Millisecond), HandshakesInFlight.Load(), HandshakesWaiting.Load())
	err = dtlsConn.HandshakeContext(hctx)
	hsDur := time.Since(hsStart)
	// Причину снимаем до собственной очистки: hcancel() не переписывает уже
	// наступивший DeadlineExceeded, но диагностика не должна зависеть от порядка.
	handshakeCtxErr := hctx.Err()
	hcancel()

	if err != nil {
		log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [DTLS] Ошибка handshake (%v, semWait=%v, hs_активно=%d, в_очереди=%d, endpoint=%s): %v",
			creds.GroupID, sessionID, creds.AttemptID, hsDur.Round(time.Millisecond), semWait.Round(time.Millisecond), HandshakesInFlight.Load(), HandshakesWaiting.Load(), turnAddr, err)

		var hsOut error
		outcome, hsOut = classifyHandshakeFailure(ctx, sessCtx, handshakeCtxErr, err, teardown.get(), turnAddr, selectedURL)
		switch outcome {
		case turnOutcomeTimeout:
			HandshakeTimeout.Add(1)
		case turnOutcomeCanceled:
			HandshakeCanceled.Add(1)
		default:
			HandshakeErrors.Add(1)
		}
		releasePermit()
		return false, 0, hsOut
	}

	outcome = turnOutcomeSuccess
	releasePermit()
	HandshakeSuccess.Add(1)
	establishedStart := time.Now()
	log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [DTLS] Соединение установлено ✓ (relay=%s, gen=%d, очередь=%v, hs=%v, ok=%d, tmout=%d, endpoint=%s)",
		creds.GroupID, sessionID, creds.AttemptID, turnAddr, creds.Generation, semWait.Round(time.Millisecond), hsDur.Round(time.Millisecond), HandshakeSuccess.Load(), HandshakeTimeout.Load(), turnAddr)

	stats.ActiveConnections.Add(1)
	defer stats.ActiveConnections.Add(-1)

	// Запрос конфига
	if configOp != nil && configCh != nil {
		conf, confErr := RequestConfig(dtlsConn, localPort, deviceID, password)
		if confErr != nil {
			errStr := confErr.Error()
			if strings.Contains(errStr, "FATAL_AUTH") {
				return false, time.Since(establishedStart), confErr
			}
			log.Printf("[ВОРКЕР #%d] Ошибка получения конфига: %v", sessionID, confErr)
			configOp.ReleaseWithCooldown(3 * time.Second)
		} else if conf != "" {
			if configOp.DeliverConfig(conf, configCh, 2*time.Second) {
				configDelivered = true
				log.Printf("[ВОРКЕР #%d] Конфиг успешно получен и передан в канал", sessionID)
			} else {
				log.Printf("[ВОРКЕР #%d] Не удалось передать полученный конфиг в канал (канал занят/заблокирован)", sessionID)
			}
		} else {
			log.Printf("[ВОРКЕР #%d] Сервер ещё не выдал WireGuard-конфиг (NOCONF), повторим позже", sessionID)
			configOp.ReleaseWithCooldown(3 * time.Second)
		}
	} else {
		if authErr := SendAuth(dtlsConn, deviceID, password); authErr != nil {
			log.Printf("[ВОРКЕР #%d] Ошибка отправки авторизации (AUTH): %v", sessionID, authErr)
			return false, time.Since(establishedStart), fmt.Errorf("ошибка отправки авторизации (AUTH): %w", authErr)
		}
		log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [AUTH] Пакет авторизации отправлен (серверное подтверждение в протоколе отсутствует)",
			creds.GroupID, sessionID, creds.AttemptID)
	}

	log.Printf("[ВОРКЕР #%d] [READY] Туннель готов к работе ✓", sessionID)

	// Регистрация в диспетчере
	slot := &WorkerSlot{
		ID:     sessionID,
		SendCh: make(chan []byte, workerSendBuf),
	}
	d.Register(slot)
	defer d.Unregister(slot)

	// Proxy DTLS ↔ Dispatcher
	proxyWg.Add(3) // +1 for keepalive goroutine

	stopDTLS := context.AfterFunc(sessCtx, func() {
		_ = dtlsConn.SetDeadline(time.Now())
	})
	defer stopDTLS()

	// DTLS Keepalive: same 15s cadence, with a different deterministic phase
	// from TURN so periodic maintenance does not synchronize across workers.
	go func() {
		defer proxyWg.Done()
		if !waitKeepalivePhase(sessCtx, keepaliveStagger(sessionID, 1)) {
			return
		}
		t := time.NewTicker(keepaliveInterval)
		defer t.Stop()
		ping := []byte{keepaliveByte}
		for {
			select {
			case <-sessCtx.Done():
				return
			case <-t.C:
				_ = dtlsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := dtlsConn.Write(ping); err != nil {
					return
				}
			}
		}
	}()

	// Writer: dispatcher → DTLS
	go func() {
		defer proxyWg.Done()
		defer sessCancel()
		for {
			select {
			case <-sessCtx.Done():
				return
			case pkt, ok := <-slot.SendCh:
				if !ok {
					return
				}
				_ = dtlsConn.SetWriteDeadline(time.Now().Add(sessionReadTimeout))
				_, writeErr := dtlsConn.Write(pkt)
				putPktBuf(pkt)
				if writeErr != nil {
					log.Printf("[ВОРКЕР #%d] Ошибка Writer: %v", sessionID, writeErr)
					return
				}
				if atomic.CompareAndSwapUint32(&firstDtlsWrite, 0, 1) {
					log.Printf("[ВОРКЕР #%d] [ДЕБАГ] Отправлен ПЕРВЫЙ пакет в DTLS-соединение (%d байт)", sessionID, len(pkt))
				}
			}
		}
	}()

	// Reader: DTLS → dispatcher
	go func() {
		defer proxyWg.Done()
		defer sessCancel()
		b := make([]byte, 2000)
		for {
			_ = dtlsConn.SetReadDeadline(time.Now().Add(sessionReadTimeout))
			n, readErr := dtlsConn.Read(b)
			if readErr != nil {
				if sessCtx.Err() != nil {
					return
				}
				if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
					continue
				}
				log.Printf("[ВОРКЕР #%d] Ошибка Reader: %v", sessionID, readErr)
				return
			}

			// Skip keepalive pong from server
			if n == 1 && b[0] == keepaliveByte {
				continue
			}

			if atomic.CompareAndSwapUint32(&firstDtlsRead, 0, 1) {
				log.Printf("[ВОРКЕР #%d] [ДЕБАГ] Получен ПЕРВЫЙ пакет из DTLS-соединения (%d байт)", sessionID, n)
			}

			pkt := getPktBuf(n)
			copy(pkt, b[:n])
			select {
			case d.ReturnCh <- pkt:
			case <-sessCtx.Done():
				putPktBuf(pkt)
				return
			}
		}
	}()

	proxyWg.Wait()
	cleanup()
	log.Printf("[СЕССИЯ #%d] Завершена (активность после handshake=%v)", sessionID, time.Since(establishedStart).Round(time.Millisecond))
	return configDelivered, time.Since(establishedStart), nil
}

func RunPing(
	ctx context.Context,
	tp *TurnParams,
	peer *net.UDPAddr,
	creds *Credentials,
) (int64, error) {
	startPing := time.Now()

	if len(creds.TurnURLs) == 0 {
		return 0, fmt.Errorf("нет TURN URL")
	}
	selectedURL := creds.TurnURLs[0]

	urlhost, urlport, err := net.SplitHostPort(selectedURL)
	if err != nil {
		return 0, err
	}
	if tp.Host != "" { urlhost = tp.Host }
	if tp.Port != "" { urlport = tp.Port }
	turnAddr := net.JoinHostPort(urlhost, urlport)

	resolved, err := net.ResolveUDPAddr("udp", turnAddr)
	if err != nil { return 0, err }
	c, err := net.DialUDP("udp", nil, resolved)
	if err != nil { return 0, err }
	defer c.Close()

	var turnConn net.PacketConn = &connectedUDPConn{c}

	var addrFamily turn.RequestedAddressFamily
	if peer.IP.To4() != nil {
		addrFamily = turn.RequestedAddressFamilyIPv4
	} else {
		addrFamily = turn.RequestedAddressFamilyIPv6
	}

	tc, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr:         turnAddr,
		TURNServerAddr:         turnAddr,
		Conn:                   turnConn,
		Net:                    new(stdnet.Net),
		Username:               creds.User,
		Password:               creds.Pass,
		RequestedAddressFamily: addrFamily,
		LoggerFactory:          &NullLoggerFactory{},
	})
	if err != nil { return 0, err }
	defer tc.Close()

	if err = tc.Listen(); err != nil { return 0, err }

	relay, err := tc.Allocate()
	if err != nil { return 0, err }
	defer relay.Close()

	pipeA, pipeB := connutil.AsyncPacketPipe()
	defer pipeA.Close()
	defer pipeB.Close()

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	var relayWg sync.WaitGroup
	relayWg.Add(2)

	useWrap := len(tp.WrapKey) == wrapKeyLen
	var obfsCfg *ObfsConfig
	var obfsWriteState *ObfsState
	if useWrap {
		obfsCfg = NewObfsConfig(tp.ObfsMode)
		obfsWriteState = NewObfsState()
	}

	// relay → pipeA
	go func() {
		defer relayWg.Done()
		defer sessCancel()
		buf := make([]byte, readBufSize+80)
		plain := make([]byte, readBufSize)
		for {
			n, _, err := relay.ReadFrom(buf)
			if err != nil { return }
			payload := buf[:n]
			if useWrap {
				if !obfsIsRTPPacket(payload) { continue }
				m, err := obfsUnwrapPacket(tp.WrapKey, payload, plain)
				if err != nil { continue }
				payload = plain[:m]
			}
			_, _ = pipeA.WriteTo(payload, peer)
		}
	}()

	// pipeA → relay
	go func() {
		defer relayWg.Done()
		defer sessCancel()
		b := make([]byte, readBufSize)
		for {
			n, _, err := pipeA.ReadFrom(b)
			if err != nil { return }
			out := b[:n]
			if useWrap {
				wrapped, err := obfsWrapPacket(tp.WrapKey, out, obfsCfg, obfsWriteState)
				if err != nil { return }
				out = wrapped
			}
			_, _ = relay.WriteTo(out, peer)
		}
	}()

	cert, err := selfsign.GenerateSelfSigned()
	if err != nil { return 0, err }

	dtlsCfg := &dtls.Config{
		Certificates:          []tls.Certificate{cert},
		InsecureSkipVerify:    true,
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(),
		MTU:                   1100,
	}

	dtlsConn, err := dtls.Client(pipeB, peer, dtlsCfg)
	if err != nil { return 0, err }
	defer dtlsConn.Close()

	hctx, hcancel := context.WithTimeout(sessCtx, 15*time.Second)
	defer hcancel()

	err = dtlsConn.HandshakeContext(hctx)
	if err != nil { return 0, err }

	rtt := time.Since(startPing).Milliseconds()
	// Handshake completes -> we have a successful round trip!
	return rtt, nil
}