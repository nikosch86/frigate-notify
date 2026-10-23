package events

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0x2142/frigate-notify/models"
)

func TestSetZoneAlerted(t *testing.T) {
	// Setup
	InitZoneCache()
	defer CloseZoneCache()
	event := models.Event{ID: "test-event-id", CurrentZones: []string{"test_zone", "test_zone"}}

	setZoneAlerted(event)

	expected := []string{"test_zone"}
	result, ok := zoneCache.Get(event.ID)
	if !ok {
		t.Error("Could not find event ID")
	}

	// Check if zone added
	if result[0] != expected[0] {
		t.Errorf("Expected: %s, Got: %s", expected, result)
	}

	// Check if duplicates removed
	if len(result) != 1 {
		t.Errorf("Expected: %s, Got: %s", expected, result)
	}
}

func TestGetCachebyID(t *testing.T) {
	// Setup
	InitZoneCache()
	defer CloseZoneCache()
	event := models.Event{ID: "test-event-id", CurrentZones: []string{"test_zone"}}
	setZoneAlerted(event)

	// Check event in cache
	result := getCachebyID(event.ID)
	if result == nil {
		t.Errorf("Expected: ['test_zone'], Got: %v", result)
	}

	// Check non-existent event
	result = getCachebyID("1234")
	if result != nil {
		t.Errorf("Expected: nil, Got: %v", result)
	}
}

func TestZoneAlreadyAlerted(t *testing.T) {
	// Setup
	InitZoneCache()
	defer CloseZoneCache()
	event := models.Event{ID: "test-event-id", CurrentZones: []string{"test_zone", "test_zone"}}

	// Test new event
	result := zoneAlreadyAlerted(event)
	if result != false {
		t.Errorf("Expected: false, Got: %v", result)
	}

	// Test adding new zone to existing event
	event.CurrentZones = append(event.CurrentZones, "another_zone")
	result = zoneAlreadyAlerted(event)
	if result != false {
		t.Errorf("Expected: false, Got: %v", result)
	}

	// Test event that has already generated alert
	result = zoneAlreadyAlerted(event)
	if result != true {
		t.Errorf("Expected: true, Got: %v", result)
	}
}

func TestDelZoneAlerted(t *testing.T) {
	// Setup
	InitZoneCache()
	defer CloseZoneCache()
	event := models.Event{ID: "test-event-id", CurrentZones: []string{"test_zone", "test_zone"}}

	// Create new cache entry
	setZoneAlerted(event)

	// Test delete
	delZoneAlerted(event)
	_, ok := zoneCache.Get(event.ID)
	if ok {
		t.Errorf("Cache entry not deleted")
	}
}

// TestZoneAlreadyAlertedConcurrent checks that when several reviews check the
// same object and zone at once, exactly one of them gets to alert.
func TestZoneAlreadyAlertedConcurrent(t *testing.T) {
	InitZoneCache()
	defer CloseZoneCache()

	for i := range 200 {
		event := models.Event{ID: fmt.Sprintf("evt-%d", i), CurrentZones: []string{"in"}}
		start := make(chan struct{})
		var alerts atomic.Int32
		var checks sync.WaitGroup
		for range 8 {
			checks.Go(func() {
				<-start
				if !zoneAlreadyAlerted(event) {
					alerts.Add(1)
				}
			})
		}
		close(start)
		checks.Wait()
		if n := alerts.Load(); n != 1 {
			t.Fatalf("round %d: %d concurrent checks alerted, want 1", i, n)
		}
	}
}
