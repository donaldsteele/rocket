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
