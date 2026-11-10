package runtime

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// startMergedEventSession serves events.subscribe on socket: it acknowledges,
// reports the requested pane ids, sends one status event for every requested
// pane and then holds the stream open until the caller closes it or end fires.
func startMergedEventSession(t *testing.T, socket string, requested chan<- []string, end <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buffer := make([]byte, 64*1024)
				n, _ := conn.Read(buffer)
				var request struct {
					ID     string `json:"id"`
					Method string `json:"method"`
					Params struct {
						Subscriptions []struct {
							PaneID string `json:"pane_id"`
						} `json:"subscriptions"`
					} `json:"params"`
				}
				if json.Unmarshal(buffer[:n], &request) != nil || request.Method != "events.subscribe" {
					return
				}
				var panes []string
				for _, subscription := range request.Params.Subscriptions {
					if subscription.PaneID != "" {
						panes = append(panes, subscription.PaneID)
					}
				}
				sort.Strings(panes)
				requested <- panes
				writeEventLine(conn, map[string]any{"id": request.ID, "result": map[string]any{"type": "subscription_started"}})
				for _, pane := range panes {
					writeEventLine(conn, map[string]any{
						"event": "pane.agent_status_changed",
						"data":  map[string]any{"pane_id": pane, "workspace_id": "w1", "agent_status": "done"},
					})
				}
				closed := make(chan struct{})
				go func() {
					_, _ = conn.Read(make([]byte, 1))
					close(closed)
				}()
				select {
				case <-end:
				case <-closed:
				}
			}(conn)
		}
	}()
}

func TestMergedEventsFanInEverySessionWithQualifiedPanes(t *testing.T) {
	configRoot := shortTestDir(t)
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootRequested, workRequested := make(chan []string, 4), make(chan []string, 4)
	rootEnd, workEnd := make(chan struct{}), make(chan struct{})
	startMergedEventSession(t, filepath.Join(configRoot, "herdr.sock"), rootRequested, rootEnd)
	startMergedEventSession(t, filepath.Join(configRoot, "sessions", "work", "herdr.sock"), workRequested, workEnd)
	h := NewHerdr(filepath.Join(configRoot, "herdr.sock"))
	h.ConfigRoot = configRoot
	merged := NewMerged(h)

	stream, err := merged.SubscribeEvents(context.Background(), DefaultSession(), EventSubscription{PaneIDs: []string{"w1:p1", "work::w1:p2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if got := <-rootRequested; len(got) != 1 || got[0] != "w1:p1" {
		t.Fatalf("default session subscribed %v", got)
	}
	if got := <-workRequested; len(got) != 1 || got[0] != "w1:p2" {
		t.Fatalf("named session must receive bare ids, got %v", got)
	}

	seen := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case event := <-stream.Events():
			seen[event.PaneID] = true
		case <-timeout:
			t.Fatalf("events seen %v", seen)
		}
	}
	if !seen["w1:p1"] || !seen["work::w1:p2"] {
		t.Fatalf("events must carry merged-view ids, got %v", seen)
	}

	// One session ending ends the merged stream so the caller resubscribes.
	close(workEnd)
	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the merged stream must end when one session's stream ends")
	}
	if _, open := <-stream.Events(); open {
		t.Fatal("Events must be closed once Done is")
	}
	close(rootEnd)
	if err := stream.Close(); err != nil || stream.Close() != nil {
		t.Fatal("Close must stay idempotent")
	}
}

func TestMergedEventsCloseStopsEverySession(t *testing.T) {
	configRoot := shortTestDir(t)
	requested := make(chan []string, 4)
	end := make(chan struct{})
	defer close(end)
	startMergedEventSession(t, filepath.Join(configRoot, "herdr.sock"), requested, end)
	h := NewHerdr(filepath.Join(configRoot, "herdr.sock"))
	h.ConfigRoot = configRoot
	stream, err := NewMerged(h).SubscribeEvents(context.Background(), DefaultSession(), EventSubscription{})
	if err != nil {
		t.Fatal(err)
	}
	<-requested
	_ = stream.Close()
	select {
	case <-stream.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Close must end the merged stream")
	}
	if stream.Err() != nil {
		t.Fatalf("a caller's Close is not a failure: %v", stream.Err())
	}
}

func TestMergedEventsFailWhenASessionCannotSubscribe(t *testing.T) {
	configRoot := shortTestDir(t)
	if err := os.MkdirAll(filepath.Join(configRoot, "sessions", "gone"), 0o700); err != nil {
		t.Fatal(err)
	}
	requested := make(chan []string, 4)
	end := make(chan struct{})
	defer close(end)
	startMergedEventSession(t, filepath.Join(configRoot, "herdr.sock"), requested, end)
	h := NewHerdr(filepath.Join(configRoot, "herdr.sock"))
	h.ConfigRoot = configRoot
	// A pane of a session that stopped since the last snapshot: subscribing
	// fails, the caller reconciles and drops it.
	_, err := NewMerged(h).SubscribeEvents(context.Background(), DefaultSession(), EventSubscription{PaneIDs: []string{"gone::w1:p1"}})
	if fault, ok := AsFault(err); !ok || fault.Code != CodeOffline {
		t.Fatalf("want offline for the stopped session, got %v", err)
	}
}
