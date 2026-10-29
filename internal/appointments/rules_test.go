package appointments

import (
	"math"
	"strings"
	"testing"
)

func TestValidateRules(t *testing.T) {
	long := strings.Repeat("x", MaxReferenceLength+1)
	cases := []struct {
		reference string
		slots     int64
		want      string
	}{
		{"REF-1", 1, ""},
		{"  REF-1  ", MaxSlotsPerAppointment, ""},
		{strings.Repeat("x", MaxReferenceLength), 1, ""},
		{"", 1, "Patient reference is required."},
		{"   ", 1, "Patient reference is required."},
		{long, 1, "Patient reference must be at most 40 characters."},
		{"REF-1", 0, "An appointment needs at least 1 slot."},
		{"REF-1", -3, "An appointment needs at least 1 slot."},
		{"REF-1", MaxSlotsPerAppointment + 1, "An appointment takes at most 4 slots."},
		{"REF-1", 4294967297, "An appointment takes at most 4 slots."},
		{"REF-1", math.MaxInt64, "An appointment takes at most 4 slots."},
	}
	for _, c := range cases {
		if got := Validate(c.reference, c.slots); got != c.want {
			t.Errorf("Validate(%q, %d) = %q, want %q", c.reference, c.slots, got, c.want)
		}
	}
}

func TestFeeIsAFeePerSlotPlusAFlatFee(t *testing.T) {
	for _, slots := range []int{1, 2, MaxSlotsPerAppointment} {
		got, err := FeeCents(slots)
		if want := int64(slots)*FeeCentsPerSlot + FlatFeeCents; err != nil || got != want {
			t.Errorf("FeeCents(%d) = %d, %v; want %d", slots, got, err, want)
		}
	}
}

func TestFeeThrowsInsteadOfWrapping(t *testing.T) {
	for _, slots := range []int{math.MaxInt32, -1} {
		if _, err := FeeCents(slots); slots < 0 && err == nil {
			t.Errorf("FeeCents(%d) must fail", slots)
		}
	}
}
