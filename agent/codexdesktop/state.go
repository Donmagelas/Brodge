package codexdesktop

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func validRequestID(id any) bool {
	switch value := id.(type) {
	case string:
		return len(value) > 0 && len(value) <= 256
	case float64:
		return value >= 0 && value <= 9007199254740991 && value == math.Trunc(value)
	default:
		return false
	}
}

func parseThread(s string) (string, error) {
	if strings.HasPrefix(s, "codex://") {
		u, err := url.Parse(s)
		if err != nil || u.Host != "threads" || u.User != nil || u.Fragment != "" || (u.Query().Get("hostId") != "" && u.Query().Get("hostId") != "local") {
			return "", fmt.Errorf("invalid-thread")
		}
		s = strings.TrimPrefix(u.Path, "/")
	}
	if !idPattern.MatchString(s) {
		return "", fmt.Errorf("invalid-thread")
	}
	return s, nil
}
func turnsOf(state wire, thread string) ([]wire, error) {
	if str(state["id"]) != thread || str(state["hostId"]) != "local" {
		return nil, fmt.Errorf("state-identity-mismatch")
	}
	items, ok := state["turns"].([]any)
	if !ok {
		return nil, fmt.Errorf("unknown-turns-schema")
	}
	if _, ok := state["requests"].([]any); !ok {
		return nil, fmt.Errorf("unknown-requests-schema")
	}
	if h := obj(state["turnHistory"]); h != nil {
		history := obj(h["history"])
		islands := list(history["islands"])
		entities := obj(history["entitiesByKey"])
		if str(h["kind"]) != "canonical" || history["isComplete"] != true || len(islands) != 1 || entities == nil {
			return nil, fmt.Errorf("incomplete-canonical-history")
		}
		entries, ok := obj(islands[0])["entries"].([]any)
		if !ok {
			return nil, fmt.Errorf("unknown-history")
		}
		for _, e := range entries {
			value, exists := entities[str(obj(e)["value"])]
			if !exists {
				return nil, fmt.Errorf("unknown-history-entry")
			}
			items = append(items, value)
		}
	}
	var turns []wire
	for _, item := range items {
		t := obj(item)
		if t == nil || obj(t["params"]) == nil {
			return nil, fmt.Errorf("unknown-turn-schema")
		}
		if _, ok := t["items"].([]any); !ok {
			return nil, fmt.Errorf("unknown-turn-items")
		}
		switch str(t["status"]) {
		case "completed", "failed", "interrupted", "inProgress":
		default:
			return nil, fmt.Errorf("unknown-turn-status")
		}
		turns = append(turns, t)
	}
	return turns, nil
}
func idle(state wire, thread string) (bool, error) {
	turns, err := turnsOf(state, thread)
	if err != nil {
		return false, err
	}
	if str(state["resumeState"]) != "resumed" || len(list(state["requests"])) > 0 || state["threadGoal"] != nil {
		return false, nil
	}
	for _, key := range []string{"unconfirmedTurnSubmissions", "pendingDisabledPluginIds"} {
		if v := state[key]; v != nil {
			a, ok := v.([]any)
			if !ok {
				return false, fmt.Errorf("unknown-busy-state")
			}
			if len(a) > 0 {
				return false, nil
			}
		}
	}
	if r := obj(state["threadRuntimeStatus"]); r != nil && str(r["type"]) != "idle" {
		return false, nil
	}
	for _, t := range turns {
		if str(t["status"]) == "inProgress" {
			return false, nil
		}
	}
	return true, nil
}
func ownedTurn(state wire, thread, message, turnID string) (wire, error) {
	turns, err := turnsOf(state, thread)
	if err != nil {
		return nil, err
	}
	var result wire
	for _, t := range turns {
		if str(obj(t["params"])["clientUserMessageId"]) != message {
			continue
		}
		id := str(t["turnId"])
		if id == "" || (turnID != "" && id != turnID) {
			return nil, fmt.Errorf("turn-correlation-mismatch")
		}
		if result != nil {
			a, _ := json.Marshal(result)
			b, _ := json.Marshal(t)
			if string(a) != string(b) {
				return nil, fmt.Errorf("turn-representations-disagree")
			}
		}
		result = t
	}
	return result, nil
}

type turnResult struct {
	Terminal               bool
	Status, Category, Text string
}

