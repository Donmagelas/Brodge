package core

import (
	"fmt"
	"strings"
	"sync"
)

// Language represents a supported language
type Language string

const (
	LangAuto               Language = "" // auto-detect from user messages
	LangEnglish            Language = "en"
	LangChinese            Language = "zh"
	LangTraditionalChinese Language = "zh-TW"
	LangJapanese           Language = "ja"
	LangSpanish            Language = "es"
)

// I18n provides internationalized messages.
//
// All exported methods are safe to call from multiple goroutines: Brodge
// fans out platform message handlers concurrently, all of which can call
// DetectAndSet (writes `detected`) and T / CurrentLang (read `lang`/`detected`)
// at the same time. Without the mutex `go test -race` flags real data races
// on the language fields.
type I18n struct {
	mu       sync.RWMutex
	lang     Language
	detected Language
}

func NewI18n(lang Language) *I18n {
	return &I18n{lang: lang}
}

func DetectLanguage(text string) Language {
	for _, r := range text {
		if isJapanese(r) {
			return LangJapanese
		}
	}
	for _, r := range text {
		if isChinese(r) {
			return LangChinese
		}
	}
	if isSpanishHint(text) {
		return LangSpanish
	}
	return LangEnglish
}

// NormalizeLanguageString parses a configuration string (e.g. from
// opts["language"] or [projects].language) into a Language constant. The
// accepted spellings mirror the ones documented in config.example.toml:
// "en"/"english", "zh"/"chinese", "zh-TW"/"zh_TW"/"zhtw", "ja"/"japanese",
// "es"/"spanish", and "" / "auto" for auto-detect. Unknown values return
// LangAuto so the engine falls back to detection rather than silently
// snapping to English — operators who set language = "klingon" should see
// "auto" behaviour, not a hard English default they didn't ask for.
//
// Issue #1655 introduced this helper so the Claude Code agent (which
// receives the language via opts["language"]) can decode the string
// without duplicating the switch statement that cmd/brodge/main.go
// uses for engine construction.
func NormalizeLanguageString(s string) Language {
	switch strings.ToLower(s) {
	case "en", "english":
		return LangEnglish
	case "zh", "chinese":
		return LangChinese
	case "zh-tw", "zh_tw", "zhtw":
		return LangTraditionalChinese
	case "ja", "japanese":
		return LangJapanese
	case "es", "spanish":
		return LangSpanish
	case "", "auto":
		return LangAuto
	default:
		return LangAuto
	}
}

func isChinese(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2B73F) ||
		(r >= 0x2B740 && r <= 0x2B81F) ||
		(r >= 0x2B820 && r <= 0x2CEAF) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0x2F800 && r <= 0x2FA1F)
}

func isJapanese(r rune) bool {
	return (r >= 0x3040 && r <= 0x309F) || // Hiragana
		(r >= 0x30A0 && r <= 0x30FF) || // Katakana
		(r >= 0x31F0 && r <= 0x31FF) || // Katakana Phonetic Extensions
		(r >= 0xFF65 && r <= 0xFF9F) // Half-width Katakana
}

// isSpanishHint checks for characters common in Spanish but not English (ñ, ¿, ¡, accented vowels).
func isSpanishHint(text string) bool {
	for _, r := range text {
		switch r {
		case 'ñ', 'Ñ', '¿', '¡', 'á', 'é', 'í', 'ó', 'ú', 'ü':
			return true
		}
	}
	return false
}

func (i *I18n) DetectAndSet(text string) {
	i.mu.RLock()
	if i.lang != LangAuto {
		i.mu.RUnlock()
		return
	}
	currentDetected := i.detected
	i.mu.RUnlock()

	detected := DetectLanguage(text)
	if currentDetected == detected {
		return
	}

	i.mu.Lock()
	// Re-check under the write lock — another goroutine may have updated
	// i.detected between our RUnlock above and the Lock here.
	if i.lang != LangAuto || i.detected == detected {
		i.mu.Unlock()
		return
	}
	i.detected = detected
	i.mu.Unlock()

}

func (i *I18n) currentLang() Language {
	// Caller holds either no lock (most public methods take RLock and call
	// this) or RLock; this helper just reads the protected fields. All
	// public methods that call currentLang() acquire i.mu.RLock first.
	if i.lang == LangAuto {
		if i.detected != "" {
			return i.detected
		}
		return LangEnglish
	}
	return i.lang
}

// CurrentLang returns the resolved language (exported for mode display).
func (i *I18n) CurrentLang() Language {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.currentLang()
}

