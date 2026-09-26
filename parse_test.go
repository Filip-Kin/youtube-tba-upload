package main

import "testing"

func TestParseFilename(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		level    string
		num      int
		play     int
		dedup    int
		prefix   string
		ext      string
		label    string
		playSfx  string
		ordering int64
	}{
		{
			in: "2026 Tornado Tumble Qualification Match 5.mp4",
			ok: true, level: "Qualification", num: 5, play: 1, dedup: 0,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
			label: "Qualification 5", playSfx: "",
		},
		{
			in: "2026 Tornado Tumble Qualification Match 5 Play 2.mp4",
			ok: true, level: "Qualification", num: 5, play: 2, dedup: 0,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
			label: "Qualification 5", playSfx: " Play 2",
		},
		{
			in: "2026 Tornado Tumble Final Match 1.mp4",
			ok: true, level: "Final", num: 1, play: 1, dedup: 0,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
			label: "Final 1", playSfx: "",
		},
		{
			in: "2026 Tornado Tumble Qualification Match 5 - 2.mp4",
			ok: true, level: "Qualification", num: 5, play: 1, dedup: 2,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
		},
		{
			in: "2026 Tornado Tumble Practice Match 3.mp4",
			ok: true, level: "Practice", num: 3, play: 1,
		},
		// FIM-AV Assistant, off-season mode: " - " separator and "(Play #N)".
		{
			in: "2026 Tornado Tumble - Qualification Match 5.mp4",
			ok: true, level: "Qualification", num: 5, play: 1,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
			label: "Qualification 5", playSfx: "",
		},
		{
			in: "2026 Tornado Tumble - Qualification Match 5 (Play #2).mp4",
			ok: true, level: "Qualification", num: 5, play: 2,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
			label: "Qualification 5", playSfx: " Play 2",
		},
		{
			in: "2026 Tornado Tumble - Playoff Match 14.mp4",
			ok: true, level: "Playoff", num: 14, play: 1,
			prefix: "2026 Tornado Tumble", ext: ".mp4",
		},
		{in: "garbage.mp4", ok: false},
		{in: "Random vmix recording.mp4", ok: false},
	}
	for _, c := range cases {
		got, ok := parseFilename(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Level != c.level || got.MatchNumber != c.num || got.Play != c.play || got.Dedup != c.dedup {
			t.Errorf("%q: got %+v", c.in, got)
		}
		if c.prefix != "" && got.VideoPrefix != c.prefix {
			t.Errorf("%q: prefix %q want %q", c.in, got.VideoPrefix, c.prefix)
		}
		if c.label != "" && got.matchLabel() != c.label {
			t.Errorf("%q: label %q want %q", c.in, got.matchLabel(), c.label)
		}
		if c.ext != "" && got.Extension != c.ext {
			t.Errorf("%q: ext %q want %q", c.in, got.Extension, c.ext)
		}
		if c.playSfx != got.playSuffix() {
			t.Errorf("%q: playSuffix %q want %q", c.in, got.playSuffix(), c.playSfx)
		}
	}
}

func TestOrderKey(t *testing.T) {
	// Match-order invariants the worker depends on:
	//   - Quals before Playoffs before Finals
	//   - Within a level, ascending match number
	//   - Replays right after their parent (same match number, higher play)
	q1, _ := parseFilename("2026 X Qualification Match 1.mp4")
	q2, _ := parseFilename("2026 X Qualification Match 2.mp4")
	q2p2, _ := parseFilename("2026 X Qualification Match 2 Play 2.mp4")
	p1, _ := parseFilename("2026 X Playoff Match 1.mp4")
	f1, _ := parseFilename("2026 X Final Match 1.mp4")

	order := []parsedFilename{q1, q2, q2p2, p1, f1}
	for i := 1; i < len(order); i++ {
		if order[i-1].orderKey() >= order[i].orderKey() {
			t.Fatalf("order %d not before %d: %d vs %d",
				i-1, i, order[i-1].orderKey(), order[i].orderKey())
		}
	}
}

