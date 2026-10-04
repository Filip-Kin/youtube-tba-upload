package ytstudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// CreatePlaylist makes a new playlist on the signed-in channel and returns it.
//
// It calls YouTube's own InnerTube endpoint (/youtubei/v1/playlist/create) from
// inside a signed-in www.youtube.com page, the same request the site's "Save →
// New playlist" makes, instead of clicking through Studio's dialog: the request
// and its JSON reply are stable, the dialog's DOM is not. Auth is the
// SAPISIDHASH header the site computes from the SAPISID cookie, and a brand
// channel is addressed through the page's DELEGATED_SESSION_ID.
func (d *ChromedpDriver) CreatePlaylist(ctx context.Context, p Profile, title, visibility string) (Playlist, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	bctx, closeTab, err := d.tab(ctx, p)
	if err != nil {
		return Playlist{}, err
	}
	defer closeTab()

	var base string
	if err := chromedp.Run(bctx,
		applyStealth(),
		chromedp.Navigate("https://www.youtube.com/"),
		chromedp.Sleep(3*time.Second),
		chromedp.Location(&base),
	); err != nil {
		return Playlist{}, wrapLiveErr(p, p.DebugPort, err)
	}
	if detectSignIn(base) {
		return Playlist{}, ErrSessionExpired
	}

	var raw string
	js := fmt.Sprintf(jsCreatePlaylist, strconv.Quote(title), strconv.Quote(visibility))
	if err := chromedp.Run(bctx, chromedp.Evaluate(js, &raw,
		func(ep *cdpruntime.EvaluateParams) *cdpruntime.EvaluateParams {
			return ep.WithAwaitPromise(true)
		},
	)); err != nil {
		return Playlist{}, fmt.Errorf("create playlist: %w", err)
	}
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Playlist{}, fmt.Errorf("create playlist: parse %q: %w", raw, err)
	}
	if out.Error != "" {
		d.logf("create playlist %q failed: %s", title, out.Error)
		return Playlist{}, errors.New("create playlist: " + out.Error)
	}
	d.logf("playlist created: %q -> %s (%s)", title, out.ID, visibility)
	return Playlist{ID: out.ID, Title: title}, nil
}

// jsCreatePlaylist: %s title, %s privacy status (PUBLIC/UNLISTED/PRIVATE), both
// as JSON string literals. Resolves to JSON {"id"} or {"error"}.
const jsCreatePlaylist = `(async () => {
  const title = %s, privacyStatus = %s;
  const cookie = (n) => {
    const c = document.cookie.split('; ').find((x) => x.startsWith(n + '='));
    return c ? c.slice(n.length + 1) : '';
  };
  const sha1 = async (s) => [...new Uint8Array(await crypto.subtle.digest('SHA-1', new TextEncoder().encode(s)))]
    .map((b) => b.toString(16).padStart(2, '0')).join('');
  const ts = Math.floor(Date.now() / 1000);
  const origin = location.origin;
  const auth = [];
  for (const [name, label] of [['SAPISID', 'SAPISIDHASH'], ['__Secure-1PAPISID', 'SAPISID1PHASH'], ['__Secure-3PAPISID', 'SAPISID3PHASH']]) {
    const v = cookie(name);
    if (v) auth.push(label + ' ' + ts + '_' + (await sha1(ts + ' ' + v + ' ' + origin)));
  }
  if (!auth.length) return JSON.stringify({ error: 'no SAPISID cookie (signed out?)' });
  const cfg = window.ytcfg;
  const context = cfg && cfg.get('INNERTUBE_CONTEXT');
  if (!context) return JSON.stringify({ error: 'no INNERTUBE_CONTEXT on the page' });
  const headers = {
    'content-type': 'application/json',
    authorization: auth.join(' '),
    'x-origin': origin,
    'x-goog-authuser': String(cfg.get('SESSION_INDEX') || 0),
  };
  const delegated = cfg.get('DELEGATED_SESSION_ID');
  if (delegated) {
    context.user = Object.assign({}, context.user, { onBehalfOfUser: delegated });
    headers['x-goog-pageid'] = delegated;
  }
  const r = await fetch('/youtubei/v1/playlist/create?prettyPrint=false', {
    method: 'POST',
    credentials: 'include',
    headers,
    body: JSON.stringify({ context, title, privacyStatus, videoIds: [] }),
  });
  const body = await r.text();
  let j = null;
  try { j = JSON.parse(body); } catch (e) { /* not JSON */ }
  if (!r.ok || !j || !j.playlistId) {
    return JSON.stringify({ error: 'HTTP ' + r.status + ': ' + body.slice(0, 400) });
  }
  return JSON.stringify({ id: j.playlistId });
})()`
