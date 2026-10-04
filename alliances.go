package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// fmsURL is the FMS base (e.g. http://10.0.100.5), set from the --fms-url flag.
// Used only to fill team names the manifest doesn't carry.
var fmsURL string

var (
	rosterMu      sync.Mutex
	rosterNames   map[int]string
	rosterFetched time.Time
)

// teamNameByNumber returns a team-number -> name map from FMS, cached ~60s. It
// backfills names for records FIM-AV Assistant wrote before it started emitting
// teamName (null/absent). Returns an empty map (never nil after first call) when
// FMS is unreachable; a previously good map is kept if a later fetch gets nothing.
func teamNameByNumber() map[int]string {
	rosterMu.Lock()
	defer rosterMu.Unlock()
	if rosterNames != nil && time.Since(rosterFetched) < 60*time.Second {
		return rosterNames
	}
	names := map[int]string{}
	// FTC events have no FMS; -fms-url's default would only time out.
	if fmsURL != "" && !isFTC() {
		client := http.Client{Timeout: 5 * time.Second}
		if resp, err := client.Get(fmsURL + "/api/v1.0/audience/get/GetQualificationRankData"); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var data struct {
					TeamRanks []struct {
						TeamNumber int    `json:"teamNumber"`
						TeamName   string `json:"teamName"`
					} `json:"teamRanks"`
				}
				if json.NewDecoder(resp.Body).Decode(&data) == nil {
					for _, t := range data.TeamRanks {
						if n := strings.TrimSpace(t.TeamName); n != "" {
							names[t.TeamNumber] = n
						}
					}
				}
			}
		}
	}
	if len(names) > 0 || rosterNames == nil {
		rosterNames = names
	}
	rosterFetched = time.Now()
	return rosterNames
}

var (
	adMu      sync.Mutex
	adNames   map[int]string
	adModTime int64
	adChecked time.Time
)

// customADNames reads %APPDATA%/audience-display/customADTeams.json, the custom
// audience-display name overrides teams pick for the broadcast. These win over
// the FMS/manifest name. Re-read when the file's mtime changes (checked ~10s).
func customADNames() map[int]string {
	adMu.Lock()
	defer adMu.Unlock()
	if adNames != nil && time.Since(adChecked) < 10*time.Second {
		return adNames
	}
	adChecked = time.Now()
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		if adNames == nil {
			adNames = map[int]string{}
		}
		return adNames
	}
	path := filepath.Join(appdata, "audience-display", "customADTeams.json")
	info, err := os.Stat(path)
	if err != nil {
		adNames = map[int]string{}
		adModTime = 0
		return adNames
	}
	if adNames != nil && info.ModTime().UnixNano() == adModTime {
		return adNames
	}
	names := map[int]string{}
	if data, err := os.ReadFile(path); err == nil {
		var f struct {
			AlternativeTeamNames []struct {
				Number int    `json:"number"`
				Name   string `json:"name"`
			} `json:"alternativeTeamNames"`
		}
		if json.Unmarshal(data, &f) == nil {
			for _, t := range f.AlternativeTeamNames {
				if n := strings.TrimSpace(t.Name); n != "" {
					names[t.Number] = n
				}
			}
		}
	}
	adNames = names
	adModTime = info.ModTime().UnixNano()
	return adNames
}

// alliancesForFile builds the description's alliance context for a recording:
// team numbers (and names when present) from FIM-AV's team columns in the shared
// database, with any missing name filled from the FMS roster. Returns nil when
// there are no teams for the file, so callers can leave the template's alliance
// lines to drop as before.
func alliancesForFile(store *stateStore, filename string) map[string][]allianceTeam {
	rec, ok := store.fimavRecord(filename)
	if !ok || rec.Teams == nil {
		return nil
	}
	ad := customADNames()       // custom broadcast overrides win
	names := teamNameByNumber() // FMS roster fallback
	build := func(src []fimavTeam) []allianceTeam {
		out := make([]allianceTeam, 0, len(src))
		for _, t := range src {
			name := ad[t.TeamNumber]
			if name == "" {
				name = strings.TrimSpace(t.TeamName)
			}
			if name == "" {
				name = names[t.TeamNumber]
			}
			out = append(out, allianceTeam{Number: t.TeamNumber, Name: name})
		}
		return out
	}
	al := map[string][]allianceTeam{}
	if len(rec.Teams.Red) > 0 {
		al["red"] = build(rec.Teams.Red)
	}
	if len(rec.Teams.Blue) > 0 {
		al["blue"] = build(rec.Teams.Blue)
	}
	if len(al) == 0 {
		return nil
	}
	return al
}

// scoreForFile returns the final alliance totals FIM-AV captured for a recording,
// or ok=false when none were recorded (older records, or FMS gave no score).
func scoreForFile(store *stateStore, filename string) (red, blue int, ok bool) {
	rec, found := store.fimavRecord(filename)
	if !found || rec.Score == nil {
		return 0, 0, false
	}
	return rec.Score.Red, rec.Score.Blue, true
}
