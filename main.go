package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Filip-Kin/youtube-tba-upload/internal/ytstudio"
)

// Version is set at build time via -ldflags "-X main.Version=...", the same as
// the root binary. It surfaces on /api/health so the host app can log which
// uploader build it spawned.
var Version = "dev"

var settings struct {
	VideoDir string
}

type FileInfo struct {
	Name  string `json:"name"`
	Mtime int64  `json:"mtime"`
}

// Module-level state populated in main().
var (
	managers   = map[string]*uploadManager{}
	managersMu sync.Mutex
	driver     ytstudio.Driver
	// loginInFlight is true while a sign-in window is open, so repeated clicks
	// don't queue a stack of Login calls behind opMu.
	loginInFlight atomic.Bool
)

func main() {
	addr := flag.String("listen", ":8807", "address to listen on")
	browserExe := flag.String("browser", "", "explicit browser executable path (else autodetect)")
	flag.StringVar(&settings.VideoDir, "video-dir", defaultVideoDir(), "folder containing recorded videos (FIM-AV's \"{year} {event name}\" folder)")
	fmsURLFlag := flag.String("fms-url", "http://10.0.100.5", "FMS base URL, used to fill team names the manifest lacks")
	tbaURLFlag := flag.String("tba-url", "https://www.thebluealliance.com", "TBA base URL for trusted match-video submission")
	flag.Parse()
	fmsURL = *fmsURLFlag
	tbaURL = *tbaURLFlag

	// Mirror all logs to a file so there's debug data after the fact, not just
	// whatever scrolled past in the console window.
	if lf, err := os.OpenFile(filepath.Join(dataRoot(), "autoav-helper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stdout, lf))
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("=== autoav-helper start ===")

	// The -video-dir flag wins when explicitly given; otherwise a folder saved
	// on a previous run wins over the default, so the choice survives a restart.
	videoDirFromFlag := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "video-dir" {
			videoDirFromFlag = true
		}
	})
	if !videoDirFromFlag {
		if c := loadHelperConfig(); c.VideoDir != "" {
			settings.VideoDir = c.VideoDir
		}
	}

	// Driver shared by all event managers. ProfileRoot is the global
	// {data_root}/profiles directory; the browser the driver downloads and
	// owns lives in {data_root}/browser.
	d := ytstudio.NewChromedpDriver(
		filepath.Join(dataRoot(), "profiles"),
		filepath.Join(dataRoot(), "browser"),
		*browserExe,
	)
	d.Verbose = true
	driver = d

	// Kill any managed Chrome left over from a previous uploader process. Those
	// orphans hold the profile's user-data-dir, so a fresh launch is forwarded to
	// a dead instance and never opens a window.
	d.KillStaleBrowsers()

	// Fetch the browser now rather than in the middle of the first match, so
	// the first upload of the day isn't waiting on a download.
	go func() {
		if d.Managed != nil {
			if _, err := d.Managed.Ensure(context.Background()); err != nil {
				log.Printf("browser not ready: %v", err)
				return
			}
		}
		// Confirm the sign-in and cache the channel id/playlists up front, so the
		// tab can show "signed in as X" (or prompt a sign-in) without waiting for
		// the first upload to reveal an expired session.
		verifyChannelOnBoot()
	}()

	lock := sync.Mutex{}
	mux := http.NewServeMux()
	handle := func(method string, p string, handler func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			lock.Lock()
			defer lock.Unlock()
			defer func() {
				if err := recover(); err != nil {
					log.Printf("Internal error: %v\n%s", err, debug.Stack())
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(fmt.Sprintf("Internal error: %v", err)))
				}
			}()

			log.Printf("%s %s %s", r.RemoteAddr, r.Method, r.URL.Path)
			w.Header().Set("access-control-allow-origin", "*")
			w.Header().Set("access-control-allow-methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("access-control-allow-headers", "content-type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if method != "" && method != r.Method {
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte("method not allowed: " + r.Method))
				return
			}
			handler(w, r)
		})
	}

	// Existing endpoints.
	handle(http.MethodGet, "/", handleRoot)
	handle(http.MethodPost, "/save", handleSaveSettings)
	handle(http.MethodGet, "/api/list", apiList)
	// /api/rename accepts both GET (legacy query params) and POST (JSON with optional meta).
	handle("", "/api/rename", apiRename)

	// YouTube auto-upload endpoints. /api/upload/config dispatches on
	// method (GET to read, POST to save) so we register it once.
	handle("", "/api/upload/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			apiUploadGetConfig(w, r)
		case http.MethodPost:
			apiUploadSaveConfig(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	handle(http.MethodPost, "/api/upload/thumbnail", apiUploadThumbnail)
	handle(http.MethodGet, "/api/upload/state", apiUploadGetState)
	handle(http.MethodPost, "/api/upload/scan", apiUploadScan)
	handle(http.MethodPost, "/api/upload/retry", apiUploadRetry)
	handle(http.MethodPost, "/api/upload/skip", apiUploadSkip)
	handle(http.MethodGet, "/api/upload/profiles", apiUploadListProfiles)
	handle(http.MethodGet, "/api/upload/browser", apiUploadBrowser)
	handle(http.MethodPost, "/api/upload/profile/login", apiUploadProfileLogin)
	// /api/upload/login is the tab-facing alias for the sign-in flow (opens a
	// headed browser for the operator to log into YouTube).
	handle(http.MethodPost, "/api/upload/login", apiUploadProfileLogin)
	handle(http.MethodPost, "/api/upload/open-channel", apiUploadOpenChannel)
	handle(http.MethodPost, "/api/upload/logout", apiUploadLogout)
	handle(http.MethodGet, "/api/upload/profile/check", apiUploadProfileCheck)
	// POST sets, DELETE clears.
	handle("", "/api/videos/manual", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			apiVideosManualSet(w, r)
		case http.MethodDelete:
			apiVideosManualDelete(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	// YouTube playlist + backfill endpoints (/api/yt/*). Handlers and their
	// route table live in web_yt.go; this is the single call that wires them in.
	registerYTRoutes(handle)

	// Liveness + identity for the host app (FIM-AV Assistant) that spawns this
	// as an uploader: it polls /api/health to know the process is up and serving
	// before it shows the Upload tab, and logs the version it launched.
	handle(http.MethodGet, "/api/health", func(w http.ResponseWriter, r *http.Request) {
		si := getSignIn()
		writeJSON(w, map[string]any{
			"status":       "ok",
			"version":      Version,
			"video_dir":    settings.VideoDir,
			"watching":     isEventFolder(settings.VideoDir),
			"signed_in":    si.SignedIn,
			"channel_name": si.ChannelName,
			"sign_in":      si,
		})
	})
	// Graceful stop for the host app to call before it quits, so the browser is
	// closed and the profile isn't left locked. Responds, then exits; the same
	// path the signal handler takes.
	handle(http.MethodPost, "/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go func() {
			log.Printf("shutting down (requested via /api/shutdown)")
			closeAllManagers()
			d.Close()
			os.Exit(0)
		}()
	})

	// Shut the browser down on the way out; on Windows it outlives the process
	// otherwise, and the next run then finds the profile locked.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-signals
		log.Printf("shutting down (%s)", sig)
		closeAllManagers()
		d.Close()
		os.Exit(0)
	}()

	log.Printf("listening on %s", *addr)
	_ = http.ListenAndServe(*addr, mux)
}

