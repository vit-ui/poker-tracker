package sheets

import (
	"testing"
	"time"
)

// TestWaitForSlotDisabled: a zero rateInterval (the state the httptest-backed
// tests rely on) must make waitForSlot a no-op — many calls return effectively
// instantly, so the suite isn't paced.
func TestWaitForSlotDisabled(t *testing.T) {
	s := &restService{} // rateInterval zero
	start := time.Now()
	for i := 0; i < 200; i++ {
		s.waitForSlot()
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("disabled limiter delayed 200 calls by %v, want ~0", elapsed)
	}
}

// TestWaitForSlotPaces: with the limiter on, the first burst calls are free,
// then every further call is spaced by ~rateInterval. Hand-checked: burst=3,
// interval=20ms → 10 calls = 3 free + 7 paced ≈ 7×20ms = 140ms, so total must
// be at least (7-1)×20ms (a margin for the free burst and scheduler slop) and
// comfortably under a generous ceiling.
func TestWaitForSlotPaces(t *testing.T) {
	const (
		interval = 20 * time.Millisecond
		burst    = 3
		calls    = 10
	)
	s := &restService{rateInterval: interval, rateBurst: burst}

	start := time.Now()
	for i := 0; i < calls; i++ {
		s.waitForSlot()
	}
	elapsed := time.Since(start)

	paced := calls - burst // 7 calls that must each wait ~interval
	min := time.Duration(paced-1) * interval
	if elapsed < min {
		t.Fatalf("limiter let %d calls through in %v, want >= %v (sustained rate too high)", calls, elapsed, min)
	}
	if max := time.Duration(calls) * interval * 2; elapsed > max {
		t.Fatalf("limiter took %v for %d calls, want < %v (over-throttling)", elapsed, calls, max)
	}
}

// TestWaitForSlotBurstIsFree: after idle, up to rateBurst calls return without
// blocking, so a lone interactive command isn't serialized to one call per
// interval.
func TestWaitForSlotBurstIsFree(t *testing.T) {
	const (
		interval = 50 * time.Millisecond
		burst    = 5
	)
	s := &restService{rateInterval: interval, rateBurst: burst}

	start := time.Now()
	for i := 0; i < burst; i++ {
		s.waitForSlot()
	}
	if elapsed := time.Since(start); elapsed > interval {
		t.Fatalf("first %d (burst) calls took %v, want < one interval (%v)", burst, elapsed, interval)
	}
}
