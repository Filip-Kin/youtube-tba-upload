// Package toa submits match videos to The Orange Alliance (TOA), FTC's
// equivalent of The Blue Alliance, and builds the TOA match keys those videos
// are attached to.
//
// Everything here is read from the TOA-API source (the-orange-alliance/TOA-API)
// rather than from documentation, because the documentation does not cover the
// write routes:
//
//   - The route is PUT /api/match/video (src/controllers/MatchController.ts)
//     with a JSON array body of {match_key, video_url}
//     (src/schemas/MatchVideo.ts).
//   - Every /api request goes through GateKeeper.authorize
//     (src/middlewares/GateKeeper.ts). It needs an X-Application-Origin header.
//     A request whose origin is one of TOA's own apps (TOA-WebApp-2223 and so
//     on) is given read-only access level 2 before the key is even looked at,
//     so this client sends its own origin name. The X-TOA-Key header is then
//     looked up in the api-key collection: the key must be approved, not
//     revoked, and GateKeeper.validate only lets a PUT through at access level
//     3 ("Event Write") or 4 ("All Access"). The key a myTOA user gets from
//     their account page is created at level 1, so it cannot write.
//   - Write keys are per event: an account requests one on its myTOA page
//     (Write keys card) for one event and the "Match videos" scope, a TOA
//     admin approves it, and it stops working 7 days after the event ends.
//     It is refused for a match of any other event.
//   - The write answers matched_count and unknown_match_keys, so a key with no
//     match is reported, not passed. An older TOA-API answered only
//     modified_count, and wrote into its current season whatever the key; for
//     that case a zero modified_count is still checked with a read.
package toa

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the public TOA API base.
const DefaultURL = "https://api.theorangealliance.org"

// ApplicationOrigin is sent as X-Application-Origin. It must not be one of the
// origins TOA-API hard-codes for its own apps: those are pinned to read-only
// access and the X-TOA-Key is never consulted.
const ApplicationOrigin = "youtube-tba-upload"

// QualMatchKey is the TOA key of a qualification match, the form the TOA
// scrapers (ftc-events-scraper, upload-data.ts parseMatch) write:
// "{event}-Q{number, zero-padded to 3}-1", e.g. "2526-AUS-CMP-Q003-1". The
// trailing "-1" is the play number, which TOA always stores as 1.
func QualMatchKey(eventKey string, number int) string {
	if eventKey == "" || number < 1 {
		return ""
	}
	return fmt.Sprintf("%s-Q%03d-1", eventKey, number)
}

// PlayoffMatchKey is the TOA key of a playoff match:
// "{event}-E{series}{match, zero-padded to 2}-1".
//
// Since 2024-25 FTC plays double elimination, and FTC Events gives every
// bracket match its own series with match number 1, finals included (seen on
// the live API: 2526-AUS-CMP-E101-1 ... E1401-1 for "Final Bracket Round 6
// Match 14"). A finals-only event keeps one series and counts matches in it
// (2526-FIM-CMP-E101-1, E102-1, E103-1). The scraper maps series 0 to 9.
func PlayoffMatchKey(eventKey string, series, match int) string {
	if eventKey == "" || series < 0 || match < 1 {
		return ""
	}
	if series == 0 {
		series = 9
	}
	return fmt.Sprintf("%s-E%d%02d-1", eventKey, series, match)
}

// VideoURL is the watch URL stored on the TOA match for a YouTube video id.
func VideoURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + videoID
}

// matchVideo is one element of the PUT /api/match/video body.
type matchVideo struct {
	MatchKey string `json:"match_key"`
	VideoURL string `json:"video_url"`
}

// videoWriteResult is the PUT /api/match/video response. UnknownMatchKeys is
// nil on an older TOA-API that did not send it.
type videoWriteResult struct {
	Success          json.RawMessage `json:"success"`
	MatchedCount     int             `json:"matched_count"`
	ModifiedCount    int             `json:"modified_count"`
	UnknownMatchKeys []string        `json:"unknown_match_keys"`
}

// apiError is TOA's error body.
type apiError struct {
	Message string `json:"_message"`
}

// statusError turns a non-200 TOA answer into an error carrying TOA's own
// message ("This event key is for a different event.") when it sent one.
func statusError(code int, raw []byte) error {
	var e apiError
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return fmt.Errorf("TOA %d: %s", code, e.Message)
	}
	return fmt.Errorf("TOA %d: %s", code, strings.TrimSpace(string(raw)))
}

var client = &http.Client{Timeout: 10 * time.Second}

// SubmitMatchVideo sets one match's video URL on TOA. apiKey is a TOA event
// write key with the "Match videos" scope (or a level-4 key); matchKey is a
// full TOA match key from QualMatchKey or PlayoffMatchKey.
func SubmitMatchVideo(baseURL, apiKey, matchKey, videoURL string) error {
	if apiKey == "" {
		return fmt.Errorf("no TOA API key")
	}
	if matchKey == "" {
		return fmt.Errorf("no TOA match key")
	}
	body, err := json.Marshal([]matchVideo{{MatchKey: matchKey, VideoURL: videoURL}})
	if err != nil {
		return err
	}
	base := strings.TrimRight(baseURL, "/")
	req, err := http.NewRequest(http.MethodPut, base+"/api/match/video", bytes.NewReader(body))
	if err != nil {
		return err
	}
	setHeaders(req, apiKey)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return statusError(res.StatusCode, raw)
	}
	var out videoWriteResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("TOA response: %v: %s", err, strings.TrimSpace(string(raw)))
	}
	if out.UnknownMatchKeys != nil {
		for _, k := range out.UnknownMatchKeys {
			if k == matchKey {
				return fmt.Errorf("TOA has no match %s", matchKey)
			}
		}
		if out.MatchedCount > 0 {
			return nil
		}
	}
	if out.ModifiedCount > 0 {
		return nil
	}
	// Older TOA-API: nothing changed. Either the match already had this URL (a
	// resubmit), or no match has this key in the season it wrote to. Read the
	// match back to tell the two apart.
	current, err := matchVideoURL(base, apiKey, matchKey)
	if err != nil {
		return fmt.Errorf("TOA changed no match for %s: %v", matchKey, err)
	}
	if current != videoURL {
		return fmt.Errorf("TOA changed no match for %s (video now %q)", matchKey, current)
	}
	return nil
}

// matchVideoURL reads a match's stored video URL with GET /api/match/{key},
// which answers a one-element array.
func matchVideoURL(base, apiKey, matchKey string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/api/match/"+url.PathEscape(matchKey), nil)
	if err != nil {
		return "", err
	}
	setHeaders(req, apiKey)
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read back: %w", statusError(res.StatusCode, raw))
	}
	var matches []struct {
		VideoURL *string `json:"video_url"`
	}
	if err := json.Unmarshal(raw, &matches); err != nil {
		return "", fmt.Errorf("read back: %v", err)
	}
	if len(matches) == 0 || matches[0].VideoURL == nil {
		return "", nil
	}
	return *matches[0].VideoURL, nil
}

func setHeaders(req *http.Request, apiKey string) {
	req.Header.Set("X-Application-Origin", ApplicationOrigin)
	req.Header.Set("X-TOA-Key", apiKey)
}