// ── helpers ────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func requireEventKey(r *http.Request, w http.ResponseWriter) (string, bool) {
	key := r.URL.Query().Get("event_key")
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "event_key query parameter is required")
		return "", false
	}
	return key, true
}

// getOrCreateManager returns the long-lived uploadManager for the given event,
// starting its background loops on first access.
func getOrCreateManager(eventKey string) (*uploadManager, error) {
	managersMu.Lock()
	defer managersMu.Unlock()
	if m, ok := managers[eventKey]; ok {
		return m, nil
	}
	store, err := openStateStore(eventKey)
	if err != nil {
		return nil, err
	}
	m := newUploadManager(store, driver)
	managers[eventKey] = m
	m.Start()
	return m, nil
}

// resumeAllManagers clears the "needs sign-in" flag on every event and wakes
// its upload loop. Called after a successful sign-in or channel check so uploads
// that stopped for re-auth pick back up without waiting for the next scan.
func resumeAllManagers() {
	managersMu.Lock()
	defer managersMu.Unlock()
	for _, m := range managers {
		m.resetSessionFlag()
	}
}

// closeAllManagers stops the upload loops and closes each event's database so
// the WAL is checkpointed on a clean shutdown.
func closeAllManagers() {
	managersMu.Lock()
	defer managersMu.Unlock()
	for _, m := range managers {
		m.Stop()
		if m.store != nil {
			_ = m.store.close()
		}
	}
}

// ── legacy "settings" endpoints (Vue calls /api/list and /api/rename) ─────

