package notifier

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
)

// webhookRequest is what the mock receiver captured from a single delivery.
type webhookRequest struct {
	method string
	query  string
	body   string
}

// setupWebhook installs a single webhook profile pointing at a mock receiver
// and returns the captured request plus the provider's status counters. The
// global config it mutates is restored via t.Cleanup.
func setupWebhook(t *testing.T, profile models.Webhook, responseStatus int) (*webhookRequest, *models.NotifierStatus) {
	t.Helper()
	got := &webhookRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method = r.Method
		got.query = r.URL.RawQuery
		got.body = string(body)
		w.WriteHeader(responseStatus)
	}))
	t.Cleanup(srv.Close)

	origAlerts := config.ConfigData.Alerts.Webhook
	origStatus := config.Internal.Status.Notifications.Webhook
	t.Cleanup(func() {
		config.ConfigData.Alerts.Webhook = origAlerts
		config.Internal.Status.Notifications.Webhook = origStatus
	})

	profile.Server = srv.URL
	config.ConfigData.Alerts.Webhook = []models.Webhook{profile}
	config.Internal.Status.Notifications.Webhook = make([]models.NotifierStatus, 1)
	return got, &config.Internal.Status.Notifications.Webhook[0]
}

// TestSendWebhookDefaultPayload covers the built-in JSON payload: the genai
// object (including the 0.18 observations list) is only present when GenAI
// data exists, and the camera link depends on the Frigate major version.
func TestSendWebhookDefaultPayload(t *testing.T) {
	base := func() models.Event {
		event := models.Event{ID: "evt1", Camera: "front_door", Label: "person", HasClip: true, HasSnapshot: true}
		event.Extra.PublicURL = "http://public"
		event.Extra.CameraName = "Front Door"
		event.Extra.EventLink = "http://public/api/events/evt1/clip.mp4"
		event.Extra.ReviewLink = "http://public/review?id=rev-1"
		event.Extra.FrigateMajorVersion = 14
		return event
	}

	tests := []struct {
		name             string
		mutate           func(*models.Event)
		wantGenAI        bool
		wantObservations []string
		wantCameraLink   string
	}{
		{
			name: "genai data with observations",
			mutate: func(e *models.Event) {
				e.Extra.GenAITitle = "Courier at the door"
				e.Extra.GenAISummary = "A parcel was delivered."
				e.Extra.GenAIThreatLevel = "Suspicious"
				e.Extra.GenAIConfidence = "85%"
				e.Extra.GenAIConcerns = "gate left open"
				e.Extra.GenAIObservations = []string{"A van arrives.", "A parcel is left."}
			},
			wantGenAI:        true,
			wantObservations: []string{"A van arrives.", "A parcel is left."},
			wantCameraLink:   "http://public/#front_door",
		},
		{
			name: "genai data without observations",
			mutate: func(e *models.Event) {
				e.Extra.Description = "A person in a blue jacket."
			},
			wantGenAI:      true,
			wantCameraLink: "http://public/#front_door",
		},
		{
			name:           "no genai data on legacy frigate",
			mutate:         func(e *models.Event) { e.Extra.FrigateMajorVersion = 13 },
			wantGenAI:      false,
			wantCameraLink: "http://public/cameras/front_door",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, status := setupWebhook(t, models.Webhook{}, http.StatusOK)
			event := base()
			tc.mutate(&event)

			SendWebhook(event, notifMeta{name: "webhook", index: 0})

			if got.method != http.MethodPost {
				t.Errorf("method = %q, want POST", got.method)
			}
			if status.Sent != 1 || status.Failed != 0 {
				t.Errorf("status sent=%d failed=%d, want 1/0", status.Sent, status.Failed)
			}

			var payload struct {
				ID    string `json:"id"`
				Links struct {
					Camera string `json:"camera"`
					Clip   string `json:"clip"`
					Review string `json:"review"`
					Snap   string `json:"snapshot"`
				} `json:"links"`
				GenAI *WebhookGenAI `json:"genai"`
			}
			if err := json.Unmarshal([]byte(got.body), &payload); err != nil {
				t.Fatalf("payload is not valid JSON: %v\n%s", err, got.body)
			}
			if payload.ID != "evt1" {
				t.Errorf("id = %q, want evt1", payload.ID)
			}
			if payload.Links.Camera != tc.wantCameraLink {
				t.Errorf("links.camera = %q, want %q", payload.Links.Camera, tc.wantCameraLink)
			}
			if payload.Links.Clip != event.Extra.EventLink || payload.Links.Review != event.Extra.ReviewLink {
				t.Errorf("links clip/review = %q / %q", payload.Links.Clip, payload.Links.Review)
			}
			if payload.Links.Snap != "http://public/api/events/evt1/snapshot.jpg" {
				t.Errorf("links.snapshot = %q", payload.Links.Snap)
			}

			if !tc.wantGenAI {
				if payload.GenAI != nil {
					t.Errorf("expected genai object to be omitted, got %+v", payload.GenAI)
				}
				return
			}
			if payload.GenAI == nil {
				t.Fatalf("expected genai object in payload:\n%s", got.body)
			}
			want := WebhookGenAI{
				Title:        event.Extra.GenAITitle,
				Summary:      event.Extra.GenAISummary,
				Description:  event.Extra.Description,
				ThreatLevel:  event.Extra.GenAIThreatLevel,
				Confidence:   event.Extra.GenAIConfidence,
				Concerns:     event.Extra.GenAIConcerns,
				Observations: tc.wantObservations,
			}
			if payload.GenAI.Title != want.Title || payload.GenAI.Summary != want.Summary ||
				payload.GenAI.Description != want.Description || payload.GenAI.ThreatLevel != want.ThreatLevel ||
				payload.GenAI.Confidence != want.Confidence || payload.GenAI.Concerns != want.Concerns {
				t.Errorf("genai = %+v, want %+v", *payload.GenAI, want)
			}
			if len(payload.GenAI.Observations) != len(tc.wantObservations) {
				t.Fatalf("genai.observations = %v, want %v", payload.GenAI.Observations, tc.wantObservations)
			}
			for i := range tc.wantObservations {
				if payload.GenAI.Observations[i] != tc.wantObservations[i] {
					t.Errorf("genai.observations[%d] = %q, want %q", i, payload.GenAI.Observations[i], tc.wantObservations[i])
				}
			}
		})
	}
}

