package notifier

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

// newMockFrigateClipServer returns an httptest server that serves the given clip
// bytes for any request (used as the Frigate backend that GetClip downloads from).
func newMockFrigateClipServer(clip []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(clip)
	}))
}

// TestTelegramGenAISummaryAfterSnapshotThenClipE2E exercises the full feature
// end-to-end against mock Frigate and Telegram servers, through the real
// SendTelegramMessage entry point (template rendering, bot HTTP calls, clip
// download, message-ID caching and the GenAI edit path):
//
//  1. An alert arrives with send_clip_after_snapshot enabled. The snapshot is
//     delivered immediately (sendPhoto), then upgraded in-place to the video
//     clip (editMessageMedia), and the snapshot's message ID is cached.
//  2. A GenAI summary update for the same review arrives later. It must reuse
//     the cached message ID to edit that same message's caption
//     (editMessageCaption) rather than posting a brand-new notification.
//
// This demonstrates that GenAI summaries correctly enrich a message that has
// already gone through the snapshot-then-clip upgrade.
func TestTelegramGenAISummaryAfterSnapshotThenClipE2E(t *testing.T) {
	const (
		reviewID    = "review-genai-1"
		genaiTitle  = "Delivery at front door"
		genaiDetail = "A delivery driver dropped off a package and left."
	)
	clipData := []byte("FAKE-CLIP-BYTES")

	// Mock Frigate: serve the event clip for the snapshot->clip upgrade.
	frigate := newMockFrigateClipServer(clipData)
	defer frigate.Close()

	// Mock Telegram Bot API (records every request in order).
	tg, reqs, mu := newMockTelegram(t)
	defer tg.Close()
	useTelegramE2ESeams(t, tg)

	// Minimal config required by GetClip + template rendering.
	config.ConfigData.Frigate.Server = frigate.URL
	config.ConfigData.Alerts.General.MaxSnapRetry = 2

	profile := models.Telegram{ChatID: 1, ClipMaxSize: 50, SendClipAfterSnapshot: true}
	profile.Enabled = true
	config.ConfigData.Alerts.Telegram = []models.Telegram{profile}
	config.Internal.Status.Notifications.Telegram = make([]models.NotifierStatus, 1)
	status := &config.Internal.Status.Notifications.Telegram[0]
	provider := notifMeta{name: "telegram", index: 0}

	// --- Step 1: initial alert (snapshot delivered, then upgraded to clip) ---
	alert := models.Event{ID: "evt1", Camera: "front_door", HasSnapshot: true, HasClip: true}
	alert.Extra.ReviewID = reviewID
	alert.Extra.CameraName = "Front Door"
	SendTelegramMessage(alert, bytes.NewReader([]byte("FAKE-SNAPSHOT")), provider)

	// The snapshot message ID must be cached for the later GenAI edit.
	if cachedID := NotifCacheGet(reviewID, "telegram:0"); cachedID != "555" {
		t.Fatalf("expected snapshot message ID 555 to be cached, got %q", cachedID)
	}

	// --- Step 2: GenAI summary update for the same review arrives later ---
	update := models.Event{ID: "evt1", Camera: "front_door", HasSnapshot: true, HasClip: true}
	update.Extra.ReviewID = reviewID
	update.Extra.IsGenAIUpdate = true
	update.Extra.CameraName = "Front Door"
	update.Extra.GenAITitle = genaiTitle
	update.Extra.GenAISummary = genaiDetail
	update.Extra.GenAIThreatLevel = "Normal"
	update.Extra.GenAIObservations = []string{"A courier walks up to the porch.", "The courier leaves a parcel and walks away."}
	SendTelegramMessage(update, bytes.NewReader(nil), provider)

	// --- Assert the full request sequence ---
	mu.Lock()
	defer mu.Unlock()

	var calls []capturedReq
	for _, r := range *reqs {
		if r.endpoint != "getMe" {
			calls = append(calls, r)
		}
	}

	want := []string{"sendPhoto", "editMessageMedia", "editMessageCaption"}
	if len(calls) != len(want) {
		t.Fatalf("expected call sequence %v, got %d calls: %+v", want, len(calls), endpoints(calls))
	}
	for i, w := range want {
		if calls[i].endpoint != w {
			t.Fatalf("call %d: expected %q, got %q (full sequence: %v)", i, w, calls[i].endpoint, endpoints(calls))
		}
	}

	// The GenAI update must NOT post a new photo/video message.
	for _, r := range calls {
		if r.endpoint == "sendVideo" {
			t.Errorf("GenAI update should edit the existing message, not send a new video")
		}
	}
	if countEndpoint(calls, "sendPhoto") != 1 {
		t.Errorf("expected exactly one sendPhoto (the initial snapshot), got %d", countEndpoint(calls, "sendPhoto"))
	}

	// Step 1b: snapshot upgraded to the clip in-place (same message, video media).
	editMedia := calls[1]
	if !strings.HasPrefix(editMedia.contentType, "multipart/form-data") {
		t.Errorf("editMessageMedia should be a multipart upload, got content-type %q", editMedia.contentType)
	}
	if !strings.Contains(editMedia.body, `"type":"video"`) {
		t.Errorf("editMessageMedia media should be a video, body: %q", editMedia.body)
	}
	if !strings.Contains(editMedia.body, string(clipData)) {
		t.Error("editMessageMedia multipart body should contain the uploaded clip bytes")
	}
	if !strings.Contains(editMedia.body, "555") {
		t.Error("editMessageMedia should target the cached snapshot message_id (555)")
	}

	// Step 2: GenAI summary edits the caption of that same message.
	editCaption := calls[2]
	captionParams, err := url.ParseQuery(editCaption.body)
	if err != nil {
		t.Fatalf("could not parse editMessageCaption body: %v", err)
	}
	if got := captionParams.Get("message_id"); got != "555" {
		t.Errorf("editMessageCaption should target the cached message_id 555, got %q", got)
	}
	if got := captionParams.Get("chat_id"); got != "1" {
		t.Errorf("editMessageCaption should target chat_id 1, got %q", got)
	}
	caption := captionParams.Get("caption")
	t.Logf("edited caption:\n%s", caption)
	if !strings.Contains(caption, genaiTitle) {
		t.Errorf("edited caption should contain the GenAI title %q, got: %q", genaiTitle, caption)
	}
	if !strings.Contains(caption, genaiDetail) {
		t.Errorf("edited caption should contain the GenAI summary %q, got: %q", genaiDetail, caption)
	}
	timeline := "• A courier walks up to the porch.\n• The courier leaves a parcel and walks away."
	if !strings.Contains(caption, timeline) {
		t.Errorf("edited caption should contain the observations timeline, got: %q", caption)
	}
	if strings.Index(caption, timeline) > strings.Index(caption, "Links:") {
		t.Errorf("observations timeline should be placed before the links line, got: %q", caption)
	}

	// Both sends are reported as successful (1 snapshot + 1 GenAI edit).
	if status.Sent != 2 || status.Failed != 0 {
		t.Errorf("expected status sent=2 failed=0, got sent=%d failed=%d", status.Sent, status.Failed)
	}
}

