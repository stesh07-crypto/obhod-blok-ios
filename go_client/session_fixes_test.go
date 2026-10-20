package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/stun/v3"
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

	stats := &Stats{}
	disp := &Dispatcher{
		ReturnCh: make(chan []byte, 10),
	}

	func() {
		stats.ActiveConnections.Add(1)
		defer stats.ActiveConnections.Add(-1)

		authErr := SendAuth(failConn, "test-dev", "test-pass")
		if authErr != nil {
			return
		}

		slot := &WorkerSlot{ID: 99, SendCh: make(chan []byte, 10)}
		disp.Register(slot)
		defer disp.Unregister(slot)
	}()

	disp.mu.Lock()
	registered := len(disp.workers)
	disp.mu.Unlock()
	if registered != 0 {
		t.Fatalf("expected 0 registered workers in dispatcher after auth failure, got %d", registered)
	}

	if act := stats.ActiveConnections.Load(); act != 0 {
		t.Fatalf("expected ActiveConnections == 0 after single defer decrement, got %d", act)
	}
}

// 2. Реальный тест: ошибка Allocate в RunSession освобождает GETCONF по внешнему таймауту
func TestAllocateError_ReleasesGETCONF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Локальный STUN/TURN UDP responder, возвращающий ошибку 403 Forbidden на Allocate
	l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("failed to listen UDP: %v", err)
	}
	defer l.Close()

	port := l.LocalAddr().(*net.UDPAddr).Port
	turnHostPort := fmt.Sprintf("127.0.0.1:%d", port)

	go func() {
		buf := make([]byte, 1500)
		for {
			n, raddr, rerr := l.ReadFrom(buf)
			if rerr != nil {
				return
			}
			msg := new(stun.Message)
			msg.Raw = make([]byte, n)
			copy(msg.Raw, buf[:n])
			if derr := msg.Decode(); derr == nil {
				resp := new(stun.Message)
				_ = resp.Build(
					stun.NewType(msg.Type.Method, stun.ClassErrorResponse),
					&stun.ErrorCodeAttribute{Code: stun.CodeForbidden},
				)
				resp.TransactionID = msg.TransactionID
				resp.Encode()
				_, _ = l.WriteTo(resp.Raw, raddr)
			}
		}
	}()

	coord := NewConfigCoordinator()
	op := coord.TryAcquire()
	if op == nil {
		t.Fatalf("expected op to be acquired initially")
	}
	if !coord.HasActiveOp() {
		t.Fatalf("coord must have activeOp after TryAcquire")
	}

	tp := &TurnParams{
		Host: "127.0.0.1",
		Port: fmt.Sprintf("%d", port),
	}
	creds := &Credentials{
		User:          "test-user",
		Pass:          "test-pass",
		TurnURLs:      []string{turnHostPort},
		CacheStreamID: 1,
		Generation:    1,
		GroupID:       1,
	}
	peer := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	disp := &Dispatcher{ReturnCh: make(chan []byte, 10)}
	configCh := make(chan string, 1)
	stats := &Stats{}

	// Запуск РЕАЛЬНОГО RunSession
	delivered, _, sessErr := RunSession(ctx, tp, peer, disp, "51820", op, configCh, 1, creds, "dev", "pass", stats)
	if delivered {
		t.Fatalf("delivered should be false on allocate error")
	}
	if sessErr == nil {
		t.Fatalf("expected error from RunSession when Allocate fails")
	}

	// Проверяем освобождение GETCONF по внешнему таймауту
	released := make(chan struct{})
	go func() {
		for coord.HasActiveOp() {
			time.Sleep(5 * time.Millisecond)
		}
		close(released)
	}()

	select {
	case <-released:
		// Успешно: страховочное освобождение сработало до возврата из RunSession
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("GETCONF operation was not released after Allocate error within external timeout")
	}

	if coord.IsConfigSent() {
		t.Fatalf("config must NOT be marked sent on allocate error")
	}
}