// TestSendWebhookCustomTemplateGET covers the custom-template branch and the
// GET method, where the rendered params travel in the query string.
func TestSendWebhookCustomTemplateGET(t *testing.T) {
	profile := models.Webhook{
		Method:   "GET",
		Template: map[string]string{"threat": "{{ .Extra.GenAIThreatLevel }}"},
		Params:   []map[string]string{{"id": "{{ .ID }}"}},
	}
	got, status := setupWebhook(t, profile, http.StatusOK)
	event := models.Event{ID: "evt1"}
	event.Extra.GenAIThreatLevel = "Critical"

	SendWebhook(event, notifMeta{name: "webhook", index: 0})

	if got.method != http.MethodGet {
		t.Errorf("method = %q, want GET", got.method)
	}
	if got.query != "id=evt1" {
		t.Errorf("query = %q, want id=evt1", got.query)
	}
	if status.Sent != 1 {
		t.Errorf("status sent = %d, want 1", status.Sent)
	}
}

// TestSendWebhookFailure records a failed delivery when the receiver answers
// with a non-2xx status.
func TestSendWebhookFailure(t *testing.T) {
	_, status := setupWebhook(t, models.Webhook{}, http.StatusInternalServerError)

	SendWebhook(models.Event{ID: "evt1"}, notifMeta{name: "webhook", index: 0})

	if status.Sent != 0 || status.Failed != 1 {
		t.Errorf("status sent=%d failed=%d, want 0/1", status.Sent, status.Failed)
	}
	if status.LastError == "" {
		t.Error("expected last_error to be recorded")
	}
}
