# youtube-tba-upload

A standalone uploader that uploads FRC match recordings to YouTube and submits
the resulting video URLs to The Blue Alliance. It runs as a small HTTP service
on `:8807` and is designed to be spawned and managed by
[FIM-AV Assistant](https://github.com/firstinmi/fimav-assistant), which hosts
the "Upload" tab that talks to it.

It watches the recording folder, waits for each match file to stop changing (and
for FIM-AV Assistant's dead-time cut to finish), uploads it to YouTube via
Chrome/YouTube Studio automation, sets the title/description (alliance teams from
the shared database + the FMS roster), thumbnail and visibility, adds it to a
playlist, and posts the video to its match on TBA's trusted `match_videos/add`.

## Shared SQLite store

Upload tracking lives in a single SQLite database, `youtube-tba-upload.db`,
**co-located with the recordings** (the same folder as the `.mp4`s). It is the
single source of truth — there is no JSON state file. FIM-AV Assistant writes
the recording / identity / team columns of the `matches` table; this uploader
writes only the upload columns. WAL mode lets both processes share the file.
The schema and column ownership are documented in [INTEGRATION.md](INTEGRATION.md).

## Build

Requires Go (see `go.mod`). The SQLite driver is pure Go (`modernc.org/sqlite`),
so it cross-compiles to Windows with no cgo:

```sh
go build ./...                 # host build
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o youtube-tba-upload.exe .
```

## Run

```sh
youtube-tba-upload \
  -listen :8807 \
  -video-dir "C:\Users\FIM\Videos\2026 Kettering University #1" \
  -fms-url http://10.0.100.5 \
  -tba-url https://www.thebluealliance.com
```

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8807` | HTTP listen address. |
| `-video-dir` | `~/Videos` | The recording folder (holds the `.mp4`s and the shared database). |
| `-fms-url` | `http://10.0.100.5` | FMS base, used to fill team names. |
| `-tba-url` | `https://www.thebluealliance.com` | TBA base for match-video submission. |
| `-program` | `frc` | `frc` (FMS names, TBA submit) or `ftc` (FTC Live names and scores, TOA submit). |
| `-ftc-url` | none | FTC Live scorekeeper base (`http://host[:port]`), FTC only: qualification scores and teams. |
| `-toa-url` | `https://api.theorangealliance.org` | TOA API base for match-video submission, FTC only. |
| `-browser` | autodetect | Explicit browser executable. |

Tool-local data (browser profiles, the managed Chrome, the playlist cache, logs)
lives under `%LOCALAPPDATA%\youtube-tba-upload` (or `$XDG_DATA_HOME`), never in
the recording folder.

## HTTP API

See [INTEGRATION.md](INTEGRATION.md) for the full contract the Upload tab
consumes (`/api/upload/*`, `/api/yt/*`, `/api/health`, `/api/shutdown`).

## Regenerating `tba/consts.go`

The bracket/level constants in `tba/consts.go` are generated from `consts.json`.
They are committed, so a normal build needs no extra step. To regenerate after
editing `consts.json`:

```sh
node consts-gen.js consts.json --output-go tba/consts.go --go-package tba
```

## License and credit

Derived from [lethosor/TBA-uploader](https://github.com/lethosor/TBA-uploader)
(via Filip-Kin/TBA-uploader). The upstream license is preserved in `LICENSE`;
see `NOTICE` for what originates upstream and what changed here.
