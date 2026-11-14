package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/node"
)

func connect(t *testing.T, svc *Service) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(svc, Options{}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func testService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	id, err := node.Load(dir, "agent-host")
	if err != nil {
		t.Fatal(err)
	}
	h, err := history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{ID: id, Reg: discovery.New(discovery.Self{Name: "agent-host", Fingerprint: id.Fingerprint()}, 29199), History: h}
}

// TestToolAuthority pins the exact tool set and checks that no tool lets an
// agent accept, trust, revoke, change settings, cancel or quit.
func TestToolAuthority(t *testing.T) {
	cs := connect(t, testService(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		low := strings.ToLower(tool.Name)
		for _, bad := range []string{"accept", "trust", "revoke", "setting", "quit", "approve", "cancel", "clear"} {
			if strings.Contains(low, bad) {
				t.Errorf("tool %q grants authority agents must not have", tool.Name)
			}
		}
	}
	want := slices.Clone(ToolNames)
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("tools %v, want exactly %v", names, want)
	}
}

func TestListHistory(t *testing.T) {
	svc := testService(t)
	for i, r := range []history.Result{history.ResultOK, history.ResultDeclined, history.ResultCancelled} {
		if err := svc.History.Add(history.Entry{ID: string(rune('a' + i)), Dir: history.DirOut, Name: "f", PeerName: "bob", Result: r, Started: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	cs := connect(t, svc)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_history", Arguments: map[string]any{"limit": 2}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out listHistoryOut
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Transfers) != 2 || out.Transfers[0].Result != history.ResultCancelled || out.Transfers[1].Result != history.ResultDeclined {
		t.Fatalf("list_history: %s", b)
	}
}
