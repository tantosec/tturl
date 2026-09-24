package ranking

import "strconv"

// OutlierDirection selects which extreme of the ranking the target occupies.
type OutlierDirection int

const (
	// Late means the target ranks late — it tends to finish among the last.
	Late OutlierDirection = iota
	// Early means the target ranks early — it tends to finish among the first.
	Early
	// Either means the occupied extreme is not known in advance: the target may
	// rank late or early. A positive result reports which extreme it found.
	Either
)

// String returns "Late", "Early", or "Either". It formats an unknown value as
// "OutlierDirection(n)".
func (d OutlierDirection) String() string {
	switch d {
	case Late:
		return "Late"
	case Early:
		return "Early"
	case Either:
		return "Either"
	default:
		return "OutlierDirection(" + strconv.Itoa(int(d)) + ")"
	}
}
