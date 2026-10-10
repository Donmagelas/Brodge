package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/chenhg5/cc-connect/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func explicitDesktopScope(scope string) bool {
	for _, id := range strings.Split(scope, ",") {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(id) == "*" {
			return false
		}
	}
	return true
}

func (p *Platform) onDesktopMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil || event.EventV2Base.Header.AppID != p.appID || event.EventV2Base.Header.TenantKey != p.desktopTenant || event.Event == nil || event.Event.Sender == nil || event.Event.Message == nil {
		return nil
	}
	s, m := event.Event.Sender, event.Event.Message
	if stringValue(s.SenderType) != "user" || s.SenderId == nil || stringValue(s.TenantKey) != p.desktopTenant {
		return nil
	}
	actor, chat, kind := stringValue(s.SenderId.OpenId), stringValue(m.ChatId), stringValue(m.ChatType)
	if actor == "" || chat == "" || stringValue(m.MessageId) == "" || !core.AllowList(p.allowFrom, actor) {
		return nil
	}
	if kind != "p2p" && kind != "group" {
		return nil
	}
	if kind == "group" && (p.getBotOpenID() == "" || !isBotMentioned(m.Mentions, p.getBotOpenID())) {
		return nil
	}
	origin := core.ConversationOrigin{AppID: p.appID, Tenant: p.desktopTenant, ChatID: chat, ChatType: kind, Actor: actor,
		MessageID: stringValue(m.MessageId), ParentID: stringValue(m.ParentId), RootID: stringValue(m.RootId), ThreadID: stringValue(m.ThreadId)}
	key := strings.Join([]string{p.platformName, p.appID, p.desktopTenant, chat, actor}, ":")
	msg := &core.Message{Platform: p.platformName, SessionKey: key, MessageID: origin.MessageID, UserID: actor, ChannelID: chat,
		ReplyCtx: replyContext{messageID: origin.MessageID, chatID: chat, sessionKey: key}, ConversationOrigin: &origin}
	if stringValue(m.MessageType) == "text" {
		var body struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(stringValue(m.Content)), &body) != nil {
			return nil
		}
		msg.Content = strings.TrimSpace(stripMentions(body.Text, m.Mentions, p.getBotOpenID()))
	} else {
		var body map[string]any
		if json.Unmarshal([]byte(stringValue(m.Content)), &body) != nil {
			return nil
		}
		switch stringValue(m.MessageType) {
		case "image":
			key, _ := body["image_key"].(string)
			if key == "" {
				msg.Content = "/unsupported-attachment"
			} else {
				msg.Media = []core.ConversationMedia{{Kind: "image", Key: key, Name: "image.png"}}
			}
		case "file":
			key, _ := body["file_key"].(string)
			name, _ := body["file_name"].(string)
			if key == "" {
				msg.Content = "/unsupported-attachment"
			} else {
				msg.Media = []core.ConversationMedia{{Kind: "file", Key: key, Name: name}}
			}
		case "post":
			// Feishu sends one locale's post; tolerate the documented localized envelope.
			if _, ok := body["content"]; !ok {
				for _, locale := range []string{"zh_cn", "en_us"} {
					if v, ok := body[locale].(map[string]any); ok {
						body = v
						break
					}
				}
			}
			text, _ := body["title"].(string)
			lines, ok := body["content"].([]any)
			if !ok {
				msg.Content = "/unsupported-attachment"
				break
			}
			supported := true
			for _, line := range lines {
				elements, ok := line.([]any)
				if !ok {
					supported = false
					break
				}
				text += "\n"
				for _, v := range elements {
					e, ok := v.(map[string]any)
					if !ok {
						supported = false
						break
					}
					tag, _ := e["tag"].(string)
					switch tag {
					case "text", "a":
						t, _ := e["text"].(string)
						text += t
						if tag == "a" {
							href, _ := e["href"].(string)
							text += " (" + href + ")"
						}
					case "at":
						id, _ := e["user_id"].(string)
						if id != p.getBotOpenID() {
							supported = false
						}
					case "img":
						key, _ := e["image_key"].(string)
						if key == "" {
							supported = false
						} else {
							msg.Media = append(msg.Media, core.ConversationMedia{Kind: "image", Key: key, Name: fmt.Sprintf("image-%d.png", len(msg.Media)+1)})
						}
					default:
						supported = false
					}
				}
			}
			if supported {
				msg.Content = strings.TrimSpace(text)
			} else {
				msg.Media = nil
				msg.Content = "/unsupported-attachment"
			}
		default:
			msg.Content = "/unsupported-attachment"
		}
	}
	// Handler does only a durable ingress transaction; the worker executes later.
	// Do not use upstream memory dedup, asynchronous dispatch or timestamp dropping.
	if p.conversationHandler == nil {
		return fmt.Errorf("durable ingress unavailable")
	}
	return p.conversationHandler(ctx, p, msg)
}

func (p *Platform) SetConversationHandler(handler func(context.Context, core.Platform, *core.Message) error) {
	p.conversationHandler = handler
}

func (p *Platform) ReplyConversation(ctx context.Context, origin core.ConversationOrigin, uuid, content string) (string, error) {
	if !p.AcceptConversationOrigin(origin) {
		return "", fmt.Errorf("feishu: invalid reply origin")
	}
	kind := "text"
	body := marshalBody(map[string]string{"text": content})
	if strings.ContainsAny(content, "\n`[]*#|") {
		kind = "interactive"
		body = marshalBody(map[string]any{"config": map[string]bool{"wide_screen_mode": true}, "elements": []any{map[string]any{"tag": "markdown", "content": content}}})
	}
	return p.replyBody(ctx, origin, uuid, kind, body)
}

