package notifier

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
)

// setupBlockingWebhook installs one webhook profile per event ID, each matching
// only its event's camera so sends never share a status slot, and holds each
// delivery until that event's release func is called. Config is restored on cleanup.
func setupBlockingWebhook(t *testing.T, eventIDs ...string) map[string]func() {
	t.Helper()
	gates := make(map[string]chan struct{}, len(eventIDs))
	releases := make(map[string]func(), len(eventIDs))
	for _, id := range eventIDs {
		gate := make(chan struct{})
		gates[id] = gate
		releases[id] = sync.OnceFunc(func() { close(gate) })
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The default payload carries the event ID as a quoted JSON value
		for id, gate := range gates {
			if strings.Contains(string(body), `"`+id+`"`) {
				<-gate
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	origConfig := config.ConfigData
	origStatus := config.Internal.Status.Notifications.Webhook
	t.Cleanup(func() {
		config.ConfigData = origConfig
		config.Internal.Status.Notifications.Webhook = origStatus
	})
	// Registered after the restore so it runs first: releasing the handlers
	// lets srv.Close return.
	t.Cleanup(srv.Close)
	for _, release := range releases {
		t.Cleanup(release)
	}

	config.ConfigData = config.Config{}
	config.Internal.Status.Notifications.Webhook = make([]models.NotifierStatus, len(eventIDs))
	for _, id := range eventIDs {
		profile := models.Webhook{Server: srv.URL}
		profile.Enabled = true
		profile.Filters.Cameras = []string{deliveryTestCamera(id)}
		config.ConfigData.Alerts.Webhook = append(config.ConfigData.Alerts.Webhook, profile)
	}
	return releases
}

// deliveryTestCamera gives each test event its own camera so its alert matches
// only its own webhook profile.
func deliveryTestCamera(eventID string) string {
	return "cam-" + eventID
}

// deliveryTestEvent builds the minimal event SendAlert needs to route a
// webhook delivery for reviewID.
func deliveryTestEvent(id, reviewID string) models.Event {
	event := models.Event{ID: id, Camera: deliveryTestCamera(id), Label: "person"}
	event.Extra.ReviewID = reviewID
	return event
}

// waitForDeliveryEntryGone polls until the review's delivery entry has been
// removed. The cleanup runs on its own goroutine once the deliveries drain and
// exposes no hook, so a bounded poll is the only way to observe it.
func waitForDeliveryEntryGone(t *testing.T, reviewID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := alertDeliveries.Load(reviewID); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery entry for review %s was not removed after delivery drained", reviewID)
		}
		time.Sleep(time.Millisecond)
	}
}

// startAwait runs awaitAlertDelivery for reviewID in the background and
// returns a channel that is closed once it returns.
func startAwait(reviewID string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		awaitAlertDelivery(reviewID)
		close(done)
	}()
	return done
}

