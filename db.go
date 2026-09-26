package main

// SQLite-backed state store, shared with FIM-AV Assistant.
//
// One .db file lives in the recording folder, beside the .mp4s. It is the single
// source of truth for upload tracking (there is no state.json). FIM-AV Assistant
// writes the recording / identity / team columns of the `matches` table; this
// sidecar writes only the upload + operational columns. WAL mode lets the two
// processes share the file: many readers, one writer, with a busy timeout to
// ride out contention.
//
// The store keeps the same facade the rest of the code already used
// (snapshot()/update(fn)), so the upload worker and HTTP handlers didn't change
// when the JSON file became a database. On every update the in-memory state is
// written back inside one transaction; on the `matches` table the sidecar's
// INSERT bootstraps a row (with filename-derived identity) only when FIM-AV has
// not created it yet, and the ON CONFLICT clause touches ONLY sidecar columns,
// so FIM-AV's identity/team/recording writes are never clobbered.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

// dbFileName is the shared database, co-located with the recordings and (until
// it is retired) the fimav-matches.json manifest.
const dbFileName = "youtube-tba-upload.db"

// dbPath returns the database path for the current recording folder.
func dbPath() string {
	return filepath.Join(settings.VideoDir, dbFileName)
}

// schema is created on open if absent. Column ownership is called out so both
// ends stay honest about who writes what.
const schema = `
CREATE TABLE IF NOT EXISTS matches (
    file_name            TEXT PRIMARY KEY,
    -- ── owned by FIM-AV Assistant (recording, identity, teams) ──
    file_path            TEXT,
    record_id            TEXT,
    event                TEXT,
    level                TEXT,
    match_number         INTEGER,
    play                 INTEGER,
    tba_match_key        TEXT,
    match_label          TEXT,
    record_status        TEXT,      -- recording | recorded | error
    has_card             INTEGER,   -- 0/1
    ended_at             INTEGER,   -- epoch ms
    teams_json           TEXT,      -- {"red":[{teamNumber,teamName,card}],"blue":[...]}
    processing_state     TEXT,      -- unprocessed | queued | processing | done | error
    processing_output    TEXT,
    processing_error     TEXT,
    -- ── owned by the sidecar (upload + operational) ──
    upload_status        TEXT,      -- new | cutting | stable | uploading | uploaded | failed | skipped
    yt_video_id          TEXT,
    yt_url               TEXT,
    tba_submitted        INTEGER,   -- 0/1
    tba_error            TEXT,
    title_used           TEXT,
    uploaded_at          TEXT,
    size                 INTEGER,
    mtime                INTEGER,
    stable_since         INTEGER,
    attempts             INTEGER,
    next_attempt         INTEGER,
    last_error           TEXT,
    warnings_json        TEXT,
    hold_reason          TEXT,
    changed_after_upload INTEGER,
    updated_at           TEXT
);
CREATE TABLE IF NOT EXISTS upload_config (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    event_key   TEXT,
    config_json TEXT
);
CREATE TABLE IF NOT EXISTS upload_manual_video_ids (
    match_key   TEXT PRIMARY KEY,
    yt_video_id TEXT
);
CREATE TABLE IF NOT EXISTS upload_kv (
    key   TEXT PRIMARY KEY,
    value TEXT
);
`

// stateStore owns one recording folder's database. Writes are serialized through
// mu (and one transaction); the *sql.DB itself is safe for the read-only helpers
// (fimavRecord) that the scan calls without taking mu.
type stateStore struct {
	db       *sql.DB
	eventKey string
	mu       sync.Mutex
	state    eventState
}

