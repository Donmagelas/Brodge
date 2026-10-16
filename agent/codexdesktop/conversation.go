package codexdesktop

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
)

func (a *Agent) consumeInbox(ctx context.Context) error {
	var app, tenant, message, source, platform, body, acceptedThread, afterBinding string
	var mediaJSON string
	var acceptedRevision int
	err := a.store.db.QueryRow(`SELECT app,tenant,message,source,platform,body,bound_thread,bound_revision,after_binding,media FROM inbox i WHERE state='ready' AND (after_binding='' OR EXISTS(SELECT 1 FROM inbox prior WHERE prior.app=i.app AND prior.tenant=i.tenant AND prior.message=i.after_binding AND prior.state='done')) ORDER BY rowid LIMIT 1`).Scan(&app, &tenant, &message, &source, &platform, &body, &acceptedThread, &acceptedRevision, &afterBinding, &mediaJSON)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var origin core.ConversationOrigin
	if json.Unmarshal([]byte(source), &origin) != nil {
		return fmt.Errorf("invalid-inbox-origin")
	}
	a.mu.Lock()
	p := a.platforms[platform]
	a.mu.Unlock()
	sender, ok := p.(core.DurableConversationPlatform)
	if !ok || !sender.AcceptConversationOrigin(origin) {
		_, err = a.store.db.Exec(`UPDATE inbox SET state='blocked' WHERE app=? AND tenant=? AND message=?`, app, tenant, message)
		return err
	}
	var media []core.ConversationMedia
	if err = json.Unmarshal([]byte(mediaJSON), &media); err != nil {
		return err
	}
	if len(media) == 0 && origin.ParentID != "" && !strings.HasPrefix(strings.TrimSpace(body), "/") {
		if resolver, ok := p.(core.ConversationMediaPlatform); ok {
			media, err = resolver.ResolveConversationMedia(ctx, origin)
			if err != nil {
				return a.rejectMedia(app, tenant, message, source, platform, err)
			}
		}
	}
	if len(media) > 0 {
		media, err = a.prepareMedia(ctx, p, origin, media)
		if err != nil {
			return a.rejectMedia(app, tenant, message, source, platform, err)
		}
		if _, err = a.store.db.Exec(`UPDATE inbox SET media=? WHERE app=? AND tenant=? AND message=?`, marshalMedia(media), app, tenant, message); err != nil {
			return err
		}
	}
	a.i18n.DetectAndSet(body)
	key := originKey(origin)
	fields := strings.Fields(body)
	command := ""
	if len(fields) > 0 && strings.HasPrefix(fields[0], "/") {
		command = strings.ToLower(fields[0])
	}
	validatedThread := ""
	validationError := ""
	if (command == "/bind" || command == "/switch") && len(fields) == 2 {
		validatedThread, err = parseThread(fields[1])
		if err != nil {
			validationError = a.i18n.T(core.MsgDesktopBindUsage)
		} else if command == "/switch" && !a.store.linked(key, validatedThread) {
			validationError = a.i18n.T(core.MsgDesktopAllowed)
		} else {
			at, e := a.attach(ctx, a.socket, validatedThread, a.timeout)
			if e != nil {
				validationError = a.i18n.T(core.MsgDesktopOffline)
			} else {
				at.c.abort(fmt.Errorf("detached"))
			}
		}
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRow(`SELECT state FROM inbox WHERE app=? AND tenant=? AND message=?`, app, tenant, message).Scan(&state); err != nil {
		return err
	}
	if state != "ready" {
		return nil
	}
	if afterBinding != "" {
		if err = tx.QueryRow(`SELECT bound_thread,bound_revision FROM inbox WHERE app=? AND tenant=? AND message=? AND state='done'`, app, tenant, afterBinding).Scan(&acceptedThread, &acceptedRevision); err != nil {
			return fmt.Errorf("resolve earlier binding: %w", err)
		}
	}
	responseID := "ingress:" + app + ":" + tenant + ":" + message
	reply := func(text string) error { return a.store.output(tx, responseID, "reply", source, platform, text) }
	var thread string
	var revision int
	bindingErr := tx.QueryRow(`SELECT thread,revision FROM bindings WHERE origin=?`, key).Scan(&thread, &revision)
	if bindingErr != nil && bindingErr != sql.ErrNoRows {
		return bindingErr
	}
	switch command {
	case "/bind", "/switch":
		if len(fields) != 2 {
			err = reply(a.i18n.T(core.MsgDesktopBindUsage))
		} else if validationError != "" {
			err = reply(validationError)
		} else {
			err = bindTx(tx, key, validatedThread)
			if err == nil {
				if err = tx.QueryRow(`SELECT thread,revision FROM bindings WHERE origin=?`, key).Scan(&acceptedThread, &acceptedRevision); err != nil {
					return err
				}
				err = reply(a.i18n.Tf(core.MsgDesktopBound, a.commandThreadTitles(ctx, validatedThread)[validatedThread]))
			}
		}
	case "/list":
		rows, e := tx.Query(`SELECT thread FROM binding_threads WHERE origin=? ORDER BY rowid`, key)
		if e != nil {
			return e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if len(ids) == 0 {
			err = reply(a.i18n.T(core.MsgDesktopUnbound))
		} else {
			titles := a.commandThreadTitles(ctx, ids...)
			entries := make([]string, 0, len(ids))
			for _, id := range ids {
				label := titles[id]
				if id == thread {
					label = "▶ " + label
				}
				entries = append(entries, label+"\n/switch "+id)
			}
			current := a.i18n.T(core.MsgDesktopUnbound)
			if bindingErr == nil {
				current = a.i18n.Tf(core.MsgDesktopCurrent, titles[thread])
			}
			err = reply(a.i18n.Tf(core.MsgDesktopList, strings.Join(entries, "\n\n")) + "\n\n" + current)
		}
	case "/current":
		if bindingErr == sql.ErrNoRows {
			err = reply(a.i18n.T(core.MsgDesktopUnbound))
		} else {
			err = reply(a.i18n.Tf(core.MsgDesktopCurrent, a.commandThreadTitles(ctx, thread)[thread]))
		}
	case "/help":
		err = reply(a.i18n.T(core.MsgDesktopHelp))
	case "":
		thread, revision = acceptedThread, acceptedRevision
		if strings.TrimSpace(body) == "" && len(media) == 0 {
			err = reply(a.i18n.T(core.MsgDesktopText))
		} else if thread == "" {
			// The first input stays durable while UI creates and submits its turn.
			// Following unbound inputs depend on this inbox row, not a mutable binding.
			_, err = tx.Exec(`INSERT INTO jobs(id,origin,source,platform,thread,revision,prompt,client_message,state,media) VALUES(?,?,?,?,'',0,?,?,'create_pending',?)`, responseID, key, source, platform, body, uuid.NewString(), marshalMedia(media))
			if err == nil {
				_, err = tx.Exec(`UPDATE inbox SET state='creating' WHERE app=? AND tenant=? AND message=?`, app, tenant, message)
			}
			if err != nil {
				return err
			}
			return tx.Commit()
		} else {
			err = a.acceptText(tx, origin, key, source, platform, thread, revision, body, media, reply)
		}
	case "/unsupported-attachment":
		err = reply(a.i18n.T(core.MsgDesktopMediaUnsupported))
	default:
		err = reply(a.i18n.T(core.MsgDesktopUnsupported))
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE inbox SET state='done',bound_thread=?,bound_revision=? WHERE app=? AND tenant=? AND message=?`, acceptedThread, acceptedRevision, app, tenant, message); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *Agent) acceptText(tx *sql.Tx, o core.ConversationOrigin, key, source, platform, thread string, revision int, body string, media []core.ConversationMedia, reply func(string) error) error {
	// Normal replies can follow a recorded bot message. Topic parent/root is
	// ambiguous and cannot identify a nested reply; ordinary text uses binding.
	replyJob, replyKind := "", ""
	if o.ParentID != "" && o.ThreadID == "" && o.RootID == "" {
		var targetSource string
		err := tx.QueryRow(`SELECT job,source,kind FROM outbox WHERE message=? AND state='sent'`, o.ParentID).Scan(&replyJob, &targetSource, &replyKind)
		if err == nil {
			var target core.ConversationOrigin
			if err = json.Unmarshal([]byte(targetSource), &target); err != nil {
				return err
			}
			if originKey(target) != key {
				return reply(a.i18n.T(core.MsgDesktopForeign))
			}
			var targetThread string
			err = tx.QueryRow(`SELECT thread FROM jobs WHERE id=?`, replyJob).Scan(&targetThread)
			if err == nil {
				thread = targetThread
			} else if err != sql.ErrNoRows {
				return err
			}
		} else if err != sql.ErrNoRows {
			return err
		}
	}
	query := `SELECT q.job,q.request,q.body,q.answers,q.state FROM questions q JOIN jobs j ON j.id=q.job WHERE j.origin=? AND j.thread=? AND j.state='waiting' AND q.state IN ('pending','ready','dispatch','uncertain')`
	args := []any{key, thread}
	if replyJob != "" {
		query += ` AND j.id=?`
		args = append(args, replyJob)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	type pending struct{ job, request, body, answers, state string }
	var questions []pending
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.job, &p.request, &p.body, &p.answers, &p.state); err != nil {
			rows.Close()
			return err
		}
		questions = append(questions, p)
	}
	rows.Close()
	if len(questions) > 1 {
		return reply(a.i18n.T(core.MsgDesktopMultiple))
	}
	if len(questions) == 1 {
		q := questions[0]
		if !a.nativeAnswers {
			return reply(a.i18n.T(core.MsgDesktopNativeDisabled))
		}
		if q.state != "pending" {
			return reply(a.i18n.T(core.MsgDesktopAnswerPending))
		}
		var request wire
		if err = json.Unmarshal([]byte(q.body), &request); err != nil {
			return err
		}
		var answers map[string][]string
		if q.answers != "" {
			if err = json.Unmarshal([]byte(q.answers), &answers); err != nil {
				return err
			}
		}
		if answers == nil {
			answers = map[string][]string{}
		}
		all := list(obj(request["params"])["questions"])
		var next wire
		for _, v := range all {
			question := obj(v)
			if _, exists := answers[str(question["id"])]; !exists {
				next = question
				break
			}
		}
		if next == nil {
			return reply(a.i18n.T(core.MsgDesktopStale))
		}
		if replyKind != "" && strings.HasPrefix(replyKind, "question:") && replyKind != "question:"+q.request+":"+str(next["id"]) && !strings.HasPrefix(replyKind, "question:"+q.request+":"+str(next["id"])+":part:") {
			return reply(a.i18n.T(core.MsgDesktopStale))
		}
		answer := strings.TrimSpace(body)
		for i, v := range list(next["options"]) {
			option := obj(v)
			if answer == fmt.Sprint(i+1) || answer == str(option["label"]) {
				answer = str(option["label"])
				break
			}
		}
		answers[str(next["id"])] = []string{answer}
		newState := "pending"
		if len(answers) == len(all) {
			newState = "ready"
		}
		if _, err = tx.Exec(`UPDATE questions SET answers=?,state=? WHERE job=? AND request=? AND state='pending'`, marshal(answers), newState, q.job, q.request); err != nil {
			return err
		}
		if newState == "pending" {
			for _, v := range all {
				question := obj(v)
				if _, exists := answers[str(question["id"])]; !exists {
					return a.store.output(tx, q.job, "question:"+q.request+":"+str(question["id"]), source, platform, questionText(question))
				}
			}
		}
		return nil // The persisted reaction acknowledges an accepted answer.
	}
	// A reply to a resolved native question never becomes a new user turn.
	if strings.HasPrefix(replyKind, "question:") {
		return reply(a.i18n.T(core.MsgDesktopQuestionEnded))
	}
	id := uuid.NewString()
	_, err = tx.Exec(`INSERT INTO jobs(id,origin,source,platform,thread,revision,prompt,client_message,state,media) VALUES(?,?,?,?,?,?,?,?,'queued',?)`, id, key, source, platform, thread, revision, body, uuid.NewString(), marshalMedia(media))
	if err != nil {
		return err
	}
	return nil
}
