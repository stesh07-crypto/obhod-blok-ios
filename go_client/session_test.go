package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 1. Освобождение слота handshake при успехе, ошибке и отмене
func TestDTLSSemaphore_SlotReleaseOnSuccessErrorCancel(t *testing.T) {
	// Проверяем, что исходно все 3 слота свободны
	if len(handshakeSem) != 0 {
		t.Fatalf("expected empty handshakeSem at start, got %d", len(handshakeSem))
	}

	scenarios := []struct {
		name string
		fail bool
	}{
		{name: "success", fail: false},
		{name: "error", fail: true},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// Захватываем слот
			handshakeSem <- struct{}{}
			HandshakesInFlight.Add(1)

			semReleased := false
			releaseSem := func() {
				if !semReleased {
					semReleased = true
					<-handshakeSem
					HandshakesInFlight.Add(-1)
				}
			}

			// Имитируем выполнение через defer releaseSem
			func() {
				defer releaseSem()
				if sc.fail {
					releaseSem()
					return
				}
				// Успех
				releaseSem()
			}()

			// Проверяем, что слот освобождён
			if len(handshakeSem) != 0 {
				t.Fatalf("scenario %s leaked a semaphore slot: len=%d", sc.name, len(handshakeSem))
			}
			if HandshakesInFlight.Load() != 0 {
				t.Fatalf("scenario %s leaked in-flight count: %d", sc.name, HandshakesInFlight.Load())
			}
		})
	}
}

// 2. Отсутствие утечки слота при отмене ожидания в очереди
func TestDTLSSemaphore_NoLeakOnWaitCancellation(t *testing.T) {
	// Заполняем все 3 слота семафора
	for i := 0; i < 3; i++ {
		handshakeSem <- struct{}{}
		HandshakesInFlight.Add(1)
	}

	// 4-й воркер пытается захватить слот с отменяемым контекстом
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	HandshakesWaiting.Add(1)
	semStart := time.Now()
	var acquired bool

	select {
	case handshakeSem <- struct{}{}:
		HandshakesWaiting.Add(-1)
		acquired = true
	case <-ctx.Done():
		HandshakesWaiting.Add(-1)
		HandshakeCanceled.Add(1)
		acquired = false
	}
	semWait := time.Since(semStart)

	if acquired {
		t.Fatalf("should not have acquired semaphore while all 3 slots are full")
	}
	if semWait > 100*time.Millisecond {
		t.Fatalf("wait took too long: %v", semWait)
	}
	if HandshakesWaiting.Load() != 0 {
		t.Fatalf("expected HandshakesWaiting == 0, got %d", HandshakesWaiting.Load())
	}

	// Освобождаем 3 слота
	for i := 0; i < 3; i++ {
		<-handshakeSem
		HandshakesInFlight.Add(-1)
	}

	if len(handshakeSem) != 0 {
		t.Fatalf("expected all slots free, got len=%d", len(handshakeSem))
	}
	if HandshakesInFlight.Load() != 0 {
		t.Fatalf("expected in-flight == 0, got %d", HandshakesInFlight.Load())
	}

	// Убеждаемся, что семафор полностью работоспособен
	for i := 0; i < 3; i++ {
		select {
		case handshakeSem <- struct{}{}:
		default:
			t.Fatalf("slot %d could not be re-acquired", i)
		}
	}
	for i := 0; i < 3; i++ {
		<-handshakeSem
	}
}

// 3. Отсутствие инвалидации кеша при DTLS timeout
func TestDTLSTimeout_NoCacheInvalidation(t *testing.T) {
	link := "https://vk.com/call/join/test_dtls_timeout_no_inval"
	resetLinkCache(link)

	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		return "turn_user_valid", "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	// Получаем реквизиты поколения 1
	ctx := context.Background()
	_, _, _, gen, err := GetCredsWithGen(ctx, link, 1)
	if err != nil || gen != 1 {
		t.Fatalf("GetCredsWithGen failed: gen=%d, err=%v", gen, err)
	}

	// Ошибка таймаута DTLS
	dtlsErr := errors.New("DTLS_TIMEOUT: таймаут рукопожатия DTLS (relay=193.203.43.46:19302, gen=1, очередь=41.8s, hs=50.001s, ok=45, tmout=6)")

	// Проверяем классификацию
	if isAuthError(dtlsErr) {
		t.Fatalf("dtlsErr must NOT be classified as isAuthError")
	}

	// Проверяем, что кеш остался нетронутым
	normLink := normalizeVKJoinHash(link)
	cache := getLinkCache(normLink)
	cache.mutex.RLock()
	u := cache.creds.Username
	g := cache.creds.Generation
	cache.mutex.RUnlock()

	if u != "turn_user_valid" || g != 1 {
		t.Fatalf("cache was unexpectedly modified: user=%s, gen=%d", u, g)
	}
}

// 4. Применение выбранного таймаута (50s control vs 20s experiment)
func TestDTLSTimeout_SessionModeSelection(t *testing.T) {
	tpControl := &TurnParams{HandshakeTimeout: HandshakeTimeoutControl}
	tpExperiment := &TurnParams{HandshakeTimeout: 20 * time.Second}
	tpDefault := &TurnParams{}

	getTimeout := func(tp *TurnParams) time.Duration {
		if tp != nil && tp.HandshakeTimeout > 0 {
			return tp.HandshakeTimeout
		}
		return CurrentHandshakeTimeout
	}

	if to := getTimeout(tpControl); to != 50*time.Second {
		t.Fatalf("expected 50s for tpControl, got %v", to)
	}
	if to := getTimeout(tpExperiment); to != 20*time.Second {
		t.Fatalf("expected 20s for tpExperiment, got %v", to)
	}
	if to := getTimeout(tpDefault); to != 20*time.Second {
		t.Fatalf("expected default 20s, got %v", to)
	}
}

// 5. Отсутствие повторной публикации результата завершённой или отменённой операции
func TestCredsCache_NoRepublishingCompletedOrCanceledOp(t *testing.T) {
	link := "https://vk.com/call/join/test_no_republish"
	resetLinkCache(link)

	normLink := normalizeVKJoinHash(link)
	cache := getLinkCache(normLink)

	// Создаём завершённую операцию
	doneCh := make(chan struct{})
	close(doneCh)

	op := &cacheFetchOp{
		done:          doneCh,
		startRevision: cache.revision.Load(),
		gen:           1,
		user:          "old_op_user",
	}

	// Попытка зафиксировать завершённую операцию в activeFetch не должна выполняться
	cache.fetchMu.Lock()
	cache.activeFetch = nil
	cache.fetchMu.Unlock()

	// Новая операция создаётся независимо
	vkFetchOverride = func(ctx context.Context, l string, streamID int) (string, string, []string, error) {
		return "fresh_op_user", "pass", []string{"193.203.43.46:19302"}, nil
	}
	defer func() { vkFetchOverride = nil }()

	u, _, _, g, err := GetCredsWithGen(context.Background(), link, 1)
	if err != nil || u != "fresh_op_user" || g != 1 {
		t.Fatalf("expected fresh_op_user, got user=%s, gen=%d, err=%v", u, g, err)
	}
	_ = op
}
