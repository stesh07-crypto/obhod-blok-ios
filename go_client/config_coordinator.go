package main

import (
	"sync/atomic"
	"time"
)

// ConfigCoordinator координирует единичный запрос WireGuard-конфига между группами и воркерами.
// Гарантирует:
// 1. Только один воркер в один момент времени выполняет GETCONF (in-flight защита).
// 2. После успешной доставки в канал выставляется configSent = 1, повторные запросы блокируются.
// 3. При NOCONF или ошибке операция немедленно освобождается с общей паузой повторов (cooldown),
//    не блокируя другие воркеры на время жизни долгоживущей сессии.
// 4. Неудачная отправка в канал (таймаут/переполнение) НЕ считается подтверждением доставки.
type ConfigCoordinator struct {
	sent           *int32
	inFlight       *int32
	retryUntilNano atomic.Int64
}

// NewConfigCoordinator создаёт координатор с привязкой к общим atomic-флагам.
func NewConfigCoordinator(sent, inFlight *int32) *ConfigCoordinator {
	return &ConfigCoordinator{
		sent:     sent,
		inFlight: inFlight,
	}
}

// TryAcquire пытается захватить право на выполнение GETCONF.
// Возвращает true только если конфиг ещё не получен, cooldown истёк и слот свободен.
func (c *ConfigCoordinator) TryAcquire() bool {
	if c == nil || c.sent == nil || c.inFlight == nil {
		return false
	}
	if atomic.LoadInt32(c.sent) != 0 {
		return false
	}
	now := time.Now().UnixNano()
	if now < c.retryUntilNano.Load() {
		return false
	}
	return atomic.CompareAndSwapInt32(c.inFlight, 0, 1)
}

// ReleaseWithCooldown освобождает операцию и устанавливает общую паузу повторов.
func (c *ConfigCoordinator) ReleaseWithCooldown(cooldown time.Duration) {
	if c == nil || c.inFlight == nil {
		return
	}
	if cooldown > 0 {
		c.retryUntilNano.Store(time.Now().Add(cooldown).UnixNano())
	}
	atomic.StoreInt32(c.inFlight, 0)
}

// DeliverConfig надёжно отправляет полученный конфиг в канал.
// Только при успешной записи в канал выставляет configSent = 1.
// При переполнении или таймауте возвращает false и освобождает операцию с паузой повторов.
func (c *ConfigCoordinator) DeliverConfig(conf string, configCh chan<- string, timeout time.Duration) bool {
	if c == nil || c.sent == nil || c.inFlight == nil {
		return false
	}
	if conf == "" || configCh == nil {
		c.ReleaseWithCooldown(3 * time.Second)
		return false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case configCh <- conf:
		atomic.StoreInt32(c.sent, 1)
		atomic.StoreInt32(c.inFlight, 0)
		return true
	case <-timer.C:
		// Не удалось передать в канал — НЕ считаем доставленным!
		c.ReleaseWithCooldown(3 * time.Second)
		return false
	}
}

// IsConfigSent возвращает true, если конфиг уже был успешно доставлен.
func (c *ConfigCoordinator) IsConfigSent() bool {
	if c == nil || c.sent == nil {
		return false
	}
	return atomic.LoadInt32(c.sent) != 0
}
