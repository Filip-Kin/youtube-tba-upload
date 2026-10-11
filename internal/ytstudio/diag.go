package ytstudio

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Step snapshots for every upload: a small screenshot and the upload dialog's
// text at each step, plus Studio's own record of the video at the end. When
// Studio changes its pages again, the evidence of what the browser saw is
// already on disk instead of having to be reproduced by hand (Goonettes
// 2026-10-10: every video stayed a Draft and the log could not say why).
//
// Kept small and off the upload's path:
//   - screenshots are viewport-only JPEGs at quality 40 (about 50-100 KB),
//     text is capped at 6 KB, and only the newest diagKeep uploads are kept,
//     so the folder stays around 10-20 MB;
//   - each capture has its own 3 s budget and a failure is ignored;
//   - files are written by one background goroutine from a buffered channel,
//     and a snapshot is dropped rather than wait if the writer falls behind.

const (
	diagKeep     = 25
	diagTextCap  = 6 << 10
	diagQuality  = 40
	diagQueueLen = 16
)

type diagItem struct {
	name string
	data []byte
}

type diagRun struct {
	dir  string
	n    int
	ch   chan diagItem
	done chan struct{}
}

// DiagRoot is where upload snapshots go: beside the profiles folder.
func (d *ChromedpDriver) DiagRoot() string {
	if d.ProfileRoot == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.ProfileRoot), "diag")
}

var diagUnsafe = regexp.MustCompile(`[^A-Za-z0-9 ._#()-]+`)

// startDiag opens a snapshot folder for one upload. It never fails: with no
// folder it returns a run whose methods do nothing.
func (d *ChromedpDriver) startDiag(label string) *diagRun {
	root := d.DiagRoot()
	if root == "" {
		return &diagRun{}
	}
	name := time.Now().Format("20060102-150405") + " " + strings.TrimSpace(diagUnsafe.ReplaceAllString(label, "_"))
	if len(name) > 90 {
		name = name[:90]
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &diagRun{}
	}
	r := &diagRun{dir: dir, ch: make(chan diagItem, diagQueueLen), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for it := range r.ch {
			_ = os.WriteFile(filepath.Join(dir, it.name), it.data, 0o644)
		}
		pruneDiag(root, diagKeep)
	}()
	return r
}

func pruneDiag(root string, keep int) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs) // names start with a sortable timestamp
	for len(dirs) > keep {
		_ = os.RemoveAll(filepath.Join(root, dirs[0]))
		dirs = dirs[1:]
	}
}

func (r *diagRun) put(name string, data []byte) {
	if r == nil || r.ch == nil {
		return
	}
	select {
	case r.ch <- diagItem{name, data}:
	default: // writer behind: drop rather than slow the upload
	}
}

// note appends a line to the run's notes (step names, results).
func (r *diagRun) note(format string, args ...any) {
	if r == nil || r.ch == nil {
		return
	}
	r.n++
	r.put(fmt.Sprintf("%02d-note.txt", r.n), []byte(fmt.Sprintf(time.Now().Format("15:04:05.000 ")+format+"\n", args...)))
}

const jsDiagText = `(() => {
	const d = document.querySelector('ytcp-uploads-dialog');
	const parts = [location.href];
	if (d && d.offsetParent !== null) parts.push('[uploads dialog]\n' + d.innerText);
	for (const sel of ['ytcp-video-share-dialog', 'ytcp-playlist-dialog', 'tp-yt-paper-dialog', 'ytcp-confirmation-dialog', 'ytcp-prechecks-warning-dialog']) {
		for (const el of document.querySelectorAll(sel)) {
			if (el.offsetParent !== null || getComputedStyle(el).display !== 'none') {
				const t = (el.innerText || '').trim();
				if (t) parts.push('[' + sel + ']\n' + t);
			}
		}
	}
	if (parts.length === 1) parts.push(document.body.innerText);
	return parts.join('\n\n');
})()`

// snap records a screenshot and the visible dialog text under the step name.
func (r *diagRun) snap(ctx context.Context, step string) {
	if r == nil || r.ch == nil {
		return
	}
	r.n++
	base := fmt.Sprintf("%02d-%s", r.n, diagUnsafe.ReplaceAllString(step, "_"))
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var shot []byte
	if err := chromedp.Run(cctx, chromedp.ActionFunc(func(c context.Context) error {
		b, err := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(diagQuality).Do(c)
		shot = b
		return err
	})); err == nil && len(shot) > 0 {
		r.put(base+".jpg", shot)
	}
	var text string
	if err := chromedp.Run(cctx, chromedp.Evaluate(jsDiagText, &text)); err == nil {
		if len(text) > diagTextCap {
			text = text[:diagTextCap]
		}
		r.put(base+".txt", []byte(text))
	}
}

// close flushes the writer. Bounded so a stuck disk cannot hold the upload.
func (r *diagRun) close() {
	if r == nil || r.ch == nil {
		return
	}
	close(r.ch)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
	}
}
