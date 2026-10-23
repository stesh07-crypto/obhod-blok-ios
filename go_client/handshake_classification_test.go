package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Внешний контекст жив, внутренний отменён, ошибка — deadline.
// Такая попытка обязана остаться таймаутом, иначе не включится переход
// на другой endpoint и статистика соврёт.
func TestClassifyHandshakeFailureTimeoutWhenSessionCanceled(t *testing.T) {
	parent := context.Background()
	sess, sessCancel := context.WithCancel(context.Background())
	sessCancel()

	hsErr := errors.New("handshake error: context deadline exceeded")
	outcome, err := classifyHandshakeFailure(parent, sess, context.DeadlineExceeded, hsErr, nil,
		"193.203.43.46:19302", "turn:193.203.43.46:19302")

	if outcome != turnOutcomeTimeout {
		t.Fatalf("outcome = %v, ожидался turnOutcomeTimeout", outcome)
	}
	var timeoutErr *DTLSHandshakeTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("ошибка %v не разбирается как *DTLSHandshakeTimeoutError", err)
	}
	if timeoutErr.Endpoint != "193.203.43.46:19302" || timeoutErr.TurnURL != "turn:193.203.43.46:19302" {
		t.Fatalf("endpoint/url потеряны: %+v", timeoutErr)
	}
	if !timeoutErr.Timeout() {
		t.Fatalf("DTLSHandshakeTimeoutError.Timeout() должен быть true")
	}
}

// Признак таймаута может прийти только из handshake-контекста, если сам
// handshake вернул context.Canceled. Проверяем и такой вход.
func TestClassifyHandshakeFailureUsesHandshakeContextCause(t *testing.T) {
	parent := context.Background()
	sess, sessCancel := context.WithCancel(context.Background())
	sessCancel()

	outcome, err := classifyHandshakeFailure(parent, sess, context.DeadlineExceeded, context.Canceled, nil, "1.1.1.1:3478", "")
	if outcome != turnOutcomeTimeout {
		t.Fatalf("outcome = %v, ожидался turnOutcomeTimeout", outcome)
	}
	var timeoutErr *DTLSHandshakeTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("ожидался *DTLSHandshakeTimeoutError, получено %v", err)
	}
}

// Внешняя отмена: возвращается причина внешнего контекста, штрафа endpoint нет.
func TestClassifyHandshakeFailureExternalCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()
	sess, sessCancel := context.WithCancel(context.Background())
	sessCancel()

	outcome, err := classifyHandshakeFailure(parent, sess, nil, context.Canceled, errors.New("relay failure"), "1.1.1.1:3478", "")
	if outcome != turnOutcomeCanceled {
		t.Fatalf("outcome = %v, ожидался turnOutcomeCanceled", outcome)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидался context.Canceled, получено %v", err)
	}
	var timeoutErr *DTLSHandshakeTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("внешняя отмена не должна превращаться в таймаут")
	}
}

// Внешний дедлайн сохраняет свой тип, а не подменяется на Canceled.
func TestClassifyHandshakeFailureExternalDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer parentCancel()
	time.Sleep(2 * time.Millisecond)

	outcome, err := classifyHandshakeFailure(parent, context.Background(), nil, errors.New("transport"), nil, "1.1.1.1:3478", "")
	if outcome != turnOutcomeCanceled {
		t.Fatalf("outcome = %v, ожидался turnOutcomeCanceled", outcome)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ожидался context.DeadlineExceeded, получено %v", err)
	}
}

// Внутренний тирдаун без таймаута: не пользовательская отмена, исход other,
// первичная причина из relay/pipe сохраняется.
func TestClassifyHandshakeFailureInternalTeardownKeepsCause(t *testing.T) {
	sess, sessCancel := context.WithCancel(context.Background())
	sessCancel()

	cause := errors.New("чтение TURN relay: read udp 10.0.0.1:5000: i/o timeout")
	outcome, err := classifyHandshakeFailure(context.Background(), sess, nil, context.Canceled, cause,
		"1.1.1.1:3478", "turn:1.1.1.1:3478")

	if outcome != turnOutcomeOther {
		t.Fatalf("outcome = %v, ожидался turnOutcomeOther (не пользовательская отмена)", outcome)
	}
	var teardownErr *SessionTeardownError
	if !errors.As(err, &teardownErr) {
		t.Fatalf("ожидался *SessionTeardownError, получено %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("первичная причина потеряна: %v", err)
	}
	if !strings.Contains(err.Error(), "внутреннее завершение сессии") {
		t.Fatalf("нет пометки внутреннего завершения: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "чтение TURN relay") {
		t.Fatalf("в тексте нет причины relay: %q", err.Error())
	}
}

// Транспортная ошибка без отмен контекстов сохраняется через errors.Is.
func TestClassifyHandshakeFailureTransportError(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	outcome, err := classifyHandshakeFailure(context.Background(), context.Background(), nil, sentinel, nil, "1.1.1.1:3478", "")
	if outcome != turnOutcomeOther {
		t.Fatalf("outcome = %v, ожидался turnOutcomeOther", outcome)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("исходная ошибка потеряна: %v", err)
	}
	if !strings.Contains(err.Error(), "DTLS_ERROR") {
		t.Fatalf("нет контекста DTLS_ERROR: %q", err.Error())
	}
}

// Внешняя отмена имеет приоритет над таймаутом handshake-контекста.
func TestClassifyHandshakeFailureExternalCancelWinsOverDeadline(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	outcome, err := classifyHandshakeFailure(parent, context.Background(), context.DeadlineExceeded,
		errors.New("context deadline exceeded"), nil, "1.1.1.1:3478", "")
	if outcome != turnOutcomeCanceled {
		t.Fatalf("outcome = %v, ожидался turnOutcomeCanceled", outcome)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидался context.Canceled, получено %v", err)
	}
}

// Записывается только первая причина тирдауна.
func TestTeardownCauseKeepsFirstError(t *testing.T) {
	var cause teardownCause
	first := errors.New("first")
	second := errors.New("second")
	cause.record(first)
	cause.record(second)
	if got := cause.get(); got != first {
		t.Fatalf("get() = %v, ожидалась первая причина %v", got, first)
	}
	cause.record(nil)
	if got := cause.get(); got != first {
		t.Fatalf("nil перезаписал причину: %v", got)
	}
}

// permitOutcome не должен переписывать уже определённый исход.
func TestPermitOutcomeDoesNotOverrideClassifiedOutcome(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	if got := permitOutcome(parent, turnOutcomeTimeout); got != turnOutcomeTimeout {
		t.Fatalf("таймаут переписан на %v", got)
	}
	if got := permitOutcome(parent, turnOutcomeSuccess); got != turnOutcomeSuccess {
		t.Fatalf("успех переписан на %v", got)
	}
	if got := permitOutcome(context.Background(), turnOutcomeOther); got != turnOutcomeOther {
		t.Fatalf("other переписан на %v при живом внешнем контексте", got)
	}
}

// Полная связка: классификация → permitOutcome → releasePermit (permit.Finish)
// должна оставить таймаут таймаутом, а планировщик — учесть отказ endpoint.
func TestClassificationToPermitKeepsTimeoutInScheduler(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t, "turn:1.1.1.1:3478", "turn:2.2.2.2:3478")
	permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)

	parent := context.Background()
	sess, sessCancel := context.WithCancel(context.Background())
	sessCancel()

	outcome, err := classifyHandshakeFailure(parent, sess, context.DeadlineExceeded,
		errors.New("handshake error: context deadline exceeded"), nil, "1.1.1.1:3478", "turn:1.1.1.1:3478")
	if outcome != turnOutcomeTimeout {
		t.Fatalf("outcome = %v, ожидался turnOutcomeTimeout", outcome)
	}
	var timeoutErr *DTLSHandshakeTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("ожидался *DTLSHandshakeTimeoutError, получено %v", err)
	}

	final := permitOutcome(parent, outcome)
	if final != turnOutcomeTimeout {
		t.Fatalf("permitOutcome переписал таймаут на %v", final)
	}
	permit.Finish(final)

	if got := len(s.global); got != 0 {
		t.Fatalf("глобальный слот не освобождён: занято %d", got)
	}
	snap := findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if snap.Active != 0 {
		t.Fatalf("active = %d, ожидалось 0", snap.Active)
	}
	if snap.Failures != 1 {
		t.Fatalf("failures = %d, ожидалось 1 (таймаут должен штрафовать endpoint)", snap.Failures)
	}
}

// Внешняя отмена не штрафует endpoint.
func TestClassificationToPermitExternalCancelDoesNotPenalize(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t, "turn:1.1.1.1:3478", "turn:2.2.2.2:3478")
	permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)

	parent, parentCancel := context.WithCancel(context.Background())
	parentCancel()

	outcome, err := classifyHandshakeFailure(parent, context.Background(), nil, context.Canceled, nil, "1.1.1.1:3478", "")
	if outcome != turnOutcomeCanceled {
		t.Fatalf("outcome = %v, ожидался turnOutcomeCanceled", outcome)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидался context.Canceled, получено %v", err)
	}
	permit.Finish(permitOutcome(parent, outcome))

	snap := findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if snap.Failures != 0 {
		t.Fatalf("failures = %d, внешняя отмена не должна штрафовать endpoint", snap.Failures)
	}
	if snap.Active != 0 {
		t.Fatalf("active = %d, ожидалось 0", snap.Active)
	}
}

// turnOutcomeOther — не «без штрафа»: неудачная пробная попытка после паузы
// снова выставляет паузу. Фиксируем это поведение, чтобы не обещать лишнего.
func TestTeardownOutcomeDuringProbeRearmsPause(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t, "turn:1.1.1.1:3478", "turn:2.2.2.2:3478")

	first := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	first.Finish(turnOutcomeTimeout)
	second := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	second.Finish(turnOutcomeTimeout)

	paused := findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if paused.PausedUntil.IsZero() {
		t.Fatalf("после двух таймаутов endpoint должен быть на паузе")
	}

	clock.Advance(turnEndpointPause + time.Second)
	probe := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if snap := findEndpointSnapshot(t, s, "1.1.1.1:3478"); !snap.Probing {
		t.Fatalf("ожидалась пробная попытка после паузы")
	}
	probe.Finish(turnOutcomeOther)

	snap := findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if snap.PausedUntil.IsZero() {
		t.Fatalf("неудачная пробная попытка с исходом other должна снова выставить паузу")
	}
}
