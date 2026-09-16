package events

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
)

// TestThreatLevelLabel pins the mapping to Frigate 0.18's threat scale
// (0 = normal, 1 = suspicious, 2 = critical threat) and checks that values
// outside that range clamp to the nearest label.
func TestThreatLevelLabel(t *testing.T) {
	tests := []struct {
		level int
		want  string
	}{
		{0, "Normal"},
		{1, "Suspicious"},
		{2, "Critical"},
		{3, "Critical"},
		{-1, "Normal"},
	}
	for _, tc := range tests {
		if got := threatLevelLabel(tc.level); got != tc.want {
			t.Errorf("threatLevelLabel(%d) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestApplyGenAIMetadata(t *testing.T) {
	meta := &models.ReviewMetadata{
		Title:                "Person delivers package",
		Scene:                "A long chronological narrative.",
		ShortSummary:         "A courier drops a package at the door.",
		Confidence:           0.85,
		PotentialThreatLevel: 1,
		OtherConcerns:        []string{"gate left open", "dog loose"},
		Observations: []string{
			"A van pulls into the driveway.",
			"A person carries a box to the porch.",
		},
	}

	var event models.Event
	applyGenAIMetadata(&event, meta)

	if event.Extra.GenAITitle != meta.Title {
		t.Errorf("GenAITitle = %q, want %q", event.Extra.GenAITitle, meta.Title)
	}
	if event.Extra.GenAISummary != meta.ShortSummary {
		t.Errorf("GenAISummary = %q, want %q", event.Extra.GenAISummary, meta.ShortSummary)
	}
	if event.Extra.GenAIScene != meta.Scene {
		t.Errorf("GenAIScene = %q, want %q", event.Extra.GenAIScene, meta.Scene)
	}
	if event.Extra.GenAIThreatLevel != "Suspicious" {
		t.Errorf("GenAIThreatLevel = %q, want %q", event.Extra.GenAIThreatLevel, "Suspicious")
	}
	if event.Extra.GenAIConcerns != "gate left open, dog loose" {
		t.Errorf("GenAIConcerns = %q", event.Extra.GenAIConcerns)
	}
	if event.Extra.GenAIConfidence != "85%" {
		t.Errorf("GenAIConfidence = %q, want %q", event.Extra.GenAIConfidence, "85%")
	}
	if len(event.Extra.GenAIObservations) != 2 || event.Extra.GenAIObservations[1] != meta.Observations[1] {
		t.Errorf("GenAIObservations = %v, want %v", event.Extra.GenAIObservations, meta.Observations)
	}
}

// TestApplyGenAIMetadataZeroValues checks that a 0.18 payload with an empty
// observations list and no concerns leaves the optional fields empty.
func TestApplyGenAIMetadataZeroValues(t *testing.T) {
	meta := &models.ReviewMetadata{Title: "Quiet", PotentialThreatLevel: 0, Observations: []string{}}

	var event models.Event
	applyGenAIMetadata(&event, meta)

	if event.Extra.GenAIThreatLevel != "Normal" {
		t.Errorf("GenAIThreatLevel = %q, want Normal", event.Extra.GenAIThreatLevel)
	}
	if event.Extra.GenAIConcerns != "" || event.Extra.GenAIConfidence != "" {
		t.Errorf("expected empty concerns/confidence, got %q / %q", event.Extra.GenAIConcerns, event.Extra.GenAIConfidence)
	}
	if len(event.Extra.GenAIObservations) != 0 {
		t.Errorf("expected no observations, got %v", event.Extra.GenAIObservations)
	}
}

// genaiUpdateFixture wires processGenAIReviewUpdate to a mock Frigate API and a
// mock webhook whose custom template echoes the Extra fields handed to
// notifier.SendAlert. Mutated globals are restored via t.Cleanup.
type genaiUpdateFixture struct {
	frigateHits []string
	webhookBody chan map[string]string
}

func newGenAIUpdateFixture(t *testing.T, frigateStatus int, detectionJSON string) *genaiUpdateFixture {
	t.Helper()
	f := &genaiUpdateFixture{webhookBody: make(chan map[string]string, 1)}

	origConfig := config.ConfigData
	origInternal := config.Internal
	t.Cleanup(func() {
		config.ConfigData = origConfig
		config.Internal = origInternal
	})

	frigate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.frigateHits = append(f.frigateHits, r.URL.Path)
		w.WriteHeader(frigateStatus)
		w.Write([]byte(detectionJSON))
	}))
	t.Cleanup(frigate.Close)

	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("webhook received invalid JSON: %v", err)
		}
		f.webhookBody <- body
	}))
	t.Cleanup(webhook.Close)

	config.ConfigData.Frigate.Server = frigate.URL
	config.ConfigData.Frigate.PublicURL = "http://public"
	config.ConfigData.Alerts.General.GenAI.Enabled = true
	config.ConfigData.Alerts.General.GenAI.UpdateNotif = true

	profile := models.Webhook{Server: webhook.URL}
	profile.Enabled = true
	profile.Template = map[string]string{
		"review_id":    "{{ .Extra.ReviewID }}",
		"genai_update": "{{ .Extra.IsGenAIUpdate }}",
		"title":        "{{ .Extra.GenAITitle }}",
		"threat_level": "{{ .Extra.GenAIThreatLevel }}",
		"observations": "{{ range .Extra.GenAIObservations }}{{ . }};{{ end }}",
		"review_link":  "{{ .Extra.ReviewLink }}",
		"score":        "{{ .Extra.TopScorePercent }}",
		"zones":        "{{ .Extra.ZoneList }}",
	}
	config.ConfigData.Alerts.Webhook = []models.Webhook{profile}
	config.Internal.Status.Notifications.Webhook = make([]models.NotifierStatus, 1)

	// Sentinel: SendAlert stamps LastNotification synchronously, so a zero
	// value after the call proves no alert was dispatched.
	config.Internal.Status.LastNotification = time.Time{}
	return f
}

