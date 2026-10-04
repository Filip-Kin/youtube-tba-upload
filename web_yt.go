package main

// YouTube playlist + backfill HTTP handlers.
//
// These live in the autoav-helper process, not the root TBA-uploader web
// server: they need the ytstudio driver, the per-event state store and the
// browser profiles, all of which this binary owns. The Vue layer already
// reaches this server for every other driver-backed call via its
// autoAVHelperApiUrl (see /api/upload/*), so /api/yt/* belongs here too.
//
// The routes are registered from main() through registerYTRoutes so the
// registration is a single grouped call there rather than scattered edits.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"

	"github.com/Filip-Kin/youtube-tba-upload/internal/ytstudio"
)

// registerYTRoutes wires the /api/yt/* endpoints onto main()'s router. handle
// has the same signature as the closure defined in main().
func registerYTRoutes(handle func(method, path string, handler func(http.ResponseWriter, *http.Request))) {
	handle(http.MethodGet, "/api/yt/playlists", apiYTPlaylists)
	handle(http.MethodPost, "/api/yt/playlists/create", apiYTCreatePlaylist)
	handle(http.MethodPost, "/api/yt/backfill", apiYTBackfill)
	handle(http.MethodPost, "/api/yt/submit-tba", apiYTSubmitTBA)
}

// apiYTSubmitTBA posts uploaded videos' URLs to their TBA matches. With a
// {"filename": "..."} body it (re)submits that one video, which is what the
// tab's per-row retry uses; with no filename it submits every uploaded,
// key-bearing video not yet on TBA, which covers a first run over an event that
// uploaded before credentials were entered. Auto-submit already handles the
// happy path in the upload flow (worker.go); this is the manual/retry entry.
func apiYTSubmitTBA(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	var body filenameBody
	_ = json.NewDecoder(r.Body).Decode(&body) // filename optional
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	st := m.store.snapshot()
	if st.Config.TBAAuthID == "" || st.Config.TBASecret == "" {
		writeJSONError(w, http.StatusBadRequest, "TBA trusted-API auth id/secret not set")
		return
	}

	var targets []string
	if body.Filename != "" {
		targets = []string{body.Filename}
	} else {
		for name, entry := range st.Videos {
			if entry.Status == statusUploaded && entry.YTVideoID != "" && !entry.TBASubmitted {
				targets = append(targets, name)
			}
		}
	}

	// A per-row retry has to submit even a video already marked submitted (the
	// operator is forcing it); clear the flag first so submitToTBA runs.
	if body.Filename != "" {
		_ = m.store.update(func(s *eventState) {
			if v := s.Videos[body.Filename]; v != nil {
				v.TBASubmitted = false
			}
		})
	}

	submitted := 0
	for _, name := range targets {
		m.submitToTBA(name)
		if v := m.store.snapshot().Videos[name]; v != nil && v.TBASubmitted {
			submitted++
		}
	}
	writeJSON(w, map[string]any{"ok": true, "attempted": len(targets), "submitted": submitted})
}

// apiYTPlaylists returns the channel's playlists as [{"id","title"}], scraped
// live from the Studio playlists page. Backs the UI's playlist dropdown so the
// operator picks a real playlist (title shown, id stored) instead of typing a
// name that might not match.
func apiYTPlaylists(w http.ResponseWriter, r *http.Request) {
	profile := profileForRequest(r.URL.Query().Get("event_key"), r.URL.Query().Get("profile_name"))
	// Serve the cached list (survives restarts, no Chrome) unless ?refresh=1.
	if r.URL.Query().Get("refresh") != "1" {
		if c := loadPlaylistCache(); len(c.Playlists) > 0 {
			writeJSON(w, c.Playlists)
			return
		}
	}
	pls, err := refreshPlaylists(r.Context(), profile)
	if err != nil {
		// Fall back to a stale cache so the dropdown still populates on error.
		if c := loadPlaylistCache(); len(c.Playlists) > 0 {
			writeJSON(w, c.Playlists)
			return
		}
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, pls)
}

// apiYTCreatePlaylist makes (or reuses, by title) the event's playlist, named
// after the event's {video_prefix}, and returns it as {"id","title"}. It does
// not select it: the settings dialog sets the field and Save stores it.
func apiYTCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := m.store.snapshot()
	pl, err := ensurePlaylist(r.Context(), browserProfile(st.Config), eventPlaylistName(st), uploadVisibility(st.Config))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, pl)
}

// backfillItem is one already-uploaded video to re-process.
type backfillItem struct {
	Filename string
	VideoID  string
}