// TestTelegramGenAIUpdateDuringDeliveryE2E sends a GenAI update while the alert
// is held at its snapshot or clip download and checks that the update waits, then
// edits the delivered message's caption after the clip swap so it cannot be undone.
func TestTelegramGenAIUpdateDuringDeliveryE2E(t *testing.T) {
	for _, held := range []string{"/snapshot.jpg", "/clip.mp4"} {
		t.Run(held, func(t *testing.T) {
			const genaiTitle = "Person at the front gate"
			reviewID := "review-race" + held

			// Mock Frigate: hold the first download of the held file (the original
			// alert's) until released; serve everything else right away.
			heldRequested := make(chan struct{})
			releaseHeld := make(chan struct{})
			var firstHeld sync.Once
			frigate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, held) {
					first := false
					firstHeld.Do(func() {
						first = true
						close(heldRequested)
					})
					if first {
						<-releaseHeld
					}
				}
				w.Write([]byte("FAKE-MEDIA"))
			}))
			defer frigate.Close()

			tg, reqs, mu := newMockTelegram(t)
			defer tg.Close()
			useTelegramE2ESeams(t, tg)

			origConfig := config.ConfigData
			t.Cleanup(func() { config.ConfigData = origConfig })
			config.ConfigData = config.Config{}
			config.ConfigData.Frigate.Server = frigate.URL
			config.ConfigData.Alerts.General.MaxSnapRetry = 2
			profile := models.Telegram{ChatID: 1, ClipMaxSize: 50, SendClipAfterSnapshot: true}
			profile.Enabled = true
			config.ConfigData.Alerts.Telegram = []models.Telegram{profile}
			config.Internal.Status.Notifications.Telegram = make([]models.NotifierStatus, 1)

			// Both SendAlert calls must have returned before the config is restored.
			var sends sync.WaitGroup
			defer sends.Wait()

			alert := models.Event{ID: "evt1", Camera: "front_gate", HasSnapshot: true, HasClip: true}
			alert.Extra.ReviewID = reviewID
			sends.Go(func() { SendAlert([]models.Event{alert}) })

			select {
			case <-heldRequested:
			case <-time.After(5 * time.Second):
				t.Fatalf("original alert never requested %s", held)
			}

			update := alert
			update.Extra.IsGenAIUpdate = true
			update.Extra.GenAITitle = genaiTitle
			sends.Go(func() { SendAlert([]models.Event{update}) })

			// Give an update that does not wait the chance to post its own alert.
			time.Sleep(200 * time.Millisecond)
			close(releaseHeld)

			calls := waitForTelegramCalls(t, reqs, mu, 3)
			want := []string{"sendPhoto", "editMessageMedia", "editMessageCaption"}
			if got := endpoints(calls); !slices.Equal(got, want) {
				t.Fatalf("expected call sequence %v, got %v", want, got)
			}
			captionParams, err := url.ParseQuery(calls[2].body)
			if err != nil {
				t.Fatalf("could not parse editMessageCaption body: %v", err)
			}
			if got := captionParams.Get("message_id"); got != "555" {
				t.Errorf("editMessageCaption should target the delivered message 555, got %q", got)
			}
			if caption := captionParams.Get("caption"); !strings.Contains(caption, genaiTitle) {
				t.Errorf("edited caption should contain the GenAI title %q, got: %q", genaiTitle, caption)
			}
		})
	}
}

