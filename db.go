package main

// JSON-manifest-backed state store — the single source of truth shared with
// FIM-AV Assistant.
//
// There is exactly one file: fimav-matches.json, in the recording folder beside
// the .mp4s. FIM-AV Assistant owns the recording/identity/team fields of each
// match record; this uploader owns a single "upload" object on the same record.
// The two processes each do a locked, atomic read-modify-write (a .lock file +
// temp-file rename), and each only ever touches its own fields, so neither
// clobbers the other. Reads need no lock — the atomic rename means a reader
// always sees a complete file.
//
// Why JSON and not a database: FIM-AV Assistant is an Electron app, and a shared
// SQLite would force a native module (better-sqlite3 / node-gyp) into it, which
// breaks its build. JSON needs nothing native on either side. Config is not
// persisted here (FIM-AV pushes it via /api/upload/config on start and on save),
// so the manifest stays the only persisted store. Recording never depends on the
// uploader being up.
//
// The store keeps the same facade the rest of the code used (snapshot()/update),
// so the upload worker and HTTP handlers didn't change.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const manifestFileName = "fimav-matches.json"

func manifestPath() string { return filepath.Join(settings.VideoDir, manifestFileName) }

// uploaderStateFileName holds SETTINGS ONLY (config, manual ids, reauth flag) —
// not match/upload tracking, which is the shared manifest's single source of
// truth. FIM-AV pushes config over the API; persisting it here just means a
// uploader restart doesn't upload with default config before the next push.
const uploaderStateFileName = "youtube-tba-upload.json"

func uploaderStatePath() string { return filepath.Join(settings.VideoDir, uploaderStateFileName) }

type uploaderState struct {
	Config         eventConfig       `json:"config"`
	ManualVideoIDs map[string]string `json:"manual_video_ids,omitempty"`
	NeedsReauth    bool              `json:"needs_reauth,omitempty"`
	// UnmatchedUploads is the upload state of videos with no FIM-AV record
	// (hand-placed files). It lives here, not in the shared manifest: a record
	// there without FIM-AV's own fields (status/id) crashes FIM-AV's renderer.
	UnmatchedUploads map[string]*videoEntry `json:"unmatched_uploads,omitempty"`
	LastChannelName  string                 `json:"last_channel_name,omitempty"`
}

// rawManifest preserves every match record verbatim (as raw JSON objects) so a
// write never drops a FIM-AV-owned field we don't model.
type rawManifest struct {
	Version int                          `json:"version"`
	Matches []map[string]json.RawMessage `json:"matches"`
}

// typedMatch is the subset we read: identity + teams + processing + our upload
// object. Extra fields in the file are ignored on read (they're preserved on
// write via rawManifest, not through this type).
type typedMatch struct {
	ID          string           `json:"id"`
	FileName    string           `json:"fileName"`
	FilePath    string           `json:"filePath"`
	Level       string           `json:"level"`
	MatchNumber int              `json:"matchNumber"`
	PlayNumber  int              `json:"playNumber"`
	EventCode   string           `json:"eventCode"`
	EndedAt     int64            `json:"endedAt"`
	Status      string           `json:"status"`
	HasCard     bool             `json:"hasCard"`
	Teams       *fimavTeams      `json:"teams,omitempty"`
	Score       *fimavScore      `json:"score,omitempty"`
	Processing  *fimavProcessing `json:"processing,omitempty"`
	Upload      *videoEntry      `json:"upload,omitempty"`
}

type typedManifest struct {
	Version int          `json:"version"`
	Matches []typedMatch `json:"matches"`
}

// stateStore owns one recording folder's manifest-backed state.
type stateStore struct {
	eventKey string
	mu       sync.Mutex
	state    eventState

	// cache of the parsed manifest for read-only helpers (fimavRecord/present),
	// keyed on the file's size+mtime so the 5s scan loop isn't re-parsing.
	cacheMu   sync.Mutex
	cacheSize int64
	cacheMod  int64
	cache     map[string]typedMatch
	cacheHad  bool
}

