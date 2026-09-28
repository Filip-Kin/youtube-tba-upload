package ytstudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// ChromedpDriver is the production Driver. Construct with NewChromedpDriver.
type ChromedpDriver struct {
	ProfileRoot string // directory containing one subdir per profile_name
	BrowserExe  string // optional explicit path; empty => managed browser, then autodetect
	// Managed is the browser the driver downloads and owns. Used for
	// tool-owned profiles unless BrowserExe says otherwise.
	Managed *ManagedBrowser
	Verbose bool

	// opMu serializes whole browser operations (from opening a tab to closing
	// it). Chrome locks a profile's user-data-dir, so an upload and a sign-in
	// racing on the same profile leaves half-dead Chromes holding the lock
	// ("Opening in existing browser session") and the profile jammed. One
	// operation at a time keeps the profile clean. tab() acquires it; the close
	// func the caller defers releases it.
	opMu sync.Mutex

	// One browser stays open for the whole session and each operation gets a
	// tab in it. Starting Chrome, loading a profile and getting Studio warm
	// again costs more than every other step of an upload put together, and at
	// an event that happens once per match.
	mu            sync.Mutex
	sessionKey    string
	allocCancel   context.CancelFunc
	browserCtx    context.Context
	browserCancel context.CancelFunc
}

// NewChromedpDriver returns a driver that stores profiles under profileRoot and
// keeps its own browser under browserRoot. If browserExe is set it overrides
// both the managed browser and autodetection.
func NewChromedpDriver(profileRoot, browserRoot, browserExe string) *ChromedpDriver {
	d := &ChromedpDriver{ProfileRoot: profileRoot, BrowserExe: browserExe}
	if browserRoot != "" {
		// Download progress is always logged, Verbose or not: a first run is
		// a couple of hundred megabytes and silence looks like a hang.
		d.Managed = &ManagedBrowser{Root: browserRoot, Logf: func(format string, args ...any) {
			log.Printf("ytstudio: "+format, args...)
		}}
	}
	return d
}

func (d *ChromedpDriver) logf(format string, args ...any) {
	if d.Verbose {
		log.Printf("ytstudio: "+format, args...)
	}
}

// browserFor resolves the binary to drive for a profile: an explicit path
// first, then the browser we manage ourselves, then whatever is installed.
//
// The managed browser is the default because it cannot collide with the
// operator's everyday browser. Chrome refuses to open a profile directory that
// another process holds, so driving an installed browser only works while the
// operator is not using it.
func (d *ChromedpDriver) browserFor(ctx context.Context, p Profile) (string, error) {
	if p.Exe != "" {
		return p.Exe, nil
	}
	if d.BrowserExe != "" {
		return d.BrowserExe, nil
	}
	// A live profile belongs to an installed browser and has to be driven by
	// that same browser, not by our own copy.
	if !p.Live() && d.Managed != nil {
		exe, err := d.Managed.Ensure(ctx)
		if err == nil {
			return exe, nil
		}
		// No download (offline, or an unsupported platform): fall back to an
		// installed browser rather than refusing to upload.
		d.logf("managed browser unavailable (%v), falling back to an installed browser", err)
	}
	return d.findBrowser(p.UserDataDir)
}

// browserFamily guesses which browser owns a user-data directory from its
// path, so a live Chrome profile is driven by chrome.exe and not by whichever
// Chromium happens to be installed first.
func browserFamily(userDataDir string) string {
	p := strings.ToLower(filepath.ToSlash(userDataDir))
	switch {
	case strings.Contains(p, "bravesoftware"), strings.Contains(p, "brave"):
		return "brave"
	case strings.Contains(p, "microsoft/edge"), strings.Contains(p, "edge"):
		return "edge"
	case strings.Contains(p, "google/chrome"), strings.Contains(p, "chrome"):
		return "chrome"
	}
	return ""
}

