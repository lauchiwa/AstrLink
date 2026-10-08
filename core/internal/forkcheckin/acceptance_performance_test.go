package forkcheckin_test

import (
	"reflect"
	"testing"
	"time"
)

func TestAcceptanceP95GateKeepsPublishedLimits(t *testing.T) {
	for _, test := range []struct {
		name       string
		base, next time.Duration
		exceeded   bool
	}{
		{"equal", 20 * time.Millisecond, 20 * time.Millisecond, false},
		{"improvement", 20 * time.Millisecond, 10 * time.Millisecond, false},
		{"exact_absolute_limit", 10 * time.Millisecond, 11 * time.Millisecond, false},
		{"exact_relative_limit", 40 * time.Millisecond, 42 * time.Millisecond, false},
		{"relative_budget_only", 100 * time.Millisecond, 104 * time.Millisecond, false},
		{"absolute_budget_only", time.Millisecond, 1900 * time.Microsecond, false},
		{"one_nanosecond_over_absolute", 10 * time.Millisecond, 11*time.Millisecond + time.Nanosecond, true},
		{"one_nanosecond_over_relative", 40 * time.Millisecond, 42*time.Millisecond + time.Nanosecond, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := acceptanceP95Exceeded(test.base, test.next); got != test.exceeded {
				t.Fatalf("base=%v next=%v exceeded=%t want=%t", test.base, test.next, got, test.exceeded)
			}
		})
	}
}

func TestAcceptanceIdenticalControlsCheckBothDirections(t *testing.T) {
	for _, pair := range [][2]time.Duration{
		{10 * time.Millisecond, 20 * time.Millisecond},
		{20 * time.Millisecond, 10 * time.Millisecond},
	} {
		if !acceptanceP95Exceeded(pair[0], pair[1]) && !acceptanceP95Exceeded(pair[1], pair[0]) {
			t.Fatal("identical-control drift went undetected")
		}
	}
}

func TestAcceptanceMedianRetainsAllOriginalSamples(t *testing.T) {
	values := []time.Duration{4, 10, 1, 3, 2}
	before := append([]time.Duration(nil), values...)
	if median := acceptanceMedian(values); median != 3 {
		t.Fatalf("median=%v want=3ns", median)
	}
	if !reflect.DeepEqual(values, before) {
		t.Fatal("calculating a median reordered the original observations")
	}
}
