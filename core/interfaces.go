package core

import (
	"context"
	"fmt"
)

// Platform is the lifecycle boundary for an IM connection. Only Feishu is shipped.
type Platform interface {
	Name() string
	Start(MessageHandler) error
	Stop() error
}
type MessageHandler func(Platform, *Message)

// Agent controls existing conversations; it does not own the Desktop process or settings.
type Agent interface {
	Name() string
	ListSessions(context.Context) ([]AgentSessionInfo, error)
	Stop() error
	BoundConversationAgent
}
type BoundConversationAgent interface {
	HandleConversation(context.Context, Platform, *Message) error
	ResumeConversations(context.Context, []Platform) error
	ConversationState() any
	BindConversation(context.Context, string, string) error
}
type LocalPlatformSetup interface {
	State() any
	Action(string) (any, error)
	Close() error
}

// ConversationOrigin retains the verified sender and original reply destination.
type ConversationOrigin struct {
	AppID     string `json:"app_id"`
	Tenant    string `json:"tenant"`
	ChatID    string `json:"chat_id"`
	ChatType  string `json:"chat_type"`
	Actor     string `json:"actor"`
	MessageID string `json:"message_id"`
	ParentID  string `json:"parent_id,omitempty"`
	RootID    string `json:"root_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}
type DurableConversationPlatform interface {
	Platform
	AcceptConversationOrigin(ConversationOrigin) bool
	ReplyConversation(context.Context, ConversationOrigin, string, string) (string, error)
}
type DurableIngressPlatform interface {
	SetConversationHandler(func(context.Context, Platform, *Message) error)
}
type DeliveryError struct{ Code int }

func (e *DeliveryError) Error() string { return fmt.Sprintf("platform delivery code %d", e.Code) }

// ConversationReactor acknowledges a persisted inbound message without a text reply.
type ConversationReactor interface {
	ReactConversation(context.Context, ConversationOrigin, string) (string, error)
}

// Official resource I/O stays in the IM adapter; persistent state stays in the agent.
type ConversationMediaPlatform interface {
	ResolveConversationMedia(context.Context, ConversationOrigin) ([]ConversationMedia, error)
	DownloadConversationMedia(context.Context, ConversationOrigin, ConversationMedia) ([]byte, error)
	UploadConversationMedia(context.Context, ConversationOrigin, ConversationMedia) (string, error)
	ReplyConversationMedia(context.Context, ConversationOrigin, string, ConversationMedia) (string, error)
}
