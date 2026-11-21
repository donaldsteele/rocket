package main

import (
	"flag"
	"fmt"
	"os"
)

const pairFlagUsage = "open a pairing window this long once started (e.g. 5m, at most 1h): new rocket senders are accepted and trusted without asking"

const trustFlagUsage = "trust this sender before starting, by public key (from 'rocket id') or fingerprint; repeatable, and kept like 'rocket trust add'"

// runUI starts the server and opens the local web UI.
func runUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	cf := addCommonFlags(fs)
	noBrowser := fs.Bool("no-browser", false, "do not open a browser window")
	var trust trustFlag
	fs.Var(&trust, "trust", trustFlagUsage)
	pair := fs.Duration("pair", 0, pairFlagUsage)
	if fs.Parse(args) != nil {
		return 2
	}
	rs, err := setup(cf)
	if err == nil {
		err = applyTrustFlags(rs, trust)
	}
	if err == nil {
		rs.pair, err = pairDuration(*pair)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return rs.serve(!*noBrowser)
}

// runRecv runs headless: announce and accept transfers, no browser.
// Offers from trusted senders arrive without asking; any other offer waits
// for an answer in the local page (http://127.0.0.1:<port>/) and times out.
func runRecv(args []string) int {
	fs := flag.NewFlagSet("recv", flag.ContinueOnError)
	cf := addCommonFlags(fs)
	var trust trustFlag
	fs.Var(&trust, "trust", trustFlagUsage)
	pair := fs.Duration("pair", 0, pairFlagUsage)
	if fs.Parse(args) != nil {
		return 2
	}
	rs, err := setup(cf)
	if err == nil {
		err = applyTrustFlags(rs, trust)
	}
	if err == nil {
		rs.pair, err = pairDuration(*pair)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("headless receiver; press Ctrl+C to stop")
	return rs.serve(false)
}
