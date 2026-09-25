package project

import (
	"testing"
	"uuid"
)

func TestMessageIDIsDeterministicRFCUUID(t *testing.T) {
	a := MessageID("sess-1", "resp_1", KindMessage)
	if a != MessageID("sess-1", "resp_1", KindMessage) {
		t.Fatal("MessageID is not deterministic")
	}
	parsed, err := uuid.Parse(a)
	if err != nil {
		t.Fatalf("MessageID %q is not a UUID: %v", a, err)
	}
	if v := parsed[6] >> 4; v != 5 {
		t.Fatalf("version = %d, want 5", v)
	}
	if variant := parsed[8] >> 6; variant != 0b10 {
		t.Fatalf("variant bits = %b, want 10 (RFC 4122)", variant)
	}
	distinct := map[string]bool{a: true}
	for _, other := range []string{
		MessageID("sess-1", "resp_1", KindThought),
		MessageID("sess-2", "resp_1", KindMessage),
		MessageID("sess-1", "resp_2", KindMessage),
		MessageID("sess-1r", "esp_1", KindMessage), // no ambiguity at the field boundary
	} {
		if distinct[other] {
			t.Fatalf("collision: %s", other)
		}
		distinct[other] = true
	}
}