// openStateStore builds an in-memory state seeded with default config (FIM-AV
// pushes the real config) and loads any upload state already in the manifest.
func openStateStore(eventKey string) (*stateStore, error) {
	if settings.VideoDir == "" {
		return nil, fmt.Errorf("no recording folder set; pass -video-dir")
	}
	s := &stateStore{
		eventKey: eventKey,
		state: eventState{
			Config: eventConfig{
				EventKey:            eventKey,
				ProfileName:         defaultProfileName,
				Visibility:          defaultVisibility,
				TitleTemplate:       defaultTitleTemplate,
				DescriptionTemplate: defaultDescriptionTemplate,
				AutoSubmitTBA:       true,
				AutoSubmitTOA:       true,
				Headless:            true,
			},
			Videos:         map[string]*videoEntry{},
			ManualVideoIDs: map[string]string{},
		},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load reads the uploader's upload state out of the manifest's "upload" objects.
func (s *stateStore) load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := readTypedManifest()
	if err != nil {
		return err
	}
	for _, m := range doc.Matches {
		if m.FileName == "" || m.Upload == nil {
			continue
		}
		e := *m.Upload // copy; Meta is recomputed on scan (fillMetaFromFilename)
		e.Meta = nil
		s.state.Videos[m.FileName] = &e
	}
	// Settings (config/kv/manual ids) from the uploader-private file, if present.
	if data, err := os.ReadFile(uploaderStatePath()); err == nil && len(data) > 0 {
		var ss uploaderState
		// A settings file from before TOA support has no auto_submit_toa;
		// absent means the default, on.
		ss.Config.AutoSubmitTOA = true
		if json.Unmarshal(data, &ss) == nil {
			if ss.Config.ProfileName != "" || ss.Config.EventKey != "" {
				s.state.Config = ss.Config
				// A folder reused for a new event keeps its other settings, but
				// never the old event's playlist: last week's matches playlist
				// must not take this week's videos.
				if s.state.Config.EventKey != "" && s.state.Config.EventKey != s.eventKey {
					s.state.Config.PlaylistID = ""
					s.state.Config.PlaylistName = ""
				}
				if s.state.Config.EventKey == "" {
					s.state.Config.EventKey = s.eventKey
				}
			}
			if ss.ManualVideoIDs != nil {
				s.state.ManualVideoIDs = ss.ManualVideoIDs
			}
			s.state.NeedsReauth = ss.NeedsReauth
			s.state.LastChannelName = ss.LastChannelName
			// Hand-placed videos with no FIM-AV record live here, not in the
			// shared manifest. The manifest wins if the file later got a record.
			for name, e := range ss.UnmatchedUploads {
				if e == nil {
					continue
				}
				if _, ok := s.state.Videos[name]; !ok {
					cp := *e
					cp.Meta = nil
					s.state.Videos[name] = &cp
				}
			}
		}
	}
	// An entry still marked uploading was cut off by a crash or restart; no
	// upload is running at load time. Put it back in the queue as a failed
	// attempt so it retries with the normal backoff and attempt cap.
	for _, e := range s.state.Videos {
		if e.Status == statusUploading {
			e.Status = statusStable
			e.Attempts++
			e.NextAttempt = 0
			e.LastError = "upload interrupted"
			if e.Attempts >= maxAttempts {
				e.Status = statusFailed
			}
		}
	}
	return nil
}

// snapshot returns a deep copy of the in-memory state.
func (s *stateStore) snapshot() eventState {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.state)
	var out eventState
	_ = json.Unmarshal(data, &out)
	return out
}

// update mutates the in-memory state and writes the upload fields back to the
// manifest. Config/kv are in-memory only, so a mutation that only touches those
// still persists cheaply (no upload change => same file content on rename).
func (s *stateStore) update(fn func(*eventState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.state)
	return s.persistLocked()
}

// persistLocked writes each known video's upload object into its manifest record
// under a cross-process lock, preserving every FIM-AV-owned field. Caller holds
// s.mu (the in-process lock); the .lock file guards against FIM-AV writing at
// the same instant.
func (s *stateStore) persistLocked() error {
	// Videos split two ways: those with a FIM-AV record get their upload object
	// written into that record in the shared manifest; those without (hand-placed
	// files) are kept in our private state file. Never inject a record into the
	// shared manifest without FIM-AV's own fields — status/id absent there blanks
	// FIM-AV's Auto AV table.
	unmatched := map[string]*videoEntry{}

	if len(s.state.Videos) > 0 {
		release, err := acquireManifestLock()
		if err != nil {
			return err
		}
		defer release()

		raw, err := readRawManifest()
		if err != nil {
			return err
		}
		if raw.Version == 0 {
			raw.Version = 1
		}

		// Index existing records by fileName.
		idx := map[string]int{}
		for i, rec := range raw.Matches {
			if fn := rawString(rec, "fileName"); fn != "" {
				idx[fn] = i
			}
		}

		wrote := false
		for name, e := range s.state.Videos {
			cp := *e
			cp.Meta = nil
			i, ok := idx[name]
			if !ok {
				unmatched[name] = &cp
				continue
			}
			ub, err := json.Marshal(&cp)
			if err != nil {
				return fmt.Errorf("marshal upload %s: %w", name, err)
			}
			raw.Matches[i]["upload"] = json.RawMessage(ub)
			wrote = true
		}

		// Only touch the shared manifest when a real record took an upload object.
		if wrote {
			out, err := json.MarshalIndent(raw, "", "  ")
			if err != nil {
				return err
			}
			if err := writeFileAtomic(manifestPath(), out); err != nil {
				return err
			}
		}
	}

	// Settings + unmatched uploads -> the uploader-private file (atomic; no shared
	// lock needed, only this process writes it).
	ss := uploaderState{
		Config:           s.state.Config,
		ManualVideoIDs:   s.state.ManualVideoIDs,
		NeedsReauth:      s.state.NeedsReauth,
		LastChannelName:  s.state.LastChannelName,
		UnmatchedUploads: unmatched,
	}
	if b, err := json.MarshalIndent(ss, "", "  "); err == nil {
		_ = writeFileAtomic(uploaderStatePath(), b)
	}
	return nil
}

