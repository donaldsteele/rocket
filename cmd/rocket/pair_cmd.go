package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/httpapi"
	"github.com/donaldsteele/rocket/internal/receiver"
)

// pairDuration checks a --pair or rocket pair duration.
func pairDuration(d time.Duration) (time.Duration, error) {
	switch {
	case d < 0:
		return 0, errors.New("--pair: the duration cannot be negative")
	case d > receiver.MaxPairing:
		return 0, fmt.Errorf("--pair: at most %s", receiver.MaxPairing)
	}
	return d, nil
}

// runPair opens, closes or shows the pairing window of the rocket running
// on this computer (rocket ui or rocket recv), through its loopback API:
//
//	rocket pair [5m]     open it (default 5 minutes, at most 1 hour)
//	rocket pair stop     close it
//	rocket pair status   show it
func runPair(args []string) int {
	return runPairTo(args, os.Stdout, os.Stderr)
}

func runPairTo(args []string, stdout, stderr io.Writer) int {
	what := receiver.DefaultPairing.String()
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		what, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 0, "HTTP port of the running rocket (default 29005)")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: rocket pair [DURATION | stop | status] [--port N]")
		return 2
	}
	cfg, err := config.Load(config.Overrides{HTTPPort: *port})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	var body any
	switch what {
	case "status":
	case "stop":
		body = map[string]int{"seconds": 0}
	default:
		d, err := time.ParseDuration(what)
		if err == nil {
			d, err = pairDuration(d)
		}
		if err != nil || d == 0 {
			fmt.Fprintf(stderr, "error: %q is not a duration such as 5m (at most 1h), stop or status\n", what)
			return 2
		}
		body = map[string]int{"seconds": int(d / time.Second)}
	}
	st, err := pairCall(cfg.HTTPPort, body)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if st.Open {
		fmt.Fprintf(stdout, "pairing open until %s (%s left): new rocket senders are accepted and trusted without asking\n",
			st.Until.Local().Format("15:04:05"), (time.Duration(st.Seconds) * time.Second).String())
	} else {
		fmt.Fprintln(stdout, "pairing closed: new senders are asked")
	}
	return 0
}

// pairCall reads (body nil) or changes the pairing window of the rocket on
// 127.0.0.1:port.
func pairCall(port int, body any) (httpapi.PairingState, error) {
	var st httpapi.PairingState
	host := fmt.Sprintf("127.0.0.1:%d", port)
	method, rd := http.MethodGet, io.Reader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		method, rd = http.MethodPost, strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "http://"+host+"/api/pair", rd)
	if err != nil {
		return st, err
	}
	req.Header.Set("X-Rocket-CSRF", "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return st, fmt.Errorf("no rocket answers on %s; start it with rocket ui or rocket recv (or pass --port)", host)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return st, fmt.Errorf("the rocket on %s is older and cannot pair; update it", host)
	}
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("unexpected answer from %s: %v", host, err)
	}
	return st, nil
}
