package cli

import (
	"errors"
	"fmt"
)

// ExitCodeError lets a RunE pin the process exit code that main will honor;
// every other error keeps the generic 1. The stable convention (scripts rely
// on it): 0 = success, 1 = command error / doctor FAIL findings, 2 = doctor
// internal error (first real producer: #5 serve liveness probe).
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		// hand-rolled literal that bypassed NewExitCodeError — never
		// nil-deref at print time; the code alone is still meaningful.
		return fmt.Sprintf("ssh-manager: exit code %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// NewExitCodeError is the sanctioned constructor: code in [1,125] and err != nil
// are internal invariants — violations panic loudly (pinned by test) instead of
// silently producing a zero-code success or a nil-deref at print time. (125:
// exit codes are truncated to the low 8 bits by the OS; >125 risks colliding
// with shell-reserved codes like 126/127.)
func NewExitCodeError(code int, err error) *ExitCodeError {
	if code < 1 || code > 125 || err == nil {
		panic(fmt.Sprintf("NewExitCodeError: invalid code=%d err=%v", code, err))
	}
	return &ExitCodeError{Code: code, Err: err}
}

// ExitCodeFor maps a root-command error to the process exit code: an
// ExitCodeError pins its code, anything else is 1. A hand-rolled literal that
// bypassed NewExitCodeError with a nonsense code (<1 or >125) falls back to 1.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ec *ExitCodeError
	if errors.As(err, &ec) {
		if ec.Code < 1 || ec.Code > 125 {
			return 1
		}
		return ec.Code
	}
	return 1
}
