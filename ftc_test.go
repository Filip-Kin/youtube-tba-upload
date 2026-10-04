package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// asFTC runs the rest of a test as an FTC event (-program ftc).
func asFTC(t *testing.T) {
	t.Helper()
	old := program
	program = programFTC
	t.Cleanup(func() { program = old })
}

func TestParseProgram(t *testing.T) {
	for in, want := range map[string]string{"": "frc", "frc": "frc", "FTC": "ftc", " ftc ": "ftc"} {
		got, err := parseProgram(in)
		if err != nil || got != want {
			t.Errorf("parseProgram(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseProgram("vex"); err == nil {
		t.Error("parseProgram(vex) should fail")
	}
}

func TestParseFTCFilename(t *testing.T) {
	asFTC(t)
	cases := []struct {
		in      string
		ok      bool
		level   string
		num     int
		short   string
		code    string
		prefix  string
		label   string
		include bool
	}{
		{in: "Q3_fimavtest.mp4", ok: true, level: "Qualification", num: 3, short: "Q3", code: "fimavtest", label: "Q3", include: true},
		{in: "Q12_USMIDET1.mp4", ok: true, level: "Qualification", num: 12, short: "Q12", code: "USMIDET1", label: "Q12", include: true},
		{in: "P1_fimavtest.mp4", ok: true, level: "Practice", num: 1, short: "P1", code: "fimavtest", label: "P1"},
		{in: "PR-2_fimavtest.mp4", ok: true, level: "Practice", num: 2, short: "PR-2", code: "fimavtest", label: "PR-2"},
		{in: "M5_fimavtest.mp4", ok: true, level: "Playoff", num: 5, short: "M5", code: "fimavtest", label: "M5", include: true},
		{in: "F1-2_fimavtest.mp4", ok: true, level: "Playoff", num: 1, short: "F1-2", code: "fimavtest", label: "F1-2", include: true},
		// An event code that falls back to a name with underscores stays whole.
		{in: "Q3_FIM_AV_Test.mp4", ok: true, level: "Qualification", num: 3, short: "Q3", code: "FIM_AV_Test", label: "Q3", include: true},

		{in: "2026 FIM AV Test - Qualification Q3.mp4", ok: true, level: "Qualification", num: 3, short: "Q3", prefix: "2026 FIM AV Test", label: "Q3", include: true},
		{in: "2026 FIM AV Test - Practice P2.mp4", ok: true, level: "Practice", num: 2, short: "P2", prefix: "2026 FIM AV Test", label: "P2"},
		{in: "2026 FIM AV Test - Playoff M4.mp4", ok: true, level: "Playoff", num: 4, short: "M4", prefix: "2026 FIM AV Test", label: "M4", include: true},
		// A written-out level wins over the short name.
		{in: "2026 FIM AV Test - Playoff P4.mp4", ok: true, level: "Playoff", num: 4, short: "P4", prefix: "2026 FIM AV Test", label: "P4", include: true},
		// FIM-AV's name when FTC Live sends no short name.
		{in: "2026 FIM AV Test - Qualification Match 3.mp4", ok: true, level: "Qualification", num: 3, prefix: "2026 FIM AV Test", label: "Qualification 3", include: true},

		{in: "random recording.mp4", ok: false},
		{in: "zz_fimavtest.mp4", ok: false},
	}
	for _, c := range cases {
		p, ok := parseFilename(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if p.Level != c.level || p.MatchNumber != c.num || p.ShortName != c.short ||
			p.EventCode != c.code || p.VideoPrefix != c.prefix || p.Play != 1 || p.Extension != ".mp4" {
			t.Errorf("%q: got %+v", c.in, p)
		}
		if got := p.matchLabel(); got != c.label {
			t.Errorf("%q: label %q want %q", c.in, got, c.label)
		}
		if got := p.includeLevel(); got != c.include {
			t.Errorf("%q: includeLevel %v want %v", c.in, got, c.include)
		}
	}
}

// FTC names mean nothing at an FRC event, and FRC parsing is untouched.
func TestFTCNamesIgnoredAtFRC(t *testing.T) {
	if _, ok := parseFilename("Q3_fimavtest.mp4"); ok {
		t.Error("Q3_fimavtest.mp4 parsed at an FRC event")
	}
	if _, ok := parseFilename("2026 FIM AV Test - Qualification Q3.mp4"); ok {
		t.Error("FTC off-season name parsed at an FRC event")
	}
	p, ok := parseFilename("QM5_MIKET.mp4")
	if !ok || p.ShortName != "" || p.matchLabel() != "Qualification 5" {
		t.Errorf("FRC parse changed: %+v", p)
	}
}

func TestToaMatchKeyFromFilename(t *testing.T) {
	asFTC(t)
	const ev = "2627-FIM-TEST"
	for name, want := range map[string]string{
		"Q3_fimavtest.mp4":                             "2627-FIM-TEST-Q003-1",
		"Q112_fimavtest.mp4":                           "2627-FIM-TEST-Q112-1",
		"2026 FIM AV Test - Qualification Q3.mp4":      "2627-FIM-TEST-Q003-1",
		"M5_fimavtest.mp4":                             "2627-FIM-TEST-E501-1",
		"M14_fimavtest.mp4":                            "2627-FIM-TEST-E1401-1",
		"F1-2_fimavtest.mp4":                           "2627-FIM-TEST-E102-1",
		"2026 FIM AV Test - Playoff Match 5.mp4":       "2627-FIM-TEST-E501-1",
		"2026 FIM AV Test - Qualification Match 7.mp4": "2627-FIM-TEST-Q007-1",
		"P1_fimavtest.mp4":                             "",
		"2026 FIM AV Test - Practice P2.mp4":           "",
	} {
		p, ok := parseFilename(name)
		if !ok {
			t.Errorf("%q did not parse", name)
			continue
		}
		if got := toaMatchKey(p, ev); got != want {
			t.Errorf("toaMatchKey(%q) = %q, want %q", name, got, want)
		}
		if got := toaMatchKey(p, ""); got != "" {
			t.Errorf("toaMatchKey(%q) with no event key = %q", name, got)
		}
	}
}

func TestFTCTemplates(t *testing.T) {
	asFTC(t)
	p, _ := parseFilename("Q3_fimavtest.mp4")
	ctx := buildTemplateContext(p, nil, eventConfig{EventName: "2026 FIM AV Test"})
	if got := renderTitle(defaultTitleTemplate, ctx); got != "2026 FIM AV Test Qualification Match 3" {
		t.Errorf("title = %q", got)
	}
	if got := renderTitle("{video_prefix} {match_label}", ctx); got != "2026 FIM AV Test Q3" {
		t.Errorf("label title = %q", got)
	}

	// Two teams per alliance: the third line of each alliance drops.
	ctx.Title = "T"
	ctx.Alliances = map[string][]allianceTeam{
		"red":  {{Number: 1001}, {Number: 1002}},
		"blue": {{Number: 2001}, {Number: 2002}},
	}
	ctx.RedScore, ctx.BlueScore, ctx.HasScore = 120, 95, true
	want := "T\n\nFinal Score: Red 120 - Blue 95\n\nRed Alliance:\n- 1001\n- 1002\n\nBlue Alliance:\n- 2001\n- 2002"
	if got := renderDescription(defaultDescriptionTemplate, ctx); got != want {
		t.Errorf("description:\n%s\nwant:\n%s", got, want)
	}
}

// fakeFTCLive serves one qualification result and counts requests.
func fakeFTCLive(t *testing.T, finished bool) (*httptest.Server, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/events/fimavtest/matches/3/" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"errorCode":"NO_SUCH_MATCH"}`))
			return
		}
		_, _ = io.WriteString(w, `{"matchBrief":{"matchName":"Q3","matchNumber":3,"field":1,
			"red":{"team1":1001,"team2":1002,"isTeam1Surrogate":false,"isTeam2Surrogate":false},
			"blue":{"team1":2001,"team2":2002,"isTeam1Surrogate":false,"isTeam2Surrogate":false},
			"finished":`+map[bool]string{true: "true", false: "false"}[finished]+`,"matchState":"COMMITTED","time":1},
			"startTime":1,"scheduledTime":1,"resultPostedTime":1,"redScore":120,"blueScore":95}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func useFTCLive(t *testing.T, base string) {
	t.Helper()
	oldURL := ftcURL
	ftcURL = base
	ftcMu.Lock()
	oldCache := ftcCache
	ftcCache = map[string]*ftcMatchResult{}
	ftcMu.Unlock()
	t.Cleanup(func() {
		ftcURL = oldURL
		ftcMu.Lock()
		ftcCache = oldCache
		ftcMu.Unlock()
	})
}

func TestFillFromFTCLive(t *testing.T) {
	asFTC(t)
	srv, hits := fakeFTCLive(t, true)
	useFTCLive(t, srv.URL)
	s := openStoreFor(t, t.TempDir())

	p, _ := parseFilename("Q3_fimavtest.mp4")
	for i := 0; i < 3; i++ {
		ctx := buildTemplateContext(p, nil, eventConfig{})
		fillFromFTCLive(ctx, s, "Q3_fimavtest.mp4", p)
		if !ctx.HasScore || ctx.RedScore != 120 || ctx.BlueScore != 95 {
			t.Fatalf("score = %d-%d (%v)", ctx.RedScore, ctx.BlueScore, ctx.HasScore)
		}
		if len(ctx.Alliances["red"]) != 2 || ctx.Alliances["red"][1].Number != 1002 || ctx.Alliances["blue"][0].Number != 2001 {
			t.Fatalf("alliances = %+v", ctx.Alliances)
		}
	}
	// Rate limit: one call per match, however often the video is rendered.
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("FTC Live hit %d times, want 1", n)
	}

	// A failed lookup is not repeated either.
	p4, _ := parseFilename("Q4_fimavtest.mp4")
	for i := 0; i < 2; i++ {
		ctx := buildTemplateContext(p4, nil, eventConfig{})
		fillFromFTCLive(ctx, s, "Q4_fimavtest.mp4", p4)
		if ctx.HasScore {
			t.Error("Q4 got a score from a 404")
		}
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("FTC Live hit %d times, want 2", n)
	}

	// Playoffs are not on the v1 route, so no call at all.
	pm, _ := parseFilename("M1_fimavtest.mp4")
	fillFromFTCLive(buildTemplateContext(pm, nil, eventConfig{}), s, "M1_fimavtest.mp4", pm)
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("FTC Live hit %d times for a playoff", n)
	}
}

func TestFillFromFTCLiveManifestWins(t *testing.T) {
	asFTC(t)
	srv, hits := fakeFTCLive(t, true)
	useFTCLive(t, srv.URL)
	s := openStoreFor(t, t.TempDir())

	rec := recorded("Q3_fimavtest.mp4", 0)
	rec.Score = &fimavScore{Red: 1, Blue: 2}
	rec.Teams = &fimavTeams{Red: []fimavTeam{{TeamNumber: 7}}, Blue: []fimavTeam{{TeamNumber: 8}}}
	writeFimavRec(t, s, rec)

	p, _ := parseFilename("Q3_fimavtest.mp4")
	ctx := buildTemplateContext(p, nil, eventConfig{})
	ctx.Alliances = alliancesForFile(s, "Q3_fimavtest.mp4")
	r, b, ok := scoreForFile(s, "Q3_fimavtest.mp4")
	ctx.RedScore, ctx.BlueScore, ctx.HasScore = r, b, ok
	fillFromFTCLive(ctx, s, "Q3_fimavtest.mp4", p)
	if ctx.RedScore != 1 || ctx.BlueScore != 2 || ctx.Alliances["red"][0].Number != 7 {
		t.Errorf("manifest data overwritten: %+v", ctx)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("FTC Live hit %d times with a complete manifest", n)
	}
}

func TestFillFromFTCLiveUsesRecordEventCode(t *testing.T) {
	asFTC(t)
	srv, hits := fakeFTCLive(t, false)
	useFTCLive(t, srv.URL)
	s := openStoreFor(t, t.TempDir())

	// The off-season name has no event code; the manifest record does.
	name := "2026 FIM AV Test - Qualification Q3.mp4"
	rec := recorded(name, 0)
	rec.EventCode = "fimavtest"
	writeFimavRec(t, s, rec)

	p, _ := parseFilename(name)
	ctx := buildTemplateContext(p, nil, eventConfig{})
	fillFromFTCLive(ctx, s, name, p)
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("FTC Live hit %d times, want 1", n)
	}
	// Unfinished: teams yes, a 0-0 score no.
	if ctx.HasScore {
		t.Error("unfinished match gave a score")
	}
	if len(ctx.Alliances["blue"]) != 2 {
		t.Errorf("alliances = %+v", ctx.Alliances)
	}
}

// fakeTOAServer records PUT /api/match/video bodies.
func fakeTOAServer(t *testing.T) (*httptest.Server, *[]string) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPut || r.URL.Path != "/api/match/video" || r.Header.Get("X-TOA-Key") != "k3" {
			w.WriteHeader(400)
			return
		}
		bodies = append(bodies, string(b))
		_ = json.NewEncoder(w).Encode(map[string]any{"success": 1, "modified_count": 1})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestSubmitToTOA(t *testing.T) {
	asFTC(t)
	srv, bodies := fakeTOAServer(t)
	oldURL := toaURL
	toaURL = srv.URL
	t.Cleanup(func() { toaURL = oldURL })

	s := openStoreFor(t, t.TempDir())
	m := newUploadManager(s, nil)
	_ = s.update(func(es *eventState) {
		es.Videos["Q3_fimavtest.mp4"] = &videoEntry{Status: statusUploaded, YTVideoID: "abcdefghijk"}
		es.Videos["P1_fimavtest.mp4"] = &videoEntry{Status: statusUploaded, YTVideoID: "practice123"}
	})

	if !s.snapshot().Config.AutoSubmitTOA {
		t.Fatal("auto_submit_toa should default on")
	}

	// No credentials: silent no-op, nothing recorded.
	m.submitToTOA("Q3_fimavtest.mp4")
	if len(*bodies) != 0 || s.snapshot().Videos["Q3_fimavtest.mp4"].TOASubmitError != "" {
		t.Fatalf("submitted without credentials: %v", *bodies)
	}

	_ = s.update(func(es *eventState) {
		es.Config.TOAAPIKey = "k3"
		es.Config.TOAEventKey = "2627-FIM-TEST"
	})
	m.submitToTOA("Q3_fimavtest.mp4")
	m.submitToTOA("P1_fimavtest.mp4") // practice: no key, no call
	m.submitToTOA("Q3_fimavtest.mp4") // already submitted
	if len(*bodies) != 1 || !strings.Contains((*bodies)[0], `"match_key":"2627-FIM-TEST-Q003-1"`) ||
		!strings.Contains((*bodies)[0], `"video_url":"https://www.youtube.com/watch?v=abcdefghijk"`) {
		t.Fatalf("bodies = %v", *bodies)
	}
	v := s.snapshot().Videos["Q3_fimavtest.mp4"]
	if !v.TOASubmitted || v.TOASubmitError != "" || v.TOAMatchKey != "2627-FIM-TEST-Q003-1" {
		t.Errorf("entry = %+v", v)
	}
	// TBA is never used at an FTC event.
	if v.TBASubmitted || v.TBASubmitError != "" {
		t.Errorf("TBA state touched: %+v", v)
	}

	// A refused key is recorded for the tab to show.
	_ = s.update(func(es *eventState) {
		es.Config.TOAAPIKey = "level1"
		es.Videos["Q4_fimavtest.mp4"] = &videoEntry{Status: statusUploaded, YTVideoID: "bbbbbbbbbbb"}
	})
	m.submitToTOA("Q4_fimavtest.mp4")
	if v := s.snapshot().Videos["Q4_fimavtest.mp4"]; v.TOASubmitted || v.TOASubmitError == "" {
		t.Errorf("refused submit not recorded: %+v", v)
	}
}

// A scan gives FTC videos their TOA key and no TBA key.
func TestScanFillsToaMatchKey(t *testing.T) {
	asFTC(t)
	t.Setenv("YT_TBA_UPLOAD_DATA_DIR", t.TempDir())
	videoDir := t.TempDir()
	for _, name := range []string{"Q3_fimavtest.mp4", "P1_fimavtest.mp4"} {
		if err := os.WriteFile(filepath.Join(videoDir, name), []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := openStoreFor(t, videoDir)
	_ = s.update(func(es *eventState) { es.Config.TOAEventKey = "2627-FIM-TEST" })
	m := newUploadManager(s, nil)
	m.scanNow()

	st := s.snapshot()
	q := st.Videos["Q3_fimavtest.mp4"]
	if q == nil || q.Meta == nil || q.Meta.TOAMatchKey != "2627-FIM-TEST-Q003-1" || q.Meta.TBAMatchKey != "" || q.Meta.MatchLabel != "Q3" {
		t.Errorf("Q3 meta = %+v", q)
	}
	if p := st.Videos["P1_fimavtest.mp4"]; p == nil || p.Meta != nil {
		t.Errorf("practice entry = %+v", p)
	}
}

// Settings saved before TOA support have no auto_submit_toa; it defaults on.
func TestAutoSubmitTOADefaultsOnForOldSettings(t *testing.T) {
	dir := t.TempDir()
	old := `{"config":{"event_key":"2026test","profile_name":"youtube","auto_submit_tba":false}}`
	if err := os.WriteFile(filepath.Join(dir, uploaderStateFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s := openStoreFor(t, dir)
	if !s.snapshot().Config.AutoSubmitTOA {
		t.Error("auto_submit_toa off after loading old settings")
	}
	// An explicit false survives a reload.
	_ = s.update(func(es *eventState) { es.Config.AutoSubmitTOA = false })
	s2 := openStoreFor(t, dir)
	if s2.snapshot().Config.AutoSubmitTOA {
		t.Error("explicit auto_submit_toa=false lost on reload")
	}
}