// useTelegramE2ESeams points the bot constructor at the mock Telegram server,
// renders with the on-disk notification templates and backs the notification
// cache with a map, restoring all three when the test ends.
func useTelegramE2ESeams(t *testing.T, tg *httptest.Server) {
	t.Helper()

	origNewBot, origTemplates := newTelegramBot, TemplateFiles
	origSet, origGet := NotifCacheSet, NotifCacheGet
	t.Cleanup(func() {
		newTelegramBot, TemplateFiles = origNewBot, origTemplates
		NotifCacheSet, NotifCacheGet = origSet, origGet
	})

	newTelegramBot = func(token string) (*tgbotapi.BotAPI, error) {
		return tgbotapi.NewBotAPIWithAPIEndpoint(token, tg.URL+"/bot%s/%s")
	}
	TemplateFiles = os.DirFS("..")

	notifCache := map[string]string{}
	var cacheMu sync.Mutex
	NotifCacheSet = func(rID, provider, msgID string) {
		cacheMu.Lock()
		defer cacheMu.Unlock()
		notifCache[rID+":"+provider] = msgID
	}
	NotifCacheGet = func(rID, provider string) string {
		cacheMu.Lock()
		defer cacheMu.Unlock()
		return notifCache[rID+":"+provider]
	}
}

// waitForTelegramCalls polls the mock Telegram server until it has received n
// requests other than getMe, or 5s pass, and returns those requests in order.
func waitForTelegramCalls(t *testing.T, reqs *[]capturedReq, mu *sync.Mutex, n int) []capturedReq {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		var calls []capturedReq
		for _, r := range *reqs {
			if r.endpoint != "getMe" {
				calls = append(calls, r)
			}
		}
		mu.Unlock()
		if len(calls) >= n || time.Now().After(deadline) {
			return calls
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func endpoints(calls []capturedReq) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.endpoint
	}
	return out
}

func countEndpoint(calls []capturedReq, endpoint string) int {
	n := 0
	for _, c := range calls {
		if c.endpoint == endpoint {
			n++
		}
	}
	return n
}
