package clone

// RefusalError wraps one of the sentinel errors with the exact user-facing
// refusal text (WP1 §3, §4): its Error() returns the message verbatim, with
// nothing prefixed, so callers can print err.Error() directly to stderr and
// match the documented refusal contract exactly, while errors.Is still
// resolves against the wrapped sentinel for tests/callers that branch on
// failure kind.
type RefusalError struct {
	Err     error
	Message string
}

func (e *RefusalError) Error() string { return e.Message }
func (e *RefusalError) Unwrap() error { return e.Err }
