package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate gives a test its own data root, recording folder, event hub and
// manager table, and puts the package globals back afterwards.
func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv("YT_TBA_UPLOAD_DATA_DIR", t.TempDir())
	dir := t.TempDir()

	oldDir, oldHub, oldProg, oldSI := settings.VideoDir, hub, currentProgram(), getSignIn()
	settings.VideoDir = dir
	hub = &eventHub{clients: map[chan []byte]struct{}{}}
	managersMu.Lock()
	oldManagers := managers
	managers = map[string]*uploadManager{}
	managersMu.Unlock()

	t.Cleanup(func() {
		managersMu.Lock()
		for _, m := range managers {
			m.Stop()
			m.loops.Wait()
		}
		managers = oldManagers
		managersMu.Unlock()
		settings.VideoDir = oldDir
		setProgram(oldProg)
		signInMu.Lock()
		signInStat = oldSI
		signInMu.Unlock()
		hub = oldHub
	})
	return dir
}

// newTestServer serves the real routes. Closed by t.Cleanup, which runs after
// the stream bodies close (cleanups run last-in first-out); a deferred Close
// would wait forever on the open stream.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux())
	t.Cleanup(srv.Close)
	return srv
}

// sseReader reads one "data:" message at a time from an event stream,
// skipping ": ping" comments.
type sseReader struct {
	t  *testing.T
	sc *bufio.Scanner
	ch chan map[string]any
}

func openEvents(t *testing.T, srv *httptest.Server) *sseReader {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("content-type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	r := &sseReader{t: t, sc: bufio.NewScanner(resp.Body), ch: make(chan map[string]any, 16)}
	go func() {
		for r.sc.Scan() {
			line := r.sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Errorf("bad data line %q: %v", line, err)
				continue
			}
			r.ch <- m
		}
		close(r.ch)
	}()
	return r
}

// next returns the next message of the given type, failing after a timeout.
func (r *sseReader) next(kind string) map[string]any {
	r.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-r.ch:
			if !ok {
				r.t.Fatalf("stream closed waiting for %s", kind)
			}
			if m["type"] == kind {
				return m
			}
		case <-timeout:
			r.t.Fatalf("no %s message", kind)
		}
	}
}

func postJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, out
}

func TestEventsHelloAndSignin(t *testing.T) {
	dir := isolate(t)
	setSignIn(signInState{SignedIn: false})
	srv := newTestServer(t)

	ev := openEvents(t, srv)
	hello := ev.next("hello")
	if hello["addon"] != "youtube-tba-upload" || hello["protocol"] != float64(1) || hello["version"] != Version {
		t.Fatalf("hello identity: %v", hello)
	}
	si, _ := hello["signin"].(map[string]any)
	if si == nil || si["signedIn"] != false || si["channel"] != nil {
		t.Fatalf("hello signin: %v", hello["signin"])
	}
	w, _ := hello["watching"].(map[string]any)
	if w == nil || w["videoDir"] != dir || w["program"] != "frc" {
		t.Fatalf("hello watching: %v", hello["watching"])
	}
	if q, _ := hello["queue"].(map[string]any); q == nil || q["counts"] == nil {
		t.Fatalf("hello queue: %v", hello["queue"])
	}

	// A change: the profile signs in.
	setSignIn(signInState{SignedIn: true, ChannelName: "FIM AV"})
	got := ev.next("signin")
	if got["signedIn"] != true || got["channel"] != "FIM AV" {
		t.Fatalf("signin: %v", got)
	}

	// A session that expires mid-upload flips the event's re-auth flag; the
	// stream reports that as signed out.
	if _, out := postJSON(t, srv.URL+"/api/control/event", map[string]any{"videoDir": dir, "eventKey": "2026test"}); out["ok"] != true {
		t.Fatalf("control/event: %v", out)
	}
	managersMu.Lock()
	m := managers["2026test"]
	managersMu.Unlock()
	_ = m.store.update(func(s *eventState) { s.NeedsReauth = true })
	if got := ev.next("signin"); got["signedIn"] != false {
		t.Fatalf("signin after expiry: %v", got)
	}
}

