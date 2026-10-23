package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// SessionTeardownError описывает внутреннее завершение сессии — не отмену
// пользователем. Handshake в этом случае обычно возвращает только
// context.Canceled, поэтому первичную причину (ошибку relay/pipe-горутины)
// несёт поле Cause.
type SessionTeardownError struct {
	Handshake error
	Cause     error
}

func (e *SessionTeardownError) Error() string {
	switch {
	case e == nil:
		return "внутреннее завершение сессии"
	case e.Handshake != nil && e.Cause != nil:
		return fmt.Sprintf("внутреннее завершение сессии: %v (причина: %v)", e.Handshake, e.Cause)
	case e.Cause != nil:
		return fmt.Sprintf("внутреннее завершение сессии: %v", e.Cause)
	case e.Handshake != nil:
		return fmt.Sprintf("внутреннее завершение сессии: %v", e.Handshake)
	default:
		return "внутреннее завершение сессии"
	}
}

// Unwrap отдаёт первичную причину, чтобы errors.Is/As работали по ней.
func (e *SessionTeardownError) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Cause != nil {
		return e.Cause
	}
	return e.Handshake
}

// teardownCause однократно запоминает первую ошибку relay/pipe, которая
// отменяет сессию. Без этого при внутренней отмене handshake возвращает
// только context.Canceled и настоящая причина теряется.
type teardownCause struct {
	mu  sync.Mutex
	err error
}

func (t *teardownCause) record(err error) {
	if t == nil || err == nil {
		return
	}
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
}

func (t *teardownCause) get() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// classifyHandshakeFailure — единственное место, где решается исход неудачного
// DTLS handshake. RunSession вызывает её напрямую, поэтому тесты проверяют
// production-код, а не копию логики.
//
// Порядок проверок важен:
//  1. внешний контекст остановлен — причина именно в нём, возвращаем parent.Err();
//  2. истёк handshake-контекст или ошибка классифицируется как timeout;
//  3. внутренний тирдаун сессии — не пользовательская отмена и не вина endpoint;
//  4. прочие транспортные ошибки.
func classifyHandshakeFailure(
	parent context.Context,
	sess context.Context,
	handshakeCtxErr error,
	hsErr error,
	cause error,
	endpoint string,
	turnURL string,
) (turnHandshakeOutcome, error) {
	if parent != nil && parent.Err() != nil {
		return turnOutcomeCanceled, parent.Err()
	}

	if errors.Is(handshakeCtxErr, context.DeadlineExceeded) || isTimeoutError(hsErr) {
		return turnOutcomeTimeout, &DTLSHandshakeTimeoutError{
			Endpoint: endpoint,
			TurnURL:  turnURL,
			Err:      hsErr,
		}
	}

	if sess != nil && sess.Err() != nil {
		return turnOutcomeOther, &SessionTeardownError{Handshake: hsErr, Cause: cause}
	}

	return turnOutcomeOther, fmt.Errorf("DTLS_ERROR: сбой рукопожатия DTLS (relay=%s): %w", endpoint, hsErr)
}

// permitOutcome применяет единственное разрешённое переписывание исхода:
// если остановлен внешний контекст, а исход ещё не определён — это отмена.
// Внутренняя отмена сессии сюда не попадает и не маскирует ни
// SessionTeardownError, ни turnOutcomeOther.
func permitOutcome(parent context.Context, outcome turnHandshakeOutcome) turnHandshakeOutcome {
	if outcome == turnOutcomeOther && parent != nil && parent.Err() != nil {
		return turnOutcomeCanceled
	}
	return outcome
}
