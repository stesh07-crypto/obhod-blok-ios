package main

import (
	"sync"
	"time"
)

// ConfigOperation представляет отдельную попытку запроса WireGuard-конфига.
// Завершение допускается только владельцем текущей операции и строго один раз.
type ConfigOperation struct {
	id       uint64
	coord    *ConfigCoordinator
	doneOnce sync.Once
}

// ID возвращает идентификатор операции.
func (op *ConfigOperation) ID() uint64 {
	if op == nil {
		return 0
	}
	return op.id
}

// DeliverConfig надёжно отправляет полученный конфиг в канал.
// Только при успешной физической записи в канал операция считается выполненной,
// выставляется sent = true и снимается блокировка активной операции.
func (op *ConfigOperation) DeliverConfig(conf string, configCh chan<- string, timeout time.Duration) bool {
	if op == nil || op.coord == nil {
		return false
	}
	delivered := false
	op.doneOnce.Do(func() {
		if conf == "" || configCh == nil {
			op.coord.finishOp(op, false, 3*time.Second)
			return
		}

		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case configCh <- conf:
			delivered = true
			op.coord.finishOp(op, true, 0)
		case <-timer.C:
			// Канал заблокирован или переполнен — НЕ считаем доставкой
			op.coord.finishOp(op, false, 3*time.Second)
		}
	})
	return delivered
}

// ReleaseWithCooldown освобождает операцию и выставляет паузу повторов (cooldown).
// Безопасен для повторного вызова: завершение происходит ровно один раз.
func (op *ConfigOperation) ReleaseWithCooldown(cooldown time.Duration) {
	if op == nil || op.coord == nil {
		return
	}
	op.doneOnce.Do(func() {
		op.coord.finishOp(op, false, cooldown)
	})
}

// ConfigCoordinator координирует получение WireGuard-конфига между всеми группами и воркерами туннеля.
// Состояние sent, текущая операция и cooldown согласованы под одним mutex.
type ConfigCoordinator struct {
	mu          sync.Mutex
	sent        bool
	currentOpID uint64
	activeOp    *ConfigOperation
	retryUntil  time.Time
}

// NewConfigCoordinator создаёт один координатор на запуск туннеля.
func NewConfigCoordinator() *ConfigCoordinator {
	return &ConfigCoordinator{}
}

// TryAcquire пытается начать операцию получения конфига.
// Возвращает объект операции владельцу или nil, если конфиг уже отправлен,
// операция уже выполняется другим воркером, или действует cooldown.
func (c *ConfigCoordinator) TryAcquire() *ConfigOperation {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sent {
		return nil
	}
	if c.activeOp != nil {
		return nil
	}
	if time.Now().Before(c.retryUntil) {
		return nil
	}

	c.currentOpID++
	op := &ConfigOperation{
		id:    c.currentOpID,
		coord: c,
	}
	c.activeOp = op
	return op
}

// finishOp завершает операцию под мьютексом.
// Гарантирует, что только владелец текущей активной операции может повлиять на состояние координатора.
func (c *ConfigCoordinator) finishOp(op *ConfigOperation, success bool, cooldown time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Завершение допускается только владельцем ТЕКУЩЕЙ активной операции
	if c.activeOp != op {
		return
	}

	c.activeOp = nil
	if success {
		c.sent = true
		c.retryUntil = time.Time{}
	} else if cooldown > 0 {
		c.retryUntil = time.Now().Add(cooldown)
	}
}

// IsConfigSent сообщает, был ли конфиг уже успешно доставлен.
func (c *ConfigCoordinator) IsConfigSent() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent
}

// HasActiveOp проверяет, выполняется ли операция прямо сейчас (для тестов/диагностики).
func (c *ConfigCoordinator) HasActiveOp() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeOp != nil
}

// CooldownRemaining возвращает оставшееся время кулдауна.
func (c *ConfigCoordinator) CooldownRemaining() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.retryUntil) {
		return time.Until(c.retryUntil)
	}
	return 0
}
