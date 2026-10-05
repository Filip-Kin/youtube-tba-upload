# Integrating youtube-tba-upload with FIM-AV Assistant

This uploader folds the YouTube match-video uploader into FIM-AV Assistant so the
AV PC runs fewer apps. FIM-AV Assistant spawns this binary, hosts an **Upload**
tab that talks to its HTTP API on `:8807`, and shares one SQLite database with
it. The chromedp YouTube automation is NOT reimplemented in TypeScript — it
stays in this Go binary.

Two ends, one store:
- **This uploader** (Go) owns the YouTube upload and the TBA submission, and
  writes the **upload columns** of the shared database.
- **FIM-AV Assistant** (Electron/TS) owns recording, hosts the tab, spawns the
  uploader, and writes the **recording / identity / team columns**.

---

## 1. The shared SQLite database

One file, `youtube-tba-upload.db`, **in the recording folder** (beside the
`.mp4`s — the same folder that used to hold `fimav-matches.json`). It is the
single source of truth for match + upload tracking. There is no JSON state file
on either end after migration.

- Driver: pure-Go `modernc.org/sqlite` on the Go side; `better-sqlite3` on the
  TS side. No cgo, cross-compiles to windows/amd64.
- Journal mode: **WAL**, `busy_timeout=5000`, `synchronous=NORMAL`. WAL allows
  many readers and one writer across processes; the busy timeout rides out the
  brief moments both write.
- Concurrency rule: each end writes **only its own columns**. The uploader's
  upsert uses `ON CONFLICT(file_name) DO UPDATE` touching only upload columns, so
  it never clobbers FIM-AV's writes, and vice versa.

### Schema

```sql
CREATE TABLE IF NOT EXISTS matches (
    file_name            TEXT PRIMARY KEY,        -- e.g. QM5_MIKET.mp4
    -- ── owned by FIM-AV Assistant (recording, identity, teams) ──
    file_path            TEXT,
    record_id            TEXT,                    -- FIM-AV MatchRecord id
    event                TEXT,                    -- TBA event key, e.g. 2026miket
    level                TEXT,                    -- Qualification|Playoff|Final|Practice|Test|Manual
    match_number         INTEGER,
    play                 INTEGER,
    tba_match_key        TEXT,                    -- partial key: qm5 / sf3m1 / f1m1
    match_label          TEXT,                    -- "Qualification 5"
    record_status        TEXT,                    -- recording | recorded | error
    has_card             INTEGER,                 -- 0/1
    ended_at             INTEGER,                 -- epoch ms
    teams_json           TEXT,                    -- {"red":[{teamNumber,teamName,card}],"blue":[...]}
    processing_state     TEXT,                    -- unprocessed | queued | processing | done | error
    processing_output    TEXT,
    processing_error     TEXT,
    -- ── owned by this uploader (upload + operational) ──
    upload_status        TEXT,                    -- new|cutting|stable|uploading|uploaded|failed|skipped
    yt_video_id          TEXT,
    yt_url               TEXT,                    -- https://www.youtube.com/watch?v=<id>
    tba_submitted        INTEGER,                 -- 0/1
    tba_error            TEXT,
    title_used           TEXT,
    uploaded_at          TEXT,                    -- RFC3339
    size                 INTEGER,
    mtime                INTEGER,
    stable_since         INTEGER,
    attempts             INTEGER,
    next_attempt         INTEGER,
    last_error           TEXT,
    warnings_json        TEXT,                    -- ["description: ...","playlist: ..."]
    hold_reason          TEXT,                    -- "cut running", ...
    changed_after_upload INTEGER,                 -- 0/1
    updated_at           TEXT
);

-- uploader-owned side tables
CREATE TABLE IF NOT EXISTS upload_config (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    event_key   TEXT,
    config_json TEXT                              -- the eventConfig blob (see §4)
);
CREATE TABLE IF NOT EXISTS upload_manual_video_ids (
    match_key   TEXT PRIMARY KEY,
    yt_video_id TEXT
);
CREATE TABLE IF NOT EXISTS upload_kv (
    key   TEXT PRIMARY KEY,                       -- needs_reauth | last_channel_name
    value TEXT
);
```

