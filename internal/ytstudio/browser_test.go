package ytstudio

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestCftPlatform(t *testing.T) {
	platform, err := cftPlatform()
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		if err != nil || platform != "linux64" {
			t.Fatalf("platform = %q, %v", platform, err)
		}
	}
	if err == nil && platform == "" {
		t.Error("no error but no platform")
	}
}

func TestVersionOrdering(t *testing.T) {
	versions := []string{"99.0.1.2", "153.0.8010.52", "120.0.1.1", "153.0.8010.9"}
	sort.Sort(sort.Reverse(byVersion(versions)))
	if versions[0] != "153.0.8010.52" {
		t.Errorf("newest = %q, want 153.0.8010.52 (got order %v)", versions[0], versions)
	}
	if versions[len(versions)-1] != "99.0.1.2" {
		t.Errorf("oldest = %q, want 99.0.1.2", versions[len(versions)-1])
	}
}

// An existing download must be reused without going near the network, so an
// event with no internet still runs.
func TestInstalledFoundWithoutNetwork(t *testing.T) {
	root := t.TempDir()
	exeName := "chrome"
	if runtime.GOOS == "windows" {
		exeName = "chrome.exe"
	}
	if runtime.GOOS == "darwin" {
		// Skip: the mac layout is an .app bundle, covered by the real download
		// test below.
		t.Skip("mac layout differs")
	}
	dir := filepath.Join(root, "153.0.8010.52", "chrome-linux64")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, exeName), []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An older version and an interrupted download must both be ignored in
	// favour of the newest complete one.
	if err := os.MkdirAll(filepath.Join(root, ".download-154.0.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	m := &ManagedBrowser{Root: root}
	got := m.Installed()
	if !strings.HasSuffix(got, filepath.Join("153.0.8010.52", "chrome-linux64", exeName)) {
		t.Errorf("Installed() = %q", got)
	}
	if v := m.InstalledVersion(); v != "153.0.8010.52" {
		t.Errorf("InstalledVersion() = %q", v)
	}

	exe, err := m.Ensure(context.Background())
	if err != nil || exe != got {
		t.Errorf("Ensure() = %q, %v", exe, err)
	}
}

func TestInstalledEmptyWhenNothingDownloaded(t *testing.T) {
	m := &ManagedBrowser{Root: filepath.Join(t.TempDir(), "browser")}
	if got := m.Installed(); got != "" {
		t.Errorf("Installed() = %q, want empty", got)
	}
	if got := m.InstalledVersion(); got != "" {
		t.Errorf("InstalledVersion() = %q, want empty", got)
	}
}

// The real thing: download Chrome for Testing and drive it. Downloads a few
// hundred megabytes, so it only runs when asked.
//
//	YTSTUDIO_DOWNLOAD_TEST=1 go test ./internal/ytstudio/ -run TestManagedBrowserDownloadAndRun -v
func TestManagedBrowserDownloadAndRun(t *testing.T) {
	if os.Getenv("YTSTUDIO_DOWNLOAD_TEST") == "" {
		t.Skip("set YTSTUDIO_DOWNLOAD_TEST=1 to download a browser")
	}
	root := os.Getenv("YTSTUDIO_DOWNLOAD_DIR")
	if root == "" {
		root = filepath.Join(t.TempDir(), "browser")
	}

	m := &ManagedBrowser{Root: root, Logf: func(format string, args ...any) {
		t.Logf(format, args...)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	exe, err := m.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		t.Fatalf("browser binary %q: %v", exe, err)
	}
	t.Logf("browser at %s (version dir %s)", exe, m.InstalledVersion())

	// Second call must be free.
	again, err := m.Ensure(ctx)
	if err != nil || again != exe {
		t.Errorf("second Ensure = %q, %v", again, err)
	}

	// Drive it: launch with a throwaway profile and read the user agent back.
	profile := filepath.Join(t.TempDir(), "profile")
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(exe),
		chromedp.UserDataDir(profile),
		chromedp.NoSandbox,
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Headless,
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	runCtx, cancelRun := chromedp.NewContext(allocCtx)
	defer cancelRun()

	var ua string
	runCtx, cancelTO := context.WithTimeout(runCtx, 2*time.Minute)
	defer cancelTO()
	if err := chromedp.Run(runCtx,
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(`navigator.userAgent`, &ua),
	); err != nil {
		t.Fatalf("drive browser: %v", err)
	}
	t.Logf("user agent: %s", ua)
	if !strings.Contains(ua, "Chrome/") {
		t.Errorf("user agent does not look like Chrome: %q", ua)
	}
}