// findBrowser returns an absolute path to a Brave/Edge/Chrome binary. When
// userDataDir is a live profile, the browser that owns it is tried first.
func (d *ChromedpDriver) findBrowser(userDataDir string) (string, error) {
	if d.BrowserExe != "" {
		return d.BrowserExe, nil
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LOCALAPPDATA")
		candidates = []string{
			filepath.Join(pf, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			filepath.Join(pf86, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			filepath.Join(local, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
			filepath.Join(pf, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(pf86, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(pf86, "Google", "Chrome", "Application", "chrome.exe"),
		}
	case "darwin":
		candidates = []string{
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
	default:
		candidates = []string{
			"/usr/bin/brave-browser",
			"/usr/bin/brave",
			"/opt/brave.com/brave/brave",
			"/snap/bin/brave",
			"/usr/bin/microsoft-edge",
			"/usr/bin/google-chrome",
			"/usr/bin/chromium",
		}
	}
	if family := browserFamily(userDataDir); family != "" {
		var preferred, rest []string
		for _, c := range candidates {
			if strings.Contains(strings.ToLower(c), family) {
				preferred = append(preferred, c)
			} else {
				rest = append(rest, c)
			}
		}
		candidates = append(preferred, rest...)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("ytstudio: no Brave/Edge/Chrome binary found (set browser_exe in config)")
}

// DefaultDebugPort is the CDP port used for live profiles when none is set.
const DefaultDebugPort = 9222

// LiveProfileDirs returns the installed browsers' user-data directories that
// exist on this machine, most likely first. Used to prefill the config.
func LiveProfileDirs() []string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		candidates = []string{
			filepath.Join(local, "Google", "Chrome", "User Data"),
			filepath.Join(local, "BraveSoftware", "Brave-Browser", "User Data"),
			filepath.Join(local, "Microsoft", "Edge", "User Data"),
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		candidates = []string{
			filepath.Join(home, "Library", "Application Support", "Google", "Chrome"),
			filepath.Join(home, "Library", "Application Support", "BraveSoftware", "Brave-Browser"),
			filepath.Join(home, "Library", "Application Support", "Microsoft Edge"),
		}
	default:
		home, _ := os.UserHomeDir()
		candidates = []string{
			filepath.Join(home, ".config", "google-chrome"),
			filepath.Join(home, ".config", "BraveSoftware", "Brave-Browser"),
			filepath.Join(home, ".config", "microsoft-edge"),
		}
	}
	out := []string{}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

// debugPortWS asks a running browser for its DevTools websocket URL. Empty
// string means nothing is listening on that port.
func debugPortWS(port int) string {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return ""
	}
	return body.WebSocketDebuggerURL
}

// DebugPortActive reports whether a browser is already listening for CDP on
// the given port (0 => DefaultDebugPort).
func DebugPortActive(port int) bool {
	if port == 0 {
		port = DefaultDebugPort
	}
	return debugPortWS(port) != ""
}

// isNoDisplayErr recognises a browser refusing to start because there is no
// desktop to draw on.
func isNoDisplayErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"missing x server", "$display", "ozone", "failed to initialize"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// profileKey identifies the browser a profile needs, so a session is only
// reused for the same one.
func profileKey(p Profile) string {
	if p.Live() {
		return "live:" + p.UserDataDir + ":" + p.Directory
	}
	// Headedness is part of the key: Chrome won't share one profile between a
	// headless and a headed instance, so a change has to restart the session.
	mode := "headed"
	if p.Headless {
		mode = "headless"
	}
	return "profile:" + p.Name + ":" + mode
}

// tab returns a context for one operation, in a browser that stays open after
// the operation finishes. The returned cancel closes the tab, not the browser.
//
// The browser is started on first use and reused until it dies or a different
// profile is asked for. It deliberately does not hang off the caller's context,
// which ends with the upload.
func (d *ChromedpDriver) tab(ctx context.Context, p Profile) (_ context.Context, _ context.CancelFunc, err error) {
	// Serialize the whole operation on the profile. Held until the caller runs
	// the close func this returns; released here if we fail before returning it.
	d.opMu.Lock()
	release := d.opMu.Unlock
	defer func() {
		if release != nil {
			release()
		}
	}()

	d.mu.Lock()
	key := profileKey(p)
	if d.browserCtx != nil && (d.sessionKey != key || d.browserCtx.Err() != nil) {
		// Wrong profile, or the operator closed the window: start over.
		d.logf("browser session ending (%s)", d.sessionKey)
		d.closeSessionLocked()
	}
	if d.browserCtx == nil {
		// Resolve the binary under the caller's context, since this is what
		// downloads the managed browser on a first run.
		exe, err := d.browserFor(ctx, p)
		if err != nil {
			d.mu.Unlock()
			return nil, nil, err
		}
		// The browser itself must outlive this request: allocating it from the
		// caller's context kills it the moment the upload that started it
		// finishes, which defeats the whole point of keeping it open.
		session := p
		session.Exe = exe

		// Open order. Default (headed) profiles open headed first and fall back
		// to headless on a machine with no display. A headless-preference profile
		// (uploads, channel checks) opens headless first — the UA spoof in
		// allocate keeps YT Studio from bouncing it — and only falls back to a
		// headed window if headless will not start at all.
		order := []bool{false, true}
		if session.Headless {
			order = []bool{true, false}
		}
		for i, headless := range order {
			allocCtx, cancelAlloc, err := d.allocate(context.Background(), session, headless)
			if err != nil {
				d.mu.Unlock()
				return nil, nil, err
			}
			browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
			// Start the browser now, so a failure is reported here rather than
			// halfway through the first upload.
			err = chromedp.Run(browserCtx)
			if err == nil {
				d.sessionKey = key
				d.allocCancel = cancelAlloc
				d.browserCtx = browserCtx
				d.browserCancel = cancelBrowser
				d.logf("browser session open (%s, headless=%v)", key, headless)
				break
			}
			cancelBrowser()
			cancelAlloc()
			// Fall back to the next mode if there is one: headed→headless when
			// the machine has no display, headless→headed when headless will not
			// start. Otherwise surface the error.
			if i+1 < len(order) && (headless || isNoDisplayErr(err)) {
				d.logf("browser did not start (headless=%v: %v); trying %v", headless, err, order[i+1])
				continue
			}
			d.mu.Unlock()
			return nil, nil, wrapLiveErr(p, p.DebugPort, err)
		}
	}
	browserCtx := d.browserCtx
	d.mu.Unlock()

	tabCtx, cancelTab := chromedp.NewContext(browserCtx)
	if err := chromedp.Run(tabCtx); err != nil {
		cancelTab()
		// The browser went away between the check above and here.
		d.mu.Lock()
		d.closeSessionLocked()
		d.mu.Unlock()
		return nil, nil, fmt.Errorf("open tab: %w", err)
	}
	// A cancelled caller closes the tab, without taking the browser with it.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cancelTab()
		case <-done:
		case <-tabCtx.Done():
		}
	}()
	release = nil // ownership of opMu passes to the close func
	return tabCtx, func() {
		close(done)
		cancelTab()
		d.opMu.Unlock()
	}, nil
}

// closeSessionLocked tears down the open browser. Caller holds d.mu.
func (d *ChromedpDriver) closeSessionLocked() {
	if d.browserCancel != nil {
		d.browserCancel()
	}
	if d.allocCancel != nil {
		d.allocCancel()
	}
	d.browserCtx = nil
	d.browserCancel = nil
	d.allocCancel = nil
	d.sessionKey = ""
}

// Close shuts the browser down. Called when the helper exits; without it the
// browser outlives the process on Windows.
func (d *ChromedpDriver) Close() {
	// Wait for any in-flight operation so we don't cancel a browser mid-upload
	// or mid-sign-in. Same lock order as tab(): opMu before mu.
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.browserCtx != nil {
		d.logf("closing browser session (%s)", d.sessionKey)
	}
	d.closeSessionLocked()
}

// allocate returns a chromedp allocator bound to the given profile. Callers
// must defer the returned cancel func.
func (d *ChromedpDriver) allocate(ctx context.Context, p Profile, headless bool) (context.Context, context.CancelFunc, error) {
	if p.Live() {
		return d.allocateLive(ctx, p)
	}
	if p.Name == "" {
		return nil, nil, errors.New("ytstudio: profile_name is empty")
	}
	profileDir := filepath.Join(d.ProfileRoot, p.Name)
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, nil, err
	}
	// Remove the SingletonLock left behind by an interrupted previous run.
	// chromedp's "cannot start, profile in use" failures all come back to
	// this file. Only ever done for a profile we own; a live profile's lock
	// belongs to the operator's running browser.
	_ = os.Remove(filepath.Join(profileDir, "SingletonLock"))

	exe, err := d.browserFor(ctx, p)
	if err != nil {
		return nil, nil, err
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(exe),
		chromedp.UserDataDir(profileDir),
		chromedp.NoSandbox,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		// Kills the navigator.webdriver tell at the Blink level; the
		// applyStealth init script handles anything that flag misses.
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-restore-last-session", true),
		chromedp.Flag("restore-last-session", "false"),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1400, 900),
	}
	if headless {
		// Headless Chrome advertises "HeadlessChrome/…" and YT Studio bounces
		// us to an unsupported-browser page. A spoofed UA fixes that path,
		// but the spoof itself is a fingerprintable mismatch against the
		// browser's Client Hints — so we only apply it here, not in headed
		// runs where Brave's native UA passes the check on its own.
		opts = append(opts,
			chromedp.UserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
			chromedp.Headless,
		)
	} else {
		opts = append(opts, chromedp.Flag("headless", false))
	}
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	return allocCtx, cancel, nil
}

// allocateLive drives the operator's own installed browser profile, so there is
// no second YouTube sign-in to keep alive.
//
// Preferred path: the browser is already running with CDP open on the debug
// port, so we attach to it and never touch the window layout or the session.
// Start it once with
//
//	chrome.exe --remote-debugging-port=9222
//
// (the profile is whatever that instance already has open). Otherwise we start
// the browser ourselves on the configured profile with the port open, which
// only works when that profile is not already open in another window: Chrome
// hands a second launch off to the running instance and exits, and there is no
// CDP endpoint to talk to. That case is reported by wrapLiveErr.
//
// Live runs are always headed. Chrome refuses to share a profile between a
// headless and a headed instance, and a real profile's own user agent is what
// gets YT Studio to behave in the first place.
func (d *ChromedpDriver) allocateLive(ctx context.Context, p Profile) (context.Context, context.CancelFunc, error) {
	port := p.DebugPort
	if port == 0 {
		port = DefaultDebugPort
	}
	if ws := debugPortWS(port); ws != "" {
		d.logf("attaching to the running browser on port %d", port)
		allocCtx, cancel := chromedp.NewRemoteAllocator(ctx, ws)
		return allocCtx, cancel, nil
	}

	exe, err := d.browserFor(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	dir := p.Directory
	if dir == "" {
		dir = "Default"
	}
	d.logf("starting %s on the live profile %s (%s), CDP port %d", filepath.Base(exe), dir, p.UserDataDir, port)
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(exe),
		chromedp.UserDataDir(p.UserDataDir),
		chromedp.Flag("profile-directory", dir),
		chromedp.Flag("remote-debugging-port", strconv.Itoa(port)),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("no-restore-last-session", true),
		chromedp.Flag("restore-last-session", "false"),
		chromedp.Flag("headless", false),
		chromedp.WindowSize(1400, 900),
	}
	allocCtx, cancel := chromedp.NewExecAllocator(ctx, opts...)
	return allocCtx, cancel, nil
}

// wrapLiveErr turns a live-profile startup failure into something actionable.
// The usual cause is the profile already being open in a browser window that
// was started without a debug port.
func wrapLiveErr(p Profile, port int, err error) error {
	if err == nil || !p.Live() {
		return err
	}
	if port == 0 {
		port = DefaultDebugPort
	}
	if debugPortWS(port) != "" {
		return err
	}
	return fmt.Errorf(
		"%w (live profile %s: if the browser is already open on this profile, "+
			"either close it or restart it with --remote-debugging-port=%d so it can be attached to)",
		err, p.UserDataDir, port)
}

// detectSignIn checks whether the current page URL indicates a sign-in
// redirect. Any URL on accounts.google.com or anything containing "/signin"
// signals an expired session.
func detectSignIn(currentURL string) bool {
	u := strings.ToLower(currentURL)
	return strings.Contains(u, "accounts.google") || strings.Contains(u, "/signin")
}

// signedIn reports whether Studio has landed on a signed-in channel page
// (studio.youtube.com/channel/<id>) rather than a Google sign-in redirect.
func signedIn(currentURL string) bool {
	u := strings.ToLower(currentURL)
	return strings.Contains(u, "/channel/") && !detectSignIn(u)
}

// Login opens a single headed YouTube Studio window on the tool profile and
// waits for the operator to sign in. It returns the signed-in channel name and
// closes the window as soon as sign-in lands (Studio redirects to
// /channel/<id>), so the operator never has to close it by hand. If the operator
// closes the window first, or the deadline passes, it returns an empty name and
// no error.
//
// Unlike other operations it runs its OWN dedicated browser, not the warm shared
// session: allocating from the allocator navigates the browser's first tab, so
// there is one window with one tab (no leftover about:blank). It still holds
// opMu, so it can't race an upload or a channel check on the same profile dir,
// and closes any warm session first for the same reason.
func (d *ChromedpDriver) Login(ctx context.Context, p Profile) (string, error) {
	p.Headless = false // always headed; the operator has to see the Google login
	ctx, cancelTO := context.WithTimeout(ctx, DefaultLoginDeadline)
	defer cancelTO()

	d.opMu.Lock()
	defer d.opMu.Unlock()

	// Close any warm (headless) session so this headed window is the only Chrome
	// on the profile dir; two instances on one dir jam each other.
	d.mu.Lock()
	d.closeSessionLocked()
	d.mu.Unlock()

	// Open the headed window, retrying a few times. Cancelling the warm session
	// above kills its Chrome asynchronously, and if the new headed launch beats
	// that exit, Chrome forwards it to the dying instance ("Opening in existing
	// browser session") and no visible window appears. A short wait + retry lets
	// the old process release the profile dir first.
	var (
		browserCtx    context.Context
		cancelAlloc   context.CancelFunc
		cancelBrowser context.CancelFunc
	)
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		ac, ca, err := d.allocate(context.Background(), p, false)
		if err != nil {
			return "", err
		}
		bc, cb := chromedp.NewContext(ac)
		if err := chromedp.Run(bc,
			applyStealth(),
			chromedp.Navigate("https://studio.youtube.com"),
		); err != nil {
			cb()
			ca()
			lastErr = err
			d.logf("login: window did not open (attempt %d: %v); retrying", attempt, err)
			time.Sleep(800 * time.Millisecond)
			continue
		}
		browserCtx, cancelAlloc, cancelBrowser = bc, ca, cb
		break
	}
	if browserCtx == nil {
		return "", wrapLiveErr(p, p.DebugPort, lastErr)
	}
	defer cancelAlloc() // closing the allocator shuts the window when we return
	defer cancelBrowser()
	d.logf("login: window open, waiting for sign-in")

	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-browserCtx.Done():
			return "", nil // operator closed the window
		case <-ctx.Done():
			return "", nil // gave up waiting; not an error
		case <-ticker.C:
			var currentURL, channelName string
			if err := chromedp.Run(browserCtx,
				chromedp.Location(&currentURL),
				chromedp.Evaluate(jsReadChannelName, &channelName),
			); err != nil {
				continue // mid-navigation, or the window is closing
			}
			if signedIn(currentURL) && strings.TrimSpace(channelName) != "" {
				d.logf("login: signed in as %q; closing window", channelName)
				return strings.TrimSpace(channelName), nil
			}
		}
	}
}

// OpenChannel opens (or reuses) a headed window on the tool profile, points it
// at YouTube Studio, and leaves it open for the operator — handy when a stream
// runs off the same machine and should share this one YouTube login. It shares
// the tool profile, so a later headless upload will close this window to reclaim
// the profile: Chrome allows only one instance per user-data-dir.
//
// It navigates the browser's primary tab (not a child tab), so there is one
// window with one tab and no leftover about:blank, and it does not tear the
// session down on return, so the window stays up.
func (d *ChromedpDriver) OpenChannel(ctx context.Context, p Profile) error {
	p.Headless = false
	d.opMu.Lock()
	defer d.opMu.Unlock()

	d.mu.Lock()
	key := profileKey(p)
	if d.browserCtx == nil || d.sessionKey != key || d.browserCtx.Err() != nil {
		// No usable headed session: drop any other session and open a fresh one.
		d.closeSessionLocked()
		session := p
		exe, err := d.browserFor(ctx, session)
		if err != nil {
			d.mu.Unlock()
			return err
		}
		session.Exe = exe
		allocCtx, cancelAlloc, err := d.allocate(context.Background(), session, false)
		if err != nil {
			d.mu.Unlock()
			return err
		}
		browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
		if err := chromedp.Run(browserCtx); err != nil {
			cancelBrowser()
			cancelAlloc()
			d.mu.Unlock()
			return wrapLiveErr(p, p.DebugPort, err)
		}
		d.sessionKey = key
		d.allocCancel = cancelAlloc
		d.browserCtx = browserCtx
		d.browserCancel = cancelBrowser
		d.logf("open-channel: headed window open (%s)", key)
	}
	browserCtx := d.browserCtx
	d.mu.Unlock()

	return chromedp.Run(browserCtx,
		applyStealth(),
		chromedp.Navigate("https://studio.youtube.com"),
	)
}

