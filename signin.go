package main

// YouTube sign-in status and the sign-in / sign-out endpoints.
//
// The uploader keeps a single global sign-in status (which channel the tool
// profile is logged into, if any), resolved once on boot and refreshed after a
// sign-in, a sign-out, or an on-demand channel check. The Upload tab reads it to
// show "signed in as X" or to prompt a sign-in, instead of discovering an
// expired session only when an upload fails.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/Filip-Kin/youtube-tba-upload/internal/ytstudio"
)

// signInState is the tool profile's YouTube sign-in status.
type signInState struct {
	SignedIn    bool   `json:"signed_in"`
	ChannelName string `json:"channel_name,omitempty"`
	Error       string `json:"error,omitempty"`
	CheckedAt   int64  `json:"checked_at"`
}

var (
	signInMu   sync.Mutex
	signInStat signInState
)

func getSignIn() signInState {
	signInMu.Lock()
	defer signInMu.Unlock()
	return signInStat
}

func setSignIn(s signInState) {
	s.CheckedAt = nowUnix()
	signInMu.Lock()
	signInStat = s
	signInMu.Unlock()
}

// verifyChannelOnBoot resolves the signed-in channel for the default tool
// profile, records it, and caches the channel id + playlists so the dropdown is
// ready without launching Chrome again. Runs headless and best-effort: any error
// records "not signed in" rather than failing anything.
func verifyChannelOnBoot() {
	profile := ytstudio.Profile{Name: defaultProfileName, Headless: true}
	name, err := driver.CheckChannel(context.Background(), profile)
	if err != nil {
		setSignIn(signInState{SignedIn: false, Error: err.Error()})
		log.Printf("channel check: not signed in (%v)", err)
		return
	}
	setSignIn(signInState{SignedIn: true, ChannelName: name})
	log.Printf("channel check: signed in as %q", name)
	if _, err := refreshPlaylists(context.Background(), profile); err != nil {
		log.Printf("channel check: playlist prefetch failed: %v", err)
	}
}

// afterLogin runs once sign-in lands (Login already read the channel name and
// closed the window). It records the signed-in state, warms the playlist cache,
// and resumes any upload loops that had stopped for re-auth. The playlist warm
// reopens the browser headless, but opMu serializes it behind the login that
// just closed, so the two never race on the profile.
func afterLogin(profile ytstudio.Profile, channelName string) {
	setSignIn(signInState{SignedIn: true, ChannelName: channelName})
	log.Printf("after login: signed in as %q", channelName)
	if _, err := refreshPlaylists(context.Background(), profile); err != nil {
		log.Printf("after login: playlist prefetch failed: %v", err)
	}
	resumeAllManagers()
}

// apiUploadLogout signs out by closing any open browser and deleting the managed
// profile directory, so the next sign-in starts fresh. It refuses to touch a
// live (operator-owned) browser profile.
func apiUploadLogout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileName string `json:"profile_name"`
		EventKey    string `json:"event_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	profile := profileForRequest(body.EventKey, body.ProfileName)
	if profile.Live() {
		writeJSONError(w, http.StatusBadRequest,
			"signed in through the operator's own browser; log out there")
		return
	}
	// Close any open session first so the profile directory isn't locked.
	if cd, ok := driver.(*ytstudio.ChromedpDriver); ok {
		cd.Close()
	}
	if err := os.RemoveAll(profileDir(profile.Name)); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "remove profile: "+err.Error())
		return
	}
	setSignIn(signInState{SignedIn: false})
	log.Printf("logout: removed profile %q", profile.Name)
	writeJSON(w, map[string]bool{"ok": true})
}
