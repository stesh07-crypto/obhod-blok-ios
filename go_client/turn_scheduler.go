package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	turnEndpointFailureLimit = 2
	turnEndpointPause        = 60 * time.Second
)

// turnEndpointCandidate — один уникальный TURN endpoint и URL, из которого он
// получен. URL нужен для переключения воркера после DTLS_TIMEOUT.
type turnEndpointCandidate struct {
	URL      string
	Endpoint string
}

type turnHandshakeOutcome int

const (
	turnOutcomeOther turnHandshakeOutcome = iota
	turnOutcomeSuccess
	turnOutcomeTimeout
	turnOutcomeCanceled
)

type turnEndpointState struct {
	endpoint    string
	active      int
	failures    int
	lastUsed    time.Time
	pausedUntil time.Time
	probing     bool
}

type turnScheduler struct {
	mu        sync.Mutex
	global    chan struct{}
	endpoints map[string]*turnEndpointState
	notify    chan struct{}
	now       func() time.Time
}

type turnPermit struct {
	scheduler       *turnScheduler
	candidate       turnEndpointCandidate
	hasAlternatives bool
	once            sync.Once
}

func newTurnScheduler(global chan struct{}, now func() time.Time) *turnScheduler {
	if now == nil {
		now = time.Now
	}
	return &turnScheduler{
		global:    global,
		endpoints: make(map[string]*turnEndpointState),
		notify:    make(chan struct{}),
		now:       now,
	}
}

// turnEndpointScheduler — общий планировщик handshake на весь процесс.
// Глобальный лимит остаётся 3, но один endpoint не может занять все слоты.
var turnEndpointScheduler = newTurnScheduler(handshakeSem, time.Now)

func turnEndpointKey(endpoint string) string {
	return "udp|" + strings.ToLower(endpoint)
}

func (s *turnScheduler) stateLocked(endpoint string) *turnEndpointState {
	key := turnEndpointKey(endpoint)
	st := s.endpoints[key]
	if st == nil {
		st = &turnEndpointState{endpoint: endpoint}
		s.endpoints[key] = st
	}
	return st
}

func (s *turnScheduler) signalLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// broadcast используется при изменении состояния снаружи (например, тестом часов).
func (s *turnScheduler) broadcast() {
	s.mu.Lock()
	s.signalLocked()
	s.mu.Unlock()
}

