package main

import (
	"context"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"
)

const (
	pollEvery   = 2 * time.Second // how often the tray reads rocket's status
	pollTimeout = 1500 * time.Millisecond
	startGrace  = 15 * time.Second // how long "Starting rocket..." may show
)

// app is the running tray: the poll loop, the menu and its click handlers.
type app struct {
	client *Client
	port   int

	refresh chan struct{} // poll now instead of waiting for the ticker

	mu            sync.Mutex
	startingUntil time.Time // a launched rocket may still be coming up
	startErr      string    // why the last Start failed
	shown         View
	shownOnce     bool

	status, offers, activity, open, start, quit, exit *systray.MenuItem
}

func newApp(client *Client, port int) *app {
	return &app{client: client, port: port, refresh: make(chan struct{}, 1)}
}

// runTray blocks until the person picks "Exit tray".
func runTray(a *app) {
	ctx, cancel := context.WithCancel(context.Background())
	systray.Run(func() { a.onReady(ctx) }, cancel)
}

func (a *app) onReady(ctx context.Context) {
	systray.SetIcon(iconBytes(runtime.GOOS, false))
	systray.SetTooltip("rocket")

	a.status = systray.AddMenuItem("Checking rocket...", "")
	a.status.Disable()
	a.offers = systray.AddMenuItem("", "Open rocket to accept or decline")
	a.offers.Hide()
	a.activity = systray.AddMenuItem("", "Open rocket to see the transfers")
	a.activity.Hide()
	systray.AddSeparator()
	a.open = systray.AddMenuItem("Open rocket", "Open the rocket window in your browser")
	a.start = systray.AddMenuItem("Start rocket", "Start rocket in the background")
	a.quit = systray.AddMenuItem("Quit rocket", "Stop rocket on this computer")
	systray.AddSeparator()
	a.exit = systray.AddMenuItem("Exit tray", "Close this tray icon; rocket keeps running")

	go a.handleClicks(ctx)
	go a.pollLoop(ctx)
}

func (a *app) pollLoop(ctx context.Context) {
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		a.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.refresh:
		}
	}
}

func (a *app) poll(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, pollTimeout)
	st := a.client.Status(pctx)
	cancel()

	a.mu.Lock()
	if st.Running {
		a.startingUntil = time.Time{}
		a.startErr = ""
	}
	starting := time.Now().Before(a.startingUntil)
	v := viewFor(st, starting, a.startErr)
	a.mu.Unlock()
	a.apply(v)
}

// pollSoon asks the loop for an immediate poll without blocking.
func (a *app) pollSoon() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

// apply updates only what changed, so the icon is not rewritten every poll.
func (a *app) apply(v View) {
	a.mu.Lock()
	old, first := a.shown, !a.shownOnce
	a.shown, a.shownOnce = v, true
	a.mu.Unlock()

	if first || v.Pending != old.Pending {
		systray.SetIcon(iconBytes(runtime.GOOS, v.Pending))
	}
	if first || v.Tooltip != old.Tooltip {
		systray.SetTooltip(v.Tooltip)
	}
	if first || v.Status != old.Status {
		a.status.SetTitle(v.Status)
	}
	if first || v.Offers != old.Offers {
		if v.Offers == "" {
			a.offers.Hide()
		} else {
			a.offers.SetTitle(v.Offers + ": open rocket")
			a.offers.Show()
		}
	}
	if first || v.Activity != old.Activity {
		if v.Activity == "" {
			a.activity.Hide()
		} else {
			a.activity.SetTitle(v.Activity + ": open rocket")
			a.activity.Show()
		}
	}
	setEnabled(a.open, v.CanOpen)
	setEnabled(a.start, v.CanStart)
	setEnabled(a.quit, v.CanQuit)
}

func setEnabled(item *systray.MenuItem, on bool) {
	if on == !item.Disabled() {
		return
	}
	if on {
		item.Enable()
	} else {
		item.Disable()
	}
}

func (a *app) handleClicks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.offers.ClickedCh:
			openInBrowser(a.client.UIURL())
		case <-a.activity.ClickedCh:
			openInBrowser(a.client.UIURL())
		case <-a.open.ClickedCh:
			openInBrowser(a.client.UIURL())
		case <-a.start.ClickedCh:
			a.startRocket()
		case <-a.quit.ClickedCh:
			qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = a.client.Quit(qctx) // the next poll shows the result either way
			cancel()
			a.pollSoon()
		case <-a.exit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (a *app) startRocket() {
	err := startRocket(a.port)
	a.mu.Lock()
	if err != nil {
		a.startErr = startErrText(err)
		a.startingUntil = time.Time{}
	} else {
		a.startErr = ""
		a.startingUntil = time.Now().Add(startGrace)
	}
	a.mu.Unlock()
	a.pollSoon()
}