func TestIncludeLevel(t *testing.T) {
	cfg := eventConfig{IncludePractice: false, IncludeTest: false}
	p, _ := parseFilename("2026 X Qualification Match 5.mp4")
	if !p.includeLevel(cfg) {
		t.Fatal("qual should always be included")
	}
	prac, _ := parseFilename("2026 X Practice Match 5.mp4")
	if prac.includeLevel(cfg) {
		t.Fatal("practice should be excluded when IncludePractice=false")
	}
	cfg.IncludePractice = true
	if !prac.includeLevel(cfg) {
		t.Fatal("practice should be included when IncludePractice=true")
	}
	man, _ := parseFilename("2026 X Manual Match 1.mp4")
	if man.includeLevel(cfg) {
		t.Fatal("manual recordings should always be excluded")
	}
}

// FIM-AV Assistant in-season names, from src/utils/recording.ts.
func TestParseFIMInSeasonFilename(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		level string
		num   int
		play  int
		code  string
	}{
		{in: "QM1_MIKET.mp4", ok: true, level: "Qualification", num: 1, play: 1, code: "MIKET"},
		{in: "QM12_P2_MIKET.mp4", ok: true, level: "Qualification", num: 12, play: 2, code: "MIKET"},
		{in: "SF3M1_MIKET.mp4", ok: true, level: "Playoff", num: 3, play: 1, code: "MIKET"},
		{in: "F1M2_MIKET.mp4", ok: true, level: "Final", num: 2, play: 1, code: "MIKET"},
		{in: "zz_PR1_MIKET.mp4", ok: true, level: "Practice", num: 1, play: 1, code: "MIKET"},
		{in: "zz_TM4_MIKET.mp4", ok: true, level: "Test", num: 4, play: 1, code: "MIKET"},
		// Event code falls back to the event name, which can hold spaces and
		// underscores.
		{in: "QM7_Tornado Tumble.mp4", ok: true, level: "Qualification", num: 7, play: 1, code: "Tornado Tumble"},
		{in: "QM7_Unknown_Event.mp4", ok: true, level: "Qualification", num: 7, play: 1, code: "Unknown_Event"},
		{in: "fimav-matches.json", ok: false},
		{in: "Capture 2026-09-18 19-04-12.mp4", ok: false},
	}
	for _, c := range cases {
		got, ok := parseFilename(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Level != c.level || got.MatchNumber != c.num || got.Play != c.play || got.EventCode != c.code {
			t.Errorf("%q: got %+v", c.in, got)
		}
		if got.VideoPrefix != "" {
			t.Errorf("%q: expected empty prefix, got %q", c.in, got.VideoPrefix)
		}
	}
}

// An in-season name has no prefix of its own, so titles come from config.
func TestInSeasonTitleUsesConfiguredEventName(t *testing.T) {
	p, ok := parseFilename("QM5_MIKET.mp4")
	if !ok {
		t.Fatal("parse failed")
	}
	cfg := eventConfig{
		EventKey:      "2026miket",
		EventName:     "2026 Kettering University",
		TitleTemplate: defaultTitleTemplate,
	}
	ctx := buildTemplateContext(p, nil, cfg)
	got := renderTitle(cfg.TitleTemplate, ctx)
	want := "2026 Kettering University Qualification Match 5"
	if got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if ctx.EventYear != "2026" {
		t.Errorf("event year = %q, want 2026", ctx.EventYear)
	}
}

// With no event name configured, fall back to the code in the filename rather
// than rendering a title with a leading space.
func TestInSeasonTitleFallsBackToEventCode(t *testing.T) {
	p, _ := parseFilename("QM5_MIKET.mp4")
	ctx := buildTemplateContext(p, nil, eventConfig{TitleTemplate: defaultTitleTemplate})
	if got, want := renderTitle(defaultTitleTemplate, ctx), "MIKET Qualification Match 5"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}
