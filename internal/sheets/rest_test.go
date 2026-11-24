package sheets

import (
	"testing"
	"time"
)

func TestWaitForSlotDisabled(t *testing.T) {
	s := &restService{}
	start := time.Now()
	for i := 0; i < 200; i++ {
		s.waitForSlot()
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("disabled limiter delayed 200 calls by %v, want ~0", elapsed)
	}
}

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

	paced := calls - burst
	min := time.Duration(paced-1) * interval
	if elapsed < min {
		t.Fatalf("limiter let %d calls through in %v, want >= %v (sustained rate too high)", calls, elapsed, min)
	}
	if max := time.Duration(calls) * interval * 2; elapsed > max {
		t.Fatalf("limiter took %v for %d calls, want < %v (over-throttling)", elapsed, calls, max)
	}
}

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