// CheckChannel opens YT Studio headlessly and reads the channel name. Returns
// ErrSessionExpired when the profile no longer has a session.
func (d *ChromedpDriver) CheckChannel(ctx context.Context, p Profile) (string, error) {
	// A channel check just reads the signed-in channel name; run it hidden so it
	// never pops a window (used on boot and from the settings panel).
	p.Headless = true
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	bctx, closeTab, err := d.tab(ctx, p)
	if err != nil {
		return "", err
	}
	defer closeTab()

	var currentURL, channelName string
	err = chromedp.Run(bctx,
		applyStealth(),
		chromedp.Navigate("https://studio.youtube.com"),
		chromedp.Sleep(2*time.Second),
		chromedp.Location(&currentURL),
		chromedp.Evaluate(jsReadChannelName, &channelName),
	)
	if err != nil {
		return "", wrapLiveErr(p, p.DebugPort, err)
	}
	if detectSignIn(currentURL) {
		return "", ErrSessionExpired
	}
	channelName = strings.TrimSpace(channelName)
	return channelName, nil
}

// ListPlaylists scrapes the channel's playlists page and returns each playlist's
// id and title. This is the only place Studio exposes the playlist ID: the
// upload/edit dialog's playlist dropdown hides IDs entirely (see addToPlaylist),
// which is why name matching there is fragile. Backing the UI dropdown from this
// list, and resolving a stored ID back to its current title before an add, is
// what stops a typo or a rename from stranding videos outside the playlist.
func (d *ChromedpDriver) ListPlaylists(ctx context.Context, p Profile, channelID string) ([]Playlist, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	bctx, closeTab, err := d.tab(ctx, p)
	if err != nil {
		return nil, "", err
	}
	defer closeTab()

	// Studio playlist management is channel-scoped
	// (studio.youtube.com/channel/<id>/content/playlists); the bare /playlists URL
	// errors. A cached channel id lets us skip the studio.youtube.com redirect and
	// go straight to the playlists page; otherwise resolve it from the redirect.
	resolved := channelID
	if resolved == "" {
		var base string
		if err = chromedp.Run(bctx,
			applyStealth(),
			chromedp.Navigate("https://studio.youtube.com"),
			chromedp.Sleep(3*time.Second),
			chromedp.Location(&base),
		); err != nil {
			return nil, "", wrapLiveErr(p, p.DebugPort, err)
		}
		if detectSignIn(base) {
			return nil, "", ErrSessionExpired
		}
		if m := regexp.MustCompile(`/channel/([^/?#]+)`).FindStringSubmatch(base); m != nil {
			resolved = m[1]
		}
	}
	playlistsURL := "https://studio.youtube.com/playlists"
	if resolved != "" {
		playlistsURL = "https://studio.youtube.com/channel/" + resolved + "/content/playlists"
	}
	var currentURL, raw string
	if err = chromedp.Run(bctx,
		applyStealth(),
		chromedp.Navigate(playlistsURL),
		chromedp.Sleep(3*time.Second),
		chromedp.Location(&currentURL),
		chromedp.Evaluate(jsScrapePlaylists, &raw),
	); err != nil {
		return nil, resolved, wrapLiveErr(p, p.DebugPort, err)
	}
	if detectSignIn(currentURL) {
		return nil, resolved, ErrSessionExpired
	}
	if resolved == "" {
		if m := regexp.MustCompile(`/channel/([^/?#]+)`).FindStringSubmatch(currentURL); m != nil {
			resolved = m[1]
		}
	}
	var out []Playlist
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, resolved, fmt.Errorf("parse playlists %q: %w", raw, err)
		}
	}
	d.logf("playlists: found %d (channel %s)", len(out), resolved)
	return out, resolved, nil
}

// Backfill re-applies a description and/or playlist membership to an
// already-uploaded video, addressed by its 11-char ID. It opens the video's
// edit page once, sets the description, ticks the playlist, then commits with a
// single Save. Each step is non-fatal: the returned BackfillResult carries a
// per-step error string so the operator can see which videos need another pass.
func (d *ChromedpDriver) Backfill(ctx context.Context, p Profile, videoID string, in BackfillInput) (BackfillResult, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultUploadDeadline)
	defer cancel()

	bctx, closeTab, err := d.tab(ctx, p)
	if err != nil {
		return BackfillResult{}, err
	}
	defer closeTab()

	editURL := fmt.Sprintf("https://studio.youtube.com/video/%s/edit", videoID)
	var currentURL string
	if err := chromedp.Run(bctx,
		applyStealth(),
		chromedp.Navigate(editURL),
		chromedp.Sleep(2*time.Second),
		chromedp.Location(&currentURL),
	); err != nil {
		return BackfillResult{}, fmt.Errorf("open edit page: %w", wrapLiveErr(p, p.DebugPort, err))
	}
	if detectSignIn(currentURL) {
		return BackfillResult{}, ErrSessionExpired
	}
	if err := chromedp.Run(bctx,
		chromedp.WaitVisible("#title-textarea", chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		return BackfillResult{}, fmt.Errorf("wait for edit page: %w", err)
	}

	var res BackfillResult

	descAttempted := false
	if in.Description != "" {
		if err := chromedp.Run(bctx, setTextbox("#description-textarea #textbox", in.Description, d.logf)); err != nil {
			d.logf("backfill: description not set for %s: %v", videoID, err)
			res.DescriptionError = err.Error()
		} else {
			descAttempted = true
		}
	}

	// selectPlaylist opens the dropdown on the current edit page, ticks the row
	// and closes it with Done; it does not commit. savePlaylistEdit below does.
	playlistTicked := false
	if in.PlaylistName != "" {
		if err := d.selectPlaylist(bctx, in.PlaylistName); err != nil {
			d.logf("backfill: playlist for %s: %v", videoID, err)
			res.PlaylistError = err.Error()
		} else {
			playlistTicked = true
		}
	}

	// One Save commits both the description and the playlist change.
	if descAttempted || playlistTicked {
		if err := d.savePlaylistEdit(bctx); err != nil {
			d.logf("backfill: save for %s: %v", videoID, err)
			if descAttempted && res.DescriptionError == "" {
				res.DescriptionError = err.Error()
			}
			if playlistTicked && res.PlaylistError == "" {
				res.PlaylistError = err.Error()
			}
		}
	}

	return res, nil
}

// jsScrapePlaylists walks every shadow root on the Studio playlists page and
// collects {id, title} for each playlist link. IDs come from the anchor href
// (/playlist/<id> or ?list=<id>); the title is the anchor text, falling back to
// the first non-empty line of an ancestor row. Deduplicated by id.
const jsScrapePlaylists = `
	(() => {
		function walk(root, fn) {
			fn(root);
			const all = root.querySelectorAll('*');
			for (const el of all) {
				if (el.shadowRoot) walk(el.shadowRoot, fn);
			}
		}
		const out = [];
		const seen = new Set();
		// Each Studio playlist row has several /playlist/<id>/edit anchors (thumbnail,
		// title, hover-edit). The thumbnail one's only text is the video COUNT, so
		// target the title anchor specifically: a#playlist-title-link.
		walk(document, root => {
			root.querySelectorAll("a#playlist-title-link[href*='/playlist/']").forEach(a => {
				const href = a.getAttribute('href') || '';
				const m = href.match(/\/playlist\/([A-Za-z0-9_-]{6,})/);
				if (!m) return;
				const id = m[1];
				if (seen.has(id)) return;
				const title = (a.textContent || '').trim();
				if (!title) return;
				seen.add(id);
				out.push({id: id, title: title});
			});
		});
		return JSON.stringify(out);
	})()
`