// assertWaiting fails the test if the await has already returned; a returning
// await shows up within the grace period, so this cannot fail on correct code.
func assertWaiting(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
		t.Fatal(msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// assertReturns fails the test if the await does not return.
func assertReturns(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

// TestAwaitAlertDeliveryWithoutPending checks the wait returns at once when no
// alert for the review is being delivered.
func TestAwaitAlertDeliveryWithoutPending(t *testing.T) {
	assertReturns(t, startAwait("rev-none"), "await blocked on a review with no pending delivery")
}

// TestAwaitAlertDeliveryWaitsForEveryProvider checks a GenAI update also waits
// for providers other than Telegram, so its follow-up lands after the alert.
func TestAwaitAlertDeliveryWaitsForEveryProvider(t *testing.T) {
	releases := setupBlockingWebhook(t, "evt-1")
	const reviewID = "rev-webhook"

	SendAlert([]models.Event{deliveryTestEvent("evt-1", reviewID)})
	done := startAwait(reviewID)
	assertWaiting(t, done, "await returned while the webhook send was in flight")

	releases["evt-1"]()
	assertReturns(t, done, "await did not return after the webhook send finished")
}

// TestAwaitAlertDeliveryWaitsForLaterAlert checks a GenAI update also waits for
// an alert of the same review that starts while it is already waiting.
func TestAwaitAlertDeliveryWaitsForLaterAlert(t *testing.T) {
	releases := setupBlockingWebhook(t, "evt-1", "evt-2")
	const reviewID = "rev-later"

	SendAlert([]models.Event{deliveryTestEvent("evt-1", reviewID)})
	done := startAwait(reviewID)
	// Let the await pick up the first delivery before the second replaces it.
	time.Sleep(20 * time.Millisecond)
	SendAlert([]models.Event{deliveryTestEvent("evt-2", reviewID)})

	releases["evt-1"]()
	assertWaiting(t, done, "await returned while the later alert was in flight")

	releases["evt-2"]()
	assertReturns(t, done, "await did not return after the later alert finished")
}

// TestSendAlertRemovesDeliveryEntryAfterDrain checks the entry exists while a
// provider send is in flight and is removed once that send finishes.
func TestSendAlertRemovesDeliveryEntryAfterDrain(t *testing.T) {
	releases := setupBlockingWebhook(t, "evt-1")
	const reviewID = "rev-drain"

	SendAlert([]models.Event{deliveryTestEvent("evt-1", reviewID)})
	if _, ok := alertDeliveries.Load(reviewID); !ok {
		t.Fatal("delivery entry should exist while the send is in flight")
	}

	releases["evt-1"]()
	waitForDeliveryEntryGone(t, reviewID)
}

// TestSendAlertSecondDeliveryOwnsEntry checks that a second alert for the same
// review replaces the entry, and that the first alert's cleanup does not remove
// the second alert's entry while it is still in flight.
func TestSendAlertSecondDeliveryOwnsEntry(t *testing.T) {
	releases := setupBlockingWebhook(t, "evt-1", "evt-2")
	const reviewID = "rev-replace"

	SendAlert([]models.Event{deliveryTestEvent("evt-1", reviewID)})
	first, _ := alertDeliveries.Load(reviewID)

	SendAlert([]models.Event{deliveryTestEvent("evt-2", reviewID)})
	second, ok := alertDeliveries.Load(reviewID)
	if !ok || second == first {
		t.Fatal("second alert should replace the first alert's delivery entry")
	}

	releases["evt-1"]()
	// The first delivery's cleanup runs asynchronously once it drains; pause so
	// it has a chance to run before checking the entry is still the second's.
	time.Sleep(50 * time.Millisecond)
	if got, _ := alertDeliveries.Load(reviewID); got != second {
		t.Fatal("first delivery's cleanup changed the entry while the second delivery was in flight")
	}

	releases["evt-2"]()
	waitForDeliveryEntryGone(t, reviewID)
}

// TestSendAlertMarksReviewNotifiedAfterRegistering checks a review is marked
// notified only once its delivery is registered, so a GenAI update that sees
// the mark always finds the delivery to wait for.
func TestSendAlertMarksReviewNotifiedAfterRegistering(t *testing.T) {
	releases := setupBlockingWebhook(t, "evt-1")
	const reviewID = "rev-mark"

	origSet := ReviewNotifiedSet
	t.Cleanup(func() { ReviewNotifiedSet = origSet })
	var marked, registered bool
	ReviewNotifiedSet = func(id, _ string) {
		marked = id == reviewID
		_, registered = alertDeliveries.Load(id)
	}

	SendAlert([]models.Event{deliveryTestEvent("evt-1", reviewID)})
	releases["evt-1"]()
	if !marked || !registered {
		t.Fatalf("marked=%v registered=%v, want the review marked after its delivery is registered", marked, registered)
	}
	waitForDeliveryEntryGone(t, reviewID)
}
