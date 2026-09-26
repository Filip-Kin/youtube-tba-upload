package ytstudio

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestProfileLiveAndLabel(t *testing.T) {
	tool := Profile{Name: "tornado-tumble"}
	if tool.Live() {
		t.Error("a named profile is not a live one")
	}
	if tool.Label() != "tornado-tumble" {
		t.Errorf("label = %q", tool.Label())
	}
	live := Profile{UserDataDir: `C:\Users\filip\AppData\Local\Google\Chrome\User Data`}
	if !live.Live() {
		t.Error("a user-data dir means a live profile")
	}
	if live.Label() != "live:Default" {
		t.Errorf("label = %q, want live:Default", live.Label())
	}
}

// The browser that owns a profile has to be the one driving it: Chrome can't
// open Brave's profile and vice versa.
func TestBrowserFamily(t *testing.T) {
	cases := map[string]string{
		`C:\Users\filip\AppData\Local\Google\Chrome\User Data`:               "chrome",
		`C:\Users\filip\AppData\Local\BraveSoftware\Brave-Browser\User Data`: "brave",
		`C:\Users\filip\AppData\Local\Microsoft\Edge\User Data`:              "edge",
		"/home/filip/.config/google-chrome":                                  "chrome",
		"/home/filip/.config/BraveSoftware/Brave-Browser":                    "brave",
		"":                  "",
		"/tmp/some/profile": "",
	}
	for dir, want := range cases {
		if got := browserFamily(dir); got != want {
			t.Errorf("browserFamily(%q) = %q, want %q", dir, got, want)
		}
	}
}

// An explicit browser_exe always wins, so a live profile can be pinned to a
// specific binary.
func TestFindBrowserHonoursExplicitExe(t *testing.T) {
	d := &ChromedpDriver{BrowserExe: "/opt/weird/chrome"}
	got, err := d.findBrowser(`C:\Google\Chrome\User Data`)
	if err != nil || got != "/opt/weird/chrome" {
		t.Errorf("findBrowser = %q, %v", got, err)
	}
}

func TestDebugPortProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Browser":"Chrome/131.0.0.0","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/abc"}`))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	if ws := debugPortWS(port); ws != "ws://127.0.0.1:1/devtools/browser/abc" {
		t.Errorf("debugPortWS = %q", ws)
	}
	if !DebugPortActive(port) {
		t.Error("DebugPortActive = false for a listening browser")
	}
	// Nothing listening: the probe must fail fast rather than hang.
	srv.Close()
	if DebugPortActive(port) {
		t.Error("DebugPortActive = true after the browser went away")
	}
}

// A live-profile failure is almost always "the profile is already open in a
// window with no debug port", so say that instead of leaking a chromedp error.
func TestWrapLiveErr(t *testing.T) {
	inner := errors.New("websocket url timeout reached")

	if got := wrapLiveErr(Profile{Name: "tool-owned"}, 0, inner); got != inner {
		t.Errorf("tool-owned profile error was rewritten: %v", got)
	}
	if got := wrapLiveErr(Profile{UserDataDir: "/profile"}, 0, nil); got != nil {
		t.Errorf("nil error became %v", got)
	}

	got := wrapLiveErr(Profile{UserDataDir: "/profile"}, 0, inner)
	if !errors.Is(got, inner) {
		t.Error("wrapped error lost the cause")
	}
	msg := got.Error()
	for _, want := range []string{"/profile", "--remote-debugging-port=9222", "close it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}
