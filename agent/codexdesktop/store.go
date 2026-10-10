package codexdesktop

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/chenhg5/cc-connect/core"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS bindings (origin TEXT PRIMARY KEY,thread TEXT NOT NULL,revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS binding_threads (origin TEXT NOT NULL,thread TEXT NOT NULL,PRIMARY KEY(origin,thread));
INSERT OR IGNORE INTO binding_threads SELECT origin,thread FROM bindings;
CREATE TABLE IF NOT EXISTS inbox (app TEXT,tenant TEXT,message TEXT,source TEXT NOT NULL,key TEXT NOT NULL,platform TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,bound_thread TEXT NOT NULL DEFAULT '',bound_revision INTEGER NOT NULL DEFAULT 0,after_binding TEXT NOT NULL DEFAULT '',PRIMARY KEY(app,tenant,message));
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY,origin TEXT NOT NULL,source TEXT NOT NULL,platform TEXT NOT NULL,thread TEXT NOT NULL,revision INTEGER NOT NULL,prompt TEXT NOT NULL,client_message TEXT NOT NULL UNIQUE,turn TEXT NOT NULL DEFAULT '',state TEXT NOT NULL,owner TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX IF NOT EXISTS active_thread ON jobs(thread) WHERE state IN ('claimed','dispatch','running','waiting','uncertain');
CREATE TABLE IF NOT EXISTS outbox (id TEXT PRIMARY KEY,job TEXT NOT NULL,kind TEXT NOT NULL,source TEXT NOT NULL,platform TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'ready',message TEXT NOT NULL DEFAULT '',first_attempt INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,UNIQUE(job,kind));
CREATE TABLE IF NOT EXISTS questions (job TEXT NOT NULL,request TEXT NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',answers TEXT NOT NULL DEFAULT '',PRIMARY KEY(job,request));
`

type store struct{ db *sql.DB }
type job struct {
	ID, Origin, Source, Platform, Thread, Prompt, ClientMessage, Turn, State, Owner, Error string
	Revision                                                                               int
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	s, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := s.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe-state-file")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, table := range []string{"inbox", "jobs"} {
		rows, e := db.Query("PRAGMA table_info(" + table + ")")
		if e != nil {
			db.Close()
			return nil, e
		}
		found := false
		for rows.Next() {
			var cid, nn, pk int
			var name, typ string
			var def any
			if e = rows.Scan(&cid, &name, &typ, &nn, &def, &pk); e != nil {
				break
			}
			if name == "media" {
				found = true
			}
		}
		rows.Close()
		if e != nil {
			db.Close()
			return nil, e
		}
		if !found {
			if _, e = db.Exec("ALTER TABLE " + table + " ADD COLUMN media TEXT NOT NULL DEFAULT '[]'"); e != nil {
				db.Close()
				return nil, e
			}
		}
	}
	// Retire old progress notifications when upgrading; history remains intact.
	if _, err = db.Exec(`UPDATE outbox SET state='skipped' WHERE kind IN ('accepted','running') AND state='ready'`); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db}, nil
}
func originKey(o core.ConversationOrigin) string {
	b, _ := json.Marshal([]string{o.AppID, o.Tenant, o.ChatID, o.Actor})
	return string(b)
}
func marshal(v any) string { b, _ := json.Marshal(v); return string(b) }
func (s *store) bind(key, thread string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = bindTx(tx, key, thread); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *store) ingest(p core.Platform, msg *core.Message) error {
	o := *msg.ConversationOrigin
	source := marshal(o)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing, body, key, state, media string
	err = tx.QueryRow(`SELECT source,body,key,state,media FROM inbox WHERE app=? AND tenant=? AND message=?`, o.AppID, o.Tenant, o.MessageID).Scan(&existing, &body, &key, &state, &media)
	if err == nil {
		// A pairing message may be redelivered after the temporary SDK listener
		// exits. Its identity is a tombstone: the code must never become a prompt.
		if state == "paired" && existing == source && key == originKey(o) {
			return nil
		}
		if existing != source || body != msg.Content || key != originKey(o) || !sameMediaReferences(media, marshalMedia(msg.Media)) {
			return fmt.Errorf("duplicate-message-conflict")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var boundThread string
	var boundRevision int
	err = tx.QueryRow(`SELECT thread,revision FROM bindings WHERE origin=?`, originKey(o)).Scan(&boundThread, &boundRevision)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	// A quickly following message observes an earlier queued binding command,
	// while unrelated later Web switches cannot retarget an accepted message.
	rows, err := tx.Query(`SELECT message,body,state FROM inbox WHERE app=? AND tenant=? AND key=? AND state IN ('ready','creating') ORDER BY rowid`, o.AppID, o.Tenant, originKey(o))
	if err != nil {
		return err
	}
	afterBinding := ""
	for rows.Next() {
		var id, body, previousState string
		if err = rows.Scan(&id, &body, &previousState); err != nil {
			rows.Close()
			return err
		}
		fields := strings.Fields(body)
		if len(fields) > 0 && (strings.EqualFold(fields[0], "/bind") || strings.EqualFold(fields[0], "/switch")) {
			afterBinding = id
		}
		if boundThread == "" && !strings.HasPrefix(strings.TrimSpace(msg.Content), "/") && (previousState == "creating" || !strings.HasPrefix(strings.TrimSpace(body), "/")) {
			afterBinding = id
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO inbox(app,tenant,message,source,key,platform,body,state,bound_thread,bound_revision,after_binding,media) VALUES(?,?,?,?,?,?,?,'ready',?,?,?,?)`, o.AppID, o.Tenant, o.MessageID, source, originKey(o), p.Name(), msg.Content, boundThread, boundRevision, afterBinding, marshalMedia(msg.Media))
	if err != nil {
		return err
	}
	if _, ok := p.(core.ConversationReactor); ok {
		if err = s.output(tx, "ingress:"+o.AppID+":"+o.Tenant+":"+o.MessageID, "reaction", source, p.Name(), "OK"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func scanJob(row interface{ Scan(...any) error }) (job, error) {
	var j job
	err := row.Scan(&j.ID, &j.Origin, &j.Source, &j.Platform, &j.Thread, &j.Revision, &j.Prompt, &j.ClientMessage, &j.Turn, &j.State, &j.Owner, &j.Error)
	return j, err
}

const jobCols = `id,origin,source,platform,thread,revision,prompt,client_message,turn,state,owner,error`

func (s *store) claim(owner string) (job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return job{}, err
	}
	defer tx.Rollback()
	j, err := scanJob(tx.QueryRow(`SELECT ` + jobCols + ` FROM jobs j WHERE state='queued' AND NOT EXISTS(SELECT 1 FROM jobs active WHERE active.thread=j.thread AND active.state IN ('claimed','dispatch','running','waiting','uncertain')) ORDER BY rowid LIMIT 1`))
	if err != nil {
		return j, err
	}
	result, err := tx.Exec(`UPDATE jobs SET state='claimed',owner=? WHERE id=? AND state='queued'`, owner, j.ID)
	if err != nil {
		return j, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return j, fmt.Errorf("claim-lost")
	}
	j.State = "claimed"
	j.Owner = owner
	return j, tx.Commit()
}
func (s *store) recover() error {
	_, err := s.db.Exec(`UPDATE jobs SET state=CASE WHEN state='claimed' THEN 'queued' ELSE 'uncertain' END,owner='' WHERE state IN ('claimed','dispatch'); UPDATE jobs SET state='create_failed',error='interrupted-before-send' WHERE state='create_preparing'; UPDATE jobs SET state='create_uncertain',error='creation-send-outcome-unknown' WHERE state='create_sending'; UPDATE questions SET state='uncertain' WHERE state='dispatch'; UPDATE outbox SET state='uncertain' WHERE state='sending'`)
	return err
}
func (s *store) output(tx *sql.Tx, id, kind, source, platform, text string) error {
	parts := renderedReplyParts(text)
	for part, body := range parts {
		partKind := kind
		if len(parts) > 1 {
			partKind = fmt.Sprintf("%s:part:%d", kind, part)
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO outbox(id,job,kind,source,platform,body) VALUES(?,?,?,?,?,?)`, uuid.NewString(), id, partKind, source, platform, body); err != nil {
			return err
		}
	}
	return nil
}
func (s *store) notice(j job, kind, text string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.output(tx, j.ID, kind, j.Source, j.Platform, text); err != nil {
		return err
	}
	return tx.Commit()
}

func bindTx(tx *sql.Tx, key, thread string) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO binding_threads(origin,thread) VALUES(?,?)`, key, thread); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO bindings VALUES(?,?,1) ON CONFLICT(origin) DO UPDATE SET thread=excluded.thread,revision=revision+1`, key, thread)
	return err
}
func (s *store) linked(key, thread string) bool {
	var n int
	return s.db.QueryRow(`SELECT count(*) FROM binding_threads WHERE origin=? AND thread=?`, key, thread).Scan(&n) == nil && n == 1
}

func marshalMedia(media []core.ConversationMedia) string {
	if len(media) == 0 {
		return "[]"
	}
	return marshal(media)
}
