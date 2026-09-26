package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Status values used for entries in state.videos.
const (
	statusNew       = "new"     // discovered, still being written to disk
	statusCutting   = "cutting" // FIM-AV Assistant is trimming it; hold the upload
	statusStable    = "stable"  // size+mtime stable, ready to upload
	statusUploading = "uploading"
	statusUploaded  = "uploaded"
	statusFailed    = "failed"
	statusSkipped   = "skipped"
)

// eventConfig is the user-editable per-event configuration.
//
// Field tags follow the JSON shape from tba-uploader-yt-upload-plan.md so the
// file on disk stays stable and human-editable.
type eventConfig struct {
	EventKey    string `json:"event_key"`
	EventName   string `json:"event_name"`
	ProfileName string `json:"profile_name"`
	// PlaylistID is the stable PL... id and is authoritative for adding videos
	// to the playlist (resolved to its current title at upload time, so a
	// rename can't break the add). PlaylistName is kept for display and as a
	// fallback for configs saved before playlist_id existed.
	PlaylistID          string `json:"playlist_id"`
	PlaylistName        string `json:"playlist_name"`
	TitleTemplate       string `json:"title_template"`
	DescriptionTemplate string `json:"description_template"`
	ThumbnailPath       string `json:"thumbnail_path"`
	// Visibility is PUBLIC, UNLISTED or PRIVATE. Empty means unlisted.
	Visibility string `json:"visibility,omitempty"`
	// Headless runs the upload browser hidden (default). Turned off, uploads run
	// in a visible window. Sign-in is always headed regardless of this.
	Headless bool `json:"headless"`
	// Browser* point the driver at an installed browser's own profile instead
	// of a profile this tool owns, so there is no second YouTube sign-in.
	// BrowserUserDataDir is the browser's "User Data" folder,
	// BrowserProfileDirectory the profile inside it ("Default", "Profile 2"),
	// BrowserDebugPort the CDP port to attach to when the browser is already
	// running, and BrowserExe an explicit browser binary.
	BrowserUserDataDir      string `json:"browser_user_data_dir,omitempty"`
	BrowserProfileDirectory string `json:"browser_profile_directory,omitempty"`
	BrowserDebugPort        int    `json:"browser_debug_port,omitempty"`
	BrowserExe              string `json:"browser_exe,omitempty"`
	// CutWaitSeconds is the grace period given to FIM-AV Assistant to queue a
	// dead-time cut before a recording is treated as final. 0 uses the default
	// (see defaultCutWaitSeconds); a negative value uploads recordings as soon
	// as they are stable, cut or not.
	CutWaitSeconds int `json:"cut_wait_seconds,omitempty"`
	// AutoSubmitTBA, when set, posts each finished upload's YouTube URL to the
	// match on The Blue Alliance (trusted match_videos/add) as part of the
	// upload flow. Needs TBAAuthID + TBASecret; does nothing without them.
	AutoSubmitTBA bool `json:"auto_submit_tba"`
	// TBAAuthID / TBASecret are the event's trusted-API credentials. The root
	// TBA-uploader binary takes these per-request from the web UI; the sidecar
	// holds them in config so it can submit without that binary running. They
	// live only in state.json under the OS data dir, never in the repo.
	TBAAuthID string `json:"tba_auth_id,omitempty"`
	TBASecret string `json:"tba_secret,omitempty"`
}

// allianceTeam is one team's data inside a match's red or blue alliance.
type allianceTeam struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
}

// videoMeta is everything the upload pipeline needs that isn't derivable
// from the filename. Sent in by the Vue layer via /api/rename.
type videoMeta struct {
	TBAMatchKey string                    `json:"tba_match_key"`
	MatchLevel  string                    `json:"match_level"`
	MatchNumber int                       `json:"match_number"`
	MatchLabel  string                    `json:"match_label"`
	Play        int                       `json:"play"`
	Alliances   map[string][]allianceTeam `json:"alliances,omitempty"`
}

// videoEntry is one row in state.videos.
type videoEntry struct {
	Size        int64  `json:"size"`
	Mtime       int64  `json:"mtime"`
	Status      string `json:"status"`
	StableSince int64  `json:"stable_since,omitempty"`
	YTVideoID   string `json:"yt_video_id,omitempty"`
	TitleUsed   string `json:"title_used,omitempty"`
	UploadedAt  string `json:"uploaded_at,omitempty"`
	Attempts    int    `json:"attempts"`
	NextAttempt int64  `json:"next_attempt,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// Warnings are non-fatal problems from an upload or backfill that the video
	// survived but the operator should see: a description that would not set, a
	// playlist add that failed. Kept separate from LastError (which drives retry
	// state) so they stay visible on an otherwise-uploaded video.
	Warnings []string `json:"warnings,omitempty"`
	// HoldReason explains a "cutting" status, e.g. "cut running".
	HoldReason string `json:"hold_reason,omitempty"`
	// ChangedAfterUpload is set when the file on disk changed after we
	// published it, which means YouTube has the raw recording and the cut only
	// exists locally. The entry stays uploaded; this is here so the operator
	// can see it and re-upload by hand.
	ChangedAfterUpload bool       `json:"changed_after_upload,omitempty"`
	Meta               *videoMeta `json:"meta,omitempty"`
	// TBASubmitted is set once the video's URL has been posted to its TBA match.
	// TBASubmitError holds the last submit failure so the tab can show it and
	// offer a retry; it is cleared on a successful submit.
	TBASubmitted   bool   `json:"tba_submitted,omitempty"`
	TBASubmitError string `json:"tba_submit_error,omitempty"`
}

// eventState is the sidecar's in-memory view. It is persisted to the shared
// SQLite database (see db.go); the Videos map is projected onto the sidecar's
// columns of the `matches` table. The json tags are what the HTTP API serializes
// to the Upload tab, and are unchanged from the JSON-file days.
type eventState struct {
	Config          eventConfig            `json:"config"`
	Videos          map[string]*videoEntry `json:"videos"`
	ManualVideoIDs  map[string]string      `json:"manual_video_ids"`
	NeedsReauth     bool                   `json:"needs_reauth"`
	LastChannelName string                 `json:"last_channel_name,omitempty"`
}

// dataRoot returns %LOCALAPPDATA%\youtube-tba-upload on Windows, otherwise
// $XDG_DATA_HOME/youtube-tba-upload (or ~/.local/share/youtube-tba-upload). On
// any OS it honours YT_TBA_UPLOAD_DATA_DIR for tests. This holds only tool-local
// data (browser profiles, the managed Chrome, the playlist cache, logs, the
// remembered folder) — never the shared upload tracking, which lives in the
// database beside the recordings.
func dataRoot() string {
	if v := os.Getenv("YT_TBA_UPLOAD_DATA_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "youtube-tba-upload")
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "youtube-tba-upload")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "youtube-tba-upload")
}

func profileDir(profileName string) string {
	return filepath.Join(dataRoot(), "profiles", profileName)
}

// listProfiles returns the names of all existing profile directories.
func listProfiles() ([]string, error) {
	root := filepath.Join(dataRoot(), "profiles")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// nowUnix returns the current time as a unix timestamp.
func nowUnix() int64 {
	return time.Now().Unix()
}

// nowRFC3339 is the timestamp format written to the database's updated_at.
func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