func classify(t wire) turnResult {
	r := turnResult{Terminal: str(t["status"]) != "inProgress", Status: str(t["status"])}
	var nativeErrors []any
	var texts []string
	if t["error"] != nil {
		nativeErrors = append(nativeErrors, t["error"])
	}
	for _, v := range list(t["items"]) {
		i := obj(v)
		switch str(i["type"]) {
		case "error":
			nativeErrors = append(nativeErrors, i)
		case "agentMessage":
			if str(i["delivery"]) == "async" || len(list(i["questions"])) > 0 {
				continue
			}
			phase := str(i["phase"])
			if phase == "commentary" {
				continue
			}
			if phase != "final_answer" {
				r.Category = "unknown-assistant-phase"
				continue
			}
			if text, ok := i["text"].(string); ok {
				texts = append(texts, text)
			} else {
				r.Category = "unknown-assistant-item"
			}
		}
	}
	r.Text = strings.Join(texts, "\n\n")
	if len(nativeErrors) > 0 {
		r.Category = "native-error"
		b, _ := json.Marshal(nativeErrors)
		s := strings.ToLower(string(b))
		if strings.Contains(s, "content_filter") {
			r.Category = "content-filter"
		} else if strings.Contains(s, "stream disconnected") || strings.Contains(s, "incomplete response") {
			r.Category = "stream-error"
		}
	}
	if r.Category == "" && strings.Contains(r.Text, "I'm sorry, but I cannot assist with that request.") {
		r.Category = "refusal-observed"
	}
	if r.Category == "" && r.Terminal && r.Status != "completed" {
		r.Category = r.Status
	}
	return r
}
func questionRequests(state, turn wire) ([]wire, error) {
	var out []wire
	// Desktop's async question form is an agentMessage, not a server request.
	// Normalize it only for persistence; submission uses Desktop's exact
	// questionItemId envelope and steering protocol below.
	answered := asyncAnswerValues(turn)
	for _, v := range list(turn["items"]) {
		i := obj(v)
		if str(i["type"]) != "agentMessage" || len(list(i["questions"])) == 0 {
			continue
		}
		if str(i["id"]) == "" {
			return nil, fmt.Errorf("unknown-async-question")
		}
		questions := []any{}
		pending := false
		for index, v := range list(i["questions"]) {
			q := obj(v)
			id := marshal([]any{"request_user_input_async", i["id"], index})
			if str(q["title"]) == "" {
				return nil, fmt.Errorf("unknown-async-question")
			}
			options := []any{}
			for _, v := range list(q["options"]) {
				label, ok := v.(string)
				if !ok {
					return nil, fmt.Errorf("unknown-async-option")
				}
				options = append(options, wire{"label": label})
			}
			questions = append(questions, wire{"id": id, "question": q["title"], "options": options})
			if a, ok := answered[id]; !ok || a.Question != str(q["title"]) {
				pending = true
			}
		}
		if pending {
			out = append(out, wire{"id": i["id"], "method": "request_user_input_async", "params": wire{"threadId": state["id"], "turnId": turn["turnId"], "questions": questions}})
		}
	}
	for _, v := range list(state["requests"]) {
		r := obj(v)
		p := obj(r["params"])
		if str(r["method"]) != "item/tool/requestUserInput" || str(p["threadId"]) != str(state["id"]) || str(p["turnId"]) != str(turn["turnId"]) {
			continue
		}
		questions, ok := p["questions"].([]any)
		if !ok || len(questions) == 0 || !validRequestID(r["id"]) {
			return nil, fmt.Errorf("unknown-question-schema")
		}
		ids := map[string]bool{}
		for _, v := range questions {
			q := obj(v)
			id := str(q["id"])
			if id == "" || ids[id] || str(q["question"]) == "" || q["isSecret"] == true || q["is_secret"] == true {
				return nil, fmt.Errorf("unsupported-question")
			}
			ids[id] = true
		}
		out = append(out, r)
	}
	return out, nil
}

type asyncAnswer struct {
	QuestionItemID string `json:"questionItemId"`
	Question       string `json:"question"`
	Answer         string `json:"answer"`
}

const asyncReplyOpen = "<send_user_message_question_reply>"
const asyncReplyClose = "</send_user_message_question_reply>"

func asyncAnswerValues(turn wire) map[string]asyncAnswer {
	answers := map[string]asyncAnswer{}
	for _, v := range list(turn["items"]) {
		i := obj(v)
		var input []any
		switch str(i["type"]) {
		case "userMessage":
			input = list(i["content"])
		case "steeringUserMessage":
			if str(i["status"]) != "accepted" || str(i["targetTurnId"]) != str(turn["turnId"]) {
				continue
			}
			input = list(i["input"])
		default:
			continue
		}
		if len(input) != 1 || str(obj(input[0])["type"]) != "text" {
			continue
		}
		text := strings.TrimSpace(str(obj(input[0])["text"]))
		if !strings.HasPrefix(text, asyncReplyOpen) || !strings.HasSuffix(text, asyncReplyClose) {
			continue
		}
		var values []asyncAnswer
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, asyncReplyOpen), asyncReplyClose))), &values) != nil {
			continue
		}
		for _, a := range values {
			if a.QuestionItemID != "" && a.Question != "" {
				answers[a.QuestionItemID] = a
			}
		}
	}
	return answers
}

func asyncAnswerEvidence(turn, request wire, answers map[string][]string) bool {
	actual := asyncAnswerValues(turn)
	questions := list(obj(request["params"])["questions"])
	if len(questions) == 0 || len(answers) != len(questions) {
		return false
	}
	for _, v := range questions {
		q := obj(v)
		id := str(q["id"])
		expected := answers[id]
		got, ok := actual[id]
		if !ok || got.Question != str(q["question"]) || len(expected) != 1 || got.Answer != expected[0] {
			return false
		}
	}
	return true
}
func answerEvidence(state, turn wire, request any, answers map[string][]string) bool {
	for _, v := range list(state["requests"]) {
		if obj(v)["id"] == request {
			return false
		}
	}
	for _, v := range list(turn["items"]) {
		i := obj(v)
		if str(i["type"]) != "userInputResponse" || i["requestId"] != request || i["completed"] != true || str(i["turnId"]) != str(turn["turnId"]) {
			continue
		}
		valid := len(obj(i["answers"])) == len(answers)
		for id, answer := range answers {
			a, _ := json.Marshal(answer)
			b, _ := json.Marshal(obj(i["answers"])[id])
			if string(a) != string(b) {
				valid = false
			}
		}
		if valid {
			return true
		}
	}
	return false
}
