package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
)

// trustUsage lists the trust subcommands.
func trustUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: rocket trust [list | add <public-key|fingerprint> [--name NAME] | remove <fingerprint>] [--data-dir DIR]")
	fmt.Fprintln(w, "  list     the senders whose files arrive without asking (default)")
	fmt.Fprintln(w, "  add      trust a sender ahead of time, by the public key 'rocket id' prints on")
	fmt.Fprintln(w, "           it (safest), or by its fingerprint; its key is then pinned on its first offer")
	fmt.Fprintln(w, "  remove   forget a trusted sender; its next offer asks again")
}

// runTrust manages the trusted senders from the command line. It edits
// trusted.json in the data folder; a rocket already running on that folder
// picks the change up on its next offer.
func runTrust(args []string) int {
	return runTrustTo(args, os.Stdout, os.Stderr)
}

func runTrustTo(args []string, stdout, stderr io.Writer) int {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("trust "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "folder for the identity key and trust store (default: user config dir/rocket)")
	name := fs.String("name", "", "add: a name for the sender (default: the name its first offer gives)")
	pos, ok := parseInterspersed(fs, args)
	if !ok {
		return 2
	}
	switch sub {
	case "list", "add", "remove":
	case "-h", "help":
		trustUsage(stdout)
		return 0
	default:
		trustUsage(stderr)
		return 2
	}
	cfg, err := config.Load(config.Overrides{DataDir: *dataDir})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	store, err := peers.Open(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	switch sub {
	case "list":
		if len(pos) != 0 {
			trustUsage(stderr)
			return 2
		}
		printTrusted(stdout, store.List())
		return 0
	case "add":
		if len(pos) != 1 {
			trustUsage(stderr)
			return 2
		}
		t, err := addTrusted(store, pos[0], *name)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		describeAdded(stdout, t)
		warnAutoAccept(stdout, cfg.DataDir)
		return 0
	default: // remove
		if len(pos) != 1 {
			trustUsage(stderr)
			return 2
		}
		fp := strings.ToLower(strings.TrimSpace(pos[0]))
		if _, ok := store.Get(fp); !ok {
			fmt.Fprintf(stderr, "error: %s is not a trusted sender (see rocket trust list)\n", pos[0])
			return 1
		}
		if err := store.Revoke(fp); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed %s; its next offer asks again\n", fp)
		return 0
	}
}

// addTrusted adds ref, a base64 public key or a 16-hex fingerprint.
func addTrusted(store *peers.Store, ref, name string) (peers.Trusted, error) {
	ref = strings.TrimSpace(ref)
	if fp := strings.ToLower(ref); peers.IsFingerprint(fp) {
		return store.Add(fp, name, "")
	}
	if _, err := node.DecodePublicKey(ref); err != nil {
		return peers.Trusted{}, errors.New("not a public key (as 'rocket id' prints it) or a fingerprint (16 hex characters)")
	}
	return store.Add("", name, ref)
}

func describeAdded(w io.Writer, t peers.Trusted) {
	label := t.Fingerprint
	if t.Name != "" {
		label = fmt.Sprintf("%s (%s)", t.Name, t.Fingerprint)
	}
	if t.Pub != "" {
		fmt.Fprintf(w, "trusted %s, pinned to its public key: its files arrive without asking\n", label)
		return
	}
	fmt.Fprintf(w, "trusted %s by fingerprint: its files arrive without asking, and its key is pinned on its first offer\n", label)
	fmt.Fprintln(w, "note: a fingerprint is 64 bits of the key's hash; adding the full public key ('rocket id' on that device) is stronger")
}

// warnAutoAccept says so when trusted senders are still asked.
func warnAutoAccept(w io.Writer, dataDir string) {
	if s, err := config.ReadSettings(dataDir); err == nil && !s.AutoAcceptTrusted {
		fmt.Fprintln(w, "warning: 'Accept files from trusted senders without asking' is off in Settings, so trusted senders are still asked")
	}
}

func printTrusted(w io.Writer, list []peers.Trusted) {
	if len(list) == 0 {
		fmt.Fprintln(w, "no trusted senders; every offer asks")
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ApprovedAt.After(list[j].ApprovedAt) })
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FINGERPRINT\tNAME\tPINNED\tSINCE")
	for _, t := range list {
		pinned := "key"
		if t.Pub == "" {
			pinned = "fingerprint (key on first offer)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Fingerprint, t.Name, pinned, t.ApprovedAt.Local().Format("2006-01-02 15:04"))
	}
	_ = tw.Flush()
}

// runID prints this host's identity: what another device needs to trust it
// ahead of time.
func runID(args []string) int {
	fs := flag.NewFlagSet("id", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "folder for the identity key and trust store (default: user config dir/rocket)")
	devName := fs.String("name", "", "device name shown to peers (default: hostname)")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, err := config.Load(config.Overrides{DataDir: *dataDir, Name: *devName})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	id, err := node.Load(cfg.DataDir, cfg.Name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("name:        %s\n", id.Name)
	fmt.Printf("fingerprint: %s\n", id.Fingerprint())
	fmt.Printf("public key:  %s\n", id.PublicKeyB64())
	fmt.Println()
	fmt.Println("To receive from this device without being asked, run on the receiving one:")
	fmt.Printf("  rocket trust add %s --name %s\n", id.PublicKeyB64(), shellQuote(id.Name))
	return 0
}

// trustFlag collects repeated --trust values.
type trustFlag []string

func (t *trustFlag) String() string     { return strings.Join(*t, ",") }
func (t *trustFlag) Set(v string) error { *t = append(*t, v); return nil }

// applyTrustFlags adds the --trust senders before a host starts.
func applyTrustFlags(rs *runtimeState, refs []string) error {
	for _, ref := range refs {
		t, err := addTrusted(rs.trust, ref, "")
		if err != nil {
			return fmt.Errorf("--trust %s: %w", ref, err)
		}
		describeAdded(os.Stdout, t)
	}
	if len(refs) > 0 {
		warnAutoAccept(os.Stdout, rs.cfg.DataDir)
	}
	return nil
}

// parseInterspersed parses flags that may come before or after the
// positional arguments, which it returns.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, bool) {
	var pos []string
	for {
		if fs.Parse(args) != nil {
			return nil, false
		}
		if fs.NArg() == 0 {
			return pos, true
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// shellQuote quotes s for a shell when it holds anything but plain
// characters.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
