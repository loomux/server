package sqlite

import "strings"

// SQLite's C library formats these substrings into its error message
// consistently regardless of build/driver, so matching on them is more
// stable than depending on modernc.org/sqlite's internal extended-code
// constants (not part of its public API surface).
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isForeignKeyConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