// IsZhLike returns true for Simplified and Traditional Chinese.
func (i *I18n) IsZhLike() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	l := i.currentLang()
	return l == LangChinese || l == LangTraditionalChinese
}

// SetLang overrides the language (disabling auto-detect).
func (i *I18n) SetLang(lang Language) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.lang = lang
	i.detected = ""
}

// Message keys
type MsgKey string

const (
	MsgDesktopAllowed        MsgKey = "desktop_allowed"
	MsgDesktopOffline        MsgKey = "desktop_offline"
	MsgDesktopBindUsage      MsgKey = "desktop_bindusage"
	MsgDesktopBound          MsgKey = "desktop_bound"
	MsgDesktopList           MsgKey = "desktop_list"
	MsgDesktopUnbound        MsgKey = "desktop_unbound"
	MsgDesktopCurrent        MsgKey = "desktop_current"
	MsgDesktopHelp           MsgKey = "desktop_help"
	MsgDesktopText           MsgKey = "desktop_text"
	MsgDesktopUnsupported    MsgKey = "desktop_unsupported"
	MsgDesktopForeign        MsgKey = "desktop_foreign"
	MsgDesktopMultiple       MsgKey = "desktop_multiple"
	MsgDesktopNativeDisabled MsgKey = "desktop_nativedisabled"
	MsgDesktopAnswerPending  MsgKey = "desktop_answerpending"
	MsgDesktopStale          MsgKey = "desktop_stale"
	MsgDesktopAnswerAccepted MsgKey = "desktop_answeraccepted"
	MsgDesktopQuestionEnded  MsgKey = "desktop_questionended"
	MsgDesktopAccepted       MsgKey = "desktop_accepted"
	MsgDesktopRunning        MsgKey = "desktop_running"
	MsgDesktopError          MsgKey = "desktop_error"
	MsgDesktopEmpty          MsgKey = "desktop_empty"
	MsgDesktopDesktopInput   MsgKey = "desktop_desktopinput"
)