// apiYTBackfill re-applies the description (from the template) and the playlist
// membership (by stored id) to every already-uploaded video in the event. It
// runs in the background because a full pass drives the browser one video at a
// time and easily outlasts an HTTP request; progress lands in each video's
// state (warnings + last_error), which the UI already polls.
func apiYTBackfill(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := m.store.snapshot()
	cfg := st.Config
	profile := browserProfile(cfg)

	// Collect the videos already on YouTube.
	var items []backfillItem
	for name, entry := range st.Videos {
		if entry.YTVideoID != "" {
			items = append(items, backfillItem{Filename: name, VideoID: entry.YTVideoID})
		}
	}
	// Process in MATCH order (qm1, qm2, ... not string order where "Match 10"
	// sorts before "Match 2"), so the playlist — which YT only ever appends to —
	// comes out in the right order.
	orderKeyOf := func(name string) int64 {
		if p, ok := parseFilename(name); ok {
			return p.orderKey()
		}
		return int64(1) << 62
	}
	sort.Slice(items, func(i, j int) bool {
		ki, kj := orderKeyOf(items[i].Filename), orderKeyOf(items[j].Filename)
		if ki != kj {
			return ki < kj
		}
		return items[i].Filename < items[j].Filename
	})

	if len(items) == 0 {
		writeJSON(w, map[string]any{"ok": true, "count": 0})
		return
	}

	// Resolve the playlist title from the cache (Chrome only on a cache miss);
	// the stored id is authoritative, the configured name is the fallback.
	playlistName := cfg.PlaylistName
	if cfg.PlaylistID != "" {
		if title := resolvePlaylistTitle(context.Background(), profile, cfg.PlaylistID); title != "" {
			playlistName = title
		} else {
			log.Printf("backfill: playlist id %s not resolvable (using name %q)", cfg.PlaylistID, cfg.PlaylistName)
		}
	}

	go runBackfill(m, profile, playlistName, items, cfg)

	writeJSON(w, map[string]any{"ok": true, "count": len(items)})
}

// runBackfill re-applies description + playlist to each already-uploaded video,
// recording per-video progress into state. It runs off the request goroutine.
//
// NOTE(verify-live): this drives the same browser session as the upload worker.
// YT Studio dislikes two operations on one profile at once, so run a backfill
// when the upload queue is idle.
func runBackfill(m *uploadManager, profile ytstudio.Profile, playlistName string, items []backfillItem, cfg eventConfig) {
	log.Printf("backfill: starting over %d video(s)", len(items))
	for _, it := range items {
		// Render the description from the template + the video's meta, exactly as
		// a fresh upload would. If the filename doesn't parse, skip the
		// description (nothing reliable to render) but still fix the playlist.
		description := ""
		if p, ok := parseFilename(it.Filename); ok {
			var meta *videoMeta
			if entry := m.store.snapshot().Videos[it.Filename]; entry != nil {
				meta = entry.Meta
			}
			ctx := buildTemplateContext(p, meta, cfg)
			if al := alliancesForFile(m.store, it.Filename); len(al) > 0 {
				ctx.Alliances = al
			}
			if r, b, ok := scoreForFile(m.store, it.Filename); ok {
				ctx.RedScore, ctx.BlueScore, ctx.HasScore = r, b, true
			}
			ctx.Title = renderTitle(cfg.TitleTemplate, ctx)
			description = renderDescription(cfg.DescriptionTemplate, ctx)
		}

		res, err := driver.Backfill(context.Background(), profile, it.VideoID, ytstudio.BackfillInput{
			Description:  description,
			PlaylistName: playlistName,
		})

		_ = m.store.update(func(s *eventState) {
			v, ok := s.Videos[it.Filename]
			if !ok {
				return
			}
			v.Warnings = nil
			if err != nil {
				// A whole-video failure (open edit page, session expired, ...).
				v.LastError = "backfill: " + err.Error()
				if errors.Is(err, ytstudio.ErrSessionExpired) {
					s.NeedsReauth = true
				}
				return
			}
			v.LastError = ""
			if res.DescriptionError != "" {
				v.Warnings = append(v.Warnings, "description: "+res.DescriptionError)
			}
			if res.PlaylistError != "" {
				v.Warnings = append(v.Warnings, "playlist: "+res.PlaylistError)
			}
		})
		if err != nil {
			log.Printf("backfill: %s (%s): %v", it.Filename, it.VideoID, err)
		} else {
			log.Printf("backfill: %s (%s): done (descErr=%q playlistErr=%q)", it.Filename, it.VideoID, res.DescriptionError, res.PlaylistError)
		}
	}
	log.Printf("backfill: finished")
}
