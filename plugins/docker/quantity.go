package docker

import (
	"errors"
	"math"
	"math/big"
	"strings"
)

// Kubernetes-style quantities (the sizes' cpu, memory and disk), parsed
// without apimachinery: a decimal number with an optional suffix. cpu
// takes "m" (milli); bytes take the binary (Ki, Mi, Gi, Ti, Pi, Ei) and
// decimal (k, M, G, T, P, E) suffixes.

var errQuantity = errors.New("not a quantity")

// parseCPU is a cpu quantity in nanocpus (Docker's NanoCPUs).
func parseCPU(s string) (int64, error) {
	num, suffix := splitQuantity(s)
	scale := big.NewRat(1e9, 1)
	switch suffix {
	case "":
	case "m":
		scale = big.NewRat(1e6, 1)
	default:
		return 0, errQuantity
	}
	return scaled(num, scale)
}

// parseBytes is a memory or disk quantity in bytes.
func parseBytes(s string) (int64, error) {
	num, suffix := splitQuantity(s)
	var scale *big.Rat
	switch suffix {
	case "":
		scale = big.NewRat(1, 1)
	case "Ki":
		scale = big.NewRat(1<<10, 1)
	case "Mi":
		scale = big.NewRat(1<<20, 1)
	case "Gi":
		scale = big.NewRat(1<<30, 1)
	case "Ti":
		scale = big.NewRat(1<<40, 1)
	case "Pi":
		scale = big.NewRat(1<<50, 1)
	case "Ei":
		scale = big.NewRat(1<<60, 1)
	case "k":
		scale = big.NewRat(1e3, 1)
	case "M":
		scale = big.NewRat(1e6, 1)
	case "G":
		scale = big.NewRat(1e9, 1)
	case "T":
		scale = big.NewRat(1e12, 1)
	case "P":
		scale = big.NewRat(1e15, 1)
	case "E":
		scale = big.NewRat(1e18, 1)
	default:
		return 0, errQuantity
	}
	return scaled(num, scale)
}

// splitQuantity is the numeric part and the suffix: the suffix starts at
// the first letter.
func splitQuantity(s string) (num, suffix string) {
	i := strings.IndexFunc(s, func(r rune) bool { return !(r >= '0' && r <= '9') && r != '.' })
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// scaled is num × scale as an integer: a non-negative decimal of digits
// and at most one dot, with no fraction left after scaling.
func scaled(num string, scale *big.Rat) (int64, error) {
	if num == "" || num == "." || strings.Count(num, ".") > 1 {
		return 0, errQuantity
	}
	r, ok := new(big.Rat).SetString(num)
	if !ok || r.Sign() < 0 {
		return 0, errQuantity
	}
	r.Mul(r, scale)
	if !r.IsInt() || !r.Num().IsInt64() || r.Num().Int64() > math.MaxInt64 {
		return 0, errQuantity
	}
	return r.Num().Int64(), nil
}
