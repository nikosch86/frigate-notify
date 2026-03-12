package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	"github.com/0x2142/frigate-notify/notifier"
	"github.com/0x2142/frigate-notify/util"
	"github.com/rs/zerolog/log"
)

// processReview handles querying detections under a review & preparing for sending an alert
func processReview(review models.Review) {
	if config.ConfigData.Alerts.General.RecheckDelay != 0 {
		review = recheckReview(review)
	}

	config.Internal.Status.LastEvent = time.Now()

	// Convert to human-readable timestamp
	reviewTime := time.Unix(int64(review.StartTime), 0)
	log.Info().
		Str("review_id", review.ID).
		Str("camera", review.Camera).
		Int("num_detections", len(review.Data.Detections)).
		Str("objects", strings.Join(review.Data.Objects, ",")).
		Str("audio", strings.Join(review.Data.Audio, ",")).
		Str("zones", strings.Join(review.Data.Zones, ",")).
		Str("severity", review.Severity).
		Msg("Processing review...")
	log.Debug().
		Str("review_id", review.ID).
		Msgf("Review start time: %s", reviewTime)

	if !config.ConfigData.Alerts.General.NotifyDetections && review.Severity == "detection" {
		log.Info().
			Str("review_id", review.ID).
			Msg("Review dropped - Event is detection only, not alert")
		return
	}

	// Check if audio-only event
	if len(review.Data.Detections) == 0 && len(review.Data.Audio) != 0 {
		if config.ConfigData.Alerts.General.AudioOnly == "allow" {
			// Assemble some info via Review item, since there is no detection event to look up
			var audioEvent models.Event
			audioEvent.StartTime = review.StartTime
			audioEvent.Extra.Audio = strings.Join(review.Data.Audio, ",")
			audioEvent.Camera = review.Camera
			audioEvent.Extra.ReviewLink = config.ConfigData.Frigate.PublicURL + "/review?id=" + review.ID
			notifier.SendAlert([]models.Event{audioEvent})
			return
		} else {
			log.Info().
				Str("review_id", review.ID).
				Msg("Review dropped - Audio only event")
			return
		}
	}

	// Retrieve detailed detection information
	reviewFiltered := false
	var detections []models.Event
	for _, id := range review.Data.Detections {
		url := fmt.Sprintf("%s/api/events/%s", config.ConfigData.Frigate.Server, id)

		response, err := util.HTTPGet(url, config.ConfigData.Frigate.Insecure, "")
		if err != nil {
			config.Internal.Status.Frigate.API = "unreachable"
			log.Error().
				Err(err).
				Str("review_id", review.ID).
				Str("detection_id", id).
				Msgf("Unable to retrieve detection information")
			continue
		}
		config.Internal.Status.Frigate.API = "ok"

		var detection models.Event
		json.Unmarshal(response, &detection)

		// For events collected via API, top-level top_score value is no longer used
		// So need to replace it with data.top_score value
		if detection.TopScore == 0 {
			detection.TopScore = detection.Data.TopScore
		}

		// Wait for license plate data before notifying, if set
		if config.ConfigData.Alerts.LicensePlate.Enabled {
			waitforLPR(&detection)
		}

		// Check that event passes configured filters
		detection.CurrentZones = detection.Zones
		if !checkEventFilters(detection) {
			reviewFiltered = true
			break
		}

		// Add special link to review page
		detection.Extra.ReviewLink = config.ConfigData.Frigate.PublicURL + "/review?id=" + review.ID

		detections = append(detections, detection)
	}

	// Check to make sure at least 1 detection passed filters
	if len(detections) == 0 {
		log.Info().
			Str("review_id", review.ID).
			Msgf("Review dropped - No events eligible for notification")
		return
	}

	// If any detection would be filtered, skip notifying on this review
	if reviewFiltered {
		log.Info().
			Str("review_id", review.ID).
			Msgf("Review dropped - One or more detections are filtered")
		return
	}

	// Populate GenAI fields from review metadata if available and enabled
	if config.ConfigData.Alerts.General.GenAI.Enabled && review.Data.Metadata != nil {
		meta := review.Data.Metadata
		detections[0].Extra.GenAITitle = meta.Title
		detections[0].Extra.GenAISummary = meta.ShortSummary
		detections[0].Extra.GenAIScene = meta.Scene

		// Convert threat level int to human-readable string
		switch meta.PotentialThreatLevel {
		case 0:
			detections[0].Extra.GenAIThreatLevel = "Normal"
		case 1:
			detections[0].Extra.GenAIThreatLevel = "Minor"
		case 2:
			detections[0].Extra.GenAIThreatLevel = "Moderate"
		case 3:
			detections[0].Extra.GenAIThreatLevel = "High"
		}

		if len(meta.OtherConcerns) > 0 {
			detections[0].Extra.GenAIConcerns = strings.Join(meta.OtherConcerns, ", ")
		}

		if meta.Confidence > 0 {
			detections[0].Extra.GenAIConfidence = fmt.Sprintf("%v%%", int(meta.Confidence*100))
		}

		log.Debug().
			Str("review_id", review.ID).
			Str("genai_title", meta.Title).
			Str("genai_threat_level", detections[0].Extra.GenAIThreatLevel).
			Msg("GenAI metadata applied to notification")
	}

	// Send alert with snapshot
	notifier.SendAlert(detections)
}