// openStateStore opens (creating if needed) the database for the current
// recording folder and loads the sidecar's state into memory.
func openStateStore(eventKey string) (*stateStore, error) {
	if settings.VideoDir == "" {
		return nil, fmt.Errorf("no recording folder set; pass -video-dir")
	}
	dsn := "file:" + dbPath() +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Serialize in-process access to one connection; cross-process concurrency
	// with FIM-AV is handled by WAL + busy_timeout.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	s := &stateStore{
		db:       db,
		eventKey: eventKey,
		state: eventState{
			Videos:         map[string]*videoEntry{},
			ManualVideoIDs: map[string]string{},
		},
	}
	if err := s.load(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// load reads the sidecar's state from the database into memory. A fresh database
// gets a default config row.
func (s *stateStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Config.
	var cfgJSON string
	err := s.db.QueryRow(`SELECT config_json FROM upload_config WHERE id = 1`).Scan(&cfgJSON)
	switch {
	case err == sql.ErrNoRows:
		s.state.Config = eventConfig{
			EventKey:            s.eventKey,
			ProfileName:         defaultProfileName,
			Visibility:          defaultVisibility,
			TitleTemplate:       defaultTitleTemplate,
			DescriptionTemplate: defaultDescriptionTemplate,
			AutoSubmitTBA:       true,
		}
	case err != nil:
		return fmt.Errorf("load config: %w", err)
	default:
		if e := json.Unmarshal([]byte(cfgJSON), &s.state.Config); e != nil {
			return fmt.Errorf("parse config: %w", e)
		}
		if s.state.Config.EventKey == "" {
			s.state.Config.EventKey = s.eventKey
		}
	}

	// Videos (sidecar + operational columns; identity reconstructed into Meta).
	rows, err := s.db.Query(`
		SELECT file_name, level, match_number, play, tba_match_key, match_label,
		       upload_status, yt_video_id, title_used, uploaded_at, tba_submitted, tba_error,
		       size, mtime, stable_since, attempts, next_attempt, last_error,
		       warnings_json, hold_reason, changed_after_upload
		FROM matches`)
	if err != nil {
		return fmt.Errorf("load matches: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name                                            string
			level, key, label, status, ytID, titleUsed      sql.NullString
			uploadedAt, lastErr, warningsJSON, holdReason   sql.NullString
			tbaErr                                          sql.NullString
			matchNum, play                                  sql.NullInt64
			size, mtime, stableSince, attempts, nextAttempt sql.NullInt64
			tbaSubmitted, changedAfterUpload                sql.NullInt64
		)
		if err := rows.Scan(&name, &level, &matchNum, &play, &key, &label,
			&status, &ytID, &titleUsed, &uploadedAt, &tbaSubmitted, &tbaErr,
			&size, &mtime, &stableSince, &attempts, &nextAttempt, &lastErr,
			&warningsJSON, &holdReason, &changedAfterUpload); err != nil {
			return fmt.Errorf("scan match: %w", err)
		}
		e := &videoEntry{
			Size:               size.Int64,
			Mtime:              mtime.Int64,
			Status:             status.String,
			StableSince:        stableSince.Int64,
			YTVideoID:          ytID.String,
			TitleUsed:          titleUsed.String,
			UploadedAt:         uploadedAt.String,
			Attempts:           int(attempts.Int64),
			NextAttempt:        nextAttempt.Int64,
			LastError:          lastErr.String,
			HoldReason:         holdReason.String,
			ChangedAfterUpload: changedAfterUpload.Int64 == 1,
			TBASubmitted:       tbaSubmitted.Int64 == 1,
			TBASubmitError:     tbaErr.String,
		}
		if warningsJSON.String != "" {
			_ = json.Unmarshal([]byte(warningsJSON.String), &e.Warnings)
		}
		if key.String != "" || level.String != "" {
			e.Meta = &videoMeta{
				TBAMatchKey: key.String,
				MatchLevel:  level.String,
				MatchNumber: int(matchNum.Int64),
				MatchLabel:  label.String,
				Play:        int(play.Int64),
			}
		}
		s.state.Videos[name] = e
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Manual video ids.
	mrows, err := s.db.Query(`SELECT match_key, yt_video_id FROM upload_manual_video_ids`)
	if err != nil {
		return fmt.Errorf("load manual ids: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var k, v string
		if err := mrows.Scan(&k, &v); err != nil {
			return err
		}
		s.state.ManualVideoIDs[k] = v
	}
	if err := mrows.Err(); err != nil {
		return err
	}

	// Sidecar kv.
	s.state.NeedsReauth = s.kv("needs_reauth") == "1"
	s.state.LastChannelName = s.kv("last_channel_name")
	return nil
}

// kv reads a value from upload_kv (caller holds mu or is single-threaded on open).
func (s *stateStore) kv(key string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM upload_kv WHERE key = ?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// snapshot returns a deep copy of the in-memory state, isolated from concurrent
// mutation (same JSON round-trip the JSON store used).
func (s *stateStore) snapshot() eventState {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.state)
	var out eventState
	_ = json.Unmarshal(data, &out)
	return out
}

// update applies a mutation to the in-memory state and persists it to the
// database in a single transaction.
func (s *stateStore) update(fn func(*eventState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	return s.persistLocked()
}

// persistLocked writes the whole in-memory state back. Caller holds mu.
func (s *stateStore) persistLocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	cfgJSON, err := json.Marshal(s.state.Config)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO upload_config (id, event_key, config_json) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET event_key = excluded.event_key, config_json = excluded.config_json`,
		s.state.Config.EventKey, string(cfgJSON)); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	now := nowRFC3339()
	for name, e := range s.state.Videos {
		var (
			level, key, label string
			matchNum, play    int
		)
		if e.Meta != nil {
			level = e.Meta.MatchLevel
			key = e.Meta.TBAMatchKey
			label = e.Meta.MatchLabel
			matchNum = e.Meta.MatchNumber
			play = e.Meta.Play
		}
		ytURL := ""
		if e.YTVideoID != "" {
			ytURL = "https://www.youtube.com/watch?v=" + e.YTVideoID
		}
		warningsJSON := ""
		if len(e.Warnings) > 0 {
			if b, err := json.Marshal(e.Warnings); err == nil {
				warningsJSON = string(b)
			}
		}
		// INSERT bootstraps identity from the sidecar's Meta only when FIM-AV has
		// not created the row; ON CONFLICT updates ONLY sidecar-owned columns.
		if _, err := tx.Exec(`
			INSERT INTO matches (
				file_name, event, level, match_number, play, tba_match_key, match_label,
				upload_status, yt_video_id, yt_url, tba_submitted, tba_error, title_used, uploaded_at,
				size, mtime, stable_since, attempts, next_attempt, last_error, warnings_json,
				hold_reason, changed_after_upload, updated_at
			) VALUES (?,?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?)
			ON CONFLICT(file_name) DO UPDATE SET
				upload_status = excluded.upload_status,
				yt_video_id = excluded.yt_video_id,
				yt_url = excluded.yt_url,
				tba_submitted = excluded.tba_submitted,
				tba_error = excluded.tba_error,
				title_used = excluded.title_used,
				uploaded_at = excluded.uploaded_at,
				size = excluded.size,
				mtime = excluded.mtime,
				stable_since = excluded.stable_since,
				attempts = excluded.attempts,
				next_attempt = excluded.next_attempt,
				last_error = excluded.last_error,
				warnings_json = excluded.warnings_json,
				hold_reason = excluded.hold_reason,
				changed_after_upload = excluded.changed_after_upload,
				updated_at = excluded.updated_at`,
			name, s.state.Config.EventKey, level, matchNum, play, key, label,
			e.Status, e.YTVideoID, ytURL, boolToInt(e.TBASubmitted), e.TBASubmitError, e.TitleUsed, e.UploadedAt,
			e.Size, e.Mtime, e.StableSince, e.Attempts, e.NextAttempt, e.LastError, warningsJSON,
			e.HoldReason, boolToInt(e.ChangedAfterUpload), now); err != nil {
			return fmt.Errorf("save match %s: %w", name, err)
		}
	}

	// Manual ids (sidecar-owned): replace wholesale.
	if _, err := tx.Exec(`DELETE FROM upload_manual_video_ids`); err != nil {
		return err
	}
	for k, v := range s.state.ManualVideoIDs {
		if _, err := tx.Exec(`INSERT INTO upload_manual_video_ids (match_key, yt_video_id) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}

	// kv.
	if err := setKVTx(tx, "needs_reauth", boolToStr(s.state.NeedsReauth)); err != nil {
		return err
	}
	if err := setKVTx(tx, "last_channel_name", s.state.LastChannelName); err != nil {
		return err
	}

	return tx.Commit()
}

func setKVTx(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`
		INSERT INTO upload_kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// fimavRecord reads the FIM-AV-owned columns for one file. It does NOT take mu
// (it only reads the database), so the scan can call it from inside an update
// closure without deadlocking. ok is false when no row exists yet.
func (s *stateStore) fimavRecord(filename string) (fimavRecord, bool) {
	var (
		filePath, status, teamsJSON      sql.NullString
		procState, procOutput, procError sql.NullString
		hasCard, endedAt                 sql.NullInt64
	)
	err := s.db.QueryRow(`
		SELECT file_path, record_status, has_card, ended_at, teams_json,
		       processing_state, processing_output, processing_error
		FROM matches WHERE file_name = ?`, filename).Scan(
		&filePath, &status, &hasCard, &endedAt, &teamsJSON,
		&procState, &procOutput, &procError)
	if err != nil {
		return fimavRecord{}, false
	}
	rec := fimavRecord{
		FileName: filename,
		FilePath: filePath.String,
		Status:   status.String,
		HasCard:  hasCard.Int64 == 1,
		EndedAt:  endedAt.Int64,
	}
	if teamsJSON.String != "" {
		var t fimavTeams
		if json.Unmarshal([]byte(teamsJSON.String), &t) == nil {
			rec.Teams = &t
		}
	}
	if procState.String != "" || procOutput.String != "" || procError.String != "" {
		rec.Processing = &fimavProcessing{
			State:      procState.String,
			OutputPath: procOutput.String,
			Error:      procError.String,
		}
	}
	return rec, true
}

// fimavPresent reports whether FIM-AV Assistant is managing this folder's
// database — i.e. any row carries recording data. Until it does, the cut-hold
// gate stays off, exactly as it did when no manifest existed.
func (s *stateStore) fimavPresent() bool {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM matches
		WHERE record_status IS NOT NULL OR processing_state IS NOT NULL OR teams_json IS NOT NULL`).Scan(&n)
	return err == nil && n > 0
}

func (s *stateStore) close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
