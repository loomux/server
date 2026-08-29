package registry

import "errors"

// ErrNotFound is returned when a requested entity does not exist.
var ErrNotFound = errors.New("registry: not found")

// ErrConflict is returned when an operation would violate a uniqueness or
// referential-integrity constraint (e.g. a duplicate name, or a foreign
// key referencing a row that doesn't exist or still has dependents).
var ErrConflict = errors.New("registry: conflict")