// processGenAIReviewUpdate handles GenAI metadata updates for a review.
// This has its own flow separate from processReview because GenAI updates
// arrive after the initial notification was already sent, so we must bypass
// zone cache and other filters that would drop the event as "already notified".
func processGenAIReviewUpdate(review models.Review) {
	if !config.ConfigData.Alerts.General.GenAI.Enabled {
		log.Debug().
			Str("review_id", review.ID).
			Msg("GenAI update ignored - GenAI is disabled")
		return
	}

	if !config.ConfigData.Alerts.General.GenAI.UpdateNotif {
		log.Debug().
			Str("review_id", review.ID).
			Msg("GenAI update ignored - update_notif is disabled")
		return
	}

	if review.Data.Metadata == nil {
		log.Debug().
			Str("review_id", review.ID).
			Msg("GenAI update ignored - no metadata present")
		return
	}

	log.Debug().
		Str("review_id", review.ID).
		Str("genai_title", review.Data.Metadata.Title).
		Str("genai_summary", review.Data.Metadata.ShortSummary).
		Int("genai_threat_level", review.Data.Metadata.PotentialThreatLevel).
		Msg("Processing GenAI review update")

	// Skip audio-only events with no detections
	if len(review.Data.Detections) == 0 {
		log.Debug().
			Str("review_id", review.ID).
			Msg("GenAI update ignored - no detections to enrich")
		return
	}

	// Retrieve detection details from Frigate API (bypass all filters/zone cache)
	var detections []models.Event
	for _, id := range review.Data.Detections {
		url := fmt.Sprintf("%s/api/events/%s", config.ConfigData.Frigate.Server, id)

		response, err := util.HTTPGet(url, config.ConfigData.Frigate.Insecure, "")
		if err != nil {
			log.Error().
				Err(err).
				Str("review_id", review.ID).
				Str("detection_id", id).
				Msg("GenAI update - Unable to retrieve detection information")
			continue
		}

		var detection models.Event
		json.Unmarshal(response, &detection)

		if detection.TopScore == 0 {
			detection.TopScore = detection.Data.TopScore
		}
		detection.CurrentZones = detection.Zones
		detection.Extra.ReviewLink = config.ConfigData.Frigate.PublicURL + "/review?id=" + review.ID

		detections = append(detections, detection)
	}

	if len(detections) == 0 {
		log.Debug().
			Str("review_id", review.ID).
			Msg("GenAI update dropped - No detections found")
		return
	}

	// Populate GenAI fields from review metadata
	meta := review.Data.Metadata
	detections[0].Extra.GenAITitle = meta.Title
	detections[0].Extra.GenAISummary = meta.ShortSummary
	detections[0].Extra.GenAIScene = meta.Scene

	switch meta.PotentialThreatLevel {
	case 0:
		detections[0].Extra.GenAIThreatLevel = "Normal"
	case 1:
		detections[0].Extra.GenAIThreatLevel = "Minor"
	case 2:
		detections[0].Extra.GenAIThreatLevel = "Moderate"
	case 3:
		detections[0].Extra.GenAIThreatLevel = "High"
	}

	if len(meta.OtherConcerns) > 0 {
		detections[0].Extra.GenAIConcerns = strings.Join(meta.OtherConcerns, ", ")
	}

	if meta.Confidence > 0 {
		detections[0].Extra.GenAIConfidence = fmt.Sprintf("%v%%", int(meta.Confidence*100))
	}

	log.Debug().
		Str("review_id", review.ID).
		Str("genai_title", meta.Title).
		Str("genai_threat_level", detections[0].Extra.GenAIThreatLevel).
		Msg("Sending GenAI-enriched notification")

	// Send notification directly, bypassing zone cache and filters
	notifier.SendAlert(detections)
}

func recheckReview(review models.Review) models.Review {
	delay := config.ConfigData.Alerts.General.RecheckDelay
	log.Debug().
		Str("review_id", review.ID).
		Int("recheck_delay", delay).
		Msg("Waiting to re-check review details")
	time.Sleep(time.Duration(delay) * time.Second)
	log.Debug().
		Str("review_id", review.ID).
		Int("recheck_delay", delay).
		Msg("Re-checking review details")

	url := config.ConfigData.Frigate.Server + "/api/review/" + review.ID
	response, err := util.HTTPGet(url, config.ConfigData.Frigate.Insecure, "", config.ConfigData.Frigate.Headers...)
	if err != nil {
		config.Internal.Status.Health = "frigate webapi unreachable"
		config.Internal.Status.Frigate.API = "unreachable"
		log.Error().
			Err(err).
			Msgf("Cannot get event from %s", url)
		return review
	}
	config.Internal.Status.Health = "ok"
	config.Internal.Status.Frigate.API = "ok"

	json.Unmarshal([]byte(response), &review)
	return review
}