// Upload performs the full upload: open Studio, click Create, attach the
// file via setInputFiles on the hidden <input type=file>, fill title and
// description, optionally set thumbnail, wait for "Checks complete", set
// visibility, extract the 11-char ID, Save, optionally add to playlist.
func (d *ChromedpDriver) Upload(ctx context.Context, p Profile, in UploadInput) (UploadResult, error) {
	ctx, cancelTO := context.WithTimeout(ctx, DefaultUploadDeadline)
	defer cancelTO()

	bctx, closeTab, err := d.tab(ctx, p)
	if err != nil {
		return UploadResult{}, err
	}
	defer closeTab()

	visibility := strings.ToUpper(strings.TrimSpace(in.Visibility))
	if visibility == "" {
		// Unlisted by default: publishing a match video is a decision the
		// caller has to make on purpose.
		visibility = "UNLISTED"
	}
	// Log what this run was actually asked to do, so "it never tried to add the
	// playlist" can be answered from the console instead of guessed at.
	d.logf("upload: title=%q visibility=%s playlist=%q", in.Title, visibility, in.PlaylistName)

	abs, err := filepath.Abs(in.VideoPath)
	if err != nil {
		return UploadResult{}, err
	}
	var thumbAbs string
	if in.ThumbnailPath != "" {
		thumbAbs, err = filepath.Abs(in.ThumbnailPath)
		if err != nil {
			return UploadResult{}, err
		}
	}

	var (
		currentURL  string
		channelName string
		videoID     string
	)

	// Step 1: navigate to YT Studio and check we're signed in.
	d.logf("step 1: navigate to studio")
	if err := chromedp.Run(bctx,
		applyStealth(),
		chromedp.Navigate("https://studio.youtube.com"),
		chromedp.Sleep(3*time.Second),
		chromedp.Location(&currentURL),
	); err != nil {
		return UploadResult{}, fmt.Errorf("open studio: %w", wrapLiveErr(p, p.DebugPort, err))
	}
	d.logf("step 1: at %s", currentURL)
	if detectSignIn(currentURL) {
		return UploadResult{}, ErrSessionExpired
	}

	// Read channel name; non-fatal if missing.
	_ = chromedp.Run(bctx, chromedp.Evaluate(jsReadChannelName, &channelName))
	channelName = strings.TrimSpace(channelName)
	d.logf("step 1: channel=%q", channelName)

	// Step 2: open the Create dropdown, click "Upload videos", and feed the
	// file in via the CDP Page.fileChooserOpened interception API. The
	// dropdown's menu items live inside ytcp-text-menu's shadow root, so we
	// click them with a shadow-piercing JS walker. The file input itself is
	// also inside shadow DOM — interception bypasses both problems by
	// setting files via the backend node ID delivered with the chooser event.
	d.logf("step 2: arming file-chooser interceptor")
	if err := attachFileViaChooser(bctx, abs, d.logf); err != nil {
		return UploadResult{}, fmt.Errorf("attach file: %w", err)
	}
	d.logf("file attached: %s", abs)

	// Step 3: details — title, description. A big file keeps Studio busy for a
	// while before it swaps the details dialog in.
	if err := chromedp.Run(bctx,
		chromedp.WaitVisible("#title-textarea", chromedp.ByQuery),
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		return UploadResult{}, fmt.Errorf("wait for details dialog: %w", err)
	}
	if err := chromedp.Run(bctx, setTextbox("#title-textarea #textbox", in.Title, d.logf)); err != nil {
		return UploadResult{}, fmt.Errorf("fill title: %w", err)
	}
	descriptionError := ""
	if err := chromedp.Run(bctx, setTextbox("#description-textarea #textbox", in.Description, d.logf)); err != nil {
		// A missing description is not worth abandoning a match video for, but a
		// swallowed failure is how a video quietly goes up with no description.
		// Record it so the operator can see it and backfill.
		d.logf("description not set: %v (continuing)", err)
		descriptionError = err.Error()
	}
	// Studio will not enable Next until the audience question is answered, and
	// a channel without a default has it blank. Match videos are not made for
	// kids.
	if err := chromedp.Run(bctx, answerAudience(d.logf)); err != nil {
		d.logf("audience: %v (continuing)", err)
	}

	// Step 4: thumbnail (optional). The thumbnail tile has its own hidden
	// <input type=file accept=image/*>; finding it by accept attribute keeps
	// us off the main video input.
	if thumbAbs != "" {
		d.logf("step 4: attaching thumbnail")
		if err := attachThumbnailViaChooser(bctx, thumbAbs, d.logf); err != nil {
			d.logf("thumbnail upload failed: %v (continuing)", err)
		} else {
			d.logf("step 4: thumbnail attached")
		}
	}

	// Step 4.5: set the playlist here, in the dialog, where it needs no video
	// ID. The dialog's own Save commits it along with everything else.
	playlistSet := false
	if in.PlaylistName != "" {
		if err := d.selectPlaylist(bctx, in.PlaylistName); err != nil {
			d.logf("playlist in dialog failed: %v (will retry after saving)", err)
		} else {
			playlistSet = true
			d.logf("playlist %q set in the upload dialog", in.PlaylistName)
		}
	}

	// Step 5: give the copyright checks a chance to finish, then carry on
	// regardless.
	//
	// This must never be fatal. YouTube words the banner differently over time,
	// it does not appear at all on a video Studio has already processed, and
	// nothing in the rest of the flow depends on it: a video can be published
	// while its checks are still running. Treating a missing banner as an error
	// is what strands an upload on the first screen of the dialog with only the
	// title filled in.
	checksCtx, cancelChecks := context.WithTimeout(bctx, DefaultChecksCompleteDeadline)
	defer cancelChecks()
	if err := chromedp.Run(checksCtx,
		waitForText(`checks complete|no issues found|no copyright issues`),
	); err != nil {
		d.logf("checks banner never appeared (%v), carrying on without it", err)
	} else {
		d.logf("checks complete")
	}

	// Step 6: walk to the Visibility step. The dialog uses test-id buttons.
	// An end-screen modal left open by a previous run swallows those clicks.
	_ = chromedp.Run(bctx, closeEndscreenModal(d.logf))
	if err := chromedp.Run(bctx,
		jsClick(`button[test-id='VIDEO_ELEMENTS']`),
		chromedp.Sleep(500*time.Millisecond),
		jsClick(`button[test-id='REVIEW']`),
		chromedp.Sleep(500*time.Millisecond),
		jsClick(`button[test-id='REVIEW']`), // some flows need it twice to advance
		chromedp.Sleep(500*time.Millisecond),
		chromedp.WaitVisible("tp-yt-paper-radio-button", chromedp.ByQuery),
		jsClick(fmt.Sprintf(`tp-yt-paper-radio-button[name='%s']`, visibility)),
		chromedp.Sleep(500*time.Millisecond),
	); err != nil {
		return UploadResult{}, fmt.Errorf("advance to visibility: %w", err)
	}

	// Step 7: capture the 11-char video ID from the dialog before saving.
	_ = chromedp.Run(bctx,
		chromedp.Evaluate(jsExtractVideoID, &videoID),
	)
	d.logf("captured video id (pre-save): %q", videoID)

	// Step 8: click Save.
	if err := chromedp.Run(bctx,
		jsClick(`ytcp-button[id='done-button'], button[aria-label='Save']:not([disabled])`),
		chromedp.Sleep(3*time.Second),
	); err != nil {
		return UploadResult{}, fmt.Errorf("save: %w", err)
	}

	// Fallback ID recovery if not captured pre-save.
	if videoID == "" {
		_ = chromedp.Run(bctx,
			chromedp.Sleep(2*time.Second),
			chromedp.Evaluate(jsExtractVideoIDFallback, &videoID),
		)
		d.logf("captured video id (fallback): %q", videoID)
	}

	if videoID == "" {
		return UploadResult{ChannelName: channelName}, errors.New("could not extract video id from YT Studio")
	}

	// Step 9: if the playlist could not be set in the dialog, try again on the
	// video's edit page now that there is an ID to open.
	playlistError := ""
	if in.PlaylistName != "" && !playlistSet {
		if err := d.addToPlaylist(bctx, videoID, in.PlaylistName); err != nil {
			d.logf("add-to-playlist failed: %v", err)
			// Non-fatal: the video is up, and the operator can fix the playlist
			// by hand. Reported so they know to.
			playlistError = err.Error()
		} else {
			d.logf("added to playlist %q", in.PlaylistName)
		}
	}

	return UploadResult{VideoID: videoID, ChannelName: channelName, PlaylistError: playlistError, DescriptionError: descriptionError}, nil
}

// addToPlaylist opens https://studio.youtube.com/video/<id>/edit, opens the
// playlist dropdown, ticks the checkbox whose label exactly matches
// playlistName, then commits.
//
// YouTube Studio's DOM doesn't store playlist IDs anywhere reachable from
// JS (neither as DOM attributes nor as Polymer/Lit properties on the
// checkbox rows). Matching by visible name is the only path that works.
// All clicks use shadow-pierce locate + native MouseClickXY because
// ytcp-button / ytcp-dropdown-trigger / ytcp-checkbox-lit all ignore
// synthetic el.click() (Polymer gesture system listens for the full
// pointerdown/up sequence).
func (d *ChromedpDriver) addToPlaylist(ctx context.Context, videoID, playlistName string) error {
	editURL := fmt.Sprintf("https://studio.youtube.com/video/%s/edit", videoID)
	if err := chromedp.Run(ctx,
		chromedp.Navigate(editURL),
		chromedp.WaitVisible("#title-textarea", chromedp.ByQuery),
		chromedp.Sleep(1*time.Second),
	); err != nil {
		return fmt.Errorf("open edit page: %w", err)
	}
	if err := d.selectPlaylist(ctx, playlistName); err != nil {
		return err
	}
	return d.savePlaylistEdit(ctx)
}

// selectPlaylist ticks a playlist in the playlist dropdown of whichever dialog
// is open, and closes the dropdown with Done. It does not commit anything: in
// the upload dialog the dialog's own Save does that, and on the edit page
// savePlaylistEdit does.
//
// Doing this inside the upload dialog is the primary path, because it needs no
// video ID: the ID is only available after saving, and when Studio hides it the
// video would otherwise be uploaded with no playlist at all.
func (d *ChromedpDriver) selectPlaylist(ctx context.Context, playlistName string) error {
	// Step a: locate and coord-click the playlist dropdown trigger.
	d.logf("playlist: locate dropdown trigger")
	var tx, ty float64
	var trigInfo string
	if err := chromedp.Run(ctx, shadowLocateBySelector(
		[]string{"ytcp-dropdown-trigger[use-placeholder]", "ytcp-dropdown-trigger"},
		&tx, &ty, &trigInfo,
	)); err != nil {
		return fmt.Errorf("locate dropdown trigger: %w", err)
	}
	if trigInfo == "" {
		return errors.New("playlist dropdown trigger not found")
	}
	d.logf("playlist: trigger at (%.0f,%.0f) %s", tx, ty, trigInfo)
	if err := chromedp.Run(ctx,
		humanSleep(150*time.Millisecond, 400*time.Millisecond),
		humanClick(tx, ty),
		humanSleep(1300*time.Millisecond, 1800*time.Millisecond),
	); err != nil {
		return fmt.Errorf("click dropdown trigger: %w", err)
	}
	before := probeDropdownState(ctx)
	d.logf("playlist: visible-checkbox count=%s", before)

	// Step a.5: type the playlist name into the dropdown's search input.
	// The dialog is virtualized — on channels with many playlists, most rows
	// render as DOM stubs with no text until scrolled into view, so a direct
	// name match can't find them. Filtering collapses the list to the row
	// we want before the matcher runs.
	if searchInfo, err := playlistFilterByName(ctx, playlistName); err != nil {
		d.logf("playlist: filter input failed: %v (proceeding without filter)", err)
	} else if searchInfo == "" {
		d.logf("playlist: no search input found, proceeding without filter")
	} else {
		d.logf("playlist: filtered via %s", searchInfo)
		if err := chromedp.Run(ctx, humanSleep(1200*time.Millisecond, 1800*time.Millisecond)); err != nil {
			return err
		}
		after := probeDropdownState(ctx)
		d.logf("playlist: post-filter count=%s", after)
		// A filter that hides everything is worse than no filter: whatever it
		// typed into, it was not the dropdown's own search.
		if visibleRowCount(after) == 0 && visibleRowCount(before) > 0 {
			d.logf("playlist: filter left no rows, clearing it and listing everything")
			if err := clearPlaylistFilter(ctx); err != nil {
				d.logf("playlist: could not clear the filter: %v", err)
			}
			if err := chromedp.Run(ctx, humanSleep(900*time.Millisecond, 1300*time.Millisecond)); err != nil {
				return err
			}
			d.logf("playlist: post-clear count=%s", probeDropdownState(ctx))
		}
	}

	// Step b: find the checkbox row whose label exactly matches playlistName
	// and coord-click it. We click the LABEL element wrapping the checkbox,
	// not the checkbox-lit itself — clicking the label is what the user does
	// and native HTML label-for-checkbox semantics make it the most reliable
	// way to toggle a Polymer ytcp-checkbox-lit.
	// Tick the row via the checkbox's own `checked` PROPERTY. Confirmed from the
	// live DOM: ytcp-checkbox-lit never sets aria-checked or a `checked`
	// attribute, so the old attribute-based probe always read "unchecked" and the
	// retry toggled the box straight back off. `el.checked` is the real state, so
	// we only ever click when it is false — a click can't toggle a ticked box off.
	ticked := false
	for attempt := 0; attempt < 4 && !ticked; attempt++ {
		found, checked, err := d.ensurePlaylistChecked(ctx, playlistName)
		d.logf("playlist: ensure attempt %d found=%v checked=%v err=%v", attempt, found, checked, err)
		if err != nil {
			return fmt.Errorf("ensure playlist checked: %w", err)
		}
		if !found {
			if err := chromedp.Run(ctx, humanSleep(500*time.Millisecond, 800*time.Millisecond)); err != nil {
				return err
			}
			continue
		}
		ticked = checked
		if !ticked {
			if err := chromedp.Run(ctx, humanSleep(500*time.Millisecond, 800*time.Millisecond)); err != nil {
				return err
			}
		}
	}
	if !ticked {
		return fmt.Errorf("playlist %q did not tick", playlistName)
	}

	// Step c: click Done to close the dropdown.
	d.logf("playlist: locate Done")
	var dx, dy float64
	var doneInfo string
	if err := chromedp.Run(ctx, shadowLocateByText(
		[]string{"ytcp-button[test-id='done-button']", "ytcp-button", "button"},
		regexp.MustCompile(`(?i)^done$`),
		&dx, &dy, &doneInfo,
	)); err != nil {
		return fmt.Errorf("locate done: %w", err)
	}
	if doneInfo == "" {
		return errors.New("Done button not found")
	}
	d.logf("playlist: Done at (%.0f,%.0f) %s", dx, dy, doneInfo)
	if err := chromedp.Run(ctx,
		humanSleep(200*time.Millisecond, 500*time.Millisecond),
		humanClick(dx, dy),
	); err != nil {
		return fmt.Errorf("click done: %w", err)
	}
	// The playlist dialog is a tp-yt-paper-dialog with an animated close.
	// If we click Save while it's still fading out, the click hits the
	// backdrop instead. Poll until the dialog is gone.
	if err := waitDialogHidden(ctx, 5*time.Second); err != nil {
		d.logf("playlist: dialog-hidden wait: %v", err)
	} else {
		d.logf("playlist: dialog closed")
	}

	// The collapsed trigger reads its placeholder ("Select") until a playlist is
	// chosen, then the name(s). Log it as the human-visible confirmation.
	var trigText string
	_ = chromedp.Run(ctx, chromedp.Evaluate(jsPlaylistTriggerText, &trigText))
	d.logf("playlist: trigger now reads %q (wanted %q)", trigText, playlistName)

	return nil
}

