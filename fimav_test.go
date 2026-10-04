package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openStoreFor opens a store on a specific recording folder, standing in for the
// folder the uploader watches.
func openStoreFor(t *testing.T, dir string) *stateStore {
	t.Helper()
	old := settings.VideoDir
	settings.VideoDir = dir
	t.Cleanup(func() { settings.VideoDir = old })
	s, err := openStateStore("2026test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	return s
}

// writeFimavRec simulates FIM-AV Assistant writing its recording/team columns
// into the shared database's matches table. It touches only FIM-AV-owned
// columns, exactly as the migrated FIM-AV match store will.
func writeFimavRec(t *testing.T, _ *stateStore, rec fimavRecord) {
	t.Helper()
	doc, err := readTypedManifest()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range doc.Matches {
		if doc.Matches[i].FileName == rec.FileName {
			doc.Matches[i].FilePath = rec.FilePath
			doc.Matches[i].Status = rec.Status
			doc.Matches[i].HasCard = rec.HasCard
			doc.Matches[i].EndedAt = rec.EndedAt
			doc.Matches[i].Teams = rec.Teams
			doc.Matches[i].Processing = rec.Processing
			found = true
			break
		}
	}
	if !found {
		doc.Matches = append(doc.Matches, typedMatch{
			ID: rec.ID, FileName: rec.FileName, FilePath: rec.FilePath,
			Status: rec.Status, HasCard: rec.HasCard, EndedAt: rec.EndedAt,
			Teams: rec.Teams, Processing: rec.Processing,
		})
	}
	if doc.Version == 0 {
		doc.Version = 1
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(manifestPath(), b); err != nil {
		t.Fatal(err)
	}
}

func recorded(name string, endedAgo time.Duration) fimavRecord {
	return fimavRecord{
		ID:       name,
		FileName: name,
		FilePath: filepath.Join("C:\\AV", name),
		EndedAt:  time.Now().Add(-endedAgo).UnixMilli(),
		Status:   "recorded",
	}
}

func TestCutHold(t *testing.T) {
	cfg := eventConfig{}
	name := "QM5_MIKET.mp4"

	t.Run("no recording data", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		if hold, _ := cutHold(s, name, cfg); hold {
			t.Error("held with no FIM-AV recording data present")
		}
	})

	t.Run("file not recorded", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		writeFimavRec(t, s, recorded("QM1_MIKET.mp4", time.Hour))
		if hold, _ := cutHold(s, name, cfg); hold {
			t.Error("held a file with no record of its own")
		}
	})

	t.Run("still recording", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		rec := recorded(name, 0)
		rec.Status = "recording"
		writeFimavRec(t, s, rec)
		if hold, reason := cutHold(s, name, cfg); !hold || reason != "still recording" {
			t.Errorf("hold=%v reason=%q", hold, reason)
		}
	})

	t.Run("cut queued and running", func(t *testing.T) {
		for state, wantReason := range map[string]string{
			"queued":     "cut queued",
			"processing": "cut running",
		} {
			s := openStoreFor(t, t.TempDir())
			rec := recorded(name, time.Hour)
			rec.Processing = &fimavProcessing{State: state}
			writeFimavRec(t, s, rec)
			if hold, reason := cutHold(s, name, cfg); !hold || reason != wantReason {
				t.Errorf("%s: hold=%v reason=%q", state, hold, reason)
			}
		}
	})

	t.Run("cut done", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		rec := recorded(name, time.Hour)
		rec.Processing = &fimavProcessing{State: "done", OutputPath: rec.FilePath}
		writeFimavRec(t, s, rec)
		if hold, _ := cutHold(s, name, cfg); hold {
			t.Error("held a finished cut")
		}
	})

	t.Run("cut failed uploads the raw video", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		rec := recorded(name, time.Hour)
		rec.Processing = &fimavProcessing{State: "error", Error: "ffmpeg exited 1"}
		writeFimavRec(t, s, rec)
		if hold, _ := cutHold(s, name, cfg); hold {
			t.Error("held a failed cut instead of uploading the original")
		}
	})

	t.Run("carded match is never cut", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		rec := recorded(name, 0)
		rec.HasCard = true
		writeFimavRec(t, s, rec)
		if hold, _ := cutHold(s, name, cfg); hold {
			t.Error("held a carded match, which FIM-AV never cuts")
		}
	})

	t.Run("grace window", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		writeFimavRec(t, s, recorded(name, 5*time.Second))
		if hold, reason := cutHold(s, name, cfg); !hold || reason != "waiting for cut" {
			t.Errorf("inside grace: hold=%v reason=%q", hold, reason)
		}

		s2 := openStoreFor(t, t.TempDir())
		writeFimavRec(t, s2, recorded(name, (defaultCutWaitSeconds+10)*time.Second))
		if hold, _ := cutHold(s2, name, cfg); hold {
			t.Error("still holding after the grace window; no cut was coming")
		}
	})

	t.Run("gate switched off", func(t *testing.T) {
		s := openStoreFor(t, t.TempDir())
		rec := recorded(name, 0)
		rec.Processing = &fimavProcessing{State: "processing"}
		writeFimavRec(t, s, rec)
		if hold, _ := cutHold(s, name, eventConfig{CutWaitSeconds: -1}); hold {
			t.Error("held despite cut_wait_seconds = -1")
		}
	})
}

