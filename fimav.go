package main

// FIM-AV Assistant's recording data.
//
// FIM-AV Assistant records each match and writes its recording state — status,
// whether the match was carded, when it ended, the alliance teams, and the
// dead-time cut's processing state — into the shared database's `matches` table
// (see db.go). It used to keep this in a fimav-matches.json manifest in the
// recording folder; that manifest is retired in favour of the database, so both
// ends read and write one store.
//
// This matters to the uploader because the cut is done IN PLACE: the raw
// recording is moved to Originals/ and ffmpeg writes the trimmed video back
// under the original name. A file on disk therefore tells us nothing about
// whether it is the raw recording or the finished cut, and once an entry is
// uploaded we never touch it again. Without the recording state we would race
// the encoder and publish raw videos with all the dead time in them.

// fimavManifest is the legacy manifest filename. It is no longer read for data
// (the database replaced it), but it still marks a folder as a FIM-AV event
// folder for auto-detection during the rollout, alongside the database file.
const fimavManifest = "fimav-matches.json"

// fimavProcessing mirrors MatchProcessing in FIM-AV's src/models/MatchRecord.ts.
type fimavProcessing struct {
	State      string `json:"state"` // unprocessed | queued | processing | done | error
	OutputPath string `json:"outputPath,omitempty"`
	Error      string `json:"error,omitempty"`
}

// fimavTeam is one team on an alliance. teamName may be empty on older records,
// so treat it as optional and fall back to the FMS roster (see alliances.go).
type fimavTeam struct {
	TeamNumber int    `json:"teamNumber"`
	TeamName   string `json:"teamName"`
	Card       string `json:"card,omitempty"`
}

// fimavTeams is the per-match alliance breakdown from the FMS results call.
type fimavTeams struct {
	Red  []fimavTeam `json:"red"`
	Blue []fimavTeam `json:"blue"`
}

// fimavScore is the final alliance totals FIM-AV captured from FMS results.
type fimavScore struct {
	Red  int `json:"red"`
	Blue int `json:"blue"`
}

// fimavRecord is the subset of a match record the uploader reads. It is loaded
// from the database's FIM-AV-owned columns (db.go: fimavRecord).
type fimavRecord struct {
	ID         string           `json:"id"`
	FileName   string           `json:"fileName"`
	FilePath   string           `json:"filePath"`
	EndedAt    int64            `json:"endedAt"` // epoch ms
	Status     string           `json:"status"`  // recording | recorded | error
	HasCard    bool             `json:"hasCard"`
	Teams      *fimavTeams      `json:"teams,omitempty"`
	Score      *fimavScore      `json:"score,omitempty"`
	Processing *fimavProcessing `json:"processing,omitempty"`
}

// defaultCutWaitSeconds is how long we give FIM-AV Assistant to queue a cut
// after a recording stops, before deciding no cut is coming. Auto-cut is queued
// within a second or two of the recording stopping, so this only delays uploads
// when cutting is switched off entirely.
const defaultCutWaitSeconds = 120

// cutWaitSeconds resolves the configured grace period. 0 means "use the
// default"; a negative value switches the whole gate off.
func cutWaitSeconds(cfg eventConfig) int64 {
	if cfg.CutWaitSeconds == 0 {
		return defaultCutWaitSeconds
	}
	return int64(cfg.CutWaitSeconds)
}

// cutHold reports whether a file should be held back from upload because
// FIM-AV Assistant is going to replace it with a trimmed cut, plus a short
// reason for the log. The recording state comes from the shared database.
//
// It holds when the record says the cut is queued or running, and also during a
// grace window after the recording stops, since a queued cut isn't visible in
// the record until the metadata fetch that precedes it finishes.
func cutHold(store *stateStore, filename string, cfg eventConfig) (bool, string) {
	wait := cutWaitSeconds(cfg)
	if wait < 0 {
		return false, ""
	}
	// Until FIM-AV is writing recording state into the database, the gate stays
	// off — the same as when no manifest existed.
	if !store.fimavPresent() {
		return false, ""
	}
	rec, ok := store.fimavRecord(filename)
	if !ok {
		// Not a FIM-AV recording (hand-placed file, or an older event's video
		// copied in). Leave it alone.
		return false, ""
	}
	if rec.Status == "recording" {
		return true, "still recording"
	}
	// A carded match is never cut: the card explanation lives in the dead time
	// the cut would remove. The raw file is final.
	if rec.HasCard {
		return false, ""
	}
	state := ""
	if rec.Processing != nil {
		state = rec.Processing.State
	}
	switch state {
	case "queued":
		return true, "cut queued"
	case "processing":
		return true, "cut running"
	case "done":
		return false, ""
	case "error":
		// The cut failed and the original was restored. Upload the raw video
		// rather than nothing.
		return false, ""
	}
	// No processing state yet: either a cut is about to be queued, or cutting is
	// switched off and this file is already final. Wait out the grace window to
	// tell the two apart.
	if rec.EndedAt > 0 && nowUnix()-rec.EndedAt/1000 >= wait {
		return false, ""
	}
	return true, "waiting for cut"
}