// ensurePlaylistChecked finds the playlist row by label and, only if its
// checkbox `checked` property is false, clicks it. Returns whether the row was
// found and its checked state AFTER. Idempotent: never clicks a checked row.
func (d *ChromedpDriver) ensurePlaylistChecked(ctx context.Context, name string) (found bool, checked bool, err error) {
	js := fmt.Sprintf(`(() => {
		%s
		const want = normalizeName(%q);
		const walk = (root, fn) => { fn(root); root.querySelectorAll('*').forEach(el => { if (el.shadowRoot) walk(el.shadowRoot, fn); }); };
		let box = null, best = 0;
		walk(document, r => r.querySelectorAll('ytcp-checkbox-lit').forEach(el => {
			if (el.offsetParent === null) return;
			const s = matchScore(rowLabel(el), want);
			if (s > best) { best = s; box = el; }
		}));
		if (!box) return JSON.stringify({found:false, checked:false});
		if (!box.checked) { box.click(); }
		return JSON.stringify({found:true, checked: !!box.checked});
	})()`, jsPlaylistMatch, name)
	var raw string
	if e := chromedp.Run(ctx, chromedp.Evaluate(js, &raw)); e != nil {
		return false, false, e
	}
	var out struct {
		Found   bool `json:"found"`
		Checked bool `json:"checked"`
	}
	if e := json.Unmarshal([]byte(raw), &out); e != nil {
		return false, false, fmt.Errorf("parse ensure %q: %w", raw, e)
	}
	return out.Found, out.Checked, nil
}

// jsPlaylistTriggerText reads the playlist dropdown trigger's visible text.
const jsPlaylistTriggerText = `(() => {
	const walk = (root, fn) => { fn(root); root.querySelectorAll('*').forEach(el => { if (el.shadowRoot) walk(el.shadowRoot, fn); }); };
	let t = null;
	walk(document, r => { if (!t) { const x = r.querySelector("ytcp-dropdown-trigger[use-placeholder], ytcp-dropdown-trigger"); if (x && x.offsetParent) t = x; } });
	return t ? (t.innerText || t.textContent || '').trim() : '';
})()`

// savePlaylistEdit commits a playlist change made on the video's edit page.
func (d *ChromedpDriver) savePlaylistEdit(ctx context.Context) error {
	// Step d: click Save on the edit dialog to commit the change.
	d.logf("playlist: locate Save")
	var saveX, saveY float64
	var saveInfo string
	if err := chromedp.Run(ctx, shadowLocateBySelector(
		[]string{
			"ytcp-button#save",
			"ytcp-button[id='save']",
			"ytcp-button[test-id='SAVE']",
			"button[aria-label='Save']:not([aria-disabled='true'])",
		},
		&saveX, &saveY, &saveInfo,
	)); err != nil {
		return fmt.Errorf("locate save: %w", err)
	}
	if saveInfo == "" {
		return errors.New("Save button not found")
	}
	d.logf("playlist: Save at (%.0f,%.0f) %s", saveX, saveY, saveInfo)
	d.logf("playlist: pre-save trigger-text=%q save-enabled=%s", probeTriggerText(ctx), probeSaveEnabled(ctx))
	if err := chromedp.Run(ctx,
		humanSleep(300*time.Millisecond, 700*time.Millisecond),
		humanClick(saveX, saveY),
		chromedp.Sleep(4*time.Second),
	); err != nil {
		return fmt.Errorf("click save: %w", err)
	}
	d.logf("playlist: post-save trigger-text=%q save-enabled=%s", probeTriggerText(ctx), probeSaveEnabled(ctx))
	return nil
}

// probeTriggerText returns the visible text of the playlist dropdown trigger.
// After Done commits a playlist toggle, the trigger should show the playlist
// name (e.g. "Test") rather than the placeholder "Select".
func probeTriggerText(ctx context.Context) string {
	const js = `(() => {
		function walk(root, fn) {
			fn(root);
			const all = root.querySelectorAll('*');
			for (const el of all) {
				if (el.shadowRoot) walk(el.shadowRoot, fn);
			}
		}
		let t = null;
		walk(document, root => {
			if (t) return;
			const cands = [
				...root.querySelectorAll('ytcp-dropdown-trigger[use-placeholder]'),
				...root.querySelectorAll('ytcp-dropdown-trigger'),
			];
			const it = cands.find(e => e.offsetParent !== null);
			if (it) t = it;
		});
		if (!t) return '';
		return (t.innerText || t.textContent || '').trim().slice(0, 80);
	})()`
	var out string
	_ = chromedp.Run(ctx, chromedp.Evaluate(js, &out))
	return out
}

// waitDialogHidden polls until any ytcp-playlist-dialog is no longer visible
// (or its inner tp-yt-paper-dialog is closed). Returns nil on success, or an
// error on timeout.
func waitDialogHidden(ctx context.Context, timeout time.Duration) error {
	const js = `(() => {
		function walk(root, fn) {
			fn(root);
			const all = root.querySelectorAll('*');
			for (const el of all) {
				if (el.shadowRoot) walk(el.shadowRoot, fn);
			}
		}
		let visible = false;
		walk(document, root => {
			if (visible) return;
			root.querySelectorAll('ytcp-playlist-dialog tp-yt-paper-dialog, ytcp-playlist-dialog').forEach(el => {
				if (el.offsetParent !== null) visible = true;
			});
		});
		return visible;
	})()`
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var visible bool
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, &visible)); err != nil {
			return err
		}
		if !visible {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return errors.New("playlist dialog still visible")
}

// visibleRowCount reads the leading number out of probeDropdownState's
// "<n> visible, <m> checked".
func visibleRowCount(state string) int {
	n := 0
	if _, err := fmt.Sscanf(state, "%d visible", &n); err != nil {
		return -1
	}
	return n
}

// clearPlaylistFilter empties the dropdown's search box, so the full list comes
// back when a filter turned out to be the wrong move.
func clearPlaylistFilter(ctx context.Context) error {
	const js = `
		(() => {
			function walk(root, fn) {
				fn(root);
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot, fn);
				}
			}
			let cleared = false;
			walk(document, root => {
				root.querySelectorAll('input').forEach(el => {
					if (cleared || el.offsetParent === null || !el.value) return;
					const label = ((el.placeholder || '') + ' ' +
						(el.getAttribute('aria-label') || '')).toLowerCase();
					if (label.includes('across your channel')) return;
					el.focus();
					el.value = '';
					el.dispatchEvent(new Event('input', {bubbles: true}));
					el.dispatchEvent(new Event('change', {bubbles: true}));
					cleared = true;
				});
			});
			return cleared;
		})()
	`
	var cleared bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &cleared)); err != nil {
		return err
	}
	if !cleared {
		return errors.New("no filter input to clear")
	}
	return nil
}

// probeDropdownState returns how many ytcp-checkbox-lit elements are visible
// anywhere on the page (piercing shadow). Used to confirm the playlist
// dropdown actually opened after the trigger click.
func probeDropdownState(ctx context.Context) string {
	const js = `(() => {
		function walk(root, fn) {
			fn(root);
			const all = root.querySelectorAll('*');
			for (const el of all) {
				if (el.shadowRoot) walk(el.shadowRoot, fn);
			}
		}
		let n = 0, checked = 0;
		walk(document, root => {
			root.querySelectorAll('ytcp-checkbox-lit').forEach(el => {
				if (el.offsetParent === null) return;
				n++;
				if (el.getAttribute('aria-checked') === 'true' || el.hasAttribute('checked')) checked++;
			});
		});
		return n + ' visible, ' + checked + ' checked';
	})()`
	var out string
	_ = chromedp.Run(ctx, chromedp.Evaluate(js, &out))
	return out
}

// probeRowChecked returns whether the checkbox row labeled with `name` is
// checked. Helps diagnose whether the row click actually toggled the
// underlying ytcp-checkbox-lit.
func probeRowChecked(ctx context.Context, name string) string {
	js := fmt.Sprintf(`(() => {
		%s
		const want = normalizeName(%q);
		function walk(root, fn) {
			fn(root);
			const all = root.querySelectorAll('*');
			for (const el of all) {
				if (el.shadowRoot) walk(el.shadowRoot, fn);
			}
		}
		let found = null;
		let best = 0;
		walk(document, root => {
			root.querySelectorAll('ytcp-checkbox-lit').forEach(el => {
				if (el.offsetParent === null) return;
				const score = matchScore(rowLabel(el), want);
				if (score > best) { best = score; found = el; }
			});
		});
		if (!found) return 'not-found';
		const ariaChecked = found.getAttribute('aria-checked');
		const hasChecked = found.hasAttribute('checked');
		const innerChecked = found.shadowRoot ? !!found.shadowRoot.querySelector('[checked]') : null;
		return 'aria=' + ariaChecked + ' attr=' + hasChecked + ' inner=' + innerChecked;
	})()`, jsPlaylistMatch, name)
	var out string
	_ = chromedp.Run(ctx, chromedp.Evaluate(js, &out))
	return out
}

