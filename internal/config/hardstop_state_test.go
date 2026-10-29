package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHardstopReceiptsPreserveExpiryAndSurviveCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	original := LockFile{TriggerID: "hardstop:first", TriggeredAt: now, ExpiresAt: now.Add(time.Hour), Reason: "consecutive_losses", DurationHours: 1}
	lock, fresh, err := ApplyHardstop(path, original, now)
	if err != nil || !fresh || lock == nil || !lock.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("first trigger: %+v %v %v", lock, fresh, err)
	}
	replay := original
	replay.TriggeredAt = now.Add(30 * time.Minute)
	replay.ExpiresAt = replay.TriggeredAt.Add(time.Hour)
	lock, fresh, err = ApplyHardstop(path, replay, replay.TriggeredAt)
	if err != nil || fresh || !lock.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatalf("replay extended active lock: %+v %v %v", lock, fresh, err)
	}
	if err := DeleteLock(path); err != nil {
		t.Fatal(err)
	}
	lock, err = LoadLock(path)
	if err != nil || lock != nil {
		t.Fatalf("cleared lock still active: %+v %v", lock, err)
	}
	replay.TriggeredAt = now.Add(2 * time.Hour)
	replay.ExpiresAt = replay.TriggeredAt.Add(time.Hour)
	lock, fresh, err = ApplyHardstop(path, replay, replay.TriggeredAt)
	if err != nil || fresh || lock != nil {
		t.Fatalf("expired trigger restarted: %+v %v %v", lock, fresh, err)
	}
	replay.TriggerID = "hardstop:new-loss"
	lock, fresh, err = ApplyHardstop(path, replay, replay.TriggeredAt)
	if err != nil || !fresh || lock == nil || !lock.ExpiresAt.Equal(replay.ExpiresAt) {
		t.Fatalf("new trigger blocked: %+v %v %v", lock, fresh, err)
	}
	// Replaying an older expired trigger cannot bypass a different active lock.
	lock, fresh, err = ApplyHardstop(path, original, replay.TriggeredAt.Add(time.Minute))
	if err != nil || fresh || lock == nil || lock.TriggerID != replay.TriggerID {
		t.Fatalf("old replay bypassed current protection: %+v %v %v", lock, fresh, err)
	}
}

func TestNewHardstopCannotShortenExistingDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lockfile.json")
	now := time.Now().UTC()
	long := LockFile{TriggerID: "long", ExpiresAt: now.Add(12 * time.Hour), Reason: "manual"}
	if _, _, err := ApplyHardstop(path, long, now); err != nil {
		t.Fatal(err)
	}
	short := LockFile{TriggerID: "short", ExpiresAt: now.Add(time.Hour), Reason: "consecutive_losses"}
	lock, fresh, err := ApplyHardstop(path, short, now)
	if err != nil || !fresh || !lock.ExpiresAt.Equal(long.ExpiresAt) {
		t.Fatalf("shortened active lock: %+v %v %v", lock, fresh, err)
	}
}

func TestConcurrentHardstopDeliveriesPreserveRequestsAndConfirmation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hardstop_deliveries.json")
	now := time.Now().UTC()
	original := HardstopDelivery{Lock: LockFile{TriggerID: "original", ExpiresAt: now.Add(time.Hour)}}
	if _, err := MergeHardstopDeliveries(path, map[string]HardstopDelivery{"original": original}); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for i := 0; i < 20; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			id := fmt.Sprintf("manual-%d", i)
			stale := original
			stale.Lock.ExpiresAt = now.Add(12 * time.Hour)
			if _, err := MergeHardstopDeliveries(path, map[string]HardstopDelivery{"original": stale, id: {Lock: LockFile{TriggerID: id, ExpiresAt: now.Add(time.Hour)}}}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	writers.Add(1)
	go func() {
		defer writers.Done()
		confirmed := original
		confirmed.Confirmed = true
		if _, err := MergeHardstopDeliveries(path, map[string]HardstopDelivery{"original": confirmed}); err != nil {
			t.Error(err)
		}
	}()
	writers.Wait()
	entries, err := LoadHardstopDeliveries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 21 || !entries["original"].Confirmed || !entries["original"].Lock.ExpiresAt.Equal(original.Lock.ExpiresAt) {
		t.Fatalf("concurrent requests lost confirmation or original expiry: %+v", entries)
	}
}