func TestControlEventSwitchesLive(t *testing.T) {
	dir2 := t.TempDir() // before isolate, so it outlives the managers
	dir := isolate(t)
	srv := newTestServer(t)

	if _, out := postJSON(t, srv.URL+"/api/control/event", map[string]any{"videoDir": dir, "eventKey": "2026aaa"}); out["ok"] != true {
		t.Fatalf("first switch: %v", out)
	}
	ev := openEvents(t, srv)
	ev.next("hello")

	code, out := postJSON(t, srv.URL+"/api/control/event", map[string]any{
		"videoDir": dir2, "eventKey": "2026bbb", "program": "ftc", "ftcUrl": "http://10.0.100.9/",
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("switch: %d %v", code, out)
	}
	if settings.VideoDir != dir2 || !isFTC() || currentFTCURL() != "http://10.0.100.9" {
		t.Fatalf("not switched: dir=%s program=%s ftc=%s", settings.VideoDir, currentProgram(), currentFTCURL())
	}
	managersMu.Lock()
	_, oldThere := managers["2026aaa"]
	m := managers["2026bbb"]
	managersMu.Unlock()
	if oldThere || m == nil || m.store.dir != dir2 {
		t.Fatalf("managers after switch: old=%v new=%v", oldThere, m)
	}
	w := ev.next("watching")
	if w["videoDir"] != dir2 || w["eventKey"] != "2026bbb" || w["program"] != "ftc" {
		t.Fatalf("watching: %v", w)
	}

	// Bad input answers ok:false.
	if code, out := postJSON(t, srv.URL+"/api/control/event", map[string]any{"videoDir": dir2, "program": "vex"}); code != http.StatusBadRequest || out["ok"] != false || out["error"] == nil {
		t.Fatalf("bad program: %d %v", code, out)
	}
	if _, out := postJSON(t, srv.URL+"/api/control/event", map[string]any{}); out["ok"] != false {
		t.Fatalf("missing videoDir: %v", out)
	}
}

func TestControlRoutesLoopbackOnly(t *testing.T) {
	isolate(t)
	mux := newMux()
	for _, p := range []string{"/api/control/event", "/api/control/video"} {
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{}`))
		req.RemoteAddr = "192.0.2.10:5000"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusForbidden || out["ok"] != false {
			t.Fatalf("%s from remote: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestControlVideoQueuesNow(t *testing.T) {
	dir := isolate(t)
	srv := newTestServer(t)

	// No event yet.
	if _, out := postJSON(t, srv.URL+"/api/control/video", map[string]any{"path": filepath.Join(dir, "QM5_MIKET.mp4")}); out["ok"] != false {
		t.Fatalf("no event: %v", out)
	}
	if _, out := postJSON(t, srv.URL+"/api/control/event", map[string]any{"videoDir": dir, "eventKey": "2026miket"}); out["ok"] != true {
		t.Fatalf("control/event: %v", out)
	}
	ev := openEvents(t, srv)
	ev.next("hello")

	name := "QM5_MIKET.mp4"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := postJSON(t, srv.URL+"/api/control/video", map[string]any{"path": filepath.Join(dir, name)})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("control/video: %d %v", code, out)
	}
	managersMu.Lock()
	m := managers["2026miket"]
	managersMu.Unlock()
	// Queued at once: the scan alone would hold it as new for stableDelay.
	if e := m.store.snapshot().Videos[name]; e == nil || e.Status != statusStable || e.Meta == nil || e.Meta.TBAMatchKey != "qm5" {
		t.Fatalf("entry: %+v", e)
	}
	q := ev.next("queue")
	counts, _ := q["counts"].(map[string]any)
	if q["eventKey"] != "2026miket" || counts["stable"] != float64(1) {
		t.Fatalf("queue: %v", q)
	}

	// Outside the watched folder, not a match, or missing: ok:false.
	for _, p := range []string{
		filepath.Join(t.TempDir(), name),
		filepath.Join(dir, "notes.mp4"),
		filepath.Join(dir, "QM6_MIKET.mp4"),
	} {
		if _, out := postJSON(t, srv.URL+"/api/control/video", map[string]any{"path": p}); out["ok"] != false || out["error"] == nil {
			t.Fatalf("%s: %v", p, out)
		}
	}
}

func TestUploadEventShape(t *testing.T) {
	isolate(t)
	ch := hub.subscribe()
	defer hub.unsubscribe(ch)
	emitUpload("2026miket", "QM5_MIKET.mp4", &videoEntry{
		Status: statusUploaded, YTVideoID: "Zhg2jLCSBAY", Meta: &videoMeta{TBAMatchKey: "qm5"},
	})
	var got map[string]any
	_ = json.Unmarshal(<-ch, &got)
	want := map[string]any{"type": "upload", "file": "QM5_MIKET.mp4", "eventKey": "2026miket", "match": "qm5",
		"status": "done", "url": "https://www.youtube.com/watch?v=Zhg2jLCSBAY", "error": nil}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v (%v)", k, got[k], v, got)
		}
	}
	if !isQuotaError(errString("Daily upload limit reached")) || isQuotaError(errString("could not extract video id")) {
		t.Fatal("quota classifier")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
