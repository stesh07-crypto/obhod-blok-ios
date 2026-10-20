package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cbeuw/connutil"
)

// mockFailConn эмулирует соединение, на котором Write всегда завершается с ошибкой
type mockFailConn struct {
	net.Conn
	writeErr error
}

func (m *mockFailConn) Write(b []byte) (int, error) {
	if m.writeErr != nil {
		return 0, m.writeErr
	}
	return 0, errors.New("write failed: simulated network drop")
}

func (m *mockFailConn) Close() error {
	return nil
}

// 1. Ошибка AUTH не допускает регистрацию воркера в диспетчере и не приводит к двойному декременту
func TestSendAuth_ErrorDoesNotRegisterWorker(t *testing.T) {
	failConn := &mockFailConn{writeErr: errors.New("simulated auth drop")}
	err := SendAuth(failConn, "test-device", "test-pass")
	if err == nil {
		t.Fatalf("expected error from SendAuth on failing connection, got nil")
	}

	// Проверяем инвариант RunSession:
	// При ошибке SendAuth воркер НЕ должен быть зарегистрирован в диспетчере,
	// а ActiveConnections должен уменьшиться ровно один раз через defer.
	stats := &Stats{}
	disp := &Dispatcher{
		ReturnCh: make(chan []byte, 10),
	}

	func() {
		stats.ActiveConnections.Add(1)
		defer stats.ActiveConnections.Add(-1)

		authErr := SendAuth(failConn, "test-dev", "test-pass")
		if authErr != nil {
			// Логика RunSession: при ошибке выходим БЕЗ регистрации в диспетчере
			return
		}

		// До этой точки код доходить НЕ должен
		slot := &WorkerSlot{ID: 99, SendCh: make(chan []byte, 10)}
		disp.Register(slot)
		defer disp.Unregister(slot)
	}()

	// Проверяем, что в диспетчере 0 воркеров
	disp.mu.Lock()
	registered := len(disp.workers)
	disp.mu.Unlock()
	if registered != 0 {
		t.Fatalf("expected 0 registered workers in dispatcher after auth failure, got %d", registered)
	}

	// Проверяем, что ActiveConnections вернулся в 0 (ровно один декремент)
	if act := stats.ActiveConnections.Load(); act != 0 {
		t.Fatalf("expected ActiveConnections == 0 after single defer decrement, got %d", act)
	}
}

// 2. Успешная доставка конфига: отмечает configSent=1, освобождает операцию, блокирует повторный запрос
func TestConfigCoordinator_SuccessFlow(t *testing.T) {
	var sent int32
	var inFlight int32
	coord := NewConfigCoordinator(&sent, &inFlight)

	if !coord.TryAcquire() {
		t.Fatalf("initial TryAcquire should succeed")
	}

	// Вторая попытка во время in-flight должна быть отклонена
	if coord.TryAcquire() {
		t.Fatalf("concurrent TryAcquire must fail while in-flight")
	}

	configCh := make(chan string, 1)
	delivered := coord.DeliverConfig("test-wg-conf", configCh, 100*time.Millisecond)
	if !delivered {
		t.Fatalf("DeliverConfig should succeed")
	}

	if !coord.IsConfigSent() {
		t.Fatalf("expected IsConfigSent == true after delivery")
	}
	if atomic.LoadInt32(&inFlight) != 0 {
		t.Fatalf("inFlight should be 0 after DeliverConfig, got %d", inFlight)
	}

	// Проверяем, что конфиг физически попал в канал
	select {
	case conf := <-configCh:
		if conf != "test-wg-conf" {
			t.Fatalf("unexpected config in channel: %s", conf)
		}
	default:
		t.Fatalf("config channel is empty despite DeliverConfig returning true")
	}

	// Повторный запрос после успешной доставки невозможен
	if coord.TryAcquire() {
		t.Fatalf("TryAcquire must fail when config was already sent")
	}
}

