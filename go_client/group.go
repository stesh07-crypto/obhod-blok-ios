package main

import (
	"context"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const workersPerGroup = 9

// WorkerGroup:
// Запускает 9 потоков с одними кредами. Ротации нет — работает до смерти воркеров.
func WorkerGroup(ctx context.Context, groupID, hashIndex int, tp *TurnParams, peer *net.UDPAddr, d *Dispatcher, localPort string,
	configChan chan<- string, workerIDs []int, pauseFlag *int32, deviceID, password string, stats *Stats, waitReady <-chan struct{}, signalReady chan<- struct{}, configSent *int32, configRequestInFlight *int32) {
	var signalOnce sync.Once
	releaseSignalReady := func() {
		if signalReady != nil {
			signalOnce.Do(func() {
				close(signalReady)
			})
		}
	}
	defer releaseSignalReady()

	// Каскадный запуск: ждем свою очередь
	if waitReady != nil {
		log.Printf("[ГРУППА #%d] Ожидание сигнала от предыдущей группы...", groupID)
		select {
		case <-waitReady:
		case <-ctx.Done():
			return
		}
	}

	// Doze-mode пауза
	for atomic.LoadInt32(pauseFlag) != 0 {
		if ctx.Err() != nil {
			return
		}
		time.Sleep(1 * time.Second)
	}

	hash := tp.Hashes[hashIndex%len(tp.Hashes)]
	shortHash := hash
	if len(shortHash) > 8 {
		shortHash = shortHash[:8]
	}
	log.Printf("[ГРУППА #%d] Запрос кредов (хеш: %s...)", groupID, shortHash)

	credStreamID := groupID * 100
	var user, pass string
	var turnURLs []string
	var gen uint64
	var err error

	user, pass, turnURLs, gen, err = GetCredsWithGen(ctx, hash, credStreamID)
	if err != nil {
		if callErr, isDead := asCallUnavailableError(err); isDead {
			log.Printf("[ГРУППА #%d] ❌ Хеш недействителен или звонок завершён (%v). Группа остановлена, передача эстафеты дальше...", groupID, callErr)
			releaseSignalReady()
			return
		}

		log.Printf("[ГРУППА #%d] Начальный запрос кредов не удался (%v). Передаём эстафету дальше и переходим в фоновый retry...", groupID, err)
		releaseSignalReady()

		retryDelay := 10 * time.Second
		for err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			if retryDelay < 30*time.Second {
				retryDelay += 5 * time.Second
			}
			user, pass, turnURLs, gen, err = GetCredsWithGen(ctx, hash, credStreamID)
			if err == nil {
				log.Printf("[ГРУППА #%d] Фоновые креды получены! (gen=%d) Запуск %d воркеров...", groupID, gen, len(workerIDs))
				break
			}
			if callErr, isDead := asCallUnavailableError(err); isDead {
				log.Printf("[ГРУППА #%d] ❌ Хеш недействителен или звонок завершён (%v). Группа остановлена.", groupID, callErr)
				return
			}
			log.Printf("[ГРУППА #%d] Фоновый запрос кредов не удался: %v (след. попытка через %v)", groupID, err, retryDelay)
		}
	} else {
		// Сигнализируем следующей группе, что мы успешно запустились (креды получены + 2 сек форы)
		if signalReady != nil {
			go func() {
				select {
				case <-time.After(2000 * time.Millisecond):
					releaseSignalReady()
					log.Printf("[ГРУППА #%d] Успешный старт! Передача эстафеты следующей группе...", groupID)
				case <-ctx.Done():
				}
			}()
		}
	}

	creds := &Credentials{User: user, Pass: pass, TurnURLs: turnURLs, CacheStreamID: credStreamID, Generation: gen, GroupID: groupID}
	coord := NewGroupCredCoordinator(groupID, hash, credStreamID, creds)

	log.Printf("[ГРУППА #%d] Креды OK (gen=%d), TURN: %v, %d воркеров", groupID, creds.Generation, creds.TurnURLs, len(workerIDs))

	var wg sync.WaitGroup


	for i, wid := range workerIDs {
		wg.Add(1)

		// Stagger: 500мс между воркерами
		workerDelay := time.Duration(i) * 500 * time.Millisecond

		go func(wid int, delay time.Duration) {
			defer wg.Done()

			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}

			shouldGetConfig := (configChan != nil)
			attempt := 0
			transientFailures := 0

			for {
				if ctx.Err() != nil {
					return
				}

				getConf := false
				if shouldGetConfig && atomic.LoadInt32(configSent) == 0 {
					getConf = atomic.CompareAndSwapInt32(configRequestInFlight, 0, 1)
				}
				var cc chan<- string
				if getConf {
					cc = configChan
				}

				credsSnapshot := coord.Snapshot(attempt)

				sessionStarted := time.Now()
				configDelivered, sessErr := RunSession(ctx, tp, peer, d, localPort,
					getConf, cc, wid, &credsSnapshot, deviceID, password, stats)
				sessionLifetime := time.Since(sessionStarted)

				quotaRetry := false
				transientSocketRetry := false
				if getConf {
					if configDelivered {
						atomic.StoreInt32(configSent, 1)
					} else {
						atomic.StoreInt32(configRequestInFlight, 0)
					}
				}

				if sessErr != nil {
					if ctx.Err() != nil {
						return
					}
					errStr := sessErr.Error()
					errStrLower := strings.ToLower(errStr)


					isTurnQuota := strings.Contains(errStr, "TURN_QUOTA") || strings.Contains(errStrLower, "quota") || strings.Contains(errStr, "486")
					isTurnForbidden := strings.Contains(errStr, "TURN_FORBIDDEN") || (strings.Contains(errStrLower, "forbidden") && !strings.Contains(errStr, "TURN_AUTH_REJECTED"))
					isTurnStaleNonce := strings.Contains(errStr, "TURN_STALE_NONCE") || strings.Contains(errStrLower, "stale nonce")
					isTurnCapacity := strings.Contains(errStr, "TURN_CAPACITY") || strings.Contains(errStr, "508")
					turnAllocAttrMissing := strings.Contains(errStr, "TURN_ATTR_NOT_FOUND") || (strings.Contains(errStrLower, "turn allocate") && strings.Contains(errStrLower, "attribute not found"))
					isDtlsTimeout := strings.Contains(errStr, "DTLS_TIMEOUT")

					transientSocketRetry = strings.Contains(errStrLower, "cannot create socket") ||
						strings.Contains(errStrLower, "network is unreachable") ||
						strings.Contains(errStrLower, "no route to host") ||
						strings.Contains(errStrLower, "network changed") ||
						strings.Contains(errStrLower, "operation not permitted")

					quotaRetry = isTurnQuota

					// ТОЛЬКО окончательный отказ авторизации (401/441/TURN_AUTH_REJECTED) вызывает обновление реквизитов!
					// 403 Forbidden, 438 Stale Nonce, 486 Quota, 508 Capacity, Attribute Not Found, DTLS_TIMEOUT НЕ являются turnAuthRejected!
					turnAuthRejected := !isTurnQuota && !isTurnForbidden && !isTurnStaleNonce && !isTurnCapacity &&
						!turnAllocAttrMissing && !isDtlsTimeout && !transientSocketRetry && (
						strings.Contains(errStr, "TURN_AUTH_REJECTED") ||
							strings.Contains(errStrLower, "unauthorized") ||
							strings.Contains(errStrLower, "wrong credentials"))

					if hint := workerErrorHint(sessErr); hint != "" {
						errStr += " | " + hint
					} else if strings.Contains(errStrLower, "rate limit") ||
						strings.Contains(errStrLower, "flood control") ||
						strings.Contains(errStrLower, "ip mismatch") ||
						strings.Contains(errStrLower, "error 29") {
						errStr += " (ошибка со стороны ВК)"
					}

					if strings.Contains(errStr, "хеш мёртв") ||
						strings.Contains(errStr, "FATAL_AUTH") {
						log.Printf("[ГР #%d | ВОРКЕР #%d] Фатальная ошибка: %s", groupID, wid, errStr)
						return
					}

					// A connection that lived for a meaningful period proves the
					// network recovered; do not carry old failure streaks forever.
					if sessionLifetime >= 20*time.Second {
						attempt = 0
						transientFailures = 0
					}
					attempt++

					if transientSocketRetry {
						transientFailures++
					} else {
						transientFailures = 0
					}

					turnRelay := ""
					if len(credsSnapshot.TurnURLs) > 0 {
						turnRelay = credsSnapshot.TurnURLs[wid%len(credsSnapshot.TurnURLs)]
					}

					switch {
					case transientSocketRetry:
						// On iOS Wi-Fi/cellular handoff may invalidate only the local
						// socket. TURN credentials remain valid, so keep them and retry
						// without hitting VK authentication again.
						log.Printf("[ГР #%d | ВОРКЕР #%d] [СЕТЬ] Локальный socket потерян, повтор с теми же кредами (попытка %d, relay=%s): %s", groupID, wid, attempt, turnRelay, errStr)
					case isTurnQuota:
						log.Printf("[ГР #%d | ВОРКЕР #%d] [TURN] Квота relay исчерпана, ждём (попытка %d, relay=%s): %s", groupID, wid, attempt, turnRelay, errStr)
					case isTurnForbidden:
						log.Printf("[ГР #%d | ВОРКЕР #%d] [TURN] Отказ сервера по политике (403 Forbidden, relay=%s), повтор (попытка %d): %s", groupID, wid, turnRelay, attempt, errStr)
					case isTurnStaleNonce:
						log.Printf("[ГР #%d | ВОРКЕР #%d] [TURN] Устаревший nonce (438 Stale Nonce, relay=%s), повтор (попытка %d): %s", groupID, wid, turnRelay, attempt, errStr)
					case isTurnCapacity:
						log.Printf("[ГР #%d | ВОРКЕР #%d] [TURN] Недостаток ёмкости сервера (508 Insufficient Capacity, relay=%s), повтор (попытка %d): %s", groupID, wid, turnRelay, attempt, errStr)
					case turnAllocAttrMissing:
						log.Printf("[ГР #%d | ВОРКЕР #%d] [TURN] Неполный STUN/TURN ответ (relay=%s), повтор (попытка %d): %s", groupID, wid, turnRelay, attempt, errStr)
					case isDtlsTimeout:
						log.Printf("[ГР #%d | ВОРКЕР #%d] [DTLS] Таймаут рукопожатия DTLS (попытка %d, relay=%s): %s", groupID, wid, attempt, turnRelay, errStr)
					case turnAuthRejected:
						log.Printf("[ГР #%d | ВОРКЕР #%d | ПОПЫТКА #%d] [TURN] Отказ авторизации TURN (relay=%s, gen=%d), обновление реквизитов: %s", groupID, wid, attempt, turnRelay, credsSnapshot.Generation, errStr)
						coord.RefreshCreds(ctx, ctx, "TURN auth rejection", credsSnapshot.Generation)
					default:
						log.Printf("[ГР #%d | ВОРКЕР #%d] Ошибка (попытка %d, relay=%s): %s", groupID, wid, attempt, turnRelay, errStr)
					}

					// Authentication/flood-control failures may require user/hash
					// intervention. A local socket failure is deliberately NOT in
					// this category because mobile interface changes are recoverable.
					isUnrecoverableRemoteFailure := strings.Contains(errStrLower, "error 29")
					if isUnrecoverableRemoteFailure {
						log.Printf("[ВОРКЕР #%d] Невосстановимая удалённая ошибка, завершение: %s", wid, errStr)
						return
					}
				} else {
					// A cleanly completed session also breaks a failure streak.
					attempt = 0
					transientFailures = 0
				}

				if ctx.Err() != nil {
					return
				}

				retryDelay := time.Duration(5+rand.Intn(11)) * time.Second
				if transientSocketRetry {
					if transientFailures >= 3 {
						// The device is probably offline rather than merely switching
						// interfaces. Back off to protect battery and VK/TURN quota.
						retryDelay = time.Duration(30+rand.Intn(31)) * time.Second
						log.Printf("[ВОРКЕР #%d] [СЕТЬ] Сеть долго недоступна; dormancy %v", wid, retryDelay)
					} else {
						retryDelay = time.Duration(1+rand.Intn(3)) * time.Second
					}
				} else if quotaRetry {
					retryDelay = time.Duration(6+rand.Intn(6)) * time.Second
				}
				select {
				case <-time.After(retryDelay):
				case <-ctx.Done():
					return
				}
			}
		}(wid, workerDelay)
	}

	wg.Wait()
	log.Printf("[ГРУППА #%d] Все воркеры группы завершились.", groupID)
}

