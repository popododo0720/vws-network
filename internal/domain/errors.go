package domain

// ErrKind classifies a failure a caller can see.
type ErrKind int

const (
	// Invalid is malformed input, independent of current state.
	Invalid ErrKind = iota + 1
	// NotFound means the target or its parent is absent from the project.
	NotFound
	// Conflict means an overlap, a taken name or address, or a reused key.
	Conflict
	// Precondition means dependents remain or the parent cannot accept the change.
	Precondition
	// Exhausted means the elastic IP pool has no usable address.
	Exhausted
	// Dataplane means the dataplane retries are exhausted.
	Dataplane
	// Busy means a concurrent writer won and the caller should retry.
	Busy
	// Forbidden means the caller may not change the target at all, such as a
	// direct network the deploy side owns.
	Forbidden
)

// Error is the only error the service returns for a rejected call.
type Error struct {
	Kind ErrKind
	Msg  string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Msg
}

func invalid(msg string) error      { return &Error{Kind: Invalid, Msg: msg} }
func notFound(msg string) error     { return &Error{Kind: NotFound, Msg: msg} }
func conflict(msg string) error     { return &Error{Kind: Conflict, Msg: msg} }
func precondition(msg string) error { return &Error{Kind: Precondition, Msg: msg} }
func exhausted(msg string) error    { return &Error{Kind: Exhausted, Msg: msg} }
