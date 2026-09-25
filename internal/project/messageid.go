package project

import (
	"crypto/sha1"
	"uuid"
)

// MessageKind distinguishes the ACP messages one model response produces.
type MessageKind string

// Message kinds.
const (
	KindMessage MessageKind = "message"
	KindThought MessageKind = "thought"
)

// messageNamespace is the UUIDv5 namespace of acp-unreal message ids.
var messageNamespace = uuid.MustParse("8f6d8e2e-3c1a-4c55-9a0e-5d2f7c1b9e41")

// MessageID is the ACP messageId of one message of a model response: a
// name-based (version 5, RFC 4122/9562) UUID over the session id, the
// response id and the kind. The live stream and session/load replay derive
// it from the same persisted response id, so both use the same id, and
// provider response ids are unique, so ids never repeat.
func MessageID(sessionID, responseID string, kind MessageKind) string {
	h := sha1.New()
	h.Write(messageNamespace[:])
	for _, field := range []string{sessionID, responseID, string(kind)} {
		h.Write([]byte(field))
		h.Write([]byte{0})
	}
	var id uuid.UUID
	copy(id[:], h.Sum(nil))
	id[6] = id[6]&0x0f | 0x50 // version 5
	id[8] = id[8]&0x3f | 0x80 // RFC 4122 variant
	return id.String()
}
