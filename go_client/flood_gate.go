package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FloodControlError указывает на ошибку flood control (код 9) от VK API.
type FloodControlError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *FloodControlError) Error() string {
	if e == nil {
		return "flood control"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("flood control active (retry after %v): %v", e.RetryAfter, e.Err)
	}
	return fmt.Sprintf("flood control: %v", e.Err)
}

func (e *FloodControlError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsFloodControl проверяет, относится ли ошибка к VK Flood Control (код 9).
func IsFloodControl(err error) bool {
	if err == nil {
		return false
	}
	var fce *FloodControlError
	if errors.As(err, &fce) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "flood control") ||
		strings.Contains(s, "error_code=9") ||
		strings.Contains(s, "error_code: 9") ||
		strings.Contains(s, "error 9") ||
		strings.Contains(s, "vk_flood_control")
}

func parseExplicitRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}
	s := strings.ToLower(err.Error())
	for _, reStr := range []string{
		`try again in (\d+)`,
		`retry after (\d+)`,
		`wait (\d+)`,
	} {
		re := regexp.MustCompile(reStr)
		if m := re.FindStringSubmatch(s); len(m) > 1 {
			if secs, err := strconv.Atoi(m[1]); err == nil && secs > 0 {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return 0
}

// FloodGate координирует запросы к VK API на уровне всего процесса,
// предотвращая шквал запросов при получении flood control (code 9).
type FloodGate struct {
	mu           sync.Mutex
	backoffStep  int
	floodedUntil time.Time
	probeActive  bool
	notifyCh     chan struct{}
}

var backoffSchedule = []time.Duration{
	60 * time.Second,
	120 * time.Second,
	240 * time.Second,
	300 * time.Second,
}

var GlobalFloodGate = NewFloodGate()

func NewFloodGate() *FloodGate {
	return &FloodGate{
		notifyCh: make(chan struct{}),
	}
}

// Acquire проверяет, разрешено ли делать запрос.
// Если действует блокировка flood control, поток засыпает до истечения таймера.
// По истечении таймера только ОДИН поток получает разрешение на пробный запрос (isProbe = true),
// остальные ждут результата пробы.
func (g *FloodGate) Acquire(ctx context.Context, streamID int) (isProbe bool, err error) {
	for {
		g.mu.Lock()
		now := time.Now()

		if now.Before(g.floodedUntil) {
			waitDur := g.floodedUntil.Sub(now)
			ch := g.notifyCh
			g.mu.Unlock()

			log.Printf("[STREAM %d] [FLOOD_GATE] Активен режим охлаждения flood control, ожидание %v...", streamID, waitDur.Truncate(time.Second))

			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(waitDur):
				// Время вышло, переходим на следующую итерацию
				continue
			case <-ch:
				// Статус изменился раньше (например, сброс)
				continue
			}
		}

		// Время flood control истекло или его не было.
		if g.backoffStep > 0 {
			// Мы находимся в фазе после flood control: разрешаем только одну пробу.
			if !g.probeActive {
				g.probeActive = true
				g.mu.Unlock()
				log.Printf("[STREAM %d] [FLOOD_GATE] Таймер истёк. Выделена одиночная проба VK API...", streamID)
				return true, nil
			}

			// Проба уже кем-то выполняется, ждём её завершения
			ch := g.notifyCh
			g.mu.Unlock()

			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(5 * time.Second):
				continue
			case <-ch:
				continue
			}
		}

		// Нормальный режим
		g.mu.Unlock()
		return false, nil
	}
}

// ReleaseProbe освобождает слот пробы, если запрос завершился без flood control
// (например, при обычной ошибке сети или отмене контекста), чтобы другие могли попробовать.
func (g *FloodGate) ReleaseProbe(streamID int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probeActive {
		g.probeActive = false
		log.Printf("[STREAM %d] [FLOOD_GATE] Одиночная проба завершена, слот пробы освобождён", streamID)
		close(g.notifyCh)
		g.notifyCh = make(chan struct{})
	}
}

// ReportFlood регистрирует получение ошибки flood control и запускает/эскалирует backoff.
func (g *FloodGate) ReportFlood(streamID int, cause error) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()

	idx := g.backoffStep
	if idx >= len(backoffSchedule) {
		idx = len(backoffSchedule) - 1
	}
	baseDur := backoffSchedule[idx]

	var fce *FloodControlError
	var explicitWait time.Duration
	if errors.As(cause, &fce) && fce.RetryAfter > 0 {
		explicitWait = fce.RetryAfter
	} else {
		explicitWait = parseExplicitRetryAfter(cause)
	}

	var totalDur time.Duration
	if explicitWait > 0 {
		totalDur = explicitWait
	} else {
		jitter := time.Duration(rand.Intn(10)) * time.Second
		totalDur = baseDur + jitter
	}

	g.floodedUntil = time.Now().Add(totalDur)
	if g.backoffStep < len(backoffSchedule)-1 {
		g.backoffStep++
	}
	g.probeActive = false

	// Оповещаем ожидающих
	close(g.notifyCh)
	g.notifyCh = make(chan struct{})

	log.Printf("[STREAM %d] [FLOOD_GATE] ⚠️ VK API вернул Flood Control! Пауза %v (до %s, шаг %d). Причина: %v",
		streamID, totalDur.Truncate(time.Second), g.floodedUntil.Format("15:04:05"), g.backoffStep, cause)

	return totalDur
}

// ReportSuccess регистрирует успешный запрос к VK API, сбрасывая состояние flood control.
func (g *FloodGate) ReportSuccess(streamID int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	wasFlooded := g.backoffStep > 0 || !g.floodedUntil.IsZero()
	g.backoffStep = 0
	g.floodedUntil = time.Time{}
	g.probeActive = false

	if wasFlooded {
		log.Printf("[STREAM %d] [FLOOD_GATE] ✓ Пробный запрос успешен! Flood gate полностью открыт.", streamID)
		close(g.notifyCh)
		g.notifyCh = make(chan struct{})
	}
}

// IsFlooded возвращает true, если в данный момент активен кулдаун flood control.
func (g *FloodGate) IsFlooded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.floodedUntil)
}

// RemainingPause возвращает оставшееся время кулдауна flood control.
func (g *FloodGate) RemainingPause() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.floodedUntil) {
		return time.Until(g.floodedUntil)
	}
	return 0
}
