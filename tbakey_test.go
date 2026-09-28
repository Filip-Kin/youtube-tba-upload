package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTbaMatchKey(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		// FIM-AV in-season names
		{"QM5_MIKET.mp4", "qm5"},
		{"QM12_P2_MIKET.mp4", "qm12"}, // a replay is still the same match
		{"SF3M1_MIKET.mp4", "sf3m1"},
		{"SF13M1_MIKET.mp4", "sf13m1"},
		{"F1M2_MIKET.mp4", "f1m2"},
		// TBA-uploader's own names
		{"2026 FSU Roboday Qualification Match 7.mp4", "qm7"},
		{"2026 FSU Roboday Playoff Match 4.mp4", "sf4m1"},
		{"2026 FSU Roboday Final Match 1.mp4", "f1m1"},
		// Not matches TBA tracks
		{"zz_PR1_MIKET.mp4", ""},
		{"zz_TM1_MIKET.mp4", ""},
		{"2026 FSU Roboday Practice Match 2.mp4", ""},
		{"random recording.mp4", ""},
	}
	for _, c := range cases {
		p, ok := parseFilename(c.filename)
		if !ok {
			if c.want != "" {
				t.Errorf("%q did not parse", c.filename)
			}
			continue
		}
		if got := tbaMatchKey(p); got != c.want {
			t.Errorf("tbaMatchKey(%q) = %q, want %q", c.filename, got, c.want)
		}
	}

	// Playoff numbering past the bracket's elimination matches becomes finals,
	// which is how FMS numbers them. Both the key and the human label follow.
	for _, c := range []struct {
		filename  string
		wantKey   string
		wantLabel string
	}{
		{"2026 FSU Roboday Playoff Match 14.mp4", "f1m1", "Final 1"},
		{"2026 FSU Roboday Playoff Match 15.mp4", "f1m2", "Final 2"},
		{"2026 FSU Roboday Playoff Match 16.mp4", "f1m3", "Final 3"},
		{"2026 FSU Roboday Playoff Match 7.mp4", "sf7m1", "Playoff 7"},
	} {
		p, ok := parseFilename(c.filename)
		if !ok {
			t.Errorf("%q did not parse", c.filename)
			continue
		}
		if got := tbaMatchKey(p); got != c.wantKey {
			t.Errorf("tbaMatchKey(%q) = %q, want %q", c.filename, got, c.wantKey)
		}
		if got := p.matchLabel(); got != c.wantLabel {
			t.Errorf("matchLabel(%q) = %q, want %q", c.filename, got, c.wantLabel)
		}
	}

	// A junk/overtime number with no bracket slot stays a playoff and gets no
	// key, so it is never auto-uploaded with a wrong label.
	if p, ok := parseFilename("2026 FSU Roboday Playoff Match 999.mp4"); ok {
		if got := tbaMatchKey(p); got != "" {
			t.Errorf("playoff 999 key = %q, want empty", got)
		}
		if got := p.Level; got != "Playoff" {
			t.Errorf("playoff 999 level = %q, want Playoff", got)
		}
	}
}

func TestFillMetaFromFilename(t *testing.T) {
	entry := &videoEntry{}
	fillMetaFromFilename(entry, "QM5_MIKET.mp4")
	if entry.Meta == nil || entry.Meta.TBAMatchKey != "qm5" {
		t.Fatalf("meta = %+v", entry.Meta)
	}
	if entry.Meta.MatchLabel != "Qualification 5" || entry.Meta.MatchNumber != 5 || entry.Meta.Play != 1 {
		t.Errorf("meta = %+v", entry.Meta)
	}

	// Richer meta from /api/rename must not be overwritten.
	existing := &videoEntry{Meta: &videoMeta{
		TBAMatchKey: "qm9",
		MatchLabel:  "Qualification 9",
		Alliances:   map[string][]allianceTeam{"red": {{Number: 2767, Name: "Stryke Force"}}},
	}}
	fillMetaFromFilename(existing, "QM5_MIKET.mp4")
	if existing.Meta.TBAMatchKey != "qm9" || len(existing.Meta.Alliances) != 1 {
		t.Errorf("existing meta was disturbed: %+v", existing.Meta)
	}

	// A practice recording gets no key, and no empty meta object either.
	practice := &videoEntry{}
	fillMetaFromFilename(practice, "zz_PR1_MIKET.mp4")
	if practice.Meta != nil {
		t.Errorf("practice meta = %+v, want nil", practice.Meta)
	}
}