func handleRoot(w http.ResponseWriter, r *http.Request) {
	current := settings.VideoDir
	// Event folders sit beside the current one; scan the parent (or the default
	// Videos folder before anything is chosen).
	parent := defaultVideoDir()
	if current != "" {
		parent = filepath.Dir(current)
	}
	folders := eventFolders(parent)

	// Exactly one event folder and nothing valid chosen yet: pick it and persist.
	if !isEventFolder(current) && len(folders) == 1 {
		current = folders[0]
		settings.VideoDir = current
		if err := saveHelperConfig(helperConfig{VideoDir: current}); err != nil {
			log.Printf("persist video dir: %v", err)
		}
	}

	var options strings.Builder
	for _, f := range folders {
		sel := ""
		if f == current {
			sel = " selected"
		}
		fmt.Fprintf(&options, "<option value=\"%s\"%s>%s</option>",
			html.EscapeString(f), sel, html.EscapeString(filepath.Base(f)))
	}
	if options.Len() == 0 {
		options.WriteString("<option value=\"\">None</option>")
	}

	w.Header().Add("content-type", "text/html")
	_, _ = fmt.Fprintf(w, `
		<form action="/save" method="POST">
			<label>Detected folders
				<select name="VideoDir">%s</select>
			</label>
			<input type="submit" value="Save">
		</form>
		<form action="/save" method="POST">
			<label>Event folder
				<input name="VideoDir" value="%s">
			</label>
			<input type="submit" value="Save">
		</form>
	`, options.String(), html.EscapeString(current))
}

// defaultVideoDir is the operator's Videos folder, which is where vMix records
// by default and therefore where FIM-AV Assistant creates its per-event folder.
// The event folder itself still has to be set; this is only a better starting
// point than a path that exists on no recording machine.
func defaultVideoDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Videos")
	}
	return filepath.Join(os.TempDir(), "videos")
}

func handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if val := r.FormValue("VideoDir"); val != "" {
		settings.VideoDir = val
		// Persist so the choice isn't lost on the next restart.
		if err := saveHelperConfig(helperConfig{VideoDir: val}); err != nil {
			log.Printf("persist video dir: %v", err)
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func apiList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	suffix := r.URL.Query().Get("suffix")

	allFiles, err := os.ReadDir(settings.VideoDir)
	if errors.Is(err, os.ErrNotExist) {
		// Say which folder is missing and what to do about it, rather than
		// reporting an internal error.
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"video folder does not exist: %s (set -video-dir to the event folder)", settings.VideoDir))
		return
	}
	if err != nil {
		panic(err)
	}

	files := make([]FileInfo, 0)
	for _, file := range allFiles {
		if !file.Type().IsRegular() {
			continue
		}
		if !strings.HasPrefix(file.Name(), prefix) || !strings.HasSuffix(file.Name(), suffix) {
			continue
		}
		info, err := file.Info()
		if err != nil {
			continue
		}
		files = append(files, FileInfo{
			Name:  file.Name(),
			Mtime: info.ModTime().Unix(),
		})
	}

	out, err := json.Marshal(files)
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(out)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return !os.IsNotExist(err)
}