// Acquire сначала резервирует endpoint (не более одного незавершённого
// handshake на endpoint при наличии альтернатив), и только затем занимает
// глобальный слот. Ожидание endpoint не удерживает глобальный слот.
func (s *turnScheduler) Acquire(ctx context.Context, preferredURL string, candidates []turnEndpointCandidate) (*turnPermit, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("нет TURN endpoint")
	}

	preferredKey := ""
	for _, c := range candidates {
		if c.URL == preferredURL {
			preferredKey = turnEndpointKey(c.Endpoint)
			break
		}
	}

	for {
		now := s.now()

		s.mu.Lock()
		selected, waitUntil := s.pickLocked(preferredKey, candidates, now)
		if selected != nil {
			st := s.stateLocked(selected.Endpoint)
			st.active++
			st.lastUsed = now
			s.mu.Unlock()

			select {
			case s.global <- struct{}{}:
				return &turnPermit{
					scheduler:       s,
					candidate:       *selected,
					hasAlternatives: len(candidates) > 1,
				}, nil
			case <-ctx.Done():
				s.mu.Lock()
				st.active--
				s.signalLocked()
				s.mu.Unlock()
				return nil, ctx.Err()
			}
		}

		notify := s.notify
		s.mu.Unlock()

		if waitUntil.IsZero() {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-notify:
			}
			continue
		}

		wait := time.Until(waitUntil)
		if wait <= 0 {
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-notify:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (s *turnScheduler) pickLocked(preferredKey string, candidates []turnEndpointCandidate, now time.Time) (*turnEndpointCandidate, time.Time) {
	// Единственный endpoint: не ограничиваем его одним handshake. Глобальный
	// лимит всё равно не даст запустить больше трёх подготовок одновременно.
	if len(candidates) == 1 {
		c := candidates[0]
		return &c, time.Time{}
	}

	var eligible []*turnEndpointCandidate
	var earliestPause time.Time

	for i := range candidates {
		c := &candidates[i]
		st := s.stateLocked(c.Endpoint)

		// Два таймаута могли накопиться, когда у группы ещё не было
		// альтернатив (single-endpoint bypass). Как только альтернатива
		// появилась, ставим endpoint на паузу.
		if st.failures >= turnEndpointFailureLimit && st.pausedUntil.IsZero() {
			st.pausedUntil = now.Add(turnEndpointPause)
			st.probing = false
			log.Printf("[TURN] endpoint %s: %d DTLS_TIMEOUT, пауза %s (появились альтернативы)",
				c.Endpoint, st.failures, turnEndpointPause)
		}

		if st.failures >= turnEndpointFailureLimit &&
			!st.pausedUntil.IsZero() &&
			!now.Before(st.pausedUntil) &&
			!st.probing {
			// Пауза истекла — разрешаем ровно одну пробную попытку.
			st.probing = true
			log.Printf("[TURN] endpoint %s: пауза истекла, разрешена пробная попытка", c.Endpoint)
		}

		if now.Before(st.pausedUntil) {
			if earliestPause.IsZero() || st.pausedUntil.Before(earliestPause) {
				earliestPause = st.pausedUntil
			}
			continue
		}
		if st.active > 0 {
			continue
		}
		eligible = append(eligible, c)
	}

	if len(eligible) == 0 {
		return nil, earliestPause
	}

	if preferredKey != "" {
		for _, c := range eligible {
			if turnEndpointKey(c.Endpoint) == preferredKey {
				return c, time.Time{}
			}
		}
	}

	best := eligible[0]
	bestUsed := s.stateLocked(best.Endpoint).lastUsed
	for _, c := range eligible[1:] {
		used := s.stateLocked(c.Endpoint).lastUsed
		if used.Before(bestUsed) {
			best = c
			bestUsed = used
		}
	}
	return best, time.Time{}
}

func (s *turnScheduler) finish(endpoint string, hasAlternatives bool, outcome turnHandshakeOutcome) {
	// Парный receive к отправке в global в Acquire. Глобальный слот
	// освобождается ровно один раз благодаря sync.Once в turnPermit.
	<-s.global

	s.mu.Lock()
	st := s.stateLocked(endpoint)
	if st.active > 0 {
		st.active--
	}

	switch outcome {
	case turnOutcomeSuccess:
		wasDegraded := st.failures > 0 || !st.pausedUntil.IsZero() || st.probing
		st.failures = 0
		st.pausedUntil = time.Time{}
		st.probing = false
		if wasDegraded {
			log.Printf("[TURN] endpoint %s: успешный handshake, оценка сброшена", endpoint)
		}
	case turnOutcomeTimeout:
		st.failures++
		if hasAlternatives && st.failures >= turnEndpointFailureLimit {
			st.pausedUntil = s.now().Add(turnEndpointPause)
			st.probing = false
			log.Printf("[TURN] endpoint %s: %d DTLS_TIMEOUT, пауза %s (есть альтернативы)",
				endpoint, st.failures, turnEndpointPause)
		} else {
			st.probing = false
		}
	case turnOutcomeCanceled:
		// Отмена пользователем/остановка — не сетевая неудача. Пробную
		// попытку не расходуем.
		st.probing = false
	default:
		// Ошибка не DTLS_TIMEOUT не увеличивает счётчик таймаутов, но
		// неудачная пробная попытка после паузы снова выдерживает паузу.
		if st.probing && hasAlternatives {
			st.pausedUntil = s.now().Add(turnEndpointPause)
			log.Printf("[TURN] endpoint %s: неудачная пробная попытка, пауза %s", endpoint, turnEndpointPause)
		}
		st.probing = false
	}

	s.signalLocked()
	s.mu.Unlock()
}

func (p *turnPermit) URL() string      { return p.candidate.URL }
func (p *turnPermit) Endpoint() string { return p.candidate.Endpoint }

func (p *turnPermit) Finish(outcome turnHandshakeOutcome) {
	p.once.Do(func() {
		p.scheduler.finish(p.candidate.Endpoint, p.hasAlternatives, outcome)
	})
}

// Reset очищает накопленную оценку endpoint'ов. Вызывается при смене сети или новом запуске,
// чтобы старые таймауты не влияли на новый маршрут.
func (s *turnScheduler) Reset() {
	s.mu.Lock()
	// Активные разрешения переживают сброс оценки: их всё ещё нужно
	// освободить, а per-endpoint лимит нельзя обходить во время смены сети.
	// Сохраняем идентичность объектов состояния, чтобы отмена ожидающего
	// Acquire уменьшала счётчик active в актуальной карте.
	next := make(map[string]*turnEndpointState, len(s.endpoints))
	for key, st := range s.endpoints {
		if st.active > 0 {
			st.failures = 0
			st.pausedUntil = time.Time{}
			st.probing = false
			next[key] = st
		}
	}
	s.endpoints = next
	s.signalLocked()
	s.mu.Unlock()
	log.Printf("[TURN] Оценка endpoint'ов сброшена")
}

type turnEndpointSnapshot struct {
	Endpoint    string
	Active      int
	Failures    int
	PausedUntil time.Time
	Probing     bool
}

func (s *turnScheduler) Snapshot() []turnEndpointSnapshot {
	s.mu.Lock()
	out := make([]turnEndpointSnapshot, 0, len(s.endpoints))
	for _, st := range s.endpoints {
		out = append(out, turnEndpointSnapshot{
			Endpoint:    st.endpoint,
			Active:      st.active,
			Failures:    st.failures,
			PausedUntil: st.pausedUntil,
			Probing:     st.probing,
		})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

// buildTurnEndpointCandidates применяет те же overrides -turn/-port, что и
// RunSession, и убирает дубликаты, чтобы один IP:port не считался разными адресами.
func buildTurnEndpointCandidates(urls []string, tp *TurnParams) ([]turnEndpointCandidate, error) {
	seen := make(map[string]struct{})
	candidates := make([]turnEndpointCandidate, 0, len(urls))

	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		address := strings.Split(raw, "?")[0]
		address = strings.TrimPrefix(strings.TrimPrefix(address, "turn:"), "turns:")
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("разбор TURN URL %q: %w", raw, err)
		}
		if tp != nil {
			if tp.Host != "" {
				host = tp.Host
			}
			if tp.Port != "" {
				port = tp.Port
			}
		}
		endpoint := net.JoinHostPort(host, port)
		key := turnEndpointKey(endpoint)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, turnEndpointCandidate{URL: raw, Endpoint: endpoint})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("нет TURN URL в учетных данных")
	}
	return candidates, nil
}

