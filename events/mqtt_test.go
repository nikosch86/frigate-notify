package events

import (
	"testing"

	"github.com/0x2142/frigate-notify/models"
)

// fakeMQTTMessage is a minimal mqtt.Message carrying only a topic and payload.
type fakeMQTTMessage struct {
	topic   string
	payload string
}

func (m fakeMQTTMessage) Duplicate() bool   { return false }
func (m fakeMQTTMessage) Qos() byte         { return 0 }
func (m fakeMQTTMessage) Retained() bool    { return false }
func (m fakeMQTTMessage) Topic() string     { return m.topic }
func (m fakeMQTTMessage) MessageID() uint16 { return 0 }
func (m fakeMQTTMessage) Payload() []byte   { return []byte(m.payload) }
func (m fakeMQTTMessage) Ack()              {}

// TestReviewEndKeepsZoneAlerts checks that a tracked object alerts once per zone
// for its lifetime: a review ending must not let the same object alert again on
// that zone in a later review.
func TestReviewEndKeepsZoneAlerts(t *testing.T) {
	InitZoneCache()
	defer CloseZoneCache()
	detection := models.Event{ID: "det-1", Camera: "back_gate", CurrentZones: []string{"in"}}
	setZoneAlerted(detection)

	handleMQTTMsg(nil, fakeMQTTMessage{
		topic:   "frigate/reviews",
		payload: `{"type":"end","after":{"id":"rev-1","camera":"back_gate","data":{"detections":["det-1"],"zones":["in"]}}}`,
	})

	if !zoneAlreadyAlerted(detection) {
		t.Error("object should still count as alerted on its zone after the review ended")
	}
}