// probeSaveEnabled inspects the Save button's enabled/disabled state.
func probeSaveEnabled(ctx context.Context) string {
	const js = `(() => {
		function walk(root, fn) {
			fn(root);
			const all = root.querySelectorAll('*');
			for (const el of all) {
				if (el.shadowRoot) walk(el.shadowRoot, fn);
			}
		}
		let btn = null;
		walk(document, root => {
			if (btn) return;
			const cands = [
				...root.querySelectorAll('ytcp-button#save'),
				...root.querySelectorAll('ytcp-button[id=save]'),
			];
			const it = cands.find(e => e.offsetParent !== null);
			if (it) btn = it;
		});
		if (!btn) return 'no-save-button';
		return 'aria-disabled=' + btn.getAttribute('aria-disabled') +
			' disabled-attr=' + btn.hasAttribute('disabled');
	})()`
	var out string
	_ = chromedp.Run(ctx, chromedp.Evaluate(js, &out))
	return out
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// applyStealth registers a Page init script that runs before any page script
// on every navigation. It patches the most-fingerprinted automation tells:
// navigator.webdriver (true under CDP), the missing window.chrome object,
// the empty navigator.plugins array, and a singleton languages list. Paired
// with --disable-blink-features=AutomationControlled, this covers the trio
// of checks every "is this a headless browser?" snippet runs first.
func applyStealth() chromedp.Action {
	const script = `
		try {
			Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
		} catch (e) {}
		if (!window.chrome) { window.chrome = { runtime: {} }; }
		try {
			Object.defineProperty(navigator, 'plugins', {
				get: () => [
					{ name: 'Chrome PDF Plugin', filename: 'internal-pdf-viewer' },
					{ name: 'Chrome PDF Viewer', filename: 'mhjfbmdgcfjbbpaeojofohoefgiehjai' },
					{ name: 'Native Client', filename: 'internal-nacl-plugin' },
				],
			});
		} catch (e) {}
		try {
			Object.defineProperty(navigator, 'languages', {
				get: () => ['en-US', 'en'],
			});
		} catch (e) {}
	`
	return chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(script).Do(ctx)
		return err
	})
}

// humanSleep blocks for a uniformly-random duration in [min, max). Used at
// action boundaries to break up the dead-on-fixed-interval pattern that a
// timing fingerprinter would otherwise see.
func humanSleep(min, max time.Duration) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		d := min
		if max > min {
			d += time.Duration(rand.Int64N(int64(max - min)))
		}
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// humanClick is a drop-in replacement for chromedp.MouseClickXY that first
// dispatches a short series of mouseMoved events from a small random offset,
// then issues a separate pressed/released pair with a human-ish dwell
// between them. chromedp.MouseClickXY sends pressed+released back-to-back at
// the exact target with no prior cursor motion — a near-zero-cost tell for
// any behaviour analysis. The motion + dwell here costs ~200–500 ms per
// click but removes one of the most obvious automation signals.
func humanClick(x, y float64) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		startX := x + rand.Float64()*120 - 60
		startY := y + rand.Float64()*120 - 60
		steps := 3 + rand.IntN(3)
		for i := 1; i <= steps; i++ {
			t := float64(i) / float64(steps)
			mx := startX + (x-startX)*t
			my := startY + (y-startY)*t
			if err := input.DispatchMouseEvent(input.MouseMoved, mx, my).Do(ctx); err != nil {
				return err
			}
			select {
			case <-time.After(time.Duration(15+rand.IntN(35)) * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := input.DispatchMouseEvent(input.MousePressed, x, y).
			WithButton(input.Left).WithClickCount(1).Do(ctx); err != nil {
			return err
		}
		select {
		case <-time.After(time.Duration(50+rand.IntN(80)) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		return input.DispatchMouseEvent(input.MouseReleased, x, y).
			WithButton(input.Left).WithClickCount(1).Do(ctx)
	})
}

// clickByText finds the first element matching tagSelector whose textContent
// matches re, and clicks it. Implemented via Evaluate because chromedp's
// built-in selectors don't support text regex.
//
// The Go regex is converted to a JS RegExp; we strip the `(?i)` inline-flag
// prefix (JS doesn't support it) and always pass the 'i' flag to RegExp.
func clickByText(tagSelector string, re *regexp.Regexp) chromedp.Action {
	var unused string
	return clickByTextResult(tagSelector, re, &unused)
}

// clickByTextResult is the same as clickByText, but writes a short description
// of the matched element (or "" if none) into *info so the caller can log it.
func clickByTextResult(tagSelector string, re *regexp.Regexp, info *string) chromedp.Action {
	pat := strings.TrimPrefix(re.String(), "(?i)")
	js := fmt.Sprintf(`
		(() => {
			const re = new RegExp(%q, 'i');
			const els = [...document.querySelectorAll(%q)];
			// Prefer an exact text match before falling back to a partial match.
			const visible = els.filter(e => e.offsetParent !== null && re.test((e.textContent || '').trim()));
			const exact = visible.find(e => re.test((e.textContent || '').trim().split('\n')[0]));
			const el = exact || visible[0];
			if (!el) return '';
			const id = el.id ? '#' + el.id : '';
			const cls = el.className ? '.' + ('' + el.className).split(' ').filter(Boolean).slice(0, 3).join('.') : '';
			const txt = ((el.textContent || '').trim().slice(0, 40)).replace(/\s+/g, ' ');
			el.click();
			return el.tagName.toLowerCase() + id + cls + ' :: ' + JSON.stringify(txt);
		})()
	`, pat, tagSelector)
	return chromedp.Evaluate(js, info)
}

// attachFileViaChooser drives the Create → "Upload videos" menu flow and
// feeds the file in via CDP file-chooser interception. The menu item lives
// in ytcp-text-menu's shadow root, so we click it with a shadow-piercing
// walker; the file input also lives in shadow DOM but interception bypasses
// that by setting files via the backend node ID delivered with the chooser
// event.
// attachThumbnailViaChooser locates the ytcp-thumbnail-uploader element (the
// big "Upload thumbnail" tile in the upload dialog's Details tab), clicks it
// with a real coordinate click, and feeds the image path through file-chooser
// interception. Same pattern as the main video upload: shadow-piercing locate
// + native click + CDP chooser intercept.
func attachThumbnailViaChooser(ctx context.Context, filePath string, logf func(string, ...any)) error {
	chooserCh := make(chan cdp.BackendNodeID, 1)
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if e, ok := ev.(*page.EventFileChooserOpened); ok {
			select {
			case chooserCh <- e.BackendNodeID:
			default:
			}
		}
	})

	if err := chromedp.Run(ctx, page.SetInterceptFileChooserDialog(true)); err != nil {
		return fmt.Errorf("enable chooser intercept: %w", err)
	}
	defer func() {
		_ = chromedp.Run(ctx, page.SetInterceptFileChooserDialog(false))
	}()

	logf("thumbnail: locate uploader tile")
	var tx, ty float64
	var info string
	if err := chromedp.Run(ctx, shadowLocateBySelector(
		[]string{"ytcp-thumbnail-uploader", "ytcp-thumbnail-uploader button", "button[aria-label*='thumbnail' i]"},
		&tx, &ty, &info,
	)); err != nil {
		return fmt.Errorf("locate thumbnail uploader: %w", err)
	}
	if info == "" {
		return errors.New("thumbnail uploader element not found")
	}
	logf("thumbnail: tile at (%.0f,%.0f) %s", tx, ty, info)

	if err := chromedp.Run(ctx,
		humanSleep(200*time.Millisecond, 500*time.Millisecond),
		humanClick(tx, ty),
	); err != nil {
		return fmt.Errorf("click thumbnail tile: %w", err)
	}

	select {
	case bnid := <-chooserCh:
		logf("thumbnail: chooser opened on backend node %d", bnid)
		return chromedp.Run(ctx, dom.SetFileInputFiles([]string{filePath}).WithBackendNodeID(bnid))
	case <-time.After(15 * time.Second):
		return errors.New("thumbnail file chooser never opened")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func attachFileViaChooser(ctx context.Context, filePath string, logf func(string, ...any)) error {
	// Listener captures the backend node ID of the file chooser when it opens.
	// Buffered so a slow consumer doesn't deadlock the CDP event dispatcher.
	chooserCh := make(chan cdp.BackendNodeID, 1)
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if e, ok := ev.(*page.EventFileChooserOpened); ok {
			select {
			case chooserCh <- e.BackendNodeID:
			default:
			}
		}
	})

	if err := chromedp.Run(ctx, page.SetInterceptFileChooserDialog(true)); err != nil {
		return fmt.Errorf("enable chooser intercept: %w", err)
	}
	defer func() {
		_ = chromedp.Run(ctx, page.SetInterceptFileChooserDialog(false))
	}()

	logf("step 2: click Create")
	var createInfo string
	if err := chromedp.Run(ctx,
		// The top-bar Create button. The class is stable on current Studio.
		// Fall back to text-based match if the class moves.
		jsClickFirstResult([]string{
			`ytcp-button.ytcpAppHeaderCreateIcon`,
			`#create-icon-button`,
		}, &createInfo),
		chromedp.Sleep(700*time.Millisecond),
	); err != nil {
		return fmt.Errorf("click create: %w", err)
	}
	logf("step 2: Create matched %s", createInfo)

	logf("step 2: shadow-pierce click Upload videos")
	var uploadInfo string
	if err := chromedp.Run(ctx,
		shadowClickByText(
			[]string{"tp-yt-paper-item", "ytcp-text-menu-item", "[role=menuitem]"},
			regexp.MustCompile(`(?i)^upload\s*videos?$`),
			&uploadInfo,
		),
	); err != nil {
		return fmt.Errorf("click upload-videos menu item: %w", err)
	}
	logf("step 2: Upload-videos matched %s", uploadInfo)

	// The upload modal opens but doesn't auto-trigger the native file chooser.
	// We have to click the "Select files" button inside it. The button is a
	// ytcp-button (Polymer custom element) whose click handler listens for
	// the full pointerdown/pointerup gesture sequence — a synthetic .click()
	// in JS is ignored. So we shadow-walk to find the button's screen
	// coordinates, then issue a real CDP mouse click there.
	logf("step 2: locate SELECT FILES button")
	var sx, sy float64
	var selectInfo string
	if err := chromedp.Run(ctx,
		chromedp.Sleep(1*time.Second),
		shadowLocateByText(
			[]string{"ytcp-button#select-files-button", "ytcp-button", "button", "[role=button]"},
			// pit-podcast: r"select file|choose file" — match either phrasing,
			// substring (not anchored), because the rendered text may wrap.
			regexp.MustCompile(`(?i)select\s*files?|choose\s*files?`),
			&sx, &sy, &selectInfo,
		),
	); err != nil {
		return fmt.Errorf("locate select-files: %w", err)
	}
	logf("step 2: SELECT FILES at (%.0f,%.0f) %s", sx, sy, selectInfo)
	if selectInfo == "" {
		return errors.New("could not find SELECT FILES button")
	}
	if err := chromedp.Run(ctx,
		humanSleep(250*time.Millisecond, 600*time.Millisecond),
		humanClick(sx, sy),
	); err != nil {
		return fmt.Errorf("native click select-files: %w", err)
	}

	select {
	case bnid := <-chooserCh:
		logf("step 2: chooser opened on backend node %d", bnid)
		return chromedp.Run(ctx, dom.SetFileInputFiles([]string{filePath}).WithBackendNodeID(bnid))
	case <-time.After(30 * time.Second):
		return errors.New("file chooser never opened after clicking SELECT FILES")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// jsClickFirstResult tries each selector in order and clicks the first
// visible match. Writes a short description of the match into *info.
func jsClickFirstResult(selectors []string, info *string) chromedp.Action {
	// Build a JS array literal of selectors.
	parts := make([]string, len(selectors))
	for i, s := range selectors {
		parts[i] = fmt.Sprintf("%q", s)
	}
	arr := "[" + strings.Join(parts, ",") + "]"
	js := fmt.Sprintf(`
		(() => {
			const sels = %s;
			for (const sel of sels) {
				const els = [...document.querySelectorAll(sel)];
				const el = els.find(e => e.offsetParent !== null);
				if (el) {
					el.click();
					const id = el.id ? '#' + el.id : '';
					return sel + ' :: ' + el.tagName.toLowerCase() + id;
				}
			}
			return '';
		})()
	`, arr)
	return chromedp.Evaluate(js, info)
}

// shadowLocateBySelector walks all shadow roots and returns the center point
// of the first visible element matching any of the given CSS selectors.
func shadowLocateBySelector(selectors []string, x, y *float64, info *string) chromedp.Action {
	parts := make([]string, len(selectors))
	for i, s := range selectors {
		parts[i] = fmt.Sprintf("%q", s)
	}
	selArr := "[" + strings.Join(parts, ",") + "]"
	js := fmt.Sprintf(`
		(() => {
			const sels = %s;
			let target = null;
			function walk(root) {
				if (target) return;
				for (const sel of sels) {
					try {
						const els = [...root.querySelectorAll(sel)];
						const it = els.find(e => e.offsetParent !== null);
						if (it) { target = it; return; }
					} catch (e) {}
				}
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot);
					if (target) return;
				}
			}
			walk(document);
			if (!target) return JSON.stringify({info: ''});
			// The upload dialog scrolls; a trigger below the fold has off-screen
			// coords and the click misses. Bring it into view before measuring.
			try { target.scrollIntoView({block: 'center', inline: 'center'}); } catch (e) {}
			const r = target.getBoundingClientRect();
			const id = target.id ? '#' + target.id : '';
			return JSON.stringify({
				x: r.left + r.width / 2,
				y: r.top + r.height / 2,
				info: target.tagName.toLowerCase() + id,
			});
		})()
	`, selArr)
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var raw string
		if err := chromedp.Evaluate(js, &raw).Do(ctx); err != nil {
			return err
		}
		var out struct {
			X, Y float64
			Info string
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return fmt.Errorf("parse locate result %q: %w", raw, err)
		}
		*x, *y, *info = out.X, out.Y, out.Info
		return nil
	})
}