func (p *Platform) AcceptConversationOrigin(origin core.ConversationOrigin) bool {
	return p.desktopBound && origin.AppID == p.appID && origin.Tenant == p.desktopTenant && origin.MessageID != "" &&
		(origin.ChatType == "p2p" || origin.ChatType == "group") && core.AllowList(p.allowFrom, origin.Actor) && origin.ChatID != ""
}

func (p *Platform) ReactConversation(ctx context.Context, origin core.ConversationOrigin, emoji string) (string, error) {
	if !p.AcceptConversationOrigin(origin) {
		return "", fmt.Errorf("feishu: invalid reaction origin")
	}
	req := larkim.NewCreateMessageReactionReqBuilder().MessageId(origin.MessageID).Body(larkim.NewCreateMessageReactionReqBodyBuilder().ReactionType(larkim.NewEmojiBuilder().EmojiType(emoji).Build()).Build()).Build()
	resp, err := p.client.Im.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: reaction outcome unknown")
	}
	if !resp.Success() {
		return "", &core.DeliveryError{Code: resp.Code}
	}
	if resp.Data == nil || resp.Data.ReactionId == nil {
		return "", fmt.Errorf("feishu: reaction receipt unknown")
	}
	return *resp.Data.ReactionId, nil
}

// The paired identity comes from the verified application creator's private chat.
func (p *Platform) SetCreator(origin core.ConversationOrigin) error {
	if origin.AppID != p.appID || origin.Tenant != p.desktopTenant || origin.Actor == "" || origin.ChatType != "p2p" {
		return fmt.Errorf("creator pairing mismatch")
	}
	p.allowFrom = origin.Actor
	return nil
}

func (p *Platform) DownloadConversationMedia(ctx context.Context, origin core.ConversationOrigin, media core.ConversationMedia) ([]byte, error) {
	if !p.AcceptConversationOrigin(origin) || media.Key == "" || (media.Kind != "image" && media.Kind != "file") {
		return nil, fmt.Errorf("invalid-resource-origin")
	}
	resp, err := p.client.Im.MessageResource.Get(ctx, larkim.NewGetMessageResourceReqBuilder().MessageId(origin.MessageID).FileKey(media.Key).Type(media.Kind).Build())
	if err != nil || resp == nil {
		return nil, fmt.Errorf("resource-download-unavailable")
	}
	if !resp.Success() || resp.File == nil {
		return nil, fmt.Errorf("resource-download-rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.File, (30<<20)+1))
	if err != nil || len(data) == 0 || len(data) > 30<<20 {
		return nil, fmt.Errorf("empty-or-oversized-resource")
	}
	return data, nil
}
func (p *Platform) UploadConversationMedia(ctx context.Context, origin core.ConversationOrigin, media core.ConversationMedia) (string, error) {
	if !p.AcceptConversationOrigin(origin) {
		return "", fmt.Errorf("invalid-upload-origin")
	}
	f, err := os.OpenFile(media.Path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("snapshot-unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 30<<20 {
		return "", fmt.Errorf("snapshot-unavailable")
	}
	if media.Kind == "image" {
		resp, err := p.client.Im.Image.Create(ctx, larkim.NewCreateImageReqBuilder().Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(f).Build()).Build())
		if err == nil && resp != nil && !resp.Success() {
			return "", &core.DeliveryError{Code: resp.Code}
		}
		if err != nil || resp == nil || !resp.Success() || resp.Data == nil || resp.Data.ImageKey == nil {
			return "", fmt.Errorf("image-upload-failed")
		}
		return *resp.Data.ImageKey, nil
	}
	if media.Kind != "file" {
		return "", fmt.Errorf("unsupported-upload-type")
	}
	resp, err := p.client.Im.File.Create(ctx, larkim.NewCreateFileReqBuilder().Body(larkim.NewCreateFileReqBodyBuilder().FileType("stream").FileName(media.Name).File(f).Build()).Build())
	if err == nil && resp != nil && !resp.Success() {
		return "", &core.DeliveryError{Code: resp.Code}
	}
	if err != nil || resp == nil || !resp.Success() || resp.Data == nil || resp.Data.FileKey == nil {
		return "", fmt.Errorf("file-upload-failed")
	}
	return *resp.Data.FileKey, nil
}
func (p *Platform) ReplyConversationMedia(ctx context.Context, origin core.ConversationOrigin, uuid string, media core.ConversationMedia) (string, error) {
	if !p.AcceptConversationOrigin(origin) || media.Key == "" {
		return "", fmt.Errorf("invalid-media-reply")
	}
	key := "file_key"
	if media.Kind == "image" {
		key = "image_key"
	} else if media.Kind != "file" {
		return "", fmt.Errorf("unsupported-media-type")
	}
	return p.replyBody(ctx, origin, uuid, media.Kind, marshalBody(map[string]string{key: media.Key}))
}
func marshalBody(v any) string { b, _ := json.Marshal(v); return string(b) }
func (p *Platform) replyBody(ctx context.Context, origin core.ConversationOrigin, uuid, kind, body string) (string, error) {
	req := larkim.NewReplyMessageReqBuilder().MessageId(origin.MessageID).Body(larkim.NewReplyMessageReqBodyBuilder().MsgType(kind).Content(body).Uuid(uuid).ReplyInThread(origin.ThreadID != "").Build()).Build()
	resp, err := p.client.Im.Message.Reply(ctx, req)
	if err != nil {
		return "", fmt.Errorf("feishu: reply outcome unknown")
	}
	if !resp.Success() {
		return "", &core.DeliveryError{Code: resp.Code}
	}
	if resp.Data == nil || resp.Data.MessageId == nil {
		return "", fmt.Errorf("feishu: reply receipt unknown")
	}
	return *resp.Data.MessageId, nil
}