// ParseHashes — парсит строку хешей
func ParseHashes(raw string) []string {
	var result []string
	seen := make(map[string]struct{})
	for _, h := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		h = normalizeVKJoinHash(h)
		if h != "" {
			if _, exists := seen[h]; exists {
				continue
			}
			seen[h] = struct{}{}
			result = append(result, h)
		}
	}
	return result
}

func normalizeVKJoinHash(input string) string {
	s := strings.Trim(strings.TrimSpace(input), "<>\"'")
	if s == "" {
		return ""
	}

	lower := strings.ToLower(s)
	if idx := strings.Index(lower, "/call/join/"); idx >= 0 {
		s = s[idx+len("/call/join/"):]
	} else if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return ""
	}

	if idx := strings.IndexAny(s, "?#/"); idx != -1 {
		s = s[:idx]
	}
	return strings.Trim(strings.TrimSpace(s), "/")
}

// TurnParams — конфигурация TURN
type TurnParams struct {
	Host     string
	Port     string
	Hashes   []string
	WrapKey  []byte // Password-derived WRAP key (32 bytes), nil = disabled
	ObfsMode string // "audio" or "video" — RTP masking mode
}

// Credentials — учетные данные TURN
type Credentials struct {
	User          string
	Pass          string
	TurnURLs      []string
	CacheStreamID int
	Generation    uint64
	GroupID       int
	AttemptID     int
}

