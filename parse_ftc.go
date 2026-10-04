package main

import (
	"regexp"
	"strconv"
	"strings"
)

// FTC recording names, as FIM-AV Assistant writes them (src/utils/recording.ts)
// when FTC Live gives the match a short name ("Q3", "P1", ...):
//
//	in-season:  Q3_fimavtest.mp4                              {ShortName}_{eventCode}
//	off-season: 2026 FIM AV Test - Qualification Q3.mp4       {year} {event} - {Level} {ShortName}
//
// FTC Live has no play numbers, so neither form carries one.
//
// The short name is FTC Live's own label for the match and is kept as the
// match label. In the in-season form the level comes from the short name, by
// the same rule FIM-AV uses when it starts the recording (autoav.ts
// onFtcUpdate): Q is a qualification, P/PR followed by a digit or dash is
// practice, and anything else is a playoff. In the off-season form the level
// is written out and wins.

// ftcInSeasonRe: a short name holding at least one digit, then the event code.
// The short name has no underscore, so the first underscore ends it.
var ftcInSeasonRe = regexp.MustCompile(`^([A-Za-z][A-Za-z-]*\d[A-Za-z0-9-]*)_(.+?)(\.[A-Za-z0-9]+)$`)

// ftcOffSeasonRe: "{prefix} - {Level} {ShortName}{ext}". The short name is one
// token with a digit, which keeps FIM-AV's no-short-name fallback ("... -
// Qualification Match 3.mp4") out of this form; that one parses as an FRC-style
// name instead.
var ftcOffSeasonRe = regexp.MustCompile(`^(.+?) - (Qualification|Practice|Playoff|Test) ([A-Za-z][A-Za-z-]*\d[A-Za-z0-9-]*)(\.[A-Za-z0-9]+)$`)

// ftcPracticeRe is FIM-AV's practice rule for short names.
var ftcPracticeRe = regexp.MustCompile(`(?i)^P(R)?[-\d]`)

// digitRunRe finds the numbers in a short name ("Q12" -> 12; "M2-1" -> 2, 1).
var digitRunRe = regexp.MustCompile(`\d+`)

// parseFTCFilename tries the FTC forms, then the FRC-style "{Level} Match {N}"
// form FIM-AV falls back to when FTC Live sends no short name. FRC's in-season
// tokens (QM5_, SF3M1_) and its double-elimination renumbering are FRC things
// and are not applied.
func parseFTCFilename(name string) (parsedFilename, bool) {
	if p, ok := parseFTCInSeason(name); ok {
		return p, true
	}
	if p, ok := parseFTCOffSeason(name); ok {
		return p, true
	}
	return parseTBAFilename(name)
}

func parseFTCInSeason(name string) (parsedFilename, bool) {
	m := ftcInSeasonRe.FindStringSubmatch(name)
	if m == nil {
		return parsedFilename{}, false
	}
	short := m[1]
	out := parsedFilename{
		ShortName:   short,
		EventCode:   m[2],
		Extension:   m[3],
		Level:       ftcLevelFromShortName(short),
		MatchNumber: shortNameNumber(short),
		Play:        1,
	}
	return out, true
}

func parseFTCOffSeason(name string) (parsedFilename, bool) {
	m := ftcOffSeasonRe.FindStringSubmatch(name)
	if m == nil {
		return parsedFilename{}, false
	}
	out := parsedFilename{
		VideoPrefix: strings.TrimSpace(m[1]),
		Level:       m[2],
		ShortName:   m[3],
		MatchNumber: shortNameNumber(m[3]),
		Extension:   m[4],
		Play:        1,
	}
	return out, true
}

// ftcLevelFromShortName applies FIM-AV's rule for the level of an FTC match.
func ftcLevelFromShortName(short string) string {
	switch {
	case strings.HasPrefix(strings.ToUpper(short), "Q"):
		return "Qualification"
	case ftcPracticeRe.MatchString(short):
		return "Practice"
	}
	return "Playoff"
}

// shortNameNumbers returns every number in a short name, in order.
func shortNameNumbers(short string) []int {
	var out []int
	for _, s := range digitRunRe.FindAllString(short, -1) {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// shortNameNumber is the first number in a short name, used as the match
// number for titles and ordering.
func shortNameNumber(short string) int {
	if ns := shortNameNumbers(short); len(ns) > 0 {
		return ns[0]
	}
	return 0
}