func (f *genaiUpdateFixture) waitForWebhook(t *testing.T) map[string]string {
	t.Helper()
	select {
	case body := <-f.webhookBody:
		return body
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for webhook notification")
		return nil
	}
}

func genaiReview(id string, detections []string, meta *models.ReviewMetadata) models.Review {
	var review models.Review
	review.ID = id
	review.Camera = "front_door"
	review.Data.Detections = detections
	review.Data.Metadata = meta
	return review
}

const genaiDetectionJSON = `{"id":"det-1","camera":"front_door","label":"person","zones":["porch"],"data":{"top_score":0.9}}`

// TestProcessGenAIReviewUpdateDropped covers every early return: the update
// must neither query Frigate (unless it reaches the fetch step) nor dispatch
// an alert.
func TestProcessGenAIReviewUpdateDropped(t *testing.T) {
	meta := &models.ReviewMetadata{Title: "Someone at the door"}
	tests := []struct {
		name          string
		frigateStatus int
		configure     func()
		review        models.Review
		wantFetches   int
	}{
		{
			name:          "genai disabled",
			frigateStatus: http.StatusOK,
			configure:     func() { config.ConfigData.Alerts.General.GenAI.Enabled = false },
			review:        genaiReview("rev-1", []string{"det-1"}, meta),
		},
		{
			name:          "update_notif disabled",
			frigateStatus: http.StatusOK,
			configure:     func() { config.ConfigData.Alerts.General.GenAI.UpdateNotif = false },
			review:        genaiReview("rev-1", []string{"det-1"}, meta),
		},
		{
			name:          "no metadata",
			frigateStatus: http.StatusOK,
			configure:     func() {},
			review:        genaiReview("rev-1", []string{"det-1"}, nil),
		},
		{
			name:          "no detections",
			frigateStatus: http.StatusOK,
			configure:     func() {},
			review:        genaiReview("rev-1", nil, meta),
		},
		{
			name:          "detection fetch fails",
			frigateStatus: http.StatusInternalServerError,
			configure:     func() {},
			review:        genaiReview("rev-1", []string{"det-1", "det-2"}, meta),
			wantFetches:   2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGenAIUpdateFixture(t, tc.frigateStatus, genaiDetectionJSON)
			tc.configure()

			processGenAIReviewUpdate(tc.review)

			if len(f.frigateHits) != tc.wantFetches {
				t.Errorf("Frigate API requests = %v, want %d", f.frigateHits, tc.wantFetches)
			}
			if !config.Internal.Status.LastNotification.IsZero() {
				t.Error("expected no alert to be dispatched")
			}
		})
	}
}

// TestProcessGenAIReviewUpdateSendsEnrichedAlert covers the happy path: the
// detection is fetched from Frigate, enriched with the GenAI metadata and
// flagged as an update so providers edit the original message.
func TestProcessGenAIReviewUpdateSendsEnrichedAlert(t *testing.T) {
	tests := []struct {
		name        string
		threatLevel int
		wantLabel   string
	}{
		{"suspicious", 1, "Suspicious"},
		{"critical", 2, "Critical"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newGenAIUpdateFixture(t, http.StatusOK, genaiDetectionJSON)
			meta := &models.ReviewMetadata{
				Title:                "Courier at the door",
				PotentialThreatLevel: tc.threatLevel,
				Observations:         []string{"A van arrives.", "A parcel is left."},
			}

			processGenAIReviewUpdate(genaiReview("rev-1", []string{"det-1"}, meta))

			if len(f.frigateHits) != 1 || f.frigateHits[0] != "/api/events/det-1" {
				t.Errorf("Frigate API requests = %v, want [/api/events/det-1]", f.frigateHits)
			}
			if config.Internal.Status.LastNotification.IsZero() {
				t.Fatal("expected an alert to be dispatched")
			}

			got := f.waitForWebhook(t)
			want := map[string]string{
				"review_id":    "rev-1",
				"genai_update": "true",
				"title":        "Courier at the door",
				"threat_level": tc.wantLabel,
				"observations": "A van arrives.;A parcel is left.;",
				"review_link":  "http://public/review?id=rev-1",
				"score":        "90%",
				"zones":        "porch",
			}
			for key, wantValue := range want {
				if got[key] != wantValue {
					t.Errorf("%s = %q, want %q", key, got[key], wantValue)
				}
			}
		})
	}
}