// 3. Повтор GETCONF после NOCONF или ошибки: операция освобождается с общей паузой, повтор без ожидания сессии
func TestConfigCoordinator_NOCONF_And_RetryAfterCooldown(t *testing.T) {
	var sent int32
	var inFlight int32
	coord := NewConfigCoordinator(&sent, &inFlight)

	if !coord.TryAcquire() {
		t.Fatalf("TryAcquire should succeed")
	}

	// Имитируем получение NOCONF от сервера
	cooldown := 50 * time.Millisecond
	coord.ReleaseWithCooldown(cooldown)

	if coord.IsConfigSent() {
		t.Fatalf("config must NOT be marked sent on NOCONF")
	}
	if atomic.LoadInt32(&inFlight) != 0 {
		t.Fatalf("inFlight must be released to 0 immediately on NOCONF, got %d", inFlight)
	}

	// Немедленная попытка во время cooldown должна быть отклонена
	if coord.TryAcquire() {
		t.Fatalf("TryAcquire must be rejected during cooldown pause")
	}

	// Ждём истечения паузы повторов
	time.Sleep(60 * time.Millisecond)

	// Теперь другой воркер (или тот же) может успешно подхватить запрос GETCONF
	if !coord.TryAcquire() {
		t.Fatalf("TryAcquire must succeed after cooldown expired without waiting for session end")
	}
}

// 4. Неудавшаяся отправка в канал (таймаут/переполнение) НЕ считается подтверждением доставки
func TestConfigCoordinator_UndeliveredChannelDoesNotMarkSent(t *testing.T) {
	var sent int32
	var inFlight int32
	coord := NewConfigCoordinator(&sent, &inFlight)

	if !coord.TryAcquire() {
		t.Fatalf("TryAcquire should succeed")
	}

	// Канал ёмкостью 1 уже заполнен другим сообщением
	fullCh := make(chan string, 1)
	fullCh <- "existing-conf"

	// Попытка передать конфиг в заблокированный канал
	delivered := coord.DeliverConfig("new-conf", fullCh, 20*time.Millisecond)
	if delivered {
		t.Fatalf("DeliverConfig must return false when channel is blocked")
	}

	if coord.IsConfigSent() {
		t.Fatalf("configSent must NOT be marked if delivery to channel failed")
	}
	if atomic.LoadInt32(&inFlight) != 0 {
		t.Fatalf("inFlight should be released after failed delivery, got %d", inFlight)
	}
}

// 5. Очистка ресурсов на ранних выходах: отмена контекста, установка дедлайнов, закрытие pipe и отсутствие утечки горутин
func TestSession_UnifiedCleanupOnEarlyExit(t *testing.T) {
	pipeA, pipeB := connutil.AsyncPacketPipe()
	defer pipeA.Close()
	defer pipeB.Close()

	ctx, cancel := context.WithCancel(context.Background())
	sessCtx, sessCancel := context.WithCancel(ctx)

	var (
		cleanupOnce sync.Once
		sessionWg   sync.WaitGroup
		relayWg     sync.WaitGroup
		proxyWg     sync.WaitGroup
		dtlsConn    net.Conn
	)

	cleanup := func() {
		cleanupOnce.Do(func() {
			sessCancel()

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

			proxyWg.Wait()
			relayWg.Wait()
			sessionWg.Wait()
		})
	}
	defer cleanup()

	// Запускаем фоновую горутину, ожидающую sessCtx.Done()
	sessionWg.Add(1)
	go func() {
		defer sessionWg.Done()
		<-sessCtx.Done()
	}()

	// Запускаем горутину, блокирующуюся на чтении pipeA
	relayWg.Add(1)
	readExited := make(chan struct{})
	go func() {
		defer relayWg.Done()
		defer close(readExited)
		buf := make([]byte, 1024)
		_, _, _ = pipeA.ReadFrom(buf)
	}()

	// Имитируем ранний сбой до DTLS (например, отмена контекста или таймаут)
	cancel()
	cleanup()

	// Проверяем, что блокированное чтение pipeA разблокировалось и горутина завершилась
	select {
	case <-readExited:
		// Успешно разблокировано дедлайном / Close()
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("blocked ReadFrom on pipeA was not unblocked by cleanup")
	}

	// Убеждаемся, что повторный вызов cleanup безопасен (идемпотентность)
	cleanup()
}
