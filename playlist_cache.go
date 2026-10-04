package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Filip-Kin/youtube-tba-upload/internal/ytstudio"
)

// The channel's playlists are cached on disk so the dropdown survives restarts
// and the upload/backfill paths can resolve a playlist_id -> title WITHOUT
// launching Chrome every time. The channel id is stored for reference only; a
// refresh always resolves it fresh (see refreshPlaylists).
type ytPlaylistCache struct {
	ChannelID string              `json:"channel_id"`
	Playlists []ytstudio.Playlist `json:"playlists"`
	FetchedAt int64               `json:"fetched_at"`
}

var plCacheMu sync.Mutex

func playlistCachePath() string {
	return filepath.Join(dataRoot(), "yt-playlists.json")
}

func loadPlaylistCache() ytPlaylistCache {
	plCacheMu.Lock()
	defer plCacheMu.Unlock()
	var c ytPlaylistCache
	if data, err := os.ReadFile(playlistCachePath()); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

func savePlaylistCache(c ytPlaylistCache) {
	plCacheMu.Lock()
	defer plCacheMu.Unlock()
	c.FetchedAt = time.Now().Unix()
	if data, err := json.MarshalIndent(c, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(playlistCachePath()), 0o755)
		_ = os.WriteFile(playlistCachePath(), data, 0o644)
	}
}

// clearPlaylistCache drops the cached channel id and playlists. Called on
// sign-in and sign-out: a new account is a new channel, and a stale channel id
// would send every playlist lookup to the previous account's channel.
func clearPlaylistCache() {
	plCacheMu.Lock()
	defer plCacheMu.Unlock()
	_ = os.Remove(playlistCachePath())
}

// refreshPlaylists scrapes the signed-in channel and persists the result.
// Returns the fresh list. It always resolves the channel id from the
// studio.youtube.com redirect rather than the cache: a cached id can belong to
// an account that is no longer signed in, and the redirect costs a few seconds
// on a refresh that runs rarely.
func refreshPlaylists(ctx context.Context, profile ytstudio.Profile) ([]ytstudio.Playlist, error) {
	pls, channelID, err := driver.ListPlaylists(ctx, profile, "")
	if err != nil {
		return nil, err
	}
	if pls == nil {
		pls = []ytstudio.Playlist{}
	}
	savePlaylistCache(ytPlaylistCache{ChannelID: channelID, Playlists: pls})
	return pls, nil
}

// resolvePlaylistTitle returns the current title for a stored playlist id,
// preferring the on-disk cache (no Chrome). On a cache miss it scrapes once to
// populate the cache, then resolves. Returns "" if it still can't be found.
func resolvePlaylistTitle(ctx context.Context, profile ytstudio.Profile, playlistID string) string {
	if playlistID == "" {
		return ""
	}
	if title := playlistTitleByID(loadPlaylistCache().Playlists, playlistID); title != "" {
		return title
	}
	pls, err := refreshPlaylists(ctx, profile)
	if err != nil {
		return ""
	}
	return playlistTitleByID(pls, playlistID)
}

// eventPlaylistName is the playlist an event's match videos go into: the
// event's {video_prefix}, rendered the way titles render it, from the first
// match video in the folder (or the configured event name when there is none).
func eventPlaylistName(st eventState) string {
	type named struct {
		p    parsedFilename
		name string
	}
	var vids []named
	for name := range st.Videos {
		if p, ok := parseFilename(name); ok {
			vids = append(vids, named{p, name})
		}
	}
	sort.Slice(vids, func(i, j int) bool {
		if ki, kj := vids[i].p.orderKey(), vids[j].p.orderKey(); ki != kj {
			return ki < kj
		}
		return vids[i].name < vids[j].name
	})
	p := parsedFilename{}
	if len(vids) > 0 {
		p = vids[0].p
	}
	return strings.TrimSpace(buildTemplateContext(p, nil, st.Config).VideoPrefix)
}

// ensurePlaylist returns the channel's playlist titled name, creating it with
// the given visibility when the channel has none by that title. Reusing an
// exact title match means a restart or a second click never makes a duplicate.
func ensurePlaylist(ctx context.Context, profile ytstudio.Profile, name, visibility string) (ytstudio.Playlist, error) {
	if name == "" {
		return ytstudio.Playlist{}, errors.New("no event name to title the playlist")
	}
	pls, err := refreshPlaylists(ctx, profile)
	if err != nil {
		return ytstudio.Playlist{}, err
	}
	for _, pl := range pls {
		if strings.EqualFold(strings.TrimSpace(pl.Title), name) {
			return pl, nil
		}
	}
	pl, err := driver.CreatePlaylist(ctx, profile, name, visibility)
	if err != nil {
		return ytstudio.Playlist{}, err
	}
	cache := loadPlaylistCache()
	cache.Playlists = append([]ytstudio.Playlist{pl}, pls...)
	savePlaylistCache(cache)
	return pl, nil
}
