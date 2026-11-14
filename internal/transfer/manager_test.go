package transfer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
)

func TestManagerLifecycle(t *testing.T) {
	mg := NewManager()
	aborted := false
	ctx := mg.Start(context.Background(), Info{ID: "a", Dir: "in", Name: "x", Total: 10}, func() { aborted = true })
	mg.Start(context.Background(), Info{ID: "b", Dir: "out", State: StateWaiting, Started: time.Now().Add(time.Second)}, nil)

	list := mg.List()
	if len(list) != 2 || list[0].ID != "a" || list[0].State != StateActive || list[1].State != StateWaiting {
		t.Fatalf("list: %+v", list)
	}
	mg.Progress("b", Progress{Sent: 4, Total: 10, File: "f", FileIndex: 1, Files: 2})
	if b := mg.List()[1]; b.State != StateActive || b.Sent != 4 || b.Files != 2 || b.File != "f" {
		t.Fatalf("after progress: %+v", b)
	}

	if mg.Cancel("nope") {
		t.Fatal("cancelled an unknown transfer")
	}
	if !mg.Cancel("a") || !aborted || ctx.Err() == nil || !mg.Cancelled("a") {
		t.Fatalf("cancel: aborted=%v ctx=%v", aborted, ctx.Err())
	}
	info, cancelled := mg.Finish("a")
	if info.ID != "a" || info.Name != "x" || !cancelled {
		t.Fatalf("finish a: %+v %v", info, cancelled)
	}
	if _, cancelled := mg.Finish("b"); cancelled {
		t.Fatal("b was not cancelled")
	}
	if mg.Len() != 0 {
		t.Fatalf("%d left", mg.Len())
	}
}

func TestOutcome(t *testing.T) {
	cases := map[error]history.Result{
		nil:                                      history.ResultOK,
		fmt.Errorf("%w: no", ErrDeclined):        history.ResultDeclined,
		fmt.Errorf("x: %w", ErrTimedOut):         history.ResultTimeout,
		fmt.Errorf("%w by peer", ErrCancelled):   history.ResultCancelled,
		fmt.Errorf("stop: %w", context.Canceled): history.ResultCancelled,
		context.DeadlineExceeded:                 history.ResultTimeout,
		errors.New("disk full"):                  history.ResultError,
	}
	for err, want := range cases {
		if got := Outcome(err); got != want {
			t.Errorf("Outcome(%v) = %s, want %s", err, got, want)
		}
	}
}
