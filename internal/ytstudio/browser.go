package ytstudio

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The uploader drives a browser it owns: Chrome for Testing, downloaded once
// into the tool's data directory. This is the same build Puppeteer fetches, and
// it exists to stay out of the way of the browser the operator actually uses.
// Chrome allows one process per profile directory, so sharing the everyday
// browser means closing it before every upload; a browser of our own has none
// of that, and its YouTube session is signed in once and then persists.
const cftVersionsURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// ManagedBrowser resolves (and downloads on first use) the browser the driver
// runs. Root is a directory containing one subdirectory per installed version.
type ManagedBrowser struct {
	Root string
	// Logf receives progress lines. Optional.
	Logf func(format string, args ...any)

	mu sync.Mutex
}

func (m *ManagedBrowser) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// cftPlatform is Chrome for Testing's name for this machine.
func cftPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "win64", nil
	case "windows/386":
		return "win32", nil
	case "darwin/arm64":
		return "mac-arm64", nil
	case "darwin/amd64":
		return "mac-x64", nil
	case "linux/amd64":
		return "linux64", nil
	case "linux/arm64":
		return "linux-arm64", nil
	}
	return "", fmt.Errorf("ytstudio: no Chrome for Testing build for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// browserExeNames are the paths a Chrome for Testing archive puts the binary at,
// relative to the extracted version directory.
func browserExeGlobs() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			filepath.Join("*", "chrome.exe"),
			"chrome.exe",
		}
	case "darwin":
		return []string{
			filepath.Join("*", "*.app", "Contents", "MacOS", "Google Chrome for Testing"),
			filepath.Join("*.app", "Contents", "MacOS", "Google Chrome for Testing"),
		}
	default:
		return []string{
			filepath.Join("*", "chrome"),
			"chrome",
		}
	}
}

// findExeIn returns the browser binary inside an extracted version directory.
func findExeIn(dir string) (string, error) {
	for _, glob := range browserExeGlobs() {
		matches, err := filepath.Glob(filepath.Join(dir, glob))
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			if st, err := os.Stat(match); err == nil && !st.IsDir() {
				return match, nil
			}
		}
	}
	return "", fmt.Errorf("no browser binary under %s", dir)
}

// Installed returns the newest already-downloaded browser, or "" when there is
// none. Never touches the network, so an event with no internet still runs on
// whatever was downloaded beforehand.
func (m *ManagedBrowser) Installed() string {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		return ""
	}
	versions := []string{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			versions = append(versions, e.Name())
		}
	}
	sort.Sort(sort.Reverse(byVersion(versions)))
	for _, v := range versions {
		if exe, err := findExeIn(filepath.Join(m.Root, v)); err == nil {
			return exe
		}
	}
	return ""
}

// InstalledVersion is the version directory name of Installed(), for display.
func (m *ManagedBrowser) InstalledVersion() string {
	exe := m.Installed()
	if exe == "" {
		return ""
	}
	rel, err := filepath.Rel(m.Root, exe)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// byVersion sorts dotted version strings numerically, so 153 beats 99.
type byVersion []string

func (v byVersion) Len() int      { return len(v) }
func (v byVersion) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v byVersion) Less(i, j int) bool {
	a := strings.Split(v[i], ".")
	b := strings.Split(v[j], ".")
	for k := 0; k < len(a) && k < len(b); k++ {
		ai, aerr := strconv.Atoi(a[k])
		bi, berr := strconv.Atoi(b[k])
		if aerr != nil || berr != nil {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
			continue
		}
		if ai != bi {
			return ai < bi
		}
	}
	return len(a) < len(b)
}

