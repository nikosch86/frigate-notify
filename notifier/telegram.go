package notifier

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

// telegramDefaultClipMaxSizeMB matches Telegram's cloud Bot API upload limit and
// is used when a profile does not configure clip_max_size.
const telegramDefaultClipMaxSizeMB = 50

// telegramClipMaxBytes returns the configured clip size limit in bytes, falling
// back to the default when the profile leaves clip_max_size unset (<= 0).
func telegramClipMaxBytes(profile models.Telegram) int64 {
	max := profile.ClipMaxSize
	if max <= 0 {
		max = telegramDefaultClipMaxSizeMB
	}
	return int64(max) * 1024 * 1024
}

// SendTelegramMessage sends alert through Telegram to individual users
func SendTelegramMessage(event models.Event, snapshot io.Reader, provider notifMeta) {
	profile := config.ConfigData.Alerts.Telegram[provider.index]
	status := &config.Internal.Status.Notifications.Telegram[provider.index]

	// Build notification
	var message string
	if profile.Template != "" {
		message = renderMessage(profile.Template, event, "message", "Telegram")
	} else {
		message = renderMessage("html", event, "message", "Telegram")
		message = strings.ReplaceAll(message, "<br />", "")
	}

	bot, err := tgbotapi.NewBotAPI(profile.Token)
	if err != nil {
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Unable to send alert")
		status.NotifFailure(err.Error())

		return
	}

	providerKey := fmt.Sprintf("telegram:%d", provider.index)

	// If this is a GenAI update, try to edit the existing message
	if event.Extra.IsGenAIUpdate && event.Extra.ReviewID != "" && NotifCacheGet != nil {
		cachedMsgID := NotifCacheGet(event.Extra.ReviewID, providerKey)
		if cachedMsgID != "" {
			msgID, _ := strconv.Atoi(cachedMsgID)
			if msgID != 0 {
				editMsg := tgbotapi.NewEditMessageCaption(profile.ChatID, msgID, message)
				editMsg.ParseMode = "HTML"
				_, err = bot.Send(editMsg)
				if err != nil {
					log.Warn().
						Str("event_id", event.ID).
						Str("provider", "Telegram").
						Int("provider_id", provider.index).
						Err(err).
						Msg("Unable to edit alert, sending new message")
				} else {
					log.Info().
						Str("event_id", event.ID).
						Str("provider", "Telegram").
						Int("provider_id", provider.index).
						Msg("Alert updated (GenAI)")
					status.NotifSuccess()
					return
				}
			}
		}
	}

	// Snapshot-first flow: deliver the snapshot immediately, then upgrade the
	// same message to the video clip once it is available & within size limit
	if profile.SendClipAfterSnapshot {
		sendTelegramSnapshotThenClip(bot, profile, status, event, snapshot, message, provider)
		return
	}

	// Collect event clip if available & configured
	var clip io.Reader
	if event.HasClip && profile.SendClip {
		clip = GetClip(event)
		if clip == nil {
			event.HasClip = false
		}
	}

	var response tgbotapi.Message
	if event.HasClip && profile.SendClip {
		msg := tgbotapi.NewVideo(profile.ChatID, tgbotapi.FileReader{Name: "Clip", Reader: clip})
		if profile.MessageThreadID != 0 {
			msg.MessageThreadID = profile.MessageThreadID
		}
		msg.Caption = message
		msg.ParseMode = "HTML"
		response, err = bot.Send(msg)
	} else if event.HasSnapshot {
		// Attach & send snapshot
		msg := tgbotapi.NewPhoto(profile.ChatID, tgbotapi.FileReader{Name: "Snapshot", Reader: snapshot})
		if profile.MessageThreadID != 0 {
			msg.MessageThreadID = profile.MessageThreadID
		}
		msg.Caption = message
		msg.ParseMode = "HTML"
		response, err = bot.Send(msg)
	} else {
		// Send plain text message if no snapshot available
		msg := tgbotapi.NewMessage(profile.ChatID, message)
		if profile.MessageThreadID != 0 {
			msg.MessageThreadID = profile.MessageThreadID
		}
		msg.ParseMode = "HTML"
		response, err = bot.Send(msg)
	}
	log.Trace().
		Interface("content", response).
		Int("provider_id", provider.index).
		Msg("Send Telegram Alert")
	if err != nil {
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Unable to send alert")
		status.NotifFailure(err.Error())
		return
	}

	// Cache message ID for potential GenAI update edits
	if event.Extra.ReviewID != "" && NotifCacheSet != nil {
		NotifCacheSet(event.Extra.ReviewID, providerKey, strconv.Itoa(response.MessageID))
	}

	log.Info().
		Str("event_id", event.ID).
		Str("provider", "Telegram").
		Int("provider_id", provider.index).
		Msg("Alert sent")
	status.NotifSuccess()
}

// sendTelegramSnapshotThenClip delivers the snapshot (or a plain text message if
// no snapshot is available) right away, then downloads the event clip and, if it
// is within the configured size limit, replaces the snapshot in-place with the
// video via editMessageMedia. The fast snapshot is always kept on any clip
// failure, so the user never ends up with no notification.
func sendTelegramSnapshotThenClip(bot *tgbotapi.BotAPI, profile models.Telegram, status *models.NotifierStatus, event models.Event, snapshot io.Reader, message string, provider notifMeta) {
	// 1. Send snapshot immediately (fall back to text when none is available)
	var sent tgbotapi.Message
	var err error
	if event.HasSnapshot {
		msg := tgbotapi.NewPhoto(profile.ChatID, tgbotapi.FileReader{Name: "Snapshot", Reader: snapshot})
		if profile.MessageThreadID != 0 {
			msg.MessageThreadID = profile.MessageThreadID
		}
		msg.Caption = message
		msg.ParseMode = "HTML"
		sent, err = bot.Send(msg)
	} else {
		msg := tgbotapi.NewMessage(profile.ChatID, message)
		if profile.MessageThreadID != 0 {
			msg.MessageThreadID = profile.MessageThreadID
		}
		msg.ParseMode = "HTML"
		sent, err = bot.Send(msg)
	}
	if err != nil {
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Unable to send alert")
		status.NotifFailure(err.Error())
		return
	}
	log.Info().
		Str("event_id", event.ID).
		Str("provider", "Telegram").
		Int("provider_id", provider.index).
		Msg("Alert sent")
	status.NotifSuccess()

	// Nothing to upgrade to if there is no clip for this event
	if !event.HasClip {
		return
	}

	// 2. Download the clip (may block while Frigate finishes processing it)
	clip := GetClip(event)
	if clip == nil {
		log.Info().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Msg("Clip unavailable, keeping snapshot")
		return
	}
	clipBytes, err := io.ReadAll(clip)
	if err != nil || len(clipBytes) == 0 {
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Could not read clip, keeping snapshot")
		return
	}

	// 3. Enforce the upload size limit
	maxBytes := telegramClipMaxBytes(profile)
	if int64(len(clipBytes)) > maxBytes {
		log.Info().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Int("clip_bytes", len(clipBytes)).
			Int64("max_bytes", maxBytes).
			Msg("Clip exceeds size limit, keeping snapshot")
		return
	}

	// 4a. Without a snapshot there is no media message to edit, so send the clip
	// as a new message instead
	if !event.HasSnapshot {
		msg := tgbotapi.NewVideo(profile.ChatID, tgbotapi.FileReader{Name: "Clip", Reader: bytes.NewReader(clipBytes)})
		if profile.MessageThreadID != 0 {
			msg.MessageThreadID = profile.MessageThreadID
		}
		msg.Caption = message
		msg.ParseMode = "HTML"
		msg.SupportsStreaming = true
		if _, err := bot.Send(msg); err != nil {
			log.Warn().
				Str("event_id", event.ID).
				Str("provider", "Telegram").
				Int("provider_id", provider.index).
				Err(err).
				Msg("Unable to send clip")
			return
		}
		log.Info().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Msg("Clip sent")
		return
	}

	// 4b. Replace the snapshot photo with the video clip in-place
	media := tgbotapi.NewInputMediaVideo(tgbotapi.FileReader{Name: "Clip", Reader: bytes.NewReader(clipBytes)})
	media.Caption = message
	media.ParseMode = "HTML"
	media.SupportsStreaming = true
	edit := tgbotapi.NewEditMessageMedia(profile.ChatID, sent.MessageID, &media)
	if _, err := bot.Send(edit); err != nil {
		// Snapshot was already delivered, so this is not a full notification
		// failure - just log and keep the snapshot
		log.Warn().
			Str("event_id", event.ID).
			Str("provider", "Telegram").
			Int("provider_id", provider.index).
			Err(err).
			Msg("Unable to replace snapshot with clip, keeping snapshot")
		return
	}
	log.Info().
		Str("event_id", event.ID).
		Str("provider", "Telegram").
		Int("provider_id", provider.index).
		Msg("Snapshot replaced with clip")
}
