package codexdesktop

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// Explicit Markdown artifact links are the only local file export source.
var artifactLink = regexp.MustCompile(`!?\[([^\]\n]*)\]\((</[^>\n]+>|/[^)\n]+)\)`)
var sourceLine = regexp.MustCompile(`:\d+$`)

func sourceReference(path string) bool {
	return sourceLine.MatchString(path)
}

var desktopDirective = regexp.MustCompile(`:{1,2}(?:code-comment|created-thread|codex-[\w-]+|artifact|automation-update)(?:\[([^\]\n]*)\])?\{((?:[^"}\n]|"(?:\\.|[^"\\])*")*)\}`)
var directiveAttribute = regexp.MustCompile(`([\w-]+)="((?:\\.|[^"\\])*)"`)

func readableFinal(text string) string {
	return desktopDirective.ReplaceAllStringFunc(text, func(raw string) string {
		match := desktopDirective.FindStringSubmatch(raw)
		parts := []string{}
		if match[1] != "" {
			parts = append(parts, match[1])
		}
		for _, field := range directiveAttribute.FindAllStringSubmatch(match[2], -1) {
			value, err := strconv.Unquote(`"` + field[2] + `"`)
			if err != nil {
				continue
			}
			switch field[1] {
			case "title", "body", "file", "path", "url":
				parts = append(parts, value)
			case "threadId":
				parts = append(parts, "codex://threads/"+value)
			}
		}
		if len(parts) == 0 {
			return "（Codex 界面标记，详见完整回复附件）"
		}
		return strings.Join(parts, "\n")
	})
}

// Prefer paragraph and line boundaries; preserve every input rune in order.
func splitReply(text string) []string {
	if text == "" {
		return []string{" "}
	}
	var parts []string
	for len(text) > 0 {
		end, runes, encoded := 0, 0, 0
		for end < len(text) && runes < 3500 {
			r, n := utf8.DecodeRuneInString(text[end:])
			cost := n
			if r < 32 || r == '<' || r == '>' || r == '&' || r == '\u2028' || r == '\u2029' {
				cost = 6
			}
			if r == '"' || r == '\\' {
				cost = 2
			}
			if end+n > 10000 || encoded+cost > 12000 {
				break
			}
			end += n
			encoded += cost
			runes++
		}
		if end == len(text) {
			parts = append(parts, text)
			break
		}
		cut := end
		// A boundary in the latter half avoids tiny or empty fragments.
		for _, separator := range []string{"\n\n", "\n", " "} {
			if pos := strings.LastIndex(text[:end], separator); pos >= end/2 {
				cut = pos + len(separator)
				break
			}
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	return parts
}

// Continue long fenced blocks on each card. The exact source is also delivered
// as an attachment, so these display-only fences never replace the original.
func renderedReplyParts(text string) []string {
	parts := splitReply(text)
	fence, opening := "", ""
	for n, part := range parts {
		prefix := ""
		if fence != "" {
			prefix = opening + "\n"
		}
		for _, line := range strings.Split(part, "\n") {
			line = strings.TrimLeft(line, " ")
			if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
				char := line[0]
				end := 0
				for end < len(line) && line[end] == char {
					end++
				}
				if fence == "" {
					fence = line[:end]
					opening = line
				} else if char == fence[0] && end >= len(fence) && strings.TrimSpace(line[end:]) == "" {
					fence = ""
					opening = ""
				}
			}
		}
		parts[n] = prefix + part
		if fence != "" {
			parts[n] += "\n" + fence
		}
	}
	return parts
}
func (a *Agent) finishResult(j job, text, category string) error {
	original := text
	text = readableFinal(text)
	fallback := category == "" && (text != original || len(splitReply(text)) > 1 || strings.Contains(text, "```") || strings.Contains(text, "~~~") || strings.Contains(text, "$$") || strings.Contains(text, "\\[") || strings.Contains(text, "|"))
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	commit := func() error {
		if fallback {
			path, _, e := a.snapshotMedia([]byte(original), "Brodge-完整回复.md")
			if e != nil {
				return e
			}
			if e = a.queueMedia(tx, j, "media:original", core.ConversationMedia{Kind: "file", Path: path}); e != nil {
				return e
			}
		}
		return tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE jobs SET state='finished',error=? WHERE id=?`, category, j.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE questions SET state='closed' WHERE job=? AND state NOT IN ('resolved','closed')`, j.ID); err != nil {
		return err
	}
	// Failures remain failures; never send partial artifacts as successful products.
	if category != "" {
		if err = a.store.output(tx, j.ID, "result", j.Source, j.Platform, text); err != nil {
			return err
		}
		return commit()
	}
	if !artifactLink.MatchString(text) {
		if err = a.store.output(tx, j.ID, "result", j.Source, j.Platform, text); err != nil {
			return err
		}
		return commit()
	}
	position, part := 0, 0
	seen := map[string]bool{}
	for _, match := range artifactLink.FindAllStringSubmatchIndex(text, -1) {
		raw := strings.Trim(text[match[4]:match[5]], "<>")
		path, e := url.PathUnescape(raw)
		if e != nil {
			continue
		}
		if !filepath.IsAbs(path) {
			continue
		}
		// A line-number source link is documentation, not an artifact export.
		if text[match[0]] != '!' && sourceReference(path) {
			continue
		}
		if position < match[0] {
			if err = a.store.output(tx, j.ID, fmt.Sprintf("result:%d", part), j.Source, j.Platform, text[position:match[0]]); err != nil {
				return err
			}
		}
		part++
		label := text[match[2]:match[3]]
		m := core.ConversationMedia{Kind: "file", Path: path, Name: label}
		if text[match[0]] == '!' {
			m.Kind = "image"
		}
		if seen[path] {
			if err = a.store.output(tx, j.ID, fmt.Sprintf("result:%d", part), j.Source, j.Platform, label+"（见上方附件）"); err != nil {
				return err
			}
			part++
		} else {
			if e = a.queueMedia(tx, j, fmt.Sprintf("media:%d", part), m); e != nil {
				// Preserve the exact reference and explain the inaccessible artifact.
				if err = a.store.output(tx, j.ID, fmt.Sprintf("result:%d", part), j.Source, j.Platform, text[match[0]:match[1]]+"\n附件无法读取，未发送。"); err != nil {
					return err
				}
			} else {
				seen[path] = true
			}
			part++
		}
		position = match[1]
	}
	if position < len(text) || part == 0 {
		if err = a.store.output(tx, j.ID, fmt.Sprintf("result:%d", part), j.Source, j.Platform, text[position:]); err != nil {
			return err
		}
	}
	return commit()
}
