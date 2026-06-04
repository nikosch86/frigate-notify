package notifier

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

func TestTelegramClipMaxBytes(t *testing.T) {
	const mb = int64(1024 * 1024)

	tests := []struct {
		name     string
		size     int
		expected int64
	}{
		{"default when unset", 0, 50 * mb},
		{"default when negative", -5, 50 * mb},
		{"custom value", 100, 100 * mb},
		{"local bot api max", 2000, 2000 * mb},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := telegramClipMaxBytes(models.Telegram{ClipMaxSize: tc.size})
			if got != tc.expected {
				t.Errorf("telegramClipMaxBytes(%d) = %d, want %d", tc.size, got, tc.expected)
			}
		})
	}
}

type capturedReq struct {
	endpoint    string
	contentType string
	body        string
}

// newMockTelegram returns an httptest server that mimics the Telegram Bot API,
// recording each request, plus a pointer to the recorded requests.
func newMockTelegram(t *testing.T) (*httptest.Server, *[]capturedReq, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var reqs []capturedReq
	okMsg := `{"ok":true,"result":{"message_id":555,"date":1,"chat":{"id":1,"type":"private"}}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		endpoint := path.Base(r.URL.Path)

		mu.Lock()
		reqs = append(reqs, capturedReq{endpoint: endpoint, contentType: r.Header.Get("Content-Type"), body: string(body)})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch endpoint {
		case "getMe":
			io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"bot","username":"bot"}}`)
		default:
			io.WriteString(w, okMsg)
		}
	}))
	return srv, &reqs, &mu
}

// TestSendTelegramSnapshotThenClip drives the full snapshot-first flow against
// mock Frigate + Telegram servers and asserts the request sequence: the snapshot
// is delivered first via sendPhoto, then the message is upgraded in-place via an
// editMessageMedia multipart upload carrying the video clip.
func TestSendTelegramSnapshotThenClip(t *testing.T) {
	clipData := []byte("FAKE-CLIP-BYTES")

	frigate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve the event clip for any /api/events/<id>/clip.mp4 request
		w.Write(clipData)
	}))
	defer frigate.Close()

	tg, reqs, mu := newMockTelegram(t)
	defer tg.Close()

	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", tg.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("failed to create mock bot: %v", err)
	}

	// Minimal config required by GetClip
	config.ConfigData.Frigate.Server = frigate.URL
	config.ConfigData.Alerts.General.MaxSnapRetry = 2

	profile := models.Telegram{ChatID: 1, ClipMaxSize: 50}
	status := &models.NotifierStatus{}
	event := models.Event{ID: "evt1", HasSnapshot: true, HasClip: true}
	snapshot := bytes.NewReader([]byte("FAKE-SNAPSHOT"))
	provider := notifMeta{name: "telegram", index: 0}

	sendTelegramSnapshotThenClip(bot, profile, status, event, snapshot, "<b>caption</b>", provider)

	mu.Lock()
	defer mu.Unlock()

	// Collect the non-getMe calls in order
	var calls []capturedReq
	for _, r := range *reqs {
		if r.endpoint != "getMe" {
			calls = append(calls, r)
		}
	}

	if len(calls) != 2 {
		t.Fatalf("expected 2 API calls (sendPhoto, editMessageMedia), got %d: %+v", len(calls), calls)
	}

	// 1. Snapshot must be sent first
	if calls[0].endpoint != "sendPhoto" {
		t.Errorf("expected first call to be sendPhoto, got %q", calls[0].endpoint)
	}

	// 2. Then the message is upgraded to the video clip in-place
	edit := calls[1]
	if edit.endpoint != "editMessageMedia" {
		t.Fatalf("expected second call to be editMessageMedia, got %q", edit.endpoint)
	}
	if !strings.HasPrefix(edit.contentType, "multipart/form-data") {
		t.Errorf("expected editMessageMedia to be a multipart upload, got content-type %q", edit.contentType)
	}
	if !strings.Contains(edit.body, `"type":"video"`) {
		t.Errorf("expected editMessageMedia media to be a video, body: %q", edit.body)
	}
	if !strings.Contains(edit.body, "attach://file-0") {
		t.Errorf("expected editMessageMedia to reference attach://file-0, body: %q", edit.body)
	}
	if !strings.Contains(edit.body, string(clipData)) {
		t.Error("expected editMessageMedia multipart body to contain the uploaded clip bytes")
	}
	if !strings.Contains(edit.body, "555") {
		t.Error("expected editMessageMedia to target the snapshot message_id (555)")
	}

	// Notification counted as a single success (snapshot delivered)
	if status.Sent != 1 || status.Failed != 0 {
		t.Errorf("expected status sent=1 failed=0, got sent=%d failed=%d", status.Sent, status.Failed)
	}
}

// TestSendTelegramSnapshotThenClipOversized verifies that a clip exceeding the
// configured size limit is skipped and the snapshot is kept (no editMessageMedia).
func TestSendTelegramSnapshotThenClipOversized(t *testing.T) {
	// ~2 MB clip, limit set to 1 MB
	clipData := bytes.Repeat([]byte("x"), 2*1024*1024)

	frigate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(clipData)
	}))
	defer frigate.Close()

	tg, reqs, mu := newMockTelegram(t)
	defer tg.Close()

	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", tg.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("failed to create mock bot: %v", err)
	}

	config.ConfigData.Frigate.Server = frigate.URL
	config.ConfigData.Alerts.General.MaxSnapRetry = 2

	profile := models.Telegram{ChatID: 1, ClipMaxSize: 1}
	status := &models.NotifierStatus{}
	event := models.Event{ID: "evt2", HasSnapshot: true, HasClip: true}
	snapshot := bytes.NewReader([]byte("FAKE-SNAPSHOT"))
	provider := notifMeta{name: "telegram", index: 0}

	sendTelegramSnapshotThenClip(bot, profile, status, event, snapshot, "caption", provider)

	mu.Lock()
	defer mu.Unlock()

	for _, r := range *reqs {
		if r.endpoint == "editMessageMedia" {
			t.Error("oversized clip should not trigger editMessageMedia; snapshot should be kept")
		}
	}
	if status.Sent != 1 {
		t.Errorf("expected snapshot to still be sent (sent=1), got sent=%d", status.Sent)
	}
}
