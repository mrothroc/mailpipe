package mailbox

import "errors"

// Error categories support errors.Is even when wrapped by a caller.
var (
	ErrUnsupported     = errors.New("mailbox operation unsupported")
	ErrStaleIdentity   = errors.New("mailbox identity stale")
	ErrAccountMismatch = errors.New("mailbox account mismatch")
)

// UnsupportedError rejects a capability or semantics without a fallback.
// Operation must be a safe operation name, never mail content or a locator.
type UnsupportedError struct{ Operation string }

func (e *UnsupportedError) Error() string {
	return e.Operation + ": operation unsupported; choose a supported adapter or mode, or retain the reviewed proposal"
}
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// StaleIdentityError requires fresh inspection and review, not a blind retry.
// Reason must describe the category, without raw remote identity or mail data.
type StaleIdentityError struct{ Operation, Reason string }

func (e *StaleIdentityError) Error() string {
	return e.Operation + ": " + e.Reason + "; refresh or resync the account and review again before acting"
}
func (e *StaleIdentityError) Unwrap() error { return ErrStaleIdentity }

// AccountMismatchError retains account context for programmatic inspection.
// Error deliberately omits account identifiers, which may be private.
type AccountMismatchError struct {
	Operation        string
	Expected, Actual AccountID
}

func (e *AccountMismatchError) Error() string {
	return e.Operation + ": account mismatch; select the correct account before continuing"
}
func (e *AccountMismatchError) Unwrap() error { return ErrAccountMismatch }
