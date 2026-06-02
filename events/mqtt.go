package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var mqtt_topic string
var mqtt_tracked_object_topic string
var client mqtt.Client

// SubscribeMQTT establishes subscription to MQTT server & listens for messages
func SubscribeMQTT() {
	config.Internal.Status.Health = "frigate mqtt connecting"
	config.Internal.Status.Frigate.MQTT = "connecting"
	mqtt_topic = fmt.Sprintf("%s/%s", config.ConfigData.Frigate.MQTT.TopicPrefix, strings.ToLower(config.ConfigData.App.Mode))
	mqtt_tracked_object_topic = fmt.Sprintf("%s/tracked_object_update", config.ConfigData.Frigate.MQTT.TopicPrefix)
	// MQTT client configuration
	mqttServer := fmt.Sprintf("tcp://%s:%d", config.ConfigData.Frigate.MQTT.Server, config.ConfigData.Frigate.MQTT.Port)
	opts := mqtt.NewClientOptions()
	opts.AddBroker(mqttServer)
	opts.SetClientID(config.ConfigData.Frigate.MQTT.ClientID)
	opts.SetAutoReconnect(true)
	// Run each message handler in its own goroutine (paho default is true, which
	// serializes all handlers on a single router goroutine). Our handlers make
	// synchronous HTTP calls to Frigate (detail fetches, snapshots, rechecks); a
	// cold or slow Frigate would otherwise block the router and freeze the entire
	// MQTT pump - and starve keepalive pings, tripping a disconnect loop.
	opts.SetOrderMatters(false)
	opts.SetConnectionLostHandler(connectionLostHandler)
	opts.SetOnConnectHandler(connectHandler)
	if config.ConfigData.Frigate.MQTT.Username != "" && config.ConfigData.Frigate.MQTT.Password != "" {
		opts.SetUsername(config.ConfigData.Frigate.MQTT.Username)
		opts.SetPassword(config.ConfigData.Frigate.MQTT.Password)
	}

	log.Trace().
		Str("server", mqttServer).
		Str("client_id", config.ConfigData.Frigate.MQTT.ClientID).
		Str("username", config.ConfigData.Frigate.MQTT.Username).
		Str("password", "--secret removed--").
		Str("topic", mqtt_topic).
		Bool("auto_reconnect", true).
		Msg("Init MQTT connection")

	// Connect to MQTT broker, retrying indefinitely with capped backoff.
	// A broker that is slow to appear on host boot (e.g. dependency ordering
	// after a reboot) must not kill the process - once connected, AutoReconnect
	// handles any later disconnects.
	backoff := 10 * time.Second
	const maxBackoff = 60 * time.Second
	for {
		config.Internal.Status.Health = "frigate mqtt unable to connect"
		config.Internal.Status.Frigate.MQTT = "unreachable"

		// Connect to MQTT broker
		client = mqtt.NewClient(opts)

		if token := client.Connect(); token.Wait() && token.Error() != nil {
			log.Warn().Msgf("Could not connect to MQTT at %v: %v", config.ConfigData.Frigate.MQTT.Server, token.Error())
			log.Warn().Msgf("Retrying in %v.", backoff)
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		return
	}
}

// disconnectMQTT simply disconnects the MQTT client
func DisconnectMQTT() {
	log.Info().Msg("Ending MQTT session")
	client.Disconnect(300)
	log.Info().Msg("MQTT disconnected")
}

// connectionLostHandler logs error message on MQTT connection loss
func connectionLostHandler(c mqtt.Client, err error) {
	config.Internal.Status.Health = "frigate mqtt connection lost"
	config.Internal.Status.Frigate.MQTT = "unreachable"
	log.Error().
		Err(err).
		Msg("Lost connection to MQTT broker")
}

// connectHandler logs message on MQTT connection
func connectHandler(client mqtt.Client) {
	log.Info().Msg("Connected to MQTT.")
	config.Internal.Status.Health = "ok"
	config.Internal.Status.Frigate.MQTT = "ok"
	if subscription := client.Subscribe(mqtt_topic, 0, handleMQTTMsg); subscription.Wait() && subscription.Error() != nil {
		config.Internal.Status.Health = "frigate mqtt unable to subscribe"
		config.Internal.Status.Frigate.MQTT = "unreachable"
		log.Error().Msgf("Failed to subscribe to topic: %s", mqtt_topic)
		time.Sleep(10 * time.Second)
	}
	log.Info().Msgf("Subscribed to MQTT topic: %s", mqtt_topic)

	// Subscribe to tracked_object_update for GenAI object descriptions
	if subscription := client.Subscribe(mqtt_tracked_object_topic, 0, handleMQTTMsg); subscription.Wait() && subscription.Error() != nil {
		log.Warn().Msgf("Failed to subscribe to topic: %s", mqtt_tracked_object_topic)
	} else {
		log.Info().Msgf("Subscribed to MQTT topic: %s", mqtt_tracked_object_topic)
	}
}

// handleMQTTMsg processes incoming MQTT messages depending on topic
func handleMQTTMsg(client mqtt.Client, msg mqtt.Message) {
	components := strings.Split(msg.Topic(), "/")
	topic := components[len(components)-1]

	log.Trace().
		RawJSON("payload", msg.Payload()).
		Msg("New MQTT message received")

	switch topic {
	case "reviews":
		var review models.MQTTReview
		json.Unmarshal(msg.Payload(), &review)

		switch review.Type {
		case "new":
			log.Debug().
				Str("review_id", review.After.ID).
				Msg("New review received")
			processReview(review.After.Review)
		case "update":
			log.Debug().
				Str("review_id", review.After.ID).
				Msg("Review update received")
			processReview(review.After.Review)
		case "genai":
			log.Debug().
				Str("review_id", review.After.ID).
				Msg("GenAI review update received")
			processGenAIReviewUpdate(review.After.Review)
		case "end":
			log.Debug().
				Str("review_id", review.After.ID).
				Msg("Review ended")
			for _, detection := range review.After.Data.Detections {
				delZoneAlerted(models.Event{
					ID:           detection,
					Camera:       review.After.Camera,
					CurrentZones: review.After.Data.Zones,
				})
			}
		}
	case "events":
		var event models.MQTTEvent
		json.Unmarshal(msg.Payload(), &event)

		switch event.Type {
		case "new":
			log.Info().
				Str("event_id", event.After.ID).
				Msg("New event received")
			processEvent(event.After.Event)
		case "update":
			log.Info().
				Str("event_id", event.After.ID).
				Msg("Event update received")
			processEvent(event.After.Event)
		case "end":
			log.Debug().
				Str("event_id", event.After.ID).
				Msg("Event ended")
			delZoneAlerted(event.After.Event)
		}

	case "tracked_object_update":
		var update models.MQTTTrackedObjectUpdate
		json.Unmarshal(msg.Payload(), &update)

		if update.Type == "description" {
			log.Debug().
				Str("event_id", update.ID).
				Str("camera", update.Camera).
				Msg("Tracked object description update received")
		}
	}
}