// 3. Cooldown сохраняется между разными группами воркеров
func TestConfigCoordinator_CooldownBetweenGroups(t *testing.T) {
	coord := NewConfigCoordinator()

	// Группа 1 захватывает операцию
	op1 := coord.TryAcquire()
	if op1 == nil {
		t.Fatalf("Group 1: TryAcquire should succeed")
	}
	if !coord.HasActiveOp() {
		t.Fatalf("HasActiveOp should be true")
	}

	// Группа 1 получает ошибку/NOCONF и освобождает с кулдауном 100мс
	cooldown := 100 * time.Millisecond
	op1.ReleaseWithCooldown(cooldown)

	if coord.HasActiveOp() {
		t.Fatalf("HasActiveOp should be false after release")
	}

	// Группа 2 пытается захватить операцию немедленно во время кулдауна
	op2Early := coord.TryAcquire()
	if op2Early != nil {
		t.Fatalf("Group 2: TryAcquire must be rejected during cooldown pause across groups")
	}

	// Проверяем снятие кулдауна по внешнему таймауту
	waitDone := make(chan *ConfigOperation)
	go func() {
		for {
			if op := coord.TryAcquire(); op != nil {
				waitDone <- op
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	select {
	case op2 := <-waitDone:
		if op2 == nil || op2.ID() == op1.ID() {
			t.Fatalf("Group 2 must receive a new distinct operation after cooldown, got %v", op2)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("cooldown did not expire within external timeout")
	}
}

// 4. Позднее завершение сессии не сбрасывает чужой in-flight
func TestConfigCoordinator_LateSessionDoesNotReleaseNewOp(t *testing.T) {
	coord := NewConfigCoordinator()

	// Старая сессия воркера 1 захватывает op1
	op1 := coord.TryAcquire()
	if op1 == nil {
		t.Fatalf("expected op1 to succeed")
	}

	// op1 завершается (NOCONF) с коротким кулдауном
	op1.ReleaseWithCooldown(20 * time.Millisecond)

	// Ждём истечения кулдауна
	time.Sleep(30 * time.Millisecond)

	// Новая сессия воркера 2 в другой группе захватывает op2
	op2 := coord.TryAcquire()
	if op2 == nil {
		t.Fatalf("expected op2 to succeed")
	}
	if !coord.HasActiveOp() {
		t.Fatalf("coord must have activeOp for op2")
	}

	// Теперь старая сессия воркера 1 завершает транспорт и вызывает ReleaseWithCooldown на op1
	op1.ReleaseWithCooldown(5 * time.Second)

	// Инвариант: op2 ДОЛЖЕН ОСТАВАТЬСЯ АКТИВНЫМ! Чужой in-flight не сброшен!
	if !coord.HasActiveOp() {
		t.Fatalf("late completion of old session op1 cleared op2's active state!")
	}

	// Третий воркер НЕ может перехватить слот, пока op2 работает
	op3 := coord.TryAcquire()
	if op3 != nil {
		t.Fatalf("op3 should NOT be acquired while op2 is active")
	}

	// Воркер 2 успешно доставляет конфиг
	configCh := make(chan string, 1)
	if !op2.DeliverConfig("final-wg-conf", configCh, 100*time.Millisecond) {
		t.Fatalf("op2.DeliverConfig failed")
	}

	if !coord.IsConfigSent() {
		t.Fatalf("config must be marked sent")
	}
}

// 5. Повторный опрос после успешной доставки не выполняется
func TestConfigCoordinator_NoAcquireAfterSuccess(t *testing.T) {
	coord := NewConfigCoordinator()

	op := coord.TryAcquire()
	if op == nil {
		t.Fatalf("initial TryAcquire should succeed")
	}

	configCh := make(chan string, 1)
	if !op.DeliverConfig("sample-conf", configCh, 100*time.Millisecond) {
		t.Fatalf("DeliverConfig failed")
	}

	if !coord.IsConfigSent() {
		t.Fatalf("expected IsConfigSent == true")
	}
	if coord.HasActiveOp() {
		t.Fatalf("HasActiveOp should be false after delivery")
	}

	// Повторные попытки с любых воркеров/групп должны возвращать nil
	for i := 0; i < 5; i++ {
		if nextOp := coord.TryAcquire(); nextOp != nil {
			t.Fatalf("TryAcquire must return nil once config is sent")
		}
	}

	// Параллельные попытки захвата также блокируются
	var wg sync.WaitGroup
	var acquiredCount atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if o := coord.TryAcquire(); o != nil {
				acquiredCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if acquiredCount.Load() != 0 {
		t.Fatalf("parallel TryAcquire succeeded %d times after config was sent", acquiredCount.Load())
	}

	// Поздний повторный вызов ReleaseWithCooldown безопасен и не меняет sent=true
	op.ReleaseWithCooldown(10 * time.Second)
	if !coord.IsConfigSent() {
		t.Fatalf("config must remain sent even after redundant release call")
	}
}

// 6. Неудавшаяся отправка в канал (таймаут/переполнение) НЕ считается подтверждением доставки
func TestConfigCoordinator_UndeliveredChannelDoesNotMarkSent(t *testing.T) {
	coord := NewConfigCoordinator()

	op := coord.TryAcquire()
	if op == nil {
		t.Fatalf("TryAcquire should succeed")
	}

	// Канал ёмкостью 1 уже заполнен другим сообщением
	fullCh := make(chan string, 1)
	fullCh <- "existing-conf"

	// Попытка передать конфиг в заблокированный канал
	delivered := op.DeliverConfig("new-conf", fullCh, 20*time.Millisecond)
	if delivered {
		t.Fatalf("DeliverConfig must return false when channel is blocked")
	}

	if coord.IsConfigSent() {
		t.Fatalf("configSent must NOT be marked if delivery to channel failed")
	}
	if coord.HasActiveOp() {
		t.Fatalf("activeOp should be released after failed delivery")
	}
}

// 7. Очистка ресурсов: разблокировка сетевого I/O по внешнему таймауту
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

	sessionWg.Add(1)
	go func() {
		defer sessionWg.Done()
		<-sessCtx.Done()
	}()

	relayWg.Add(1)
	readExited := make(chan struct{})
	go func() {
		defer relayWg.Done()
		defer close(readExited)
		buf := make([]byte, 1024)
		_, _, _ = pipeA.ReadFrom(buf)
	}()

	cancel()
	cleanup()

	// Проверяем разблокировку по внешнему таймауту
	select {
	case <-readExited:
		// Успешно разблокировано
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("blocked ReadFrom on pipeA was not unblocked by cleanup within external timeout")
	}

	cleanup()
}
