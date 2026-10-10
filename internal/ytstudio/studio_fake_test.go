package ytstudio

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Headless-Chrome tests against a page shaped like Studio's upload dialog
// (shadow-DOM textboxes, a Done button). Skipped without CHROME_PATH.
// They reproduce the two DCC 2026-10-10 failures:
//   - Studio's channel defaults landed after the typed title, so Q5/Q6 went
//     up titled "title - Pit Podcast Ep#"
//   - the session closed 3 s after Done, before Studio saved, so every video
//     stayed a Draft.

const fakeStudio = `<!doctype html><html><body>
<div id="title-textarea"></div>
<div id="dlg"><ytcp-button id="done-button" style="display:inline-block;width:80px;height:30px;background:#ccc">Save</ytcp-button></div>
<script>
const host = document.getElementById('title-textarea');
const root = host.attachShadow({mode: 'open'});
root.innerHTML = '<div id="textbox" contenteditable="true" style="width:400px;height:30px;border:1px solid #000">2026 x - Qualification Match 5</div>';
const box = root.getElementById('textbox');
// The channel's upload defaults arrive once, 600 ms after the first edit.
let defaultsApplied = false;
box.addEventListener('input', () => {
  if (defaultsApplied) return;
  defaultsApplied = true;
  setTimeout(() => { box.innerText = 'title - Pit Podcast Ep#'; }, 600);
});
// Save takes CLOSE_MS to go through, then the dialog closes.
const closeMs = Number(new URLSearchParams(location.search).get('close') || '4000');
document.getElementById('done-button').addEventListener('click', () => {
  if (closeMs < 0) return;
  setTimeout(() => { document.getElementById('dlg').remove(); }, closeMs);
});
</script></body></html>`

func browserCtx(t *testing.T) (context.Context, *httptest.Server) {
	t.Helper()
	exe := os.Getenv("CHROME_PATH")
	if exe == "" {
		t.Skip("CHROME_PATH not set")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(fakeStudio))
	}))
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(exe), chromedp.Flag("no-sandbox", true), chromedp.Headless)
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(actx)
	ctx, cancelT := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { cancelT(); cancel(); cancelA(); srv.Close() })
	return ctx, srv
}

func readTitle(ctx context.Context) string {
	var got string
	_ = chromedp.Run(ctx, chromedp.Evaluate(jsShadowTextIn("#title-textarea", "#textbox"), &got))
	return got
}

func TestSetTextAloneLosesToLateDefaults(t *testing.T) {
	ctx, srv := browserCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatal(err)
	}
	want := "2026 Detroit City Championship Qualification Match 5"
	if err := pierceSetText(ctx, "#title-textarea", want); err != nil {
		t.Fatal(err)
	}
	_ = chromedp.Run(ctx, chromedp.Sleep(1500*time.Millisecond))
	// The old code path: what Q5 and Q6 got.
	if got := readTitle(ctx); got != "title - Pit Podcast Ep#" {
		t.Fatalf("fake page should reproduce the overwrite, got %q", got)
	}
}

func TestEnsureTextHoldsAgainstLateDefaults(t *testing.T) {
	ctx, srv := browserCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatal(err)
	}
	want := "2026 Detroit City Championship Qualification Match 5"
	if err := ensureText(ctx, "#title-textarea", want, t.Logf); err != nil {
		t.Fatal(err)
	}
	_ = chromedp.Run(ctx, chromedp.Sleep(1500*time.Millisecond))
	if got := normText(readTitle(ctx)); got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

func TestWaitPublishedWaitsForStudio(t *testing.T) {
	ctx, srv := browserCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/?close=5000")); err != nil {
		t.Fatal(err)
	}
	if ok, err := pierceClick(ctx, "ytcp-button#done-button"); err != nil || !ok {
		t.Fatalf("click: %v %v", ok, err)
	}
	start := time.Now()
	if !waitPublished(ctx, 20*time.Second) {
		t.Fatal("not confirmed")
	}
	if el := time.Since(start); el < 4*time.Second {
		t.Fatalf("returned after %s, before Studio finished saving (5 s)", el)
	}
}

func TestWaitPublishedReportsNoConfirmation(t *testing.T) {
	ctx, srv := browserCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/?close=-1")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := pierceClick(ctx, "ytcp-button#done-button"); !ok {
		t.Fatal("click")
	}
	if waitPublished(ctx, 3*time.Second) {
		t.Fatal("claimed confirmation that never came")
	}
}
