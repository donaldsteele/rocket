package receiver

import (
	"context"
	"testing"

	"github.com/donaldsteele/rocket/internal/transfer"
)

// A transfer cancelled here says where it was cancelled: "this computer" on
// desktop (the default), whatever Here names on Android.
func TestLedgerCancelNamesThisHost(t *testing.T) {
	for _, tc := range []struct{ here, want string }{
		{"", "transfer cancelled on this computer"},
		{"this device", "transfer cancelled on this device"},
	} {
		var got []Event
		l := &Ledger{Transfers: transfer.NewManager(), Events: func(e Event) { got = append(got, e) }, Here: tc.here}
		l.Transfers.Start(context.Background(), transfer.Info{ID: "t1", Name: "a.txt", Dir: "out"}, nil)
		if !l.Transfers.Cancel("t1") {
			t.Fatal("Cancel did not know the transfer")
		}
		_, err := l.End("t1", context.Canceled)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("Here %q: End = %v, want %q", tc.here, err, tc.want)
		}
		if len(got) != 1 || got[0].Type != "cancelled" {
			t.Fatalf("Here %q: events %+v", tc.here, got)
		}
	}
}