// Ensure returns a browser to drive, downloading Chrome for Testing on first
// use. An existing download always wins, so this is a no-op after the first run.
func (m *ManagedBrowser) Ensure(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if exe := m.Installed(); exe != "" {
		return exe, nil
	}
	version, url, err := latestStableBuild(ctx)
	if err != nil {
		return "", err
	}
	m.logf("downloading Chrome for Testing %s (one time, ~200 MB)", version)
	dir, err := m.download(ctx, version, url)
	if err != nil {
		return "", err
	}
	exe, err := findExeIn(dir)
	if err != nil {
		return "", err
	}
	m.logf("browser ready: %s", exe)
	return exe, nil
}

// latestStableBuild asks Chrome for Testing for the current stable build for
// this platform.
func latestStableBuild(ctx context.Context) (version string, url string, err error) {
	platform, err := cftPlatform()
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cftVersionsURL, nil)
	if err != nil {
		return "", "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("ytstudio: look up Chrome for Testing: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("ytstudio: look up Chrome for Testing: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Channels map[string]struct {
			Version   string `json:"version"`
			Downloads struct {
				Chrome []struct {
					Platform string `json:"platform"`
					URL      string `json:"url"`
				} `json:"chrome"`
			} `json:"downloads"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", fmt.Errorf("ytstudio: parse Chrome for Testing versions: %w", err)
	}
	stable, ok := body.Channels["Stable"]
	if !ok {
		return "", "", errors.New("ytstudio: Chrome for Testing has no stable channel")
	}
	for _, build := range stable.Downloads.Chrome {
		if build.Platform == platform {
			return stable.Version, build.URL, nil
		}
	}
	return "", "", fmt.Errorf("ytstudio: no stable Chrome for Testing build for %s", platform)
}

// download fetches and extracts one build, and only moves it into place once it
// is complete, so an interrupted download can't leave a half-browser behind.
func (m *ManagedBrowser) download(ctx context.Context, version, url string) (string, error) {
	final := filepath.Join(m.Root, version)
	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return "", err
	}
	staging := filepath.Join(m.Root, ".download-"+version)
	_ = os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}

	archive := filepath.Join(staging, "chrome.zip")
	if err := m.fetchFile(ctx, url, archive); err != nil {
		return "", err
	}
	extracted := filepath.Join(staging, "extracted")
	if err := unzip(archive, extracted); err != nil {
		return "", fmt.Errorf("ytstudio: extract browser: %w", err)
	}
	if _, err := findExeIn(extracted); err != nil {
		return "", fmt.Errorf("ytstudio: %w", err)
	}
	_ = os.RemoveAll(final)
	if err := os.Rename(extracted, final); err != nil {
		return "", err
	}
	return final, nil
}

func (m *ManagedBrowser) fetchFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// No overall timeout: this is a few hundred megabytes, and the caller's
	// context already bounds the run.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ytstudio: download browser: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ytstudio: download browser: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	written, err := io.Copy(out, &progressReader{
		r:     resp.Body,
		total: resp.ContentLength,
		logf:  m.logf,
	})
	if err != nil {
		return fmt.Errorf("ytstudio: download browser after %d bytes: %w", written, err)
	}
	return nil
}

// progressReader logs download progress every 50 MB, so a slow event network
// doesn't look like a hang.
type progressReader struct {
	r      io.Reader
	total  int64
	read   int64
	logged int64
	logf   func(string, ...any)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)
	const step = 50 << 20
	if p.read-p.logged >= step {
		p.logged = p.read
		if p.total > 0 {
			p.logf("browser download: %d/%d MB", p.read>>20, p.total>>20)
		} else {
			p.logf("browser download: %d MB", p.read>>20)
		}
	}
	return n, err
}

// unzip extracts a zip archive, keeping the executable bits (the browser and
// its helpers are useless without them).
func unzip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		// Reject paths that would escape the destination.
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		rel, err := filepath.Rel(dest, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := f.Mode()
		if mode == 0 {
			mode = 0o644
		}
		if err := writeZipEntry(f, target, mode); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string, mode os.FileMode) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	// Symlinks appear in the mac archives.
	if mode&os.ModeSymlink != 0 {
		link, err := io.ReadAll(src)
		if err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Symlink(string(link), target)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}
