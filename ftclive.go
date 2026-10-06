package main

// Match results from FTC Live, the FTC scoring system, for titles and
// descriptions at FTC events.
//
// FIM-AV Assistant may write an FTC match's teams and score into the shared
// manifest like it does for FRC; when it has, nothing here runs. When it has
// not, the result is read from FTC Live's v1 API:
//
//	GET {ftc-url}/api/v1/events/{code}/matches/{number}/   (trailing slash required)
//
// That route is "Gets Qualification Match" in the FTC Live OpenAPI spec
// (8.0.0-beta.3, ApiV1MatchDetailed): redScore/blueScore plus the two team
// numbers per alliance in matchBrief. It covers qualifications only. Playoff
// results sit behind season-specific routes, which are not used, so a playoff
// video with no manifest score simply has no score lines.
//
// FTC Live rate-limits its API to 30 requests per 5 minutes per event once a
// match is loaded, shared with every other app on the scorekeeper. So each
// match is asked about once per process, whatever the answer, and never
// polled: a result that is not committed yet when the video goes up stays
// absent.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ftcURL is the FTC Live base (http://host[:port]), set from -ftc-url or a
// live switch (POST /api/control/event). Guarded by ftcMu.
var ftcURL string

// currentFTCURL returns the FTC Live base in use.
func currentFTCURL() string {
	ftcMu.Lock()
	defer ftcMu.Unlock()
	return ftcURL
}

// setFTCURL points FTC Live lookups at a new scorekeeper. A different base is a
// different event, so the per-match cache is dropped with it.
func setFTCURL(base string) {
	base = strings.TrimRight(base, "/")
	ftcMu.Lock()
	defer ftcMu.Unlock()
	if base != ftcURL {
		ftcURL = base
		ftcCache = map[string]*ftcMatchResult{}
	}
}

// ftcMatchResult is what one FTC Live match lookup gave.
type ftcMatchResult struct {
	RedScore, BlueScore int
	HasScore            bool
	Red, Blue           []int // team numbers, 0s dropped
}

var (
	ftcMu    sync.Mutex
	ftcCache = map[string]*ftcMatchResult{} // "code/number" -> result (nil = asked, nothing)
)

// ftcClient is swapped in tests.
var ftcClient = &http.Client{Timeout: 5 * time.Second}

// fillFromFTCLive completes a template context with FTC Live's score and team
// numbers for a qualification, where the manifest left them out.
func fillFromFTCLive(ctx *templateContext, store *stateStore, filename string, p parsedFilename) {
	if ctx.HasScore && len(ctx.Alliances) > 0 {
		return
	}
	if !strings.EqualFold(p.Level, "Qualification") || p.MatchNumber < 1 {
		return
	}
	code := p.EventCode
	if rec, ok := store.fimavRecord(filename); ok && rec.EventCode != "" {
		code = rec.EventCode
	}
	res := ftcMatch(code, p.MatchNumber)
	if res == nil {
		return
	}
	if !ctx.HasScore && res.HasScore {
		ctx.RedScore, ctx.BlueScore, ctx.HasScore = res.RedScore, res.BlueScore, true
	}
	if len(ctx.Alliances) == 0 && (len(res.Red) > 0 || len(res.Blue) > 0) {
		ctx.Alliances = map[string][]allianceTeam{
			"red":  ftcTeams(res.Red),
			"blue": ftcTeams(res.Blue),
		}
	}
}

// ftcTeams turns team numbers into alliance entries. FTC Live's v1 match has
// no names; a broadcast override from the audience display still applies.
func ftcTeams(nums []int) []allianceTeam {
	ad := customADNames()
	out := make([]allianceTeam, 0, len(nums))
	for _, n := range nums {
		out = append(out, allianceTeam{Number: n, Name: ad[n]})
	}
	return out
}

// ftcMatch returns the result for one qualification, asking FTC Live at most
// once per code+number for the life of the process.
func ftcMatch(code string, number int) *ftcMatchResult {
	key := fmt.Sprintf("%s/%d", code, number)
	ftcMu.Lock()
	defer ftcMu.Unlock()
	if ftcURL == "" || code == "" {
		return nil
	}
	if res, ok := ftcCache[key]; ok {
		return res
	}
	res, err := fetchFTCMatch(ftcURL, code, number)
	if err != nil {
		log.Printf("ftc live: match %s: %v", key, err)
	}
	ftcCache[key] = res
	return res
}

// ftcV1Match is the part of ApiV1MatchDetailed read here.
type ftcV1Match struct {
	MatchBrief struct {
		Finished bool `json:"finished"`
		Red      struct {
			Team1 int `json:"team1"`
			Team2 int `json:"team2"`
		} `json:"red"`
		Blue struct {
			Team1 int `json:"team1"`
			Team2 int `json:"team2"`
		} `json:"blue"`
	} `json:"matchBrief"`
	RedScore  *int `json:"redScore"`
	BlueScore *int `json:"blueScore"`
}

// fetchFTCMatch makes the one read-only call. A 404 (NO_SUCH_MATCH) means the
// match is not played or not committed yet.
func fetchFTCMatch(base, code string, number int) (*ftcMatchResult, error) {
	u := fmt.Sprintf("%s/api/v1/events/%s/matches/%d/", strings.TrimRight(base, "/"), url.PathEscape(code), number)
	resp, err := ftcClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	var m ftcV1Match
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("GET %s: %v", u, err)
	}
	res := &ftcMatchResult{
		Red:  nonZero(m.MatchBrief.Red.Team1, m.MatchBrief.Red.Team2),
		Blue: nonZero(m.MatchBrief.Blue.Team1, m.MatchBrief.Blue.Team2),
	}
	// An unfinished match reports zeros, which would title the video 0-0.
	if m.MatchBrief.Finished && m.RedScore != nil && m.BlueScore != nil {
		res.RedScore, res.BlueScore, res.HasScore = *m.RedScore, *m.BlueScore, true
	}
	return res, nil
}

func nonZero(nums ...int) []int {
	var out []int
	for _, n := range nums {
		if n > 0 {
			out = append(out, n)
		}
	}
	return out
}
