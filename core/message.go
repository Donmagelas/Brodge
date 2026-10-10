package core

import "strings"

type Message struct {
	ConversationOrigin                                          *ConversationOrigin
	Platform, SessionKey, MessageID, UserID, ChannelID, Content string
	ReplyCtx                                                    any
	Recalled, IsPermissionResponse                              bool
	Media                                                       []ConversationMedia
	Images                                                      []ImageAttachment
	Files                                                       []FileAttachment
	Audio                                                       *AudioAttachment
	ModeOverride                                                string
}

// Unsupported inputs remain explicit at ingress so they never become model text.
type ImageAttachment struct {
	MimeType string
	Data     []byte
	FileName string
}
type FileAttachment struct {
	MimeType string
	Data     []byte
	FileName string
}
type AudioAttachment struct {
	MimeType string
	Data     []byte
}
type AgentSessionInfo struct{ ID, Summary string }

func AllowList(scope, id string) bool {
	if strings.TrimSpace(scope) == "" || id == "" {
		return false
	}
	for _, allowed := range strings.Split(scope, ",") {
		if strings.TrimSpace(allowed) == id {
			return true
		}
	}
	return false
}

// ConversationMedia is a resource of one verified message or an explicit final artifact.
type ConversationMedia struct {
	Kind   string `json:"kind"`
	Key    string `json:"key,omitempty"`
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// MessageID is set by the adapter after resolving an authenticated reply's
	// parent attachment. Empty means the resource belongs to the input message.
	MessageID string `json:"message_id,omitempty"`
}
