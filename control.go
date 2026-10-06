package main

// Live control for FIM-AV Assistant: POST /api/control/event and
// POST /api/control/video.
//
// /api/control/event switches the watched folder, event and program without a
// restart (FIM-AV Assistant used to restart the process with new flags). The
// flags stay the startup defaults. /api/control/video queues one finished match
// video at once instead of waiting for the folder scan to see it settle; the
// scan itself is unchanged, so the uploader still works on its own.
//
// Both answer {ok:true} or {ok:false,error} and refuse anything that does not
// come from this machine.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// fromLoopback reports whether the request came from this machine.
func fromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func controlReply(w http.ResponseWriter, code int, err error) {
	w.Header().Set("content-type", "application/json")
	if err == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
}

// samePath compares two folder paths the way the OS does (case-insensitive on
// Windows).
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// errUploadRunning is returned when a switch would reopen a folder whose
// previous manager is still mid-upload. The new manager would requeue that
// video as interrupted and upload it a second time.
var errUploadRunning = errors.New("upload running in this folder")

// switchWatch points the uploader at a folder, event and program. eventKey ""
// keeps the current event. Managers that no longer fit (another event, another
// folder, another program) are stopped; an upload already running in one of
// them finishes and records its result in its own folder. Caller holds the
// request lock (every route runs under it), which serializes switches.
func switchWatch(dir, eventKey, prog string, ftcBase *string) error {
	dir = filepath.Clean(dir)
	if eventKey == "" {
		eventKey = hub.currentKey()
	}
	progChanged := prog != currentProgram()

	managersMu.Lock()
	var retire []string
	for key, m := range managers {
		if key == eventKey && samePath(m.store.dir, dir) && !progChanged {
			continue
		}
		if samePath(m.store.dir, dir) && m.busy.Load() {
			managersMu.Unlock()
			return errUploadRunning
		}
		retire = append(retire, key)
	}
	for _, key := range retire {
		managers[key].Stop()
		delete(managers, key)
	}
	managersMu.Unlock()
	if len(retire) > 0 {
		log.Printf("watch: stopped %s", strings.Join(retire, ", "))
	}

	settings.VideoDir = dir
	setProgram(prog)
	if ftcBase != nil {
		setFTCURL(*ftcBase)
	}

	if eventKey == "" {
		hub.setCurrent(nil)
		hub.noteWatching()
		return nil
	}
	m, err := getOrCreateManager(eventKey)
	if err != nil {
		return err
	}
	hub.setCurrent(m.store)
	log.Printf("watch: %s, event %s, program %s", dir, eventKey, prog)
	return nil
}

// apiControlEvent serves POST /api/control/event
// {videoDir, eventKey?, program?: "frc"|"ftc", ftcUrl?}.
func apiControlEvent(w http.ResponseWriter, r *http.Request) {
	if !fromLoopback(r) {
		controlReply(w, http.StatusForbidden, errors.New("loopback only"))
		return
	}
	var body struct {
		VideoDir string  `json:"videoDir"`
		EventKey string  `json:"eventKey"`
		Program  *string `json:"program"`
		FTCURL   *string `json:"ftcUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		controlReply(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	dir := strings.TrimSpace(body.VideoDir)
	if dir == "" {
		controlReply(w, http.StatusBadRequest, errors.New("videoDir required"))
		return
	}
	prog := currentProgram()
	if body.Program != nil {
		p, err := parseProgram(*body.Program)
		if err != nil {
			controlReply(w, http.StatusBadRequest, err)
			return
		}
		prog = p
	}
	if err := switchWatch(dir, strings.TrimSpace(body.EventKey), prog, body.FTCURL); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errUploadRunning) {
			code = http.StatusConflict
		}
		controlReply(w, code, err)
		return
	}
	controlReply(w, http.StatusOK, nil)
}

// apiControlVideo serves POST /api/control/video {path}: a finished match
// video, queued now.
func apiControlVideo(w http.ResponseWriter, r *http.Request) {
	if !fromLoopback(r) {
		controlReply(w, http.StatusForbidden, errors.New("loopback only"))
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		controlReply(w, http.StatusBadRequest, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	p := strings.TrimSpace(body.Path)
	if p == "" {
		controlReply(w, http.StatusBadRequest, errors.New("path required"))
		return
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(settings.VideoDir, p)
	}
	// Uploads read from the watched folder by file name, so the video has to be
	// in it.
	if !samePath(filepath.Dir(p), settings.VideoDir) {
		controlReply(w, http.StatusBadRequest, fmt.Errorf("not in the watched folder %s", settings.VideoDir))
		return
	}
	key := hub.currentKey()
	if key == "" {
		controlReply(w, http.StatusConflict, errors.New("no event set"))
		return
	}
	m, err := getOrCreateManager(key)
	if err != nil {
		controlReply(w, http.StatusInternalServerError, err)
		return
	}
	if err := m.queueNow(filepath.Base(p)); err != nil {
		controlReply(w, http.StatusBadRequest, err)
		return
	}
	controlReply(w, http.StatusOK, nil)
}

// queueNow marks one finished video ready to upload without waiting out the
// stable delay, then wakes the upload loop. A video already uploading,
// uploaded, skipped or failed is left as it is (retry and skip have their own
// routes). FIM-AV's own record still wins: a recording that is still being
// written or cut is held, as the scan would hold it. Only the grace window for
// a cut that might be queued is skipped, since the caller says the video is
// final.
func (m *uploadManager) queueNow(name string) error {
	if !strings.EqualFold(filepath.Ext(name), ".mp4") {
		return errors.New("not an .mp4")
	}
	pf, ok := parseFilename(name)
	if !ok || !pf.includeLevel() {
		return errors.New("not a match video name")
	}
	info, err := os.Stat(filepath.Join(m.store.dir, name))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("empty or not a file")
	}
	now := nowUnix()
	err = m.store.update(func(s *eventState) {
		entry, ok := s.Videos[name]
		if !ok {
			entry = &videoEntry{Status: statusNew}
			s.Videos[name] = entry
		}
		if isFTC() {
			fillFTCMeta(entry, name, s.Config)
		} else {
			fillMetaFromFilename(entry, name)
		}
		switch entry.Status {
		case statusNew, statusCutting, statusStable:
		default:
			return
		}
		entry.Size = info.Size()
		entry.Mtime = info.ModTime().Unix()
		entry.StableSince = now
		if hold, reason := cutHold(m.store, name, s.Config); hold && reason != reasonCutGrace {
			entry.Status = statusCutting
			entry.HoldReason = reason
			return
		}
		// NextAttempt is left alone: a retry already waiting out its backoff
		// keeps waiting.
		entry.Status = statusStable
		entry.HoldReason = ""
	})
	if err != nil {
		return err
	}
	log.Printf("control: %s queued", name)
	m.nudge()
	return nil
}