// The whole point: a stable raw recording must not be promoted to "stable"
// (and therefore uploaded) while FIM-AV Assistant is still cutting it.
func TestScanHoldsFileUntilCutFinishes(t *testing.T) {
	t.Setenv("YT_TBA_UPLOAD_DATA_DIR", t.TempDir())
	videoDir := t.TempDir()

	name := "QM5_MIKET.mp4"
	path := filepath.Join(videoDir, name)
	if err := os.WriteFile(path, []byte("raw recording"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreFor(t, videoDir)
	m := newUploadManager(store, nil)

	rec := recorded(name, 2*time.Second)
	rec.Processing = &fimavProcessing{State: "queued"}
	writeFimavRec(t, store, rec)

	// First scan discovers the file; backdate the stability timer so the next
	// scan is past stableDelay without the test having to wait for it.
	m.scanNow()
	if err := store.update(func(s *eventState) {
		s.Videos[name].StableSince = nowUnix() - int64(stableDelay/time.Second) - 1
	}); err != nil {
		t.Fatal(err)
	}

	m.scanNow()
	if got := store.snapshot().Videos[name]; got.Status != statusCutting {
		t.Fatalf("status = %q (%s), want cutting", got.Status, got.HoldReason)
	}
	if _, ok := m.pickNext(); ok {
		t.Fatal("picked a file that is still being cut")
	}

	// The cut finishes: ffmpeg wrote a new file in place, so size and mtime
	// changed, and the record now says done.
	if err := os.WriteFile(path, []byte("trimmed cut, shorter"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec.Processing = &fimavProcessing{State: "done"}
	writeFimavRec(t, store, rec)

	m.scanNow() // notices the change, resets the timer
	if got := store.snapshot().Videos[name].Status; got != statusNew {
		t.Fatalf("status = %q, want new after the file changed", got)
	}
	if err := store.update(func(s *eventState) {
		s.Videos[name].StableSince = nowUnix() - int64(stableDelay/time.Second) - 1
	}); err != nil {
		t.Fatal(err)
	}
	m.scanNow()

	got := store.snapshot().Videos[name]
	if got.Status != statusStable {
		t.Fatalf("status = %q (%s), want stable", got.Status, got.HoldReason)
	}
	if got.HoldReason != "" {
		t.Errorf("hold reason = %q, want empty", got.HoldReason)
	}
	if got.Size != int64(len("trimmed cut, shorter")) {
		t.Errorf("size = %d, want the cut's size", got.Size)
	}
}

// A cut that lands after we already published leaves YouTube holding the raw
// video. We can't unpublish it, but the operator has to be able to see it.
func TestScanFlagsChangeAfterUpload(t *testing.T) {
	t.Setenv("YT_TBA_UPLOAD_DATA_DIR", t.TempDir())
	videoDir := t.TempDir()

	name := "QM5_MIKET.mp4"
	path := filepath.Join(videoDir, name)
	if err := os.WriteFile(path, []byte("raw recording"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStoreFor(t, videoDir)
	m := newUploadManager(store, nil)
	m.scanNow()
	if err := store.update(func(s *eventState) {
		v := s.Videos[name]
		v.Status = statusUploaded
		v.YTVideoID = "abc11char23"
	}); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("trimmed cut"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.scanNow()

	got := store.snapshot().Videos[name]
	if got.Status != statusUploaded {
		t.Errorf("status = %q, want uploaded (entries stay immutable)", got.Status)
	}
	if !got.ChangedAfterUpload {
		t.Error("changed_after_upload not set; the raw video is on YouTube silently")
	}
}

// An entry left "uploading" by a crash or restart goes back in the queue on
// load, counted as one failed attempt.
func TestLoadRequeuesInterruptedUpload(t *testing.T) {
	dir := t.TempDir()
	ss := uploaderState{
		Config: eventConfig{EventKey: "2026test"},
		UnmatchedUploads: map[string]*videoEntry{
			"a.mp4": {Status: statusUploading, Attempts: 1, NextAttempt: 99},
			"b.mp4": {Status: statusUploading, Attempts: maxAttempts - 1},
		},
	}
	data, _ := json.Marshal(ss)
	if err := os.WriteFile(filepath.Join(dir, uploaderStateFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStoreFor(t, dir)
	st := s.snapshot()
	a, b := st.Videos["a.mp4"], st.Videos["b.mp4"]
	if a.Status != statusStable || a.Attempts != 2 || a.NextAttempt != 0 || a.LastError == "" {
		t.Fatalf("a: %+v", a)
	}
	if b.Status != statusFailed {
		t.Fatalf("b: %+v", b)
	}
}
