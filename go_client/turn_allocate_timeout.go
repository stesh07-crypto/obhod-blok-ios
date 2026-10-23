package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// TurnAllocateTimeoutError marks a TURN Allocate that produced no answer.
//
// Stage records which step timed out:
//   - "allocate" — the Allocate transaction itself exhausted retransmissions;
//   - "prepare"  — the bounded preparation phase aborted while Allocate was in
//     flight.
//
// Failures before Allocate starts (DNS, dial, Listen) never use this type.
type TurnAllocateTimeoutError struct {
	Stage    string
	Endpoint string
	Cause    error
}

func (e *TurnAllocateTimeoutError) Error() string {
	if e == nil {
		return "TURN_ALLOCATE_TIMEOUT"
	}
	return fmt.Sprintf("TURN_ALLOCATE_TIMEOUT (stage=%s, endpoint=%s): %v", e.Stage, e.Endpoint, e.Cause)
}

func (e *TurnAllocateTimeoutError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *TurnAllocateTimeoutError) Timeout() bool { return true }

// isAllocateNoAnswer reports whether an Allocate error means "no response after
// retransmissions" rather than a coded STUN refusal (401/403/438/486/508).
func isAllocateNoAnswer(err error) bool {
	if err == nil {
		return false
	}
	if isTimeoutError(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "all retransmissions failed")
}

// classifyAllocateFailure decides the outcome and error for a failed Allocate.
//
// Order matters: external cancellation wins over any timeout, then an aborted
// preparation phase, then a plain "no answer", and only then the caller keeps
// its own STUN-code classification. A nil error means "not a timeout: handle it
// with the existing classification".
func classifyAllocateFailure(
	parent context.Context,
	stage, endpoint string,
	timedOut bool,
	prepErr, allocErr error,
) (turnHandshakeOutcome, error) {
	if parent != nil && parent.Err() != nil {
		return turnOutcomeCanceled, parent.Err()
	}

	if timedOut {
		cause := prepErr
		if cause == nil {
			cause = allocErr
		}
		return turnOutcomeTimeout, &TurnAllocateTimeoutError{Stage: stage, Endpoint: endpoint, Cause: cause}
	}

	if isAllocateNoAnswer(allocErr) {
		return turnOutcomeTimeout, &TurnAllocateTimeoutError{Stage: "allocate", Endpoint: endpoint, Cause: allocErr}
	}

	return turnOutcomeOther, nil
}

// isAllocateTimeout reports whether err is a typed Allocate timeout. Used by
// group.go to switch endpoints without touching credentials.
func isAllocateTimeout(err error) bool {
	var target *TurnAllocateTimeoutError
	return errors.As(err, &target)
}
