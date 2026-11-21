package main

import (
	"errors"
	"fmt"
	"strings"
)

// View is what the tray shows for one Status. It holds no systray types, so
// the mapping is tested without a desktop.
type View struct {
	Status   string // disabled first menu line
	Offers   string // "2 incoming offers"; empty hides the item
	Activity string // "1 transfer reconnecting, 2 interrupted transfers"; empty hides the item
	Tooltip  string // icon tooltip
	Pending  bool   // use the badged icon
	CanOpen  bool   // Open rocket
	CanStart bool   // Start rocket
	CanQuit  bool   // Quit rocket
	// Pair is the pairing item: "Pair new devices (5 min)" or "Stop pairing
	// (4:59 left)"; empty hides it (rocket not running, or too old to pair).
	Pair     string
	PairOpen bool // the window is open, so clicking Pair closes it
}

// viewFor maps a poll result to the menu. starting is true while a rocket the
// tray launched is still coming up; startErr is why the last launch failed.
func viewFor(st Status, starting bool, startErr string) View {
	if st.Running {
		peers := plural(st.Peers, "peer", "peers")
		if st.OldPeers > 0 {
			peers += " (" + plural(st.OldPeers, "needs", "need") + " update)"
		}
		v := View{
			Status:  "Running, " + peers,
			Tooltip: "rocket: running, " + peers,
			CanOpen: true,
			CanQuit: true,
		}
		var act []string
		if st.Reconnecting > 0 {
			act = append(act, plural(st.Reconnecting, "transfer", "transfers")+" reconnecting")
		}
		if st.Interrupted > 0 {
			act = append(act, plural(st.Interrupted, "interrupted transfer", "interrupted transfers"))
		}
		if len(act) > 0 {
			v.Activity = strings.Join(act, ", ")
			v.Tooltip = "rocket: " + v.Activity
		}
		if st.Incoming > 0 {
			v.Offers = plural(st.Incoming, "incoming offer", "incoming offers")
			v.Tooltip = "rocket: " + v.Offers + " waiting"
			v.Pending = true
		}
		v.Pair, v.PairOpen = pairLabel(st.Pair)
		return v
	}
	if starting {
		return View{Status: "Starting rocket...", Tooltip: "rocket: starting"}
	}
	v := View{Status: "Not running", Tooltip: "rocket: not running", CanStart: true}
	if startErr != "" {
		v.Status = "Not running (" + startErr + ")"
	}
	return v
}

// pairLabel is the pairing item's title and whether the window is open. The
// countdown moves on every poll (pollEvery), so it steps by a couple of
// seconds; rocket rounds the seconds left up.
func pairLabel(p PairState) (string, bool) {
	switch {
	case !p.Supported:
		return "", false
	case !p.Open:
		return fmt.Sprintf("Pair new devices (%d min)", pairSeconds/60), false
	case p.Seconds <= 0:
		return "Stop pairing", true
	default:
		return fmt.Sprintf("Stop pairing (%d:%02d left)", p.Seconds/60, p.Seconds%60), true
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// startErrText is the short reason shown when Start rocket fails.
func startErrText(err error) string {
	if errors.Is(err, errNoRocket) {
		return "rocket program not found"
	}
	return "could not start rocket"
}
