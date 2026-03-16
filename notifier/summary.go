package notifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	"github.com/0x2142/frigate-notify/util"
)

var (
	summaryMu       sync.Mutex
	summaryTimer    *time.Timer
	flurryStartTime time.Time
	flurryActive    bool
)

type summaryResponse struct {
	Success bool   `json:"success"`
	Summary string `json:"summary"`
	Message string `json:"message"`
}

// ResetSummaryTimer is called after each notification is sent.
// It tracks the start of activity and resets the idle timer.
// When the timer fires (no notifications for the configured duration),
// it requests a GenAI summary from Frigate covering the activity period.
func ResetSummaryTimer() {
	idleSeconds := config.ConfigData.Alerts.General.GenAI.SummaryIdleTime
	if idleSeconds <= 0 || !config.ConfigData.Alerts.General.GenAI.Enabled {
		return
	}

	summaryMu.Lock()
	defer summaryMu.Unlock()

	// Mark start of activity flurry
	if !flurryActive {
		flurryActive = true
		flurryStartTime = time.Now()
		log.Debug().
			Time("flurry_start", flurryStartTime).
			Msg("Activity flurry started")
	}

	// Reset or create the idle timer
	idleDuration := time.Duration(idleSeconds) * time.Second
	if summaryTimer != nil {
		summaryTimer.Stop()
	}
	summaryTimer = time.AfterFunc(idleDuration, onSummaryIdle)
}

func onSummaryIdle() {
	summaryMu.Lock()
	startTime := flurryStartTime
	flurryActive = false
	summaryMu.Unlock()

	endTime := time.Now()

	log.Debug().
		Time("start", startTime).
		Time("end", endTime).
		Msg("Activity idle - requesting GenAI summary from Frigate")

	summary, err := requestFrigateSummary(startTime, endTime)
	if err != nil {
		log.Warn().
			Err(err).
			Msg("Failed to get GenAI summary from Frigate")
		return
	}

	if summary == "" {
		log.Debug().Msg("GenAI summary was empty, skipping notification")
		return
	}

	sendSummaryNotification(summary, startTime)
}

func requestFrigateSummary(start, end time.Time) (string, error) {
	url := fmt.Sprintf("%s/api/review/summarize/start/%v/end/%v",
		config.ConfigData.Frigate.Server,
		float64(start.Unix()),
		float64(end.Unix()),
	)

	log.Debug().
		Str("url", url).
		Msg("Requesting GenAI summary from Frigate")

	response, err := util.HTTPPost(url, config.ConfigData.Frigate.Insecure, nil, "", config.ConfigData.Frigate.Headers...)
	if err != nil {
		return "", fmt.Errorf("summary API request failed: %w", err)
	}

	var result summaryResponse
	if err := json.Unmarshal(response, &result); err != nil {
		return "", fmt.Errorf("failed to parse summary response: %w", err)
	}

	if !result.Success {
		return "", fmt.Errorf("Frigate summary API error: %s", result.Message)
	}

	log.Debug().
		Int("summary_length", len(result.Summary)).
		Msg("Received GenAI summary from Frigate")

	return result.Summary, nil
}

func sendSummaryNotification(summary string, activityStart time.Time) {
	config.Internal.Status.LastNotification = time.Now()

	// Build a synthetic event to carry the summary through the notification system
	// The summary goes into GenAISummary so templates render it naturally
	var event models.Event
	event.Extra.GenAITitle = "Activity Summary"
	event.Extra.GenAISummary = summary
	event.Extra.CameraName = "All Cameras"
	event.Extra.FormattedTime = activityStart.String()
	if config.ConfigData.Alerts.General.TimeFormat != "" {
		event.Extra.FormattedTime = activityStart.Format(config.ConfigData.Alerts.General.TimeFormat)
	}
	event.Extra.LocalURL = config.ConfigData.Frigate.Server
	event.Extra.PublicURL = config.ConfigData.Frigate.PublicURL
	event.Extra.FrigateMajorVersion = config.Internal.FrigateVersion

	var snap []byte // no snapshot for summaries

	// Send to all enabled providers (no per-provider filters for summaries)
	for id, profile := range config.ConfigData.Alerts.Telegram {
		if profile.Enabled {
			provider := notifMeta{name: "telegram", index: id}
			go SendTelegramMessage(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Discord {
		if profile.Enabled {
			provider := notifMeta{name: "discord", index: id}
			go SendDiscordMessage(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Gotify {
		if profile.Enabled {
			provider := notifMeta{name: "gotify", index: id}
			go SendGotifyPush(event, provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Matrix {
		if profile.Enabled {
			provider := notifMeta{name: "matrix", index: id}
			go SendMatrix(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Mattermost {
		if profile.Enabled {
			provider := notifMeta{name: "mattermost", index: id}
			go SendMattermost(event, provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Ntfy {
		if profile.Enabled {
			provider := notifMeta{name: "ntfy", index: id}
			go SendNtfyPush(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Pushover {
		if profile.Enabled {
			provider := notifMeta{name: "pushover", index: id}
			go SendPushoverMessage(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Signal {
		if profile.Enabled {
			provider := notifMeta{name: "signal", index: id}
			go SendSignalMessage(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.SMTP {
		if profile.Enabled {
			provider := notifMeta{name: "smtp", index: id}
			go SendSMTP(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.AppriseAPI {
		if profile.Enabled {
			provider := notifMeta{name: "apprise_api", index: id}
			go SendAppriseAPI(event, bytes.NewReader(snap), provider)
		}
	}
	for id, profile := range config.ConfigData.Alerts.Webhook {
		if profile.Enabled {
			provider := notifMeta{name: "webhook", index: id}
			go SendWebhook(event, provider)
		}
	}
}