Notes:
- `upload_status` uses the uploader's state-machine vocabulary
  (`new/cutting/stable/uploading/uploaded/failed/skipped`); "stable" is the
  "queued to upload" state. The Upload tab should map these to display states.
- Until FIM-AV migrates, the uploader **bootstraps** a `matches` row on first
  sighting of a file, filling `level/match_number/play/tba_match_key/match_label`
  from the filename. Once FIM-AV creates rows first, that insert is a harmless
  fallback and the uploader only updates upload columns.
- `upload_config.config_json` holds the event's TBA trusted `auth_id`/`secret`.
  The database is in the recording folder, so treat that folder as sensitive
  (don't sync it to a shared drive). If that is a problem, the credentials can be
  moved to a tool-local file later; the tab pushes them via the API either way.

---

## 2. HTTP API the Upload tab consumes

Base `http://localhost:8807`. Permissive CORS; event-scoped routes take
`?event_key=<tba_event_key>`.

**Config** — `GET /api/upload/config?event_key=E` → the config object (§4).
`POST` the same object to save (empty templates default; save triggers a rescan).
This is where the tab writes `tba_auth_id`, `tba_secret`,
`playlist_id`+`playlist_name`, templates, visibility,
`thumbnail_path`, and browser profile fields. The template defaults the UI shows
MUST equal the uploader defaults in `template.go` (`defaultTitleTemplate`,
`defaultDescriptionTemplate`) — supported placeholders are `{video_prefix}`,
`{event_name}`, `{event_year}`, `{match_level}`, `{match_number}`,
`{match_label}`, `{play}`, `{play_suffix}`, `{title}`, and
`{red[i].number}`/`{red[i].name}` / `{blue[i].number}`/`{blue[i].name}`.

Practice and test matches are **never uploaded** — the uploader hard-excludes
those levels (`includeLevel`), so there are no include-practice/test flags.

**State** — `GET /api/upload/state?event_key=E` → `{config, videos, manual_video_ids,
needs_reauth, last_channel_name}`. `videos` is keyed by filename with the fields
in §4. Poll ~5 s to render the table.

**Control** — `POST /api/upload/scan|retry|skip?event_key=E`
(`retry`/`skip` take `{"filename":"..."}`). `POST /api/upload/thumbnail?event_key=E`
(multipart `file`).

**Playlists** — `GET /api/yt/playlists?event_key=E[&refresh=1]` → `[{id,title}]`
(cached; scrape on refresh). Store `playlist_id`; title is display + fallback.

**Profiles / sign-in** — `GET /api/upload/profiles`, `GET /api/upload/browser`,
`GET /api/upload/profile/check?event_key=E` (→ `{channel_name}` or `{error}`;
also updates the global sign-in status).
- `POST /api/upload/login` (alias of `/api/upload/profile/login`) — opens a
  **headed** browser for the operator to sign in to YouTube. Uploads always run
  headless; sign-in is always headed. Returns immediately; the operator closes
  the window to finish. Body `{event_key, profile_name?}`.
- `POST /api/upload/logout` — closes any open browser and **deletes the managed
  profile directory** so the next sign-in starts fresh. Refuses a live
  (operator-owned) browser profile. Body `{event_key, profile_name?}`.

**Sign-in status** — surfaced in `GET /api/health` as `signed_in`,
`channel_name`, and a `sign_in` object `{signed_in, channel_name, error,
checked_at}`. Resolved on boot (headless channel check with the tool profile,
which also caches the channel id + playlists) and refreshed after
login/logout/check. The tab shows "signed in as X" or a sign-in prompt from this.

**Manual video ids** — `POST /api/videos/manual?event_key=E`
`{match_key, yt_video_id}` (empty id deletes); `DELETE ...&match_key=K`.

**TBA submit** — auto-submit runs inside the upload flow. For manual/retry:
`POST /api/yt/submit-tba?event_key=E` with `{"filename":"..."}` (one) or empty
body (all uploaded, key-bearing, not-yet-submitted). 400 if no creds. Results in
`tba_submitted`/`tba_submit_error` per video.

**TOA submit (FTC)** — with `-program ftc`, the upload flow sets the video URL on
The Orange Alliance instead of TBA (`PUT {toa-url}/api/match/video`, headers
`X-Application-Origin` + `X-TOA-Key`). The key needs TOA access level 3 or 4; a
myTOA account key is level 1 and is refused. Manual/retry:
`POST /api/yt/submit-toa?event_key=E`, same body and answer as submit-tba. 400 if
no `toa_api_key`/`toa_event_key`, or if not running as FTC. Results in
`toa_submitted`/`toa_submit_error`/`toa_match_key` per video.

FTC recording names: `Q3_fimavtest.mp4` (in-season) and
`2026 FIM AV Test - Qualification Q3.mp4` (off-season); the short name is
`{match_label}`. Qualification scores and team numbers come from the manifest,
else from one read of FTC Live `GET {ftc-url}/api/v1/events/{code}/matches/{n}/`
per match (code from the manifest record's `eventCode`, else the file name).

**Backfill** — `POST /api/yt/backfill?event_key=E` re-applies description +
playlist over uploaded videos in match order (background). Run only when idle.

**Health / shutdown** — `GET /api/health` → `{status, version, video_dir,
watching}`. `POST /api/shutdown` → closes the browser and exits (also
checkpoints the WAL).

---

## 3. Bundling, spawning, lifecycle (FIM-AV Assistant side)

Follow FIM-AV Assistant's existing idioms (verified against branch
`feat/auto-av-tab`):

- **Addon**: add `src/main/addons/upload-helper.ts` as a lazy singleton
  (`static get Instance`) with `start()/stop()`, scoped loggers from
  `addon-loggers.ts`, a tracked child `ChildProcessWithoutNullStreams`, and an
  identity-guarded `exit` handler — mirroring `live-captions.ts`. Register it in
  `src/main/addons/index.ts` (field + `start`/`stop`/`restart`); it is then torn
  down on every quit path by `addons.stop()` in the `before-quit` hook
  (`main.ts`).
- **Binary location**: ship `youtube-tba-upload.exe` under `assets/` (bundled by
  the existing `extraResources: ["./assets/**"]`) and resolve it with the
  `getAssetPath` pattern (`app.isPackaged ? process.resourcesPath/assets :
  __dirname/../../assets`), the same way `HWCheck.ts` resolves SoundVolumeView.
- **Spawn args**: `-video-dir <current event folder>`, `-listen :8807`,
  `-fms-url <fms>`, `-tba-url <tba>`. At an FTC event add `-program ftc` and
  `-ftc-url http://<host[:port]>`. No `shell: true`.
- **Readiness**: after spawn, poll `GET /api/health` with
  `fetch`+`AbortSignal.timeout(...)` (the idiom `register-events.ts` already uses
  to reach the captions uploader) until it answers, then report running.
- **Stop**: `POST /api/shutdown` first (clean browser close + WAL checkpoint),
  then fall back to the `killExisting()` sweep on timeout.

- **Tab**: `src/renderer/pages/upload/index.tsx` (antd + `AddonControlRow`) +
  `Route` in `AppRoutes.tsx` + `TabDef` in `TabBar.tsx`. The renderer can
  `fetch('http://localhost:8807/...')` directly (permissive CORS), as the old Vue
  UI did and as the LiveCaptions tab reaches its uploader. Show per-match
  `upload_status` + `yt_url` link + TBA-submitted, with retry hitting
  `/api/yt/submit-tba`.
- **Settings**: persist under a new `upload` key in `store.ts` (schema + default
  + dated `migrations` entry), read/written via `upload:getSettings` /
  `upload:saveSettings` ↔ `upload:settings` IPC, then pushed to the uploader with
  `POST /api/upload/config`.

---

## 4. Data shapes

`eventConfig` (`upload_config.config_json`, and `/api/upload/config`):

```jsonc
{
  "event_key": "2026miket", "event_name": "Kettering University #1",
  "profile_name": "youtube",
  "playlist_id": "PLEliS6gfgle4", "playlist_name": "...",
  "title_template": "...", "description_template": "...",
  "thumbnail_path": "...",               // local image path; ytstudio sets it as the thumbnail
  "visibility": "UNLISTED",              // PUBLIC | UNLISTED | PRIVATE
  "browser_user_data_dir": "", "browser_profile_directory": "",
  "browser_debug_port": 0, "browser_exe": "",
  "cut_wait_seconds": 0,                 // 0=default 120s; <0=don't wait for cut
  "tba_auth_id": "", "tba_secret": "",   // set = each upload's URL is posted to TBA
  "toa_api_key": "", "toa_event_key": "" // FTC, set = each upload's URL is set on TOA (key level 3+)
}
```

`videoEntry` (values of `state.videos`, projected from the `matches` row):

```jsonc
{
  "size": 0, "mtime": 0,
  "status": "new|cutting|stable|uploading|uploaded|failed|skipped",
  "yt_video_id": "NlDNGu6SHgw", "title_used": "...", "uploaded_at": "RFC3339",
  "attempts": 0, "next_attempt": 0, "last_error": "", "warnings": [...],
  "hold_reason": "", "changed_after_upload": false,
  "meta": { "tba_match_key": "qm5", "match_level": "Qualification",
            "match_number": 5, "match_label": "Qualification 5", "play": 1 },
  "tba_submitted": false, "tba_submit_error": "",
  "toa_submitted": false, "toa_submit_error": "", "toa_match_key": ""  // FTC only
}
```

---

## 5. FIM-AV migration: fimav-matches.json → the shared database

Today FIM-AV Assistant keeps match records in `fimav-matches.json`
(`src/main/recordings/matchStore.ts`). The migration:

1. **Add a db module** in FIM-AV using `better-sqlite3`, opening the same
   `youtube-tba-upload.db` in the recording folder in WAL mode. Create the
   schema (identical to §1) if absent — so whichever process starts first
   initializes it; `CREATE TABLE IF NOT EXISTS` makes this idempotent.
2. **Rewrite `matchStore.ts`** to read/write the `matches` table instead of the
   JSON file, keeping the same `MatchRecord` shape and the same public functions
   (`upsertMatch`, `updateMatch`, `getMatch`, `listMatches`). Map fields to
   columns:
   - `fileName`→`file_name` (PK), `filePath`→`file_path`, `id`→`record_id`,
     `status`→`record_status`, `hasCard`→`has_card`, `endedAt`→`ended_at`,
     `teams`→`teams_json`, `processing.{state,outputPath,error}`→
     `processing_state/processing_output/processing_error`.
   - Also write identity columns FIM-AV knows: `event`, `level`, `match_number`,
     `play`, `tba_match_key`, `match_label`.
   - **Write only these FIM-AV-owned columns** (INSERT sets them; UPDATE sets
     only them). Never touch `upload_*` / operational columns — the uploader owns
     those. Use `INSERT ... ON CONFLICT(file_name) DO UPDATE SET <fimav cols>`.
3. **`listMatches`** for the AutoAV tab now also has the upload columns available
   in the same row, so the tab can show upload status without a second store.
4. **Retire `fimav-matches.json`**: stop writing it. Recording must not break —
   keep the record fields/behavior identical through the switch. On a fresh or
   empty database, initialize cleanly (empty `matches` table). No data migration
   of old JSON is required (events are short-lived); if desired, a one-time
   import can read an existing `fimav-matches.json` into the table on first open.
5. **Uploader already reads teams + cut state from the database** (this repo:
   `db.go`/`fimav.go`), not the JSON manifest. So once FIM-AV writes the columns,
   descriptions get team names and the cut-hold gate works, with the JSON gone.

Until step 4 lands, both stores can coexist: the uploader reads recording state
from the database (and treats "no recording data yet" as "don't gate", exactly
as it treated a missing manifest). "Both ends use the same store" is fully true
only after the FIM-AV match store is migrated and `fimav-matches.json` retired.
