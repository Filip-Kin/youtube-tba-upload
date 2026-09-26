package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Filip-Kin/youtube-tba-upload/tba"
)

// The Vue layer links a finished upload to a TBA match through the entry's
// meta.tba_match_key, and that meta only arrives when TBA-uploader itself
// renamed the recording through /api/rename. FIM-AV Assistant does its own
// renaming, so nothing ever calls that endpoint at a FIM event, no entry has a
// match key, and every upload is skipped for both linking and submission.
//
// The filename already says which match it is, so derive the key from it.

// playoffBracket is the bracket the match numbers in a playoff filename refer
// to. FMS numbers the 13 elimination matches 1-13 and the finals from 14 up,
// which is the 8-alliance double elimination bracket, and FIM-AV's file naming
// assumes the same thing.
const playoffBracket = tba.BRACKET_TYPE_DOUBLE_ELIM_8_TEAM

// tbaMatchKey returns the match key for a parsed filename, or "" when the
// filename is not a match TBA knows about (practice, test, manual).
//
// This is the partial key, with no event prefix: "qm5", "sf3m1", "f1m1". That is
// what the rest of the app keys matches by, from the TBA match list
// (match.key.split("_")[1]) through to what the trusted API is sent, so a full
// "2026fsu_qm5" here links to nothing and submits nothing.
func tbaMatchKey(p parsedFilename) string {
	switch strings.ToLower(p.Level) {
	case "qualification":
		return fmt.Sprintf("qm%d", p.MatchNumber)
	case "final":
		// FIM-AV already separates finals out (F1M2), so the number is the
		// match within the single finals set.
		return fmt.Sprintf("f1m%d", p.MatchNumber)
	case "playoff":
		code := tba.GetPlayoffCode(playoffBracket, p.MatchNumber)
		if code.Level == "" {
			return ""
		}
		return fmt.Sprintf("%s%dm%d", code.Level, code.Set, code.Match)
	}
	return ""
}

// partialMatchKeyRe is the shape TBA's trusted API accepts in a match_videos
// request: a qualification number, or a level with a set and a match.
var partialMatchKeyRe = regexp.MustCompile(`^(qm\d+|(?:ef|qf|sf|f)\d+m\d+)$`)

func isPartialMatchKey(key string) bool {
	return partialMatchKeyRe.MatchString(key)
}

// fillMetaFromFilename gives an entry the match identity its filename implies,
// without disturbing anything richer that /api/rename may have supplied. The
// alliance data stays absent, so description templates behave exactly as before.
func fillMetaFromFilename(entry *videoEntry, filename string) {
	if entry == nil {
		return
	}
	if entry.Meta != nil && isPartialMatchKey(entry.Meta.TBAMatchKey) {
		return
	}
	p, ok := parseFilename(filename)
	if !ok {
		return
	}
	key := tbaMatchKey(p)
	if key == "" {
		return
	}
	if entry.Meta == nil {
		entry.Meta = &videoMeta{}
	}
	// This also repairs keys written with an event prefix ("2026mibr_qm1"),
	// which TBA rejects outright: "Invalid match IDs provided".
	entry.Meta.TBAMatchKey = key
	if entry.Meta.MatchLabel == "" {
		entry.Meta.MatchLabel = p.matchLabel()
	}
	if entry.Meta.MatchLevel == "" {
		entry.Meta.MatchLevel = p.Level
	}
	if entry.Meta.MatchNumber == 0 {
		entry.Meta.MatchNumber = p.MatchNumber
	}
	if entry.Meta.Play == 0 {
		entry.Meta.Play = p.Play
	}
}