// apiRename handles both the legacy GET form (old_name, new_name query) and
// the new POST form (JSON body with optional event_key + meta).
//
// When event_key + meta are supplied, we also record the meta on the matching
// videoEntry in the per-event state so the upload pipeline can use it.
func apiRename(w http.ResponseWriter, r *http.Request) {
	var (
		oldName  string
		newName  string
		eventKey string
		meta     *videoMeta
	)

	if r.Method == http.MethodPost {
		var body struct {
			OldName  string     `json:"old_name"`
			NewName  string     `json:"new_name"`
			EventKey string     `json:"event_key"`
			Meta     *videoMeta `json:"meta,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		oldName = body.OldName
		newName = body.NewName
		eventKey = body.EventKey
		meta = body.Meta
	} else {
		oldName = r.URL.Query().Get("old_name")
		newName = r.URL.Query().Get("new_name")
		eventKey = r.URL.Query().Get("event_key")
	}

	if oldName == "" || newName == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("old_name and new_name are required"))
		return
	}

	oldPath := path.Join(settings.VideoDir, oldName)
	newPath := path.Join(settings.VideoDir, newName)

	if !fileExists(oldPath) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("old_path not found: " + oldPath))
		return
	}

	if fileExists(newPath) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("new_path already exists: " + newPath))
		return
	}

	for i := 1; i <= 5; i++ {
		if i > 1 {
			log.Printf("retrying %d/5...", i)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			panic(err)
		}
		// For GDrive: rename can sometimes report success but actually fail.
		// Wait for a bit before checking whether the expected change actually
		// took place.
		time.Sleep(1 * time.Second)
		if fileExists(newPath) && !fileExists(oldPath) {
			break
		}
	}

	// Persist the meta on the matching state entry so the upload pipeline
	// can use it. Done after the rename so we know the file lives at the
	// new name.
	if meta != nil && eventKey != "" {
		m, err := getOrCreateManager(eventKey)
		if err != nil {
			log.Printf("apiRename: get manager for %s: %v", eventKey, err)
		} else {
			err := m.store.update(func(s *eventState) {
				entry, ok := s.Videos[newName]
				if !ok {
					entry = &videoEntry{Status: statusNew}
					s.Videos[newName] = entry
				}
				entry.Meta = meta
			})
			if err != nil {
				log.Printf("apiRename: persist meta: %v", err)
			}
			m.nudge()
		}
	}

	_, _ = w.Write([]byte("{\"ok\": true}"))
}

// ── /api/upload/* endpoints ────────────────────────────────────────────────

func apiUploadGetConfig(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, m.store.snapshot().Config)
}

func apiUploadSaveConfig(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	var cfg eventConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	cfg.EventKey = key
	if cfg.TitleTemplate == "" {
		cfg.TitleTemplate = defaultTitleTemplate
	}
	if cfg.DescriptionTemplate == "" {
		cfg.DescriptionTemplate = defaultDescriptionTemplate
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := m.store.update(func(s *eventState) {
		s.Config = cfg
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Switching video_dir should trigger an immediate scan so the UI updates.
	go m.scanNow()
	writeJSON(w, cfg)
}

func apiUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSONError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	f, header, err := r.FormFile("file")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer f.Close()
	dir := filepath.Join(dataRoot(), "events", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".png"
	}
	out := filepath.Join(dir, "thumbnail"+ext)
	fout, err := os.Create(out)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer fout.Close()
	if _, err := io.Copy(fout, f); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = m.store.update(func(s *eventState) {
		s.Config.ThumbnailPath = out
	})
	writeJSON(w, map[string]string{"thumbnail_path": out})
}

func apiUploadGetState(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, struct {
		eventState
		EventPlaylistName string `json:"event_playlist_name"`
	}{st, eventPlaylistName(st)})
}

func apiUploadScan(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.scanNow()
	writeJSON(w, map[string]bool{"ok": true})
}

type filenameBody struct {
	Filename string `json:"filename"`
}

func apiUploadRetry(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	var body filenameBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Filename == "" {
		writeJSONError(w, http.StatusBadRequest, "filename required")
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := m.requestRetry(body.Filename); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	m.nudge()
	writeJSON(w, map[string]bool{"ok": true})
}

func apiUploadSkip(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	var body filenameBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Filename == "" {
		writeJSONError(w, http.StatusBadRequest, "filename required")
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := m.requestSkip(body.Filename); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// defaultProfileName is used when no profile has been chosen, so uploads work
// without anyone having to invent a name first.
const defaultProfileName = "youtube"

// browserProfile builds the driver profile from an event's config. A configured
// browser user-data dir wins: that is the operator's own browser profile, and
// using it means no separate sign-in. Otherwise the driver uses its own browser
// and its own profile, which is signed in once through Login.
func browserProfile(cfg eventConfig) ytstudio.Profile {
	name := cfg.ProfileName
	if name == "" {
		name = defaultProfileName
	}
	return ytstudio.Profile{
		Name:        name,
		UserDataDir: cfg.BrowserUserDataDir,
		Directory:   cfg.BrowserProfileDirectory,
		DebugPort:   cfg.BrowserDebugPort,
		Exe:         cfg.BrowserExe,
		Headless:    cfg.Headless,
	}
}

// profileForRequest resolves the profile to drive for a login/check request.
// With an event_key it uses that event's browser settings; otherwise it falls
// back to a tool-owned profile by name.
func profileForRequest(eventKey, profileName string) ytstudio.Profile {
	if eventKey != "" {
		if m, err := getOrCreateManager(eventKey); err == nil {
			cfg := m.store.snapshot().Config
			if profileName != "" {
				cfg.ProfileName = profileName
			}
			return browserProfile(cfg)
		}
	}
	if profileName == "" {
		profileName = defaultProfileName
	}
	return ytstudio.Profile{Name: profileName}
}

// apiUploadBrowser reports the installed browsers' profile folders, so the UI
// can offer the live profile instead of asking the operator to sign in again.
func apiUploadBrowser(w http.ResponseWriter, r *http.Request) {
	userDataDir := r.URL.Query().Get("user_data_dir")
	dirs := ytstudio.LiveProfileDirs()
	if userDataDir == "" && len(dirs) > 0 {
		userDataDir = dirs[0]
	}
	port := 0
	if v := r.URL.Query().Get("debug_port"); v != "" {
		port, _ = strconv.Atoi(v)
	}
	managed_version := ""
	if d, ok := driver.(*ytstudio.ChromedpDriver); ok && d.Managed != nil {
		managed_version = d.Managed.InstalledVersion()
	}
	writeJSON(w, map[string]any{
		"user_data_dirs":     dirs,
		"profile_dirs":       browserProfileDirs(userDataDir),
		"default_debug_port": ytstudio.DefaultDebugPort,
		"debug_port_active":  ytstudio.DebugPortActive(port),
		"managed_version":    managed_version,
	})
}

// browserProfileDirs lists the profile folders inside a browser user-data
// directory. A profile folder is one holding a Preferences file.
func browserProfileDirs(userDataDir string) []string {
	out := []string{}
	if userDataDir == "" {
		return out
	}
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(userDataDir, e.Name(), "Preferences")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func apiUploadListProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := listProfiles()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"profiles": profiles})
}

func apiUploadProfileLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileName string `json:"profile_name"`
		EventKey    string `json:"event_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	profile := profileForRequest(body.EventKey, body.ProfileName)
	// One login window at a time. Without this, every click queues another
	// Login behind opMu, and closing one window just opens the next (seen live).
	if !loginInFlight.CompareAndSwap(false, true) {
		writeJSON(w, map[string]bool{"ok": true, "already": true})
		return
	}
	// Login runs the window and returns the channel name once sign-in lands (it
	// closes the window itself). Run it off the request goroutine; use
	// context.Background() because r.Context() is canceled when we respond.
	go func(p ytstudio.Profile) {
		defer loginInFlight.Store(false)
		name, err := driver.Login(context.Background(), p)
		if err != nil {
			log.Printf("login (%s): %v", p.Label(), err)
			return
		}
		if name == "" {
			return // operator closed the window without signing in
		}
		afterLogin(p, name)
	}(profile)
	writeJSON(w, map[string]bool{"ok": true})
}

// apiUploadOpenChannel opens a headed window on the tool profile at YouTube
// Studio and leaves it open for the operator (e.g. to share the login with a
// stream on the same machine). Fire-and-forget: the window stays up until the
// operator closes it or an upload reclaims the profile.
func apiUploadOpenChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileName string `json:"profile_name"`
		EventKey    string `json:"event_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	profile := profileForRequest(body.EventKey, body.ProfileName)
	go func(p ytstudio.Profile) {
		if err := driver.OpenChannel(context.Background(), p); err != nil {
			log.Printf("open-channel (%s): %v", p.Label(), err)
		}
	}(profile)
	writeJSON(w, map[string]bool{"ok": true})
}

func apiUploadProfileCheck(w http.ResponseWriter, r *http.Request) {
	profile := profileForRequest(r.URL.Query().Get("event_key"), r.URL.Query().Get("profile_name"))
	name, err := driver.CheckChannel(r.Context(), profile)
	if err != nil {
		setSignIn(signInState{SignedIn: false, Error: err.Error()})
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	setSignIn(signInState{SignedIn: true, ChannelName: name})
	resumeAllManagers()
	writeJSON(w, map[string]string{"channel_name": name})
}

// apiVideosManualSet records a manually-entered YT video ID for a TBA match.
func apiVideosManualSet(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	var body struct {
		MatchKey string `json:"match_key"`
		VideoID  string `json:"yt_video_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MatchKey == "" {
		writeJSONError(w, http.StatusBadRequest, "match_key required")
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := m.store.update(func(s *eventState) {
		if body.VideoID == "" {
			delete(s.ManualVideoIDs, body.MatchKey)
		} else {
			s.ManualVideoIDs[body.MatchKey] = body.VideoID
		}
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func apiVideosManualDelete(w http.ResponseWriter, r *http.Request) {
	key, ok := requireEventKey(r, w)
	if !ok {
		return
	}
	matchKey := r.URL.Query().Get("match_key")
	if matchKey == "" {
		writeJSONError(w, http.StatusBadRequest, "match_key required")
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = m.store.update(func(s *eventState) {
		delete(s.ManualVideoIDs, matchKey)
	})
	writeJSON(w, map[string]bool{"ok": true})
}