// fimavRecord returns the FIM-AV-owned fields for one file, from the manifest
// (cached by size+mtime). ok is false when there is no record yet.
func (s *stateStore) fimavRecord(filename string) (fimavRecord, bool) {
	byName, _ := s.manifestCache()
	m, ok := byName[filename]
	if !ok {
		return fimavRecord{}, false
	}
	return fimavRecord{
		ID:         m.ID,
		FileName:   m.FileName,
		FilePath:   m.FilePath,
		EndedAt:    m.EndedAt,
		Status:     m.Status,
		HasCard:    m.HasCard,
		EventCode:  m.EventCode,
		Teams:      m.Teams,
		Score:      m.Score,
		Processing: m.Processing,
	}, true
}

// fimavPresent reports whether FIM-AV is managing this folder — i.e. any record
// carries recording data (status/teams/processing). Until it does, the cut-hold
// gate stays off.
func (s *stateStore) fimavPresent() bool {
	byName, present := s.manifestCache()
	if !present {
		return false
	}
	for _, m := range byName {
		if m.Status != "" || m.Teams != nil || m.Processing != nil {
			return true
		}
	}
	return false
}

// manifestCache returns the manifest records by fileName, reparsing only when the
// file's size+mtime changed. The second return is whether the file exists.
func (s *stateStore) manifestCache() (map[string]typedMatch, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	info, err := os.Stat(manifestPath())
	if err != nil {
		s.cache = nil
		s.cacheHad = false
		return map[string]typedMatch{}, false
	}
	sz, mt := info.Size(), info.ModTime().UnixNano()
	if s.cache != nil && s.cacheSize == sz && s.cacheMod == mt {
		return s.cache, s.cacheHad
	}
	doc, err := readTypedManifest()
	byName := map[string]typedMatch{}
	if err == nil {
		for _, m := range doc.Matches {
			if m.FileName != "" {
				byName[m.FileName] = m
			}
		}
	}
	s.cache = byName
	s.cacheSize, s.cacheMod, s.cacheHad = sz, mt, true
	return byName, true
}

func (s *stateStore) close() error { return nil }

// ── manifest I/O ─────────────────────────────────────────────────────────────

func readTypedManifest() (typedManifest, error) {
	var doc typedManifest
	data, err := os.ReadFile(manifestPath())
	if err != nil {
		if os.IsNotExist(err) {
			return typedManifest{Version: 1}, nil
		}
		return doc, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) == 0 {
		return typedManifest{Version: 1}, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("parse manifest: %w", err)
	}
	return doc, nil
}

func readRawManifest() (rawManifest, error) {
	var raw rawManifest
	data, err := os.ReadFile(manifestPath())
	if err != nil {
		if os.IsNotExist(err) {
			return rawManifest{Version: 1}, nil
		}
		return raw, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) == 0 {
		return rawManifest{Version: 1}, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return raw, fmt.Errorf("parse manifest: %w", err)
	}
	return raw, nil
}

func rawString(rec map[string]json.RawMessage, key string) string {
	if v, ok := rec[key]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
	}
	return ""
}

func jsonRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// acquireManifestLock takes a cross-process advisory lock via an exclusive
// .lock file. FIM-AV Assistant uses the same lock file + protocol. A lock older
// than the stale timeout is stolen, so a crashed holder can't wedge writes.
func acquireManifestLock() (func(), error) {
	lock := manifestPath() + ".lock"
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if info, e := os.Stat(lock); e == nil && time.Since(info.ModTime()) > 10*time.Second {
			_ = os.Remove(lock) // steal a stale lock
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("manifest lock busy")
		}
		time.Sleep(40 * time.Millisecond)
	}
}
