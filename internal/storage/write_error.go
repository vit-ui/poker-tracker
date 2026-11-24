package storage

type WriteError struct {
	Err    error
	Queued bool
}

func (e *WriteError) Error() string { return e.Err.Error() }
func (e *WriteError) Unwrap() error { return e.Err }
