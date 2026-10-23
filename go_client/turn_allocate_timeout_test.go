package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyAllocateNoAnswerKeepsStageAndCause(t *testing.T) {
	cause := errors.New("all retransmissions failed")
	outcome, err := classifyAllocateFailure(context.Background(), "allocate", "193.203.43.21:19302", false, nil, cause)

	if outcome != turnOutcomeTimeout {
		t.Fatalf("outcome = %v, ожидался turnOutcomeTimeout", outcome)
	}
	var allocErr *TurnAllocateTimeoutError
	if !errors.As(err, &allocErr) {
		t.Fatalf("ожидался *TurnAllocateTimeoutError, получено %v", err)
	}
	if allocErr.Stage != "allocate" || allocErr.Endpoint != "193.203.43.21:19302" {
		t.Fatalf("потеряны этап/адрес: %+v", allocErr)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("исходная причина потеряна: %v", err)
	}
	if !allocErr.Timeout() {
		t.Fatalf("Timeout() должен быть true")
	}
}

func TestClassifyAllocatePrepareStage(t *testing.T) {
	prepErr := context.DeadlineExceeded
	_, err := classifyAllocateFailure(context.Background(), "prepare", "1.1.1.1:3478", true, prepErr, nil)
	var allocErr *TurnAllocateTimeoutError
	if !errors.As(err, &allocErr) || allocErr.Stage != "prepare" {
		t.Fatalf("ожидался этап prepare, получено %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("причина подготовки потеряна: %v", err)
	}
}

// Не-таймаут (код STUN) не должен превращаться в Allocate timeout: вызывающий
// продолжает свою классификацию.
func TestClassifyAllocateCodedRefusalIsNotTimeout(t *testing.T) {
	coded := errors.New("TURN_QUOTA (код 486): quota reached")
	outcome, err := classifyAllocateFailure(context.Background(), "allocate", "1.1.1.1:3478", false, nil, coded)
	if outcome != turnOutcomeOther || err != nil {
		t.Fatalf("кодированный отказ не должен становиться таймаутом: outcome=%v err=%v", outcome, err)
	}
}

func TestClassifyAllocateExternalCancelWins(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	outcome, err := classifyAllocateFailure(parent, "allocate", "1.1.1.1:3478", true,
		context.DeadlineExceeded, errors.New("all retransmissions failed"))
	if outcome != turnOutcomeCanceled {
		t.Fatalf("outcome = %v, ожидался turnOutcomeCanceled", outcome)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ожидался context.Canceled, получено %v", err)
	}
	var allocErr *TurnAllocateTimeoutError
	if errors.As(err, &allocErr) {
		t.Fatalf("внешняя отмена не должна давать TurnAllocateTimeoutError")
	}
}

// Связка классификация → permitOutcome → Finish: два ALLOCATE_TIMEOUT подряд
// ставят endpoint на паузу, а следующая Acquire выбирает альтернативу из тех же
// кредов.
func TestAllocateTimeoutPausesEndpointAndNextAcquireUsesAlternative(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:193.203.43.21:19302",
		"turn:193.203.43.46:19302",
	)
	parent := context.Background()

	for attempt := 0; attempt < 2; attempt++ {
		permit := mustAcquireTurnPermit(t, s, "turn:193.203.43.21:19302", candidates)
		outcome, err := classifyAllocateFailure(parent, "allocate", "193.203.43.21:19302", false, nil,
			errors.New("all retransmissions failed"))
		if outcome != turnOutcomeTimeout {
			t.Fatalf("attempt %d: outcome = %v", attempt, outcome)
		}
		var allocErr *TurnAllocateTimeoutError
		if !errors.As(err, &allocErr) {
			t.Fatalf("attempt %d: ожидался тип ошибки", attempt)
		}
		permit.Finish(permitOutcome(parent, outcome))
	}

	snap := findEndpointSnapshot(t, s, "193.203.43.21:19302")
	if snap.Failures != 2 {
		t.Fatalf("failures = %d, ожидалось 2", snap.Failures)
	}
	if snap.PausedUntil.IsZero() {
		t.Fatalf("после двух ALLOCATE_TIMEOUT endpoint должен быть на паузе")
	}

	next := mustAcquireTurnPermit(t, s, "", candidates)
	if next.Endpoint() != "193.203.43.46:19302" {
		t.Fatalf("выбран %s, ожидалась альтернатива из тех же кредов", next.Endpoint())
	}
	next.Finish(turnOutcomeSuccess)
}

// Единственный адрес: поведение не меняется — Acquire продолжает его выдавать
// даже после двух таймаутов (существующая политика single-endpoint).
func TestAllocateTimeoutSingleEndpointKeepsWorking(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t, "turn:193.203.43.21:19302")
	parent := context.Background()

	for attempt := 0; attempt < 2; attempt++ {
		permit := mustAcquireTurnPermit(t, s, "turn:193.203.43.21:19302", candidates)
		outcome, _ := classifyAllocateFailure(parent, "allocate", "193.203.43.21:19302", false, nil,
			errors.New("all retransmissions failed"))
		permit.Finish(permitOutcome(parent, outcome))
	}

	done := make(chan *turnPermit, 1)
	go func() {
		p, err := s.Acquire(context.Background(), "turn:193.203.43.21:19302", candidates)
		if err == nil {
			done <- p
		}
	}()

	select {
	case p := <-done:
		if p.Endpoint() != "193.203.43.21:19302" {
			t.Fatalf("endpoint = %s", p.Endpoint())
		}
		p.Finish(turnOutcomeSuccess)
	case <-time.After(2 * time.Second):
		t.Fatalf("единственный endpoint заблокирован после таймаутов")
	}
}

// Внешняя отмена не штрафует endpoint.
func TestAllocateExternalCancelDoesNotPenalizeEndpoint(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:193.203.43.21:19302",
		"turn:193.203.43.46:19302",
	)
	permit := mustAcquireTurnPermit(t, s, "turn:193.203.43.21:19302", candidates)

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := classifyAllocateFailure(parent, "allocate", "193.203.43.21:19302", false, nil,
		errors.New("all retransmissions failed"))
	if outcome != turnOutcomeCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	permit.Finish(permitOutcome(parent, outcome))

	snap := findEndpointSnapshot(t, s, "193.203.43.21:19302")
	if snap.Failures != 0 {
		t.Fatalf("внешняя отмена не должна штрафовать endpoint: failures=%d", snap.Failures)
	}
	if !snap.PausedUntil.IsZero() {
		t.Fatalf("внешняя отмена не должна ставить паузу")
	}
}

// Текст ошибки должен позволять отличить Allocate timeout от DTLS handshake.
func TestAllocateTimeoutErrorText(t *testing.T) {
	err := &TurnAllocateTimeoutError{Stage: "allocate", Endpoint: "1.1.1.1:3478", Cause: errors.New("all retransmissions failed")}
	text := err.Error()
	if !strings.Contains(text, "TURN_ALLOCATE_TIMEOUT") {
		t.Fatalf("нет метки типа: %s", text)
	}
	if strings.Contains(text, "DTLS_TIMEOUT") {
		t.Fatalf("Allocate timeout не должен маскироваться под DTLS: %s", text)
	}
}