// shadowLocatePlaylistRow walks the document looking for the playlist
// checkbox row whose label matches name. Returns the row's center
// coordinates so the caller can issue a real MouseClickXY (the checkboxes
// ignore synthetic .click()).
//
// Matching strategy, in priority order:
//  1. First non-empty line of the row's text equals name (case-insensitive).
//     The playlist title renders on line 1; subsequent lines are metadata
//     like "12 videos" or "Private".
//  2. Any line in the row's text equals name (case-insensitive). Covers
//     layouts where the title isn't on line 1.
//
// On failure, samples are returned in JSON so the caller can log what
// candidate row texts were actually present.
// clickPlaylistRow clicks the matched row through the DOM, for when a
// coordinate click lands but does not toggle the checkbox.
func clickPlaylistRow(name string) chromedp.Action {
	js := fmt.Sprintf(`
		(() => {
			%s
			const want = normalizeName(%q);
			function walk(root, fn) {
				fn(root);
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot, fn);
				}
			}
			let target = null;
			let best = 0;
			walk(document, root => {
				root.querySelectorAll('ytcp-checkbox-lit').forEach(el => {
					if (el.offsetParent === null) return;
					const score = matchScore(rowLabel(el), want);
					if (score > best) { best = score; target = el; }
				});
			});
			if (!target) return false;
			// The label wrapping the checkbox is what a person clicks; fall
			// back to the checkbox itself.
			const label = target.closest('label') ||
				target.parentElement?.querySelector('label') || target;
			label.click();
			return true;
		})()
	`, jsPlaylistMatch, name)
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var clicked bool
		if err := chromedp.Evaluate(js, &clicked).Do(ctx); err != nil {
			return err
		}
		if !clicked {
			return fmt.Errorf("playlist row %q not found for direct click", name)
		}
		return nil
	})
}

// jsPlaylistMatch is shared by the row locator and the checked-state probe so
// they agree on what counts as the right row.
//
// Studio's labels are not literal: they carry stray whitespace, a video count on
// the following line, and get truncated with an ellipsis when long. Matching on
// exact equality misses all of that, and a playlist that looks right on screen
// then "isn't in the dropdown".
const jsPlaylistMatch = `
	function normalizeName(s) {
		return (s || '')
			.replace(/\u200b|\u200e|\u200f/g, '')
			.replace(/[\u2026]|\.\.\.$/g, '')
			.replace(/\s+/g, ' ')
			.trim()
			.toLowerCase();
	}
	function rowLabel(el) {
		let p = el, text = '';
		for (let i = 0; i < 4 && p; i++) {
			const t = (p.innerText || p.textContent || '').trim();
			if (t) { text = t; break; }
			p = p.parentElement;
		}
		return text;
	}
	// 0 = no match, 3 = exact, 2 = label is a truncated form of the name,
	// 1 = the name appears inside the label.
	function matchScore(label, want) {
		const lines = (label || '').split('\n').map(normalizeName).filter(Boolean);
		if (!lines.length) return 0;
		if (lines.some(l => l === want)) return 3;
		if (lines.some(l => l.length > 3 && want.startsWith(l))) return 2;
		if (lines.some(l => l.includes(want))) return 1;
		return 0;
	}
`

func shadowLocatePlaylistRow(name string, x, y *float64, info *string) chromedp.Action {
	js := fmt.Sprintf(`
		(() => {
			%s
			const wantName = normalizeName(%q);
			function walk(root, fn) {
				fn(root);
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot, fn);
				}
			}
			let target = null;
			let best = 0;
			const samples = [];
			walk(document, root => {
				root.querySelectorAll('ytcp-checkbox-lit').forEach(el => {
					if (el.offsetParent === null) return;
					const label = rowLabel(el);
					if (!label) return;
					if (samples.length < 40) {
						samples.push(label.split('\n').map(s => s.trim()).filter(Boolean).slice(0, 2).join(' / '));
					}
					const score = matchScore(label, wantName);
					// Keep the best match, so an exact row wins over a row that
					// merely contains the name.
					if (score > best) { best = score; target = el; }
				});
			});
			if (!target) return JSON.stringify({info: '', samples});
			try { target.scrollIntoView({block: 'center', inline: 'center'}); } catch (e) {}
			const r = target.getBoundingClientRect();
			return JSON.stringify({
				x: r.left + r.width / 2,
				y: r.top + r.height / 2,
				info: target.tagName.toLowerCase() + (target.id ? '#' + target.id : '') + ' match=' + best,
			});
		})()
	`, jsPlaylistMatch, name)
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var raw string
		if err := chromedp.Evaluate(js, &raw).Do(ctx); err != nil {
			return err
		}
		var out struct {
			X, Y    float64
			Info    string
			Samples []string
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return fmt.Errorf("parse locate result %q: %w", raw, err)
		}
		*x, *y = out.X, out.Y
		if out.Info == "" && len(out.Samples) > 0 {
			*info = ""
			// Pack samples into the trailing error path via a sentinel:
			// the caller logs *info verbatim on success, but on failure
			// it tests info == "". We surface samples through the locate
			// log too by appending them under a SAMPLES: prefix below.
			return fmt.Errorf("no row matched; sample rows seen: %v", out.Samples)
		}
		*info = out.Info
		return nil
	})
}

// playlistFilterByName locates the playlist dropdown's search input,
// click-focuses it via a real pointer event, then types `name` through the
// CDP Input.insertText command. The dialog's filter listens for genuine
// keyboard input events (not the synthetic setter-+-event pattern that works
// for React), so we have to drive the input from above the DOM. Returns ""
// if no visible search input is found, or a "tag :: placeholder" string for
// logging on success.
func playlistFilterByName(ctx context.Context, name string) (string, error) {
	// The input has to be the one inside the open playlist dropdown. Studio's
	// own "Search across your channel" box at the top of the page also matches
	// any sensible search-input selector, and typing a playlist name into that
	// filters the page instead of the dropdown, leaving the dropdown empty and
	// the playlist "not found".
	const js = `
		(() => {
			function walk(root, fn) {
				fn(root);
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot, fn);
				}
			}
			const visible = el => el && el.offsetParent !== null;
			const isChannelSearch = el => {
				const label = ((el.placeholder || '') + ' ' +
					(el.getAttribute('aria-label') || '')).toLowerCase();
				return label.includes('across your channel') || label.includes('search across');
			};

			// Containers that could be the open dropdown: they hold the playlist
			// checkbox rows.
			const containers = [];
			walk(document, root => {
				root.querySelectorAll(
					'ytcp-playlist-dialog, tp-yt-paper-dialog, tp-yt-iron-dropdown, ytcp-dropdown-dialog'
				).forEach(el => {
					if (visible(el)) containers.push(el);
				});
			});
			let input = null;
			for (const container of containers) {
				let hasRows = false;
				const inputs = [];
				walk(container, root => {
					if (root.querySelector && root.querySelector('ytcp-checkbox-lit')) hasRows = true;
					root.querySelectorAll('input').forEach(el => {
						if (visible(el) && !isChannelSearch(el)) inputs.push(el);
					});
				});
				if (!hasRows || !inputs.length) continue;
				input = inputs[0];
				break;
			}
			// No scoped input: filter nothing rather than typing into the page.
			if (!input) return JSON.stringify({info: ''});
			const r = input.getBoundingClientRect();
			return JSON.stringify({
				x: r.left + r.width / 2,
				y: r.top + r.height / 2,
				info: input.tagName.toLowerCase() +
					' :: ' + (input.placeholder || input.getAttribute('aria-label') || ''),
			});
		})()
	`
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &raw)); err != nil {
		return "", err
	}
	var out struct {
		X, Y float64
		Info string
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("parse filter locate %q: %w", raw, err)
	}
	if out.Info == "" {
		return "", nil
	}
	if err := chromedp.Run(ctx,
		humanClick(out.X, out.Y),
		humanSleep(150*time.Millisecond, 300*time.Millisecond),
		input.InsertText(name),
	); err != nil {
		return out.Info, fmt.Errorf("insert text: %w", err)
	}
	return out.Info, nil
}

