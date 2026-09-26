package main

import (
	"regexp"
	"strconv"
	"strings"
)

// parsedFilename describes everything we can learn from a video filename
// produced by the autoAVRenameLastVideo flow in App.vue, or by FIM-AV
// Assistant's Auto AV recorder.
//
// TBA-uploader's own convention:
//
//	2026 Tornado Tumble Qualification Match 5.mp4
//	2026 Tornado Tumble Qualification Match 5 Play 2.mp4
//	2026 Tornado Tumble Final Match 1.mp4
//	2026 Tornado Tumble Qualification Match 5 - 2.mp4
//
// FIM-AV Assistant, off-season mode (src/utils/recording.ts):
//
//	2026 Tornado Tumble - Qualification Match 5.mp4
//	2026 Tornado Tumble - Qualification Match 5 (Play #2).mp4
//
// FIM-AV Assistant, in-season mode (used for every official event):
//
//	QM5_MIKET.mp4          Qualification 5
//	QM5_P2_MIKET.mp4       Qualification 5, play 2
//	SF3M1_MIKET.mp4        Playoff 3
//	F1M2_MIKET.mp4         Final 2
//	zz_PR1_MIKET.mp4       Practice 1
//	zz_TM1_MIKET.mp4       Test 1
type parsedFilename struct {
	VideoPrefix string // text before " {Level} Match ..." (e.g. "2026 Tornado Tumble")
	EventCode   string // trailing event code on FIM-AV in-season names (e.g. "MIKET")
	Level       string // "Test" | "Practice" | "Qualification" | "Playoff" | "Final" | "Manual"
	MatchNumber int
	Play        int    // 1 if no play/replay marker is present
	Dedup       int    // 0 if no " - N" suffix
	Extension   string // e.g. ".mp4"
}

// parseFilename returns the parsed form and true on success, or zero+false if the
// filename doesn't match any known shape. Order of suffixes matters: the
// "Play N" segment is parsed before the " - N" dedup tail.
//
// The play marker accepts both TBA-uploader's " Play 2" and FIM-AV's
// " (Play #2)".
var filenameRe = regexp.MustCompile(
	`^(.+?) (Test|Practice|Qualification|Playoff|Final|Manual) Match (\d+)(?: Play (\d+)| \(Play #(\d+)\))?(?: - (\d+))?(\.[A-Za-z0-9]+)$`,
)

// FIM-AV in-season names. The four match-token shapes are alternatives, then an
// optional _P{play}, then the event code, which may itself contain underscores
// or spaces (it falls back to the event name when no code is known).
var fimInSeasonRe = regexp.MustCompile(
	`^(?:(QM|zz_PR|zz_TM)(\d+)|SF(\d+)M1|F1M(\d+))(?:_P(\d+))?_(.+?)(\.[A-Za-z0-9]+)$`,
)

// fimLevels maps a FIM-AV in-season match-token prefix to a TBA-uploader level.
var fimLevels = map[string]string{
	"QM":    "Qualification",
	"zz_PR": "Practice",
	"zz_TM": "Test",
}

func parseFilename(name string) (parsedFilename, bool) {
	if p, ok := parseTBAFilename(name); ok {
		return p, true
	}
	return parseFIMInSeasonFilename(name)
}

func parseTBAFilename(name string) (parsedFilename, bool) {
	m := filenameRe.FindStringSubmatch(name)
	if m == nil {
		return parsedFilename{}, false
	}
	out := parsedFilename{
		// FIM-AV's off-season names put " - " between the event name and the
		// level; drop that separator so the prefix is just the event name and
		// rendered titles read the same either way.
		VideoPrefix: strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[1]), "-")),
		Level:       m[2],
		Extension:   m[7],
		Play:        1,
	}
	out.MatchNumber, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		out.Play, _ = strconv.Atoi(m[4])
	}
	if m[5] != "" {
		out.Play, _ = strconv.Atoi(m[5])
	}
	if m[6] != "" {
		out.Dedup, _ = strconv.Atoi(m[6])
	}
	return out, true
}

func parseFIMInSeasonFilename(name string) (parsedFilename, bool) {
	m := fimInSeasonRe.FindStringSubmatch(name)
	if m == nil {
		return parsedFilename{}, false
	}
	// No human-readable prefix in these names; the title falls back to the
	// event name from config (see buildTemplateContext).
	out := parsedFilename{
		EventCode: m[6],
		Extension: m[7],
		Play:      1,
	}
	switch {
	case m[1] != "":
		out.Level = fimLevels[m[1]]
		out.MatchNumber, _ = strconv.Atoi(m[2])
	case m[3] != "":
		out.Level = "Playoff"
		out.MatchNumber, _ = strconv.Atoi(m[3])
	case m[4] != "":
		out.Level = "Final"
		out.MatchNumber, _ = strconv.Atoi(m[4])
	}
	if out.Level == "" {
		return parsedFilename{}, false
	}
	if m[5] != "" {
		out.Play, _ = strconv.Atoi(m[5])
	}
	return out, true
}

// matchLabel is the human-readable label, e.g. "Qualification 5" or "Final 1".
func (p parsedFilename) matchLabel() string {
	return p.Level + " " + strconv.Itoa(p.MatchNumber)
}

// playSuffix renders " Play N" for replays, or empty for Play 1.
func (p parsedFilename) playSuffix() string {
	if p.Play <= 1 {
		return ""
	}
	return " Play " + strconv.Itoa(p.Play)
}

// includeLevel decides whether this filename's level passes the include_* config.
// Manual recordings are always skipped — they're ad-hoc recordings made
// outside the regular match flow and shouldn't be auto-uploaded.
func (p parsedFilename) includeLevel(cfg eventConfig) bool {
	switch strings.ToLower(p.Level) {
	case "qualification", "playoff", "final":
		return true
	case "practice":
		return cfg.IncludePractice
	case "test":
		return cfg.IncludeTest
	}
	return false
}

// orderKey returns a sortable integer for match order. Levels are bucketed so
// Quals come before Playoffs come before Finals. Within a level the match number
// (and then play number) takes over, so replays sort right after their parent.
func (p parsedFilename) orderKey() int64 {
	var bucket int64
	switch strings.ToLower(p.Level) {
	case "test":
		bucket = 0
	case "practice":
		bucket = 1
	case "qualification":
		bucket = 2
	case "playoff":
		bucket = 3
	case "final":
		bucket = 4
	case "manual":
		bucket = 5
	default:
		bucket = 9
	}
	// Pack: bucket | match_number | play | dedup. Plenty of headroom.
	return bucket*1_000_000_000 + int64(p.MatchNumber)*1_000_000 + int64(p.Play)*1_000 + int64(p.Dedup)
}
