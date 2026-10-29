// Package appointments is the HTTP side of the saga and the handler that moves an appointment forward as events arrive.
package appointments

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Appointment rules.
const (
	MaxSlotsPerAppointment = 4
	MaxReferenceLength     = 40
	FeeCentsPerSlot        = 12_000
	FlatFeeCents           = 1_500
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// Validate returns an error message, or "" when the request is acceptable. The slot count is compared as int64 on purpose:
// narrowing 4294967297 to int32 would wrap to 1 and pass.
func Validate(reference string, slots int64) string {
	trimmed := strings.TrimSpace(reference)
	switch {
	case trimmed == "":
		return "Patient reference is required."
	case len([]rune(trimmed)) > MaxReferenceLength:
		return fmt.Sprintf("Patient reference must be at most %d characters.", MaxReferenceLength)
	case slots < 1:
		return "An appointment needs at least 1 slot."
	case slots > MaxSlotsPerAppointment:
		return fmt.Sprintf("An appointment takes at most %d slots.", MaxSlotsPerAppointment)
	}
	return ""
}

// FeeCents is a fee per slot plus a flat fee, in cents. Overflow is an error, not a wrap.
func FeeCents(slots int) (int64, error) {
	if slots < 0 || int64(slots) > (math.MaxInt64-FlatFeeCents)/FeeCentsPerSlot {
		return 0, ErrOverflow
	}
	return int64(slots)*FeeCentsPerSlot + FlatFeeCents, nil
}
