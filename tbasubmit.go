package main

// Match-video submission to The Blue Alliance.
//
// The root TBA-uploader binary already proxies match_videos/add for its own
// web UI, but that binary is a separate process the operator has to run. When
// autoav-helper runs as a sidecar inside FIM-AV Assistant, the whole
// YouTube -> TBA flow has to live here so nothing else needs to be running.
//
// The signing and request shape are reused verbatim from the tba package
// (tba.SendRequest), the same call the root binary makes. The only things this
// side adds are the per-event trusted-API credentials (held in eventConfig) and
// the point in the upload flow where the submit happens (see worker.go).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Filip-Kin/youtube-tba-upload/tba"
)

// tbaURL is the TBA base, set once from the -tba-url flag in main(). It matches
// the root binary's -tba-url default.
var tbaURL = "https://www.thebluealliance.com"

// submitMatchVideo posts one YouTube video id to a match's videos on TBA via the
// trusted match_videos/add endpoint. The body shape is a map of partial match
// key to video id, exactly what the root binary forwards from the web UI.
//
// matchKey is the partial key ("qm5", "sf3m1", "f1m1"); videoID is the 11-char
// YouTube id. event is the full TBA event key (cfg.EventKey, e.g. "2026miket").
func submitMatchVideo(cfg eventConfig, matchKey, videoID string) error {
	if cfg.EventKey == "" {
		return fmt.Errorf("no event key configured")
	}
	if cfg.TBAAuthID == "" || cfg.TBASecret == "" {
		return fmt.Errorf("TBA trusted-API auth id/secret not set")
	}
	if !isPartialMatchKey(matchKey) {
		return fmt.Errorf("not a submittable match key: %q", matchKey)
	}
	if videoID == "" {
		return fmt.Errorf("no video id")
	}

	body, err := json.Marshal(map[string]string{matchKey: videoID})
	if err != nil {
		return err
	}
	params := &tba.EventParams{
		Event:  cfg.EventKey,
		Auth:   cfg.TBAAuthID,
		Secret: cfg.TBASecret,
	}
	res, err := tba.SendRequest(tbaURL, "match_videos/add", body, params)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("TBA %d: %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
