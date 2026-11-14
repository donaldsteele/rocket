package main

import (
	"errors"
	"fmt"
)

// View is what the tray shows for one Status. It holds no systray types, so
// the mapping is tested without a desktop.
type View struct {
	Status   string // disabled first menu line
	Offers   string // "2 incoming offers"; empty hides the item
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
		v := View{
			Status:  "Running, " + plural(st.Peers, "peer", "peers"),
			Tooltip: "rocket: running, " + plural(st.Peers, "peer", "peers"),
			CanOpen: true,
			CanQuit: true,
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
