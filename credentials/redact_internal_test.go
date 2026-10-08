package credentials

import (
	"regexp"
	"testing"
	"unicode"
)

// The split pattern's separator matches exactly what stripSpace removes:
// otherwise a value holding a whitespace rune only one of them knows
// would no longer match its own text (#319 re-review). Every rune
// unicode.IsSpace accepts lies in the Basic Multilingual Plane.
func TestSpaceClassMatchesUnicodeIsSpace(t *testing.T) {
	one := regexp.MustCompile(`^` + spaceClass[:len(spaceClass)-1] + `$`)
	for r := rune(0); r <= 0xFFFF; r++ {
		if got, want := one.MatchString(string(r)), unicode.IsSpace(r); got != want {
			t.Errorf("U+%04X: spaceClass matches = %v, unicode.IsSpace = %v", r, got, want)
		}
	}
}