type credRefreshOp struct {
	done chan struct{}
}

// GroupCredCoordinator управляет реквизитами группы и потокобезопасным однократным обновлением (Single-Flight)
type GroupCredCoordinator struct {
	groupID         int
	hash            string
	credStreamID    int
	mu              sync.Mutex
	credsMu         sync.RWMutex
	creds           *Credentials
	activeOp        *credRefreshOp
	lastRefreshTime int64
}

func NewGroupCredCoordinator(groupID int, hash string, credStreamID int, initialCreds *Credentials) *GroupCredCoordinator {
	return &GroupCredCoordinator{
		groupID:      groupID,
		hash:         hash,
		credStreamID: credStreamID,
		creds:        initialCreds,
	}
}

func (c *GroupCredCoordinator) Snapshot(attemptID int) Credentials {
	c.credsMu.RLock()
	defer c.credsMu.RUnlock()
	snap := *c.creds
	snap.TurnURLs = cloneStringSlice(c.creds.TurnURLs)
	snap.AttemptID = attemptID
	return snap
}

func (c *GroupCredCoordinator) CurrentGeneration() uint64 {
	c.credsMu.RLock()
	defer c.credsMu.RUnlock()
	return c.creds.Generation
}

func (c *GroupCredCoordinator) RefreshCreds(workerCtx context.Context, groupCtx context.Context, reason string, failedGen uint64) bool {
	// 1. Проверяем локальные реквизиты группы: если они уже новее, обновление не требуется
	c.credsMu.RLock()
	currentGen := c.creds.Generation
	c.credsMu.RUnlock()

	if failedGen > 0 && currentGen > failedGen {
		log.Printf("[ГРУППА #%d] [TURN] Локальные креды уже обновлены (актуальное поколение %d > failed=%d, %s)", c.groupID, currentGen, failedGen, reason)
		return true
	}

	// 2. Синхронизация через c.mu
	c.mu.Lock()

	// Повторная проверка поколения под c.mu
	c.credsMu.RLock()
	currentGen = c.creds.Generation
	c.credsMu.RUnlock()
	if failedGen > 0 && currentGen > failedGen {
		c.mu.Unlock()
		return true
	}

	// Если уже идёт операция обновления другими воркерами — ждём её завершения через канал
	if c.activeOp != nil {
		doneCh := c.activeOp.done
		c.mu.Unlock()

		select {
		case <-doneCh:
			c.credsMu.RLock()
			genAfter := c.creds.Generation
			c.credsMu.RUnlock()
			return failedGen == 0 || genAfter > failedGen
		case <-workerCtx.Done():
			// Контекст текущего воркера отменён. Он выходит, НЕ прерывая общее обновление для группы
			return false
		}
	}

	// 3. Проверка интервала повторов (15 сек)
	now := time.Now().Unix()
	if c.lastRefreshTime > 0 && now-c.lastRefreshTime < 15 {
		c.mu.Unlock()
		log.Printf("[ГРУППА #%d] [TURN] Обновление реквизитов отложено: предыдущий запрос был %d сек назад (%s)", c.groupID, now-c.lastRefreshTime, reason)
		return false
	}

	// 4. Проверяем общий кеш: если другая группа уже получила более свежее поколение,
	// забираем его сразу без повторного обращения к VK API.
	normHash := normalizeVKJoinHash(c.hash)
	if normHash == "" {
		normHash = c.hash
	}
	linkCache := getLinkCache(normHash)
	linkCache.mutex.RLock()
	cacheGen := linkCache.creds.Generation
	cacheValid := linkCache.creds.Link == normHash && time.Now().Before(linkCache.creds.ExpiresAt) && len(linkCache.creds.ServerAddrs) > 0
	linkCache.mutex.RUnlock()

	if cacheValid && failedGen > 0 && cacheGen > failedGen {
		u, p, urls, newGen, err := GetCredsWithGen(workerCtx, c.hash, c.credStreamID)
		if err == nil && newGen > failedGen {
			c.credsMu.Lock()
			c.creds = &Credentials{User: u, Pass: p, TurnURLs: urls, CacheStreamID: c.credStreamID, Generation: newGen, GroupID: c.groupID}
			c.credsMu.Unlock()
			c.mu.Unlock()
			log.Printf("[ГРУППА #%d] [TURN] Получены новые реквизиты из общего кеша (gen=%d > failed=%d)", c.groupID, newGen, failedGen)
			return true
		}
	}

	// 5. Регистрируем новую операцию
	c.lastRefreshTime = now
	op := &credRefreshOp{done: make(chan struct{})}
	c.activeOp = op
	c.mu.Unlock()

	// Гарантированное завершение операции и закрытие канала ровно один раз
	defer func() {
		c.mu.Lock()
		c.activeOp = nil
		close(op.done)
		c.mu.Unlock()
	}()

	if getVkAuthMode() == "account" {
		invalidateInjectedTurnCreds(c.hash)
	}
	InvalidateLinkCacheIfGen(c.hash, failedGen)

	// Запрос выполняется с контекстом группы, чтобы отмена отдельного воркера не срывала общую операцию
	u, p, urls, newGen, refreshErr := GetCredsWithGen(groupCtx, c.hash, c.credStreamID)
	if refreshErr != nil {
		log.Printf("[ГРУППА #%d] [TURN] Не удалось обновить реквизиты после %s: %v", c.groupID, reason, refreshErr)
		return false
	}

	c.credsMu.Lock()
	c.creds = &Credentials{User: u, Pass: p, TurnURLs: urls, CacheStreamID: c.credStreamID, Generation: newGen, GroupID: c.groupID}
	c.credsMu.Unlock()
	log.Printf("[ГРУППА #%d] [TURN] Реквизиты успешно обновлены после %s (gen=%d, urls=%d)", c.groupID, reason, newGen, len(urls))
	return true
}
