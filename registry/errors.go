package registry

import "errors"

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("registry: not found")

// ErrConflict is returned when an operation would violate a uniqueness or
// referential-integrity constraint (e.g. a duplicate name, or a foreign
// key referencing a row that doesn't exist or still has dependents).
var ErrConflict = errors.New("registry: conflict")

// ConflictError is an ErrConflict (errors.Is matches it) carrying a
// reason a person can act on — what blocks the operation, worded to be
// shown to them as is.
type ConflictError struct{ Reason string }

func (e *ConflictError) Error() string { return "registry: conflict: " + e.Reason }

// Is reports whether target is ErrConflict.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }
