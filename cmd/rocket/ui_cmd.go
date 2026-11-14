package main

import (
	"flag"
	"fmt"
	"os"
)

// runUI starts the server and opens the local web UI.
func runUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	cf := addCommonFlags(fs)
	noBrowser := fs.Bool("no-browser", false, "do not open a browser window")
	if fs.Parse(args) != nil {
		return 2
	}
	rs, err := setup(cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return rs.serve(!*noBrowser)
}

// runRecv runs headless: announce and accept transfers, no browser.
func runRecv(args []string) int {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	cf := addCommonFlags(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	rs, err := setup(cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("headless receiver; press Ctrl+C to stop")
	return rs.serve(false)
}