// shadowLocateByText walks all shadow roots to find an element matching any
// tagSelector whose textContent matches re, and writes the center of its
// bounding rect into *x, *y plus a short description into *info. If no
// match, *info is left empty so the caller can decide what to do.
func shadowLocateByText(tagSelectors []string, re *regexp.Regexp, x, y *float64, info *string) chromedp.Action {
	pat := strings.TrimPrefix(re.String(), "(?i)")
	parts := make([]string, len(tagSelectors))
	for i, s := range tagSelectors {
		parts[i] = fmt.Sprintf("%q", s)
	}
	selArr := "[" + strings.Join(parts, ",") + "]"
	js := fmt.Sprintf(`
		(() => {
			const re = new RegExp(%q, 'i');
			const sels = %s;
			let target = null;
			function walk(root) {
				if (target) return;
				for (const sel of sels) {
					const els = [...root.querySelectorAll(sel)];
					const it = els.find(e =>
						e.offsetParent !== null &&
						re.test((e.textContent || '').trim())
					);
					if (it) { target = it; return; }
				}
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot);
					if (target) return;
				}
			}
			walk(document);
			if (!target) return JSON.stringify({info: ''});
			// The upload dialog scrolls; a trigger below the fold has off-screen
			// coords and the click misses. Bring it into view before measuring.
			try { target.scrollIntoView({block: 'center', inline: 'center'}); } catch (e) {}
			const r = target.getBoundingClientRect();
			const id = target.id ? '#' + target.id : '';
			const txt = ((target.textContent || '').trim().slice(0, 40)).replace(/\s+/g, ' ');
			return JSON.stringify({
				x: r.left + r.width / 2,
				y: r.top + r.height / 2,
				info: target.tagName.toLowerCase() + id + ' :: ' + JSON.stringify(txt),
			});
		})()
	`, pat, selArr)
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var raw string
		if err := chromedp.Evaluate(js, &raw).Do(ctx); err != nil {
			return err
		}
		var out struct {
			X, Y float64
			Info string
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return fmt.Errorf("parse locate result %q: %w", raw, err)
		}
		*x, *y, *info = out.X, out.Y, out.Info
		return nil
	})
}

// shadowClickByText walks all shadow roots looking for an element matching
// any tagSelector with textContent that matches re, and clicks it.
func shadowClickByText(tagSelectors []string, re *regexp.Regexp, info *string) chromedp.Action {
	pat := strings.TrimPrefix(re.String(), "(?i)")
	parts := make([]string, len(tagSelectors))
	for i, s := range tagSelectors {
		parts[i] = fmt.Sprintf("%q", s)
	}
	selArr := "[" + strings.Join(parts, ",") + "]"
	js := fmt.Sprintf(`
		(() => {
			const re = new RegExp(%q, 'i');
			const sels = %s;
			let target = null;
			function walk(root) {
				if (target) return;
				for (const sel of sels) {
					const els = [...root.querySelectorAll(sel)];
					const it = els.find(e =>
						e.offsetParent !== null &&
						re.test((e.textContent || '').trim())
					);
					if (it) { target = it; return; }
				}
				const all = root.querySelectorAll('*');
				for (const el of all) {
					if (el.shadowRoot) walk(el.shadowRoot);
					if (target) return;
				}
			}
			walk(document);
			if (!target) return '';
			const id = target.id ? '#' + target.id : '';
			const txt = ((target.textContent || '').trim().slice(0, 40)).replace(/\s+/g, ' ');
			target.click();
			return target.tagName.toLowerCase() + id + ' :: ' + JSON.stringify(txt);
		})()
	`, pat, selArr)
	return chromedp.Evaluate(js, info)
}

// waitForVisibleInput polls until selector matches a visible element. The
// built-in WaitVisible returns when the element is in the DOM, which fires
// too early for some Studio panels that mount detached.
func waitForVisibleInput(selector string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			var nodes []*cdp.Node
			err := chromedp.Nodes(selector, &nodes, chromedp.ByQueryAll, chromedp.AtLeast(0)).Do(ctx)
			if err == nil && len(nodes) > 0 {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		return fmt.Errorf("timeout waiting for %s", selector)
	})
}

// fillTextbox clears a contenteditable element and types fresh text. YT
// Studio's title and description fields are contenteditable, not <textarea>,
// so chromedp.SendKeys-on-the-element with a Ctrl-A first works most reliably.
func fillTextbox(selector, value string) chromedp.Action {
	js := fmt.Sprintf(`
		(() => {
			const el = document.querySelector(%q);
			if (!el) return false;
			el.focus();
			document.execCommand('selectAll', false, null);
			document.execCommand('insertText', false, %q);
			return true;
		})()
	`, selector, value)
	var ok bool
	return chromedp.Tasks{
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Evaluate(js, &ok),
		chromedp.Sleep(200 * time.Millisecond),
	}
}

// setTextbox puts text into one of Studio's contenteditable boxes and checks it
// landed, retrying once. The plain "focus then execCommand" route silently does
// nothing when the click target has not settled yet, which is how an upload ends
// up with a title and no description.
func setTextbox(selector, value string, logf func(string, ...any)) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		if err := chromedp.WaitVisible(selector, chromedp.ByQuery).Do(ctx); err != nil {
			return err
		}
		var lastErr error
		for attempt := 1; attempt <= 2; attempt++ {
			// A real click focuses the box the way a person would; .focus()
			// alone is not always enough inside Studio's shadow DOM.
			if err := chromedp.Click(selector, chromedp.ByQuery, chromedp.NodeVisible).Do(ctx); err != nil {
				lastErr = err
			}
			var ok bool
			js := fmt.Sprintf(`
				(() => {
					const el = document.querySelector(%q);
					if (!el) return false;
					el.focus();
					document.execCommand('selectAll', false, null);
					document.execCommand('insertText', false, %q);
					el.dispatchEvent(new Event('input', {bubbles: true}));
					return true;
				})()
			`, selector, value)
			if err := chromedp.Evaluate(js, &ok).Do(ctx); err != nil {
				lastErr = err
			}
			if err := chromedp.Sleep(400 * time.Millisecond).Do(ctx); err != nil {
				return err
			}

			// Verify: Studio trims and re-renders, so compare the first line.
			var got string
			read := fmt.Sprintf(`(document.querySelector(%q) || {}).innerText || ""`, selector)
			if err := chromedp.Evaluate(read, &got).Do(ctx); err != nil {
				lastErr = err
				continue
			}
			wantHead := firstLine(value)
			if wantHead == "" || strings.Contains(firstLine(got), wantHead) {
				return nil
			}
			lastErr = fmt.Errorf("%s still reads %q after attempt %d", selector, firstLine(got), attempt)
			logf("%v, retrying", lastErr)
		}
		return lastErr
	})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// answerAudience picks "not made for kids" when that question is unanswered.
// Studio keeps Next disabled until it is, and a channel with no saved default
// starts blank, which strands the dialog on its first screen.
func answerAudience(logf func(string, ...any)) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var state string
		js := `
			(() => {
				const radios = [...document.querySelectorAll("tp-yt-paper-radio-button[name^='VIDEO_MADE_FOR_KIDS']")];
				if (!radios.length) return "absent";
				if (radios.some(r => r.getAttribute("aria-checked") === "true")) return "already answered";
				const no = radios.find(r => /NOT_MFK/.test(r.getAttribute("name") || ""));
				if (!no) return "no option for not-made-for-kids";
				no.click();
				return "answered";
			})()
		`
		if err := chromedp.Evaluate(js, &state).Do(ctx); err != nil {
			return err
		}
		logf("audience: %s", state)
		return nil
	})
}

// closeEndscreenModal dismisses the end-screen editor if a previous run left it
// open, since it swallows clicks meant for the dialog behind it.
func closeEndscreenModal(logf func(string, ...any)) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var closed bool
		js := `
			(() => {
				if (!document.querySelector("ytve-endscreen-modal")) return false;
				const discard = [...document.querySelectorAll("button")]
					.find(b => /discard/i.test(b.textContent || ""));
				if (discard) { discard.click(); return true; }
				return false;
			})()
		`
		if err := chromedp.Evaluate(js, &closed).Do(ctx); err != nil {
			return err
		}
		if closed {
			logf("closed a leftover end-screen modal")
			return chromedp.Sleep(time.Second).Do(ctx)
		}
		return nil
	})
}

// waitForText polls document.body.innerText against a regex. Accepts patterns
// in Go syntax; the `(?i)` inline flag is stripped because JS RegExp doesn't
// understand it (the 'i' flag is always set anyway).
func waitForText(pattern string) chromedp.Action {
	pattern = strings.TrimPrefix(pattern, "(?i)")
	return chromedp.ActionFunc(func(ctx context.Context) error {
		for {
			var found bool
			js := fmt.Sprintf(`new RegExp(%q, 'i').test(document.body.innerText)`, pattern)
			if err := chromedp.Evaluate(js, &found).Do(ctx); err != nil {
				return err
			}
			if found {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	})
}

// jsClick clicks the first element matching the CSS selector via JS, which
// bypasses pointer-event-blocking overlays that real-click intercepts.
func jsClick(selector string) chromedp.Action {
	js := fmt.Sprintf(`
		(() => {
			const el = document.querySelector(%q);
			if (el) { el.click(); return true; }
			return false;
		})()
	`, selector)
	var ok bool
	return chromedp.Evaluate(js, &ok)
}

// ─── JS snippets ──────────────────────────────────────────────────────────────

// jsReadChannelName returns the channel name visible in Studio's side nav, or
// "" if not signed in. The channel name lives inside a shadow DOM that
// querySelectorAll can't pierce, so we parse it out of document.body.innerText
// (which does pierce shadow boundaries). Sign-in is detected via the
// /channel/UC… URL prefix Studio uses after auth.
const jsReadChannelName = `
	(() => {
		if (!location.pathname.match(/\/channel\/UC[\w-]+/)) return '';
		const text = document.body.innerText || '';
		const m = text.match(/Your channel\s*\n\s*([^\n]+)/);
		return m ? m[1].trim() : '';
	})()
`

// jsExtractVideoID looks at the visible upload dialog for the YT URL link.
const jsExtractVideoID = `
	(() => {
		const links = [...document.querySelectorAll(
			"ytcp-video-info a[href*='youtu.be'], " +
			"ytcp-video-info a.video-url-fadeable, " +
			"a[href*='youtu.be/']"
		)];
		for (const l of links) {
			const m = (l.getAttribute('href') || '').match(/(?:youtu\.be\/|v=)([\w-]{11})/);
			if (m) return m[1];
		}
		return '';
	})()
`

// jsExtractVideoIDFallback runs after Save when the dialog is gone. The
// Studio "videos" page links each row to /video/<id>/edit.
const jsExtractVideoIDFallback = `
	(() => {
		const links = [...document.querySelectorAll("a[href*='studio.youtube.com/video/']")];
		for (const l of links) {
			const m = (l.getAttribute('href') || '').match(/\/video\/([\w-]{11})/);
			if (m) return m[1];
		}
		return '';
	})()
`
