package events

import (
	"slices"
	"strings"
	"time"

	"github.com/0x2142/frigate-notify/models"
	"github.com/0x2142/frigate-notify/notifier"
	"github.com/rs/zerolog/log"

	"github.com/maypok86/otter"
)

var zoneCache otter.Cache[string, []string]
var notifCache otter.Cache[string, string]

func InitZoneCache() {
	var err error
	log.Debug().Msg("Setting up zone cache...")
	zoneCache, err = otter.MustBuilder[string, []string](500).WithTTL(1 * time.Hour).Build()
	if err != nil {
		log.Warn().
			Err(err).
			Msg("Error setting up zone cache")
	}
	log.Debug().Msg("Zone cache ready")

	log.Debug().Msg("Setting up notification cache...")
	notifCache, err = otter.MustBuilder[string, string](1000).WithTTL(1 * time.Hour).Build()
	if err != nil {
		log.Warn().
			Err(err).
			Msg("Error setting up notification cache")
	}
	log.Debug().Msg("Notification cache ready")

	// Wire up cache functions for notifier package to avoid circular imports
	notifier.NotifCacheSet = SetNotifSent
	notifier.NotifCacheGet = GetNotifMessageID
}

func CloseZoneCache() {
	log.Debug().Msg("Cache tear down")
	zoneCache.Close()
	notifCache.Close()
}

// SetNotifSent stores the message ID for a notification sent for a review/provider combination
func SetNotifSent(reviewID, provider string, messageID string) {
	key := reviewID + ":" + provider
	notifCache.Set(key, messageID)
	log.Trace().
		Str("review_id", reviewID).
		Str("provider", provider).
		Str("message_id", messageID).
		Msg("Stored notification message ID in cache")
}

// GetNotifMessageID retrieves the message ID for a previously sent notification
func GetNotifMessageID(reviewID, provider string) string {
	key := reviewID + ":" + provider
	messageID, ok := notifCache.Get(key)
	if !ok {
		return ""
	}
	return messageID
}

// Add zone to list of zones that have already generated notifications for specified event ID
func setZoneAlerted(event models.Event) {
	// Get current list of zones by event ID, if it exists
	alreadyAlerted, _ := zoneCache.Get(event.ID)
	log.Trace().
		Strs("cache", alreadyAlerted).
		Str("event_id", event.ID).
		Msg("Current cache contents")
	alreadyAlerted = append(alreadyAlerted, event.CurrentZones...)
	// Remove duplicates
	slices.Sort(alreadyAlerted)
	alreadyAlerted = slices.Compact(alreadyAlerted)
	// Update cache with new list
	zoneCache.Set(event.ID, alreadyAlerted)
	log.Trace().
		Strs("cache", alreadyAlerted).
		Str("event_id", event.ID).
		Msg("New cache contents")
}

// Query cache by event ID
func getCachebyID(id string) []string {
	cacheData, ok := zoneCache.Get(id)
	log.Trace().
		Bool("in_cache", ok).
		Strs("cache", cacheData).
		Str("event_id", id).
		Msgf("Get event from cache")
	if !ok {
		return nil
	}
	return cacheData
}

// Query cache to see if zone already generated alert
func zoneAlreadyAlerted(event models.Event) bool {
	// Check if event already in cache & if so, get contents
	alreadyAlerted, ok := zoneCache.Get(event.ID)
	log.Trace().
		Bool("in_cache", ok).
		Strs("cache", alreadyAlerted).
		Str("event_id", event.ID).
		Msgf("Get event from cache")
	// If event not found, create cache entry & add zones
	if !ok {
		log.Debug().
			Str("event_id", event.ID).
			Str("camera", event.Camera).
			Str("zones", strings.Join(event.CurrentZones, ",")).
			Msg("Event not in cache, adding...")
		setZoneAlerted(event)
		return false
	}
	// If event found, check to see if there are any new zones to notify on
	for _, zone := range event.CurrentZones {
		if !slices.Contains(alreadyAlerted, zone) {
			log.Debug().
				Str("event_id", event.ID).
				Str("camera", event.Camera).
				Str("zones", strings.Join(event.CurrentZones, ",")).
				Msg("Found new zone not in cache")
			setZoneAlerted(event)
			return false
		}
	}
	// If no new zones, then assume all have been notified already
	log.Debug().
		Str("event_id", event.ID).
		Str("camera", event.Camera).
		Str("zones", strings.Join(event.CurrentZones, ",")).
		Msg("All zones in event have already notified")
	return true
}

// Remove zone alert cache for event ID
func delZoneAlerted(event models.Event) {
	zoneCache.Delete(event.ID)
	log.Debug().
		Str("event_id", event.ID).
		Str("camera", event.Camera).
		Str("zones", strings.Join(event.CurrentZones, ",")).
		Msg("Event removed from cache")
}