// nextUniqueTurnURL возвращает следующий URL из списка группы, ведущий на
// другой endpoint. Если альтернативы нет, возвращает пустую строку.
func nextUniqueTurnURL(urls []string, tp *TurnParams, currentEndpoint string) string {
	candidates, err := buildTurnEndpointCandidates(urls, tp)
	if err != nil || len(candidates) < 2 {
		return ""
	}

	currentIdx := -1
	for i, c := range candidates {
		if strings.EqualFold(c.Endpoint, currentEndpoint) {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		for _, c := range candidates {
			if !strings.EqualFold(c.Endpoint, currentEndpoint) {
				return c.URL
			}
		}
		return ""
	}

	for step := 1; step < len(candidates); step++ {
		c := candidates[(currentIdx+step)%len(candidates)]
		if !strings.EqualFold(c.Endpoint, currentEndpoint) {
			return c.URL
		}
	}
	return ""
}

// DTLSHandshakeTimeoutError сохраняет endpoint, на котором завершился
// DTLS_TIMEOUT, чтобы воркер мог переключиться на следующий адрес группы.
type DTLSHandshakeTimeoutError struct {
	Endpoint string
	TurnURL  string
	Err      error
}

func (e *DTLSHandshakeTimeoutError) Error() string {
	return fmt.Sprintf("DTLS_TIMEOUT: %v", e.Err)
}

func (e *DTLSHandshakeTimeoutError) Unwrap() error   { return e.Err }
func (e *DTLSHandshakeTimeoutError) Timeout() bool   { return true }
func (e *DTLSHandshakeTimeoutError) Temporary() bool { return true }

// isTimeoutError определяет сетевой таймаут, включая context.DeadlineExceeded.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "deadline") || strings.Contains(errStr, "timeout")
}