var messages = map[MsgKey]map[Language]string{
	MsgDesktopAllowed: {
		LangChinese:            "会话不在 allowed_threads 中。",
		LangEnglish:            "Conversation is outside allowed_threads.",
		LangTraditionalChinese: "會話不在 allowed_threads 中。",
		LangJapanese:           "会話は allowed_threads の範囲外です。",
		LangSpanish:            "La conversación no está en allowed_threads.",
	},
	MsgDesktopOffline: {
		LangChinese:            "无法连接该 Desktop 会话，原绑定保留。",
		LangEnglish:            "Cannot connect to this Desktop conversation; binding retained.",
		LangTraditionalChinese: "無法連線該 Desktop 會話，保留原綁定。",
		LangJapanese:           "Desktop の会話に接続できません。関連付けを維持します。",
		LangSpanish:            "No se puede conectar a esta conversación de Desktop; se conserva el vínculo.",
	},
	MsgDesktopBindUsage: {
		LangChinese:            "用 /bind codex://threads/会话ID 绑定已有会话。",
		LangEnglish:            "Use /bind codex://threads/ID to bind an existing conversation.",
		LangTraditionalChinese: "用 /bind codex://threads/會話ID 綁定現有會話。",
		LangJapanese:           "/bind codex://threads/ID で既存の会話を関連付けます。",
		LangSpanish:            "Use /bind codex://threads/ID para vincular una conversación existente.",
	},
	MsgDesktopBound: {
		LangChinese:            "已绑定 Codex 会话：%s",
		LangEnglish:            "Codex conversation bound: %s",
		LangTraditionalChinese: "已綁定 Codex 會話：%s",
		LangJapanese:           "Codex の会話を関連付けました：%s",
		LangSpanish:            "Conversación de Codex vinculada: %s",
	},
	MsgDesktopList: {
		LangChinese:            "本入口已关联的 Desktop 会话：\n%s\n用 /switch 会话ID 切换。",
		LangEnglish:            "Conversations linked to this chat:\n%s\nUse /switch ID to switch.",
		LangTraditionalChinese: "已授權的 Desktop 會話：\n%s\n用 /switch 會話ID 切換。",
		LangJapanese:           "許可された Desktop の会話：\n%s\n/switch ID で切り替えます。",
		LangSpanish:            "Conversaciones autorizadas de Desktop:\n%s\nUse /switch ID para cambiar.",
	},
	MsgDesktopUnbound: {
		LangChinese:            "尚未绑定。用 /bind codex://threads/会话ID 绑定已有会话。",
		LangEnglish:            "No conversation bound. Use /bind codex://threads/ID.",
		LangTraditionalChinese: "尚未綁定。用 /bind codex://threads/會話ID 綁定現有會話。",
		LangJapanese:           "未関連付けです。/bind codex://threads/ID を使用してください。",
		LangSpanish:            "Sin vínculo. Use /bind codex://threads/ID.",
	},
	MsgDesktopCurrent: {
		LangChinese:            "当前 Codex 会话：%s",
		LangEnglish:            "Current Codex conversation: %s",
		LangTraditionalChinese: "目前 Codex 會話：%s",
		LangJapanese:           "現在の Codex の会話：%s",
		LangSpanish:            "Conversación actual de Codex: %s",
	},
	MsgDesktopHelp: {
		LangChinese:            "/bind 会话链接：绑定\n/list：授权会话\n/switch 会话ID：切换\n/current：当前会话\n其他文字直接交给绑定的 Codex 会话。模型、权限和新建会话在 Desktop 管理。",
		LangEnglish:            "/bind conversation-link: bind\n/list: authorized conversations\n/switch ID: switch\n/current: current conversation\nOther text goes to the bound Codex conversation. Manage models, permissions and new conversations in Desktop.",
		LangTraditionalChinese: "/bind 會話連結：綁定\n/list：授權會話\n/switch 會話ID：切換\n/current：目前會話\n其他文字交給綁定的 Codex 會話。模型、權限和新會話在 Desktop 管理。",
		LangJapanese:           "/bind 会話リンク：関連付け\n/list：許可された会話\n/switch ID：切り替え\n/current：現在の会話\n他のテキストは関連付けた Codex の会話に送ります。モデル、権限、新規会話は Desktop で管理します。",
		LangSpanish:            "/bind enlace: vincular\n/list: conversaciones autorizadas\n/switch ID: cambiar\n/current: conversación actual\nEl texto se envía a la conversación vinculada. Gestione modelos, permisos y nuevas conversaciones en Desktop.",
	},
	MsgDesktopText: {
		LangChinese:            "请输入文字。",
		LangEnglish:            "Please enter text.",
		LangTraditionalChinese: "請輸入文字。",
		LangJapanese:           "テキストを入力してください。",
		LangSpanish:            "Introduzca texto.",
	},
	MsgDesktopUnsupported: {
		LangChinese:            "此操作暂不支持。模型、权限、新建和取消请在 Desktop 操作；当前绑定保留。",
		LangEnglish:            "This operation is unavailable. Manage models, permissions, new conversations and cancellation in Desktop; binding retained.",
		LangTraditionalChinese: "暫不支援此操作。模型、權限、新會話和取消請在 Desktop 操作；保留目前綁定。",
		LangJapanese:           "この操作は未対応です。モデル、権限、新規会話、キャンセルは Desktop で操作してください。関連付けを維持します。",
		LangSpanish:            "Operación no disponible. Gestione modelos, permisos, nuevas conversaciones y cancelación en Desktop; se conserva el vínculo.",
	},
	MsgDesktopForeign: {
		LangChinese:            "这条回复不属于当前聊天。",
		LangEnglish:            "This reply belongs to another chat.",
		LangTraditionalChinese: "這則回覆不屬於目前聊天。",
		LangJapanese:           "この返信は別のチャットに属しています。",
		LangSpanish:            "Esta respuesta pertenece a otro chat.",
	},
	MsgDesktopMultiple: {
		LangChinese:            "Codex 有多个待回答问题，请在 Desktop 选择并回答，避免答错。",
		LangEnglish:            "Codex has multiple pending requests. Answer them in Desktop.",
		LangTraditionalChinese: "Codex 有多個待回答請求，請在 Desktop 回答。",
		LangJapanese:           "Codex に複数の保留中の質問があります。Desktop で回答してください。",
		LangSpanish:            "Codex tiene varias solicitudes pendientes. Respóndalas en Desktop.",
	},
	MsgDesktopNativeDisabled: {
		LangChinese:            "原生问题远程回答尚未完成真实验证，请先在 Desktop 回答。",
		LangEnglish:            "Remote native answers await live validation. Answer in Desktop for now.",
		LangTraditionalChinese: "原生問題遠端回答尚未完成實機驗證，請先在 Desktop 回答。",
		LangJapanese:           "ネイティブ質問の遠隔回答は実機検証待ちです。Desktop で回答してください。",
		LangSpanish:            "Las respuestas nativas remotas requieren validación real. Responda en Desktop por ahora.",
	},
	MsgDesktopAnswerPending: {
		LangChinese:            "答案已登记，正在核对 Codex 是否收到；不会重复提交。",
		LangEnglish:            "Answer recorded; checking receipt in Codex without resubmitting.",
		LangTraditionalChinese: "答案已登記，正在核對 Codex 是否收到；不會重複提交。",
		LangJapanese:           "回答を記録しました。再送せず Codex の受信を確認します。",
		LangSpanish:            "Respuesta registrada; comprobando la recepción en Codex sin reenviar.",
	},
	MsgDesktopStale: {
		LangChinese:            "问题已失效，请查看 Desktop。",
		LangEnglish:            "This question is no longer active. Check Desktop.",
		LangTraditionalChinese: "問題已失效，請查看 Desktop。",
		LangJapanese:           "質問は失効しました。Desktop を確認してください。",
		LangSpanish:            "La pregunta ya no está activa. Revise Desktop.",
	},
	MsgDesktopAnswerAccepted: {
		LangChinese:            "已收到答案，继续交给 Codex。",
		LangEnglish:            "Answer received for Codex.",
		LangTraditionalChinese: "已收到答案，交給 Codex。",
		LangJapanese:           "Codex への回答を受信しました。",
		LangSpanish:            "Respuesta recibida para Codex.",
	},
	MsgDesktopQuestionEnded: {
		LangChinese:            "这条原生问题已结束，请直接向当前绑定会话发送新消息。",
		LangEnglish:            "This native question has ended. Send a new message to the current bound conversation.",
		LangTraditionalChinese: "這則原生問題已結束，請向目前綁定會話發送新訊息。",
		LangJapanese:           "このネイティブ質問は終了しました。現在の会話に新しいメッセージを送ってください。",
		LangSpanish:            "Esta pregunta nativa ha finalizado. Envíe un mensaje nuevo a la conversación vinculada.",
	},
	MsgDesktopAccepted: {
		LangChinese:            "已收到，等待 Codex 处理。",
		LangEnglish:            "Received; waiting for Codex.",
		LangTraditionalChinese: "已收到，等待 Codex 處理。",
		LangJapanese:           "受信しました。Codex の処理を待っています。",
		LangSpanish:            "Recibido; esperando a Codex.",
	},
	MsgDesktopRunning: {
		LangChinese:            "Codex 正在处理。",
		LangEnglish:            "Codex is processing.",
		LangTraditionalChinese: "Codex 正在處理。",
		LangJapanese:           "Codex が処理中です。",
		LangSpanish:            "Codex está procesando.",
	},
	MsgDesktopError: {
		LangChinese:            "Codex 返回错误（%s）。\n%s",
		LangEnglish:            "Codex returned an error (%s).\n%s",
		LangTraditionalChinese: "Codex 返回錯誤（%s）。\n%s",
		LangJapanese:           "Codex がエラーを返しました（%s）。\n%s",
		LangSpanish:            "Codex devolvió un error (%s).\n%s",
	},
	MsgDesktopEmpty: {
		LangChinese:            "Codex 回合已结束，没有文字输出。",
		LangEnglish:            "Codex turn ended without text output.",
		LangTraditionalChinese: "Codex 回合已結束，沒有文字輸出。",
		LangJapanese:           "Codex のターンがテキスト出力なしで終了しました。",
		LangSpanish:            "El turno de Codex finalizó sin texto.",
	},
	MsgDesktopDesktopInput: {
		LangChinese:            "Codex 正在等待桌面端操作，请在 Desktop 中处理后继续。",
		LangEnglish:            "Codex is waiting for an action in Desktop. Handle it there to continue.",
		LangTraditionalChinese: "Codex 正在等待桌面操作，請在 Desktop 處理後繼續。",
		LangJapanese:           "Codex は Desktop での操作を待っています。Desktop で処理してください。",
		LangSpanish:            "Codex espera una acción en Desktop. Complétela allí para continuar.",
	},
}

func (i *I18n) T(key MsgKey) string {
	i.mu.RLock()
	lang := i.currentLang()
	i.mu.RUnlock()
	if msg, ok := messages[key]; ok {
		if translated, ok := msg[lang]; ok {
			return translated
		}
		// Fallback: zh-TW → zh → en
		if lang == LangTraditionalChinese {
			if translated, ok := msg[LangChinese]; ok {
				return translated
			}
		}
		if msg[LangEnglish] != "" {
			return msg[LangEnglish]
		}
	}
	return string(key)
}

func (i *I18n) Tf(key MsgKey, args ...interface{}) string {
	template := i.T(key)
	return fmt.Sprintf(template, args...)
}