// A scan must give every match video its TBA key, since that is what the UI
// links and submits on.
func TestScanFillsTbaMatchKey(t *testing.T) {
	t.Setenv("YT_TBA_UPLOAD_DATA_DIR", t.TempDir())
	videoDir := t.TempDir()

	oldVideoDir := settings.VideoDir
	settings.VideoDir = videoDir
	defer func() { settings.VideoDir = oldVideoDir }()

	for _, name := range []string{"QM5_MIKET.mp4", "zz_PR1_MIKET.mp4"} {
		if err := os.WriteFile(filepath.Join(videoDir, name), []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := openStateStore("2026fsu")
	if err != nil {
		t.Fatal(err)
	}
	m := newUploadManager(store, nil)
	m.scanNow()
	time.Sleep(10 * time.Millisecond)

	videos := store.snapshot().Videos
	qm := videos["QM5_MIKET.mp4"]
	if qm == nil || qm.Meta == nil || qm.Meta.TBAMatchKey != "qm5" {
		t.Errorf("qualification entry meta = %+v", qm)
	}
	if pr := videos["zz_PR1_MIKET.mp4"]; pr != nil && pr.Meta != nil {
		t.Errorf("practice entry got meta %+v", pr.Meta)
	}
}

// A key with an event prefix is what TBA rejects, and earlier builds wrote them
// into the state file, so a scan has to repair them rather than keep them.
func TestFillMetaRepairsPrefixedKey(t *testing.T) {
	entry := &videoEntry{Meta: &videoMeta{
		TBAMatchKey: "2026mibr_qm1",
		MatchLabel:  "Qualification 1",
	}}
	fillMetaFromFilename(entry, "QM1_MIBR.mp4")
	if entry.Meta.TBAMatchKey != "qm1" {
		t.Errorf("key = %q, want qm1", entry.Meta.TBAMatchKey)
	}

	// Playoff and final prefixes too.
	playoff := &videoEntry{Meta: &videoMeta{TBAMatchKey: "2026mibr_sf3m1"}}
	fillMetaFromFilename(playoff, "SF3M1_MIBR.mp4")
	if playoff.Meta.TBAMatchKey != "sf3m1" {
		t.Errorf("key = %q, want sf3m1", playoff.Meta.TBAMatchKey)
	}

	// A key already in the right shape is left alone, alliance data with it.
	good := &videoEntry{Meta: &videoMeta{
		TBAMatchKey: "qm9",
		Alliances:   map[string][]allianceTeam{"red": {{Number: 2767}}},
	}}
	fillMetaFromFilename(good, "QM1_MIBR.mp4")
	if good.Meta.TBAMatchKey != "qm9" || len(good.Meta.Alliances) != 1 {
		t.Errorf("good meta disturbed: %+v", good.Meta)
	}
}

func TestIsPartialMatchKey(t *testing.T) {
	for key, want := range map[string]bool{
		"qm1":          true,
		"qm27":         true,
		"sf3m1":        true,
		"f1m2":         true,
		"qf2m3":        true,
		"ef1m1":        true,
		"2026mibr_qm1": false,
		"":             false,
		"qm":           false,
		"sf3":          false,
	} {
		if got := isPartialMatchKey(key); got != want {
			t.Errorf("isPartialMatchKey(%q) = %v, want %v", key, got, want)
		}
	}
}
