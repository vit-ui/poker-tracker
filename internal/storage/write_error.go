package storage

// WriteError distinguishes a failed remote write durably queued for recovery
// from one whose local fallback could not be saved. The original error remains
// available through errors.Is/As.
type WriteError struct {
	Err    error
	Queued bool
}

func (e *WriteError) Error() string { return e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }
