package main

// Match-video submission to The Orange Alliance, the FTC side of what
// tbasubmit.go does for The Blue Alliance. At an FTC event (-program ftc) it
// replaces the TBA submit in the upload flow; at an FRC event none of this
// runs.

import (
	"log"
	"strings"

	"github.com/Filip-Kin/youtube-tba-upload/toa"
)

// toaURL is the TOA API base, set once from the -toa-url flag in main().
var toaURL = toa.DefaultURL

// toaMatchKey returns the full TOA match key for a parsed FTC filename, or ""
// when it is not a match TOA tracks (practice, test) or no event key is set.
//
// A qualification is keyed by its number. A playoff is keyed by series and
// match: FTC's double-elimination bracket gives every match its own series, so
// a short name with one number ("M5") is series 5 match 1; a short name with
// two ("F1-2") is read as series 1 match 2. FTC Live's playoff short names are
// not confirmed yet, so this is the one guess here (see toa.PlayoffMatchKey).
func toaMatchKey(p parsedFilename, eventKey string) string {
	eventKey = strings.TrimSpace(eventKey)
	if eventKey == "" {
		return ""
	}
	switch strings.ToLower(p.Level) {
	case "qualification":
		return toa.QualMatchKey(eventKey, p.MatchNumber)
	case "playoff":
		if p.ShortName == "" {
			// FIM-AV's fallback name ("Playoff Match 5") has only the number.
			return toa.PlayoffMatchKey(eventKey, p.MatchNumber, 1)
		}
		switch ns := shortNameNumbers(p.ShortName); len(ns) {
		case 1:
			return toa.PlayoffMatchKey(eventKey, ns[0], 1)
		case 2:
			return toa.PlayoffMatchKey(eventKey, ns[0], ns[1])
		}
	}
	return ""
}

// fillFTCMeta is fillMetaFromFilename for FTC: the entry gets the match
// identity its name implies, and the TOA key instead of a TBA key. The key is
// derived again on every scan, so a TOA event key entered later still applies.
func fillFTCMeta(entry *videoEntry, filename string, cfg eventConfig) {
	if entry == nil {
		return
	}
	p, ok := parseFilename(filename)
	if !ok || !p.includeLevel() {
		return
	}
	if entry.Meta == nil {
		entry.Meta = &videoMeta{}
	}
	entry.Meta.TOAMatchKey = toaMatchKey(p, cfg.TOAEventKey)
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

// submitToTOA sets one uploaded video's URL on its TOA match, when auto-submit
// is on and the key + API key are present. Like submitToTBA, it records the
// outcome on the entry (TOASubmitted / TOASubmitError), and a missing match key
// or API key is a silent no-op: practice matches have no key, and an event
// without TOA settings simply isn't submitting.
func (m *uploadManager) submitToTOA(filename string) {
	st := m.store.snapshot()
	cfg := st.Config
	if !cfg.AutoSubmitTOA {
		return
	}
	entry := st.Videos[filename]
	if entry == nil || entry.Status != statusUploaded || entry.YTVideoID == "" || entry.TOASubmitted {
		return
	}
	p, ok := parseFilename(filename)
	if !ok {
		return
	}
	matchKey := toaMatchKey(p, cfg.TOAEventKey)
	if matchKey == "" || cfg.TOAAPIKey == "" {
		return
	}
	err := toa.SubmitMatchVideo(toaURL, cfg.TOAAPIKey, matchKey, toa.VideoURL(entry.YTVideoID))
	_ = m.store.update(func(s *eventState) {
		v := s.Videos[filename]
		if v == nil {
			return
		}
		v.TOAMatchKey = matchKey
		if err != nil {
			v.TOASubmitError = err.Error()
			log.Printf("toa submit: %s (%s -> %s): %v", filename, matchKey, v.YTVideoID, err)
			return
		}
		v.TOASubmitted = true
		v.TOASubmitError = ""
		log.Printf("toa submit: %s (%s) done", filename, matchKey)
	})
}
