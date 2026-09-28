package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateStoreRoundtrip(t *testing.T) {
	videoDir := t.TempDir()
	old := settings.VideoDir
	settings.VideoDir = videoDir
	defer func() { settings.VideoDir = old }()

	s, err := openStateStore("2026mitt")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.snapshot().Config.EventKey != "2026mitt" {
		t.Fatalf("event key not set")
	}
	if err := s.update(func(es *eventState) {
		es.Config.ProfileName = "scratch"
		es.Config.PlaylistName = "Test"
		es.Videos["foo.mp4"] = &videoEntry{Size: 100, Status: statusStable}
		es.ManualVideoIDs["qm5"] = "abc11char23"
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	_ = s.close()

	// Reopen and confirm persistence.
	s2, err := openStateStore("2026mitt")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.close()
	got := s2.snapshot()
	if got.Config.ProfileName != "scratch" || got.Config.PlaylistName != "Test" {
		t.Errorf("config not persisted: %+v", got.Config)
	}
	if got.Videos["foo.mp4"].Size != 100 || got.Videos["foo.mp4"].Status != statusStable {
		t.Errorf("videos not persisted: %+v", got.Videos)
	}
	if got.ManualVideoIDs["qm5"] != "abc11char23" {
		t.Errorf("manual ids not persisted: %+v", got.ManualVideoIDs)
	}

	// foo.mp4 has no FIM-AV record, so its upload state persists in the
	// uploader-private file and NOT as a skeleton record in the shared manifest
	// (a record without FIM-AV's status/id blanks its Auto AV table).
	if _, err := os.Stat(filepath.Join(videoDir, uploaderStateFileName)); err != nil {
		t.Errorf("uploader state file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(videoDir, manifestFileName)); err == nil {
		doc, err := readTypedManifest()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range doc.Matches {
			if m.FileName == "foo.mp4" {
				t.Errorf("unmatched video injected a skeleton manifest record: %+v", m)
			}
		}
	}
}

// A video that DOES have a FIM-AV record gets its upload object written into
// that record in the shared manifest, and it round-trips from there.
func TestMatchedVideoWritesUploadToManifest(t *testing.T) {
	dir := t.TempDir()
	s := openStoreFor(t, dir)
	writeFimavRec(t, s, recorded("QM5_MIKET.mp4", time.Hour))

	if err := s.update(func(es *eventState) {
		es.Videos["QM5_MIKET.mp4"] = &videoEntry{Status: statusUploaded, YTVideoID: "vid12345678"}
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	doc, err := readTypedManifest()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range doc.Matches {
		if m.FileName == "QM5_MIKET.mp4" {
			found = true
			if m.Status != "recorded" {
				t.Errorf("FIM-AV status clobbered: %q", m.Status)
			}
			if m.Upload == nil || m.Upload.YTVideoID != "vid12345678" {
				t.Errorf("upload object not written: %+v", m.Upload)
			}
		}
	}
	if !found {
		t.Error("record vanished from manifest")
	}
}

func TestStateStoreSnapshotIsCopy(t *testing.T) {
	videoDir := t.TempDir()
	old := settings.VideoDir
	settings.VideoDir = videoDir
	defer func() { settings.VideoDir = old }()
	s, err := openStateStore("ev")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	_ = s.update(func(es *eventState) {
		es.Videos["a.mp4"] = &videoEntry{Status: statusStable}
	})
	snap := s.snapshot()
	snap.Videos["a.mp4"].Status = "tampered"
	// The internal state must not have been mutated by the caller's edit.
	if s.snapshot().Videos["a.mp4"].Status == "tampered" {
		t.Fatal("snapshot leaked a live reference")
	}
}

// A configured live browser profile has to reach the driver, otherwise uploads
// run in the tool's own profile and ask for a second sign-in.
func TestBrowserProfileFromConfig(t *testing.T) {
	cfg := eventConfig{
		ProfileName:             "tornado-tumble",
		BrowserUserDataDir:      `C:\Users\filip\AppData\Local\Google\Chrome\User Data`,
		BrowserProfileDirectory: "Profile 2",
		BrowserDebugPort:        9222,
		BrowserExe:              `C:\Program Files\Google\Chrome\Application\chrome.exe`,
	}
	p := browserProfile(cfg)
	if !p.Live() {
		t.Fatal("configured user-data dir did not produce a live profile")
	}
	if p.Directory != "Profile 2" || p.DebugPort != 9222 || p.Exe != cfg.BrowserExe {
		t.Errorf("profile = %+v", p)
	}
	if p.Name != "tornado-tumble" {
		t.Errorf("profile name = %q", p.Name)
	}

	// No browser settings: fall back to the tool's own profile.
	p = browserProfile(eventConfig{ProfileName: "tornado-tumble"})
	if p.Live() {
		t.Error("empty user-data dir produced a live profile")
	}
}

// Login and channel-check requests without an event key still work against a
// tool-owned profile.
func TestProfileForRequestFallsBackToName(t *testing.T) {
	p := profileForRequest("", "tornado-tumble")
	if p.Live() || p.Name != "tornado-tumble" {
		t.Errorf("profile = %+v", p)
	}
}

func TestBrowserProfileDirs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Default", "Profile 2"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "Preferences"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Not a profile: no Preferences file.
	if err := os.MkdirAll(filepath.Join(dir, "ShaderCache"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := browserProfileDirs(dir)
	if len(got) != 2 || got[0] != "Default" || got[1] != "Profile 2" {
		t.Errorf("profile dirs = %v", got)
	}
	if len(browserProfileDirs("")) != 0 {
		t.Error("empty user-data dir should list nothing")
	}
}

// Visibility is a setting, and anything unrecognised must not publish.
func TestUploadVisibility(t *testing.T) {
	cases := map[string]string{
		"":         "UNLISTED",
		"unlisted": "UNLISTED",
		"UNLISTED": "UNLISTED",
		" public ": "PUBLIC",
		"Public":   "PUBLIC",
		"private":  "PRIVATE",
		"nonsense": "UNLISTED",
	}
	for in, want := range cases {
		if got := uploadVisibility(eventConfig{Visibility: in}); got != want {
			t.Errorf("uploadVisibility(%q) = %q, want %q", in, got, want)
		}
	}
	// A fresh event starts unlisted.
	videoDir := t.TempDir()
	old := settings.VideoDir
	settings.VideoDir = videoDir
	defer func() { settings.VideoDir = old }()
	s, err := openStateStore("2026mibr")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if got := s.snapshot().Config.Visibility; got != "UNLISTED" {
		t.Errorf("new event visibility = %q, want UNLISTED", got)
	}
}
