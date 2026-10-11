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

// A page shaped like the upload dialog's Visibility step as seen on the
// Goonettes cart 2026-10-10: the footer moves from "Uploading N%" to
// "Checking N%" to "Checks complete", and a Save before the checks finish
// raises "We're still checking your content" (Publish anyway / Go back),
// which holds the save until it is answered. Unanswered, the video stays a
// Draft; that is what happened to every Goonettes and DCC video that day.
const fakeChecks = `<!doctype html><html><body>
<ytcp-uploads-dialog id="dlg" style="display:block">
  <div id="progress">Uploading 0% ... 2 minutes left</div>
  <ytcp-button id="done-button" style="display:inline-block;width:80px;height:30px;background:#ccc">Save</ytcp-button>
</ytcp-uploads-dialog>
<script>
const q = new URLSearchParams(location.search);
const upMs = Number(q.get('up') || '3000'), chkMs = Number(q.get('chk') || '3000');
const t0 = Date.now();
let checked = false;
setInterval(() => {
  const e = Date.now() - t0, p = document.getElementById('progress');
  if (!p) return;
  if (e < upMs) p.innerText = 'Uploading ' + Math.floor(100 * e / upMs) + '% ... soon';
  else if (e < upMs + chkMs) p.innerText = 'Checking ' + Math.floor(100 * (e - upMs) / chkMs) + '% ... soon';
  else { p.innerText = 'Checks complete. No issues found.'; checked = true; }
}, 100);
document.getElementById('done-button').addEventListener('click', () => {
  if (checked) { setTimeout(() => document.getElementById('dlg').remove(), 500); return; }
  const w = document.createElement('ytcp-prechecks-warning-dialog');
  w.style.display = 'block';
  w.innerHTML = '<p>We’re still checking your content</p>' +
    '<ytcp-button id="anyway" style="display:inline-block;width:120px;height:30px;background:#ddd">Publish anyway</ytcp-button>' +
    '<ytcp-button id="back" style="display:inline-block;width:80px;height:30px;background:#ddd">Go back</ytcp-button>';
  document.body.appendChild(w);
  w.querySelector('#anyway').addEventListener('click', () => {
    w.remove();
    setTimeout(() => document.getElementById('dlg').remove(), 500);
  });
  w.querySelector('#back').addEventListener('click', () => w.remove());
});
</script></body></html>`

func checksCtx(t *testing.T) (context.Context, *httptest.Server) {
	t.Helper()
	exe := os.Getenv("CHROME_PATH")
	if exe == "" {
		t.Skip("CHROME_PATH not set")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(fakeChecks))
	}))
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(exe), chromedp.Flag("no-sandbox", true), chromedp.Headless)
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(actx)
	ctx, cancelT := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { cancelT(); cancel(); cancelA(); srv.Close() })
	return ctx, srv
}

// The old flow: Save straight away and wait. The page reproduces the stuck Draft.
func TestEarlySaveIsHeldByStillCheckingDialog(t *testing.T) {
	ctx, srv := checksCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/?up=20000&chk=20000")); err != nil {
		t.Fatal(err)
	}
	if ok, err := pierceClick(ctx, "ytcp-button#done-button"); err != nil || !ok {
		t.Fatalf("click: %v %v", ok, err)
	}
	if waitPublished(ctx, 3*time.Second) {
		t.Fatal("fake page should hold the save until the dialog is answered")
	}
}

func TestPublishAnywayAnswersStillChecking(t *testing.T) {
	ctx, srv := checksCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/?up=1000&chk=20000")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := pierceClick(ctx, "ytcp-button#done-button"); !ok {
		t.Fatal("click")
	}
	answered, err := publishAnyway(ctx, 5*time.Second)
	if err != nil || !answered {
		t.Fatalf("publishAnyway = %v, %v", answered, err)
	}
	if !waitPublished(ctx, 10*time.Second) {
		t.Fatal("dialog did not close after Publish anyway")
	}
}

func TestWaitsForTransferAndChecksBeforeSaving(t *testing.T) {
	ctx, srv := checksCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/?up=3000&chk=3000")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := waitTransferred(ctx, t.Logf); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 2500*time.Millisecond {
		t.Fatalf("transfer reported done after %s, while still uploading", el)
	}
	if !waitChecksDone(ctx, 20*time.Second, t.Logf) {
		t.Fatal("checks never reported complete")
	}
	if ok, _ := pierceClick(ctx, "ytcp-button#done-button"); !ok {
		t.Fatal("click")
	}
	if answered, _ := publishAnyway(ctx, 2*time.Second); answered {
		t.Fatal("no still-checking dialog expected once checks are complete")
	}
	if !waitPublished(ctx, 10*time.Second) {
		t.Fatal("not published")
	}
}

func TestWaitChecksDoneGivesUp(t *testing.T) {
	ctx, srv := checksCtx(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/?up=500&chk=60000")); err != nil {
		t.Fatal(err)
	}
	if waitChecksDone(ctx, 3*time.Second, t.Logf) {
		t.Fatal("claimed checks complete while still checking")
	}
}
