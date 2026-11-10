package wsnet

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pairfob/internal/envelope"
)

// The relay connection goes through the proxy named by the environment: the
// proxy sees a CONNECT to the relay's host and the frame still arrives.
func TestDialProtocolUsesConfiguredProxy(t *testing.T) {
	// ProxyFromEnvironment caches its environment; isolate this dial from other tests.
	if os.Getenv("PAIRFOB_PROXY_TEST_CHILD") == "1" {
		conn, err := DialProtocol("ws://pairfob-proxy-test.invalid/v2/ws", SubprotocolV2)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.RecvWithin(time.Second); err != nil {
			t.Fatal(err)
		}
		return
	}
	upgrader := UpgraderFor(SubprotocolV2)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the tunnelled dial is under test. A stray request (another
		// process reaching a reused loopback port) is refused, not a failure.
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "not a websocket request", http.StatusBadRequest)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close()
		if err := Wrap(ws).Send(envelope.JSON(envelope.TypPING, [16]byte{}, map[string]int{"n": 1})); err != nil {
			t.Error(err)
		}
	}))
	defer relay.Close()
	var tunnelled atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "pairfob-proxy-test.invalid:80" {
			http.Error(w, "unexpected proxy destination", http.StatusBadRequest)
			return
		}
		tunnelled.Store(true)
		upstream, err := net.DialTimeout("tcp", strings.TrimPrefix(relay.URL, "http://"), time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go func() { _, _ = io.Copy(upstream, buffered) }()
		_, _ = io.Copy(client, upstream)
	}))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDialProtocolUsesConfiguredProxy$")
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !strings.HasSuffix(strings.ToUpper(key), "_PROXY") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "PAIRFOB_PROXY_TEST_CHILD=1", "HTTP_PROXY="+proxy.URL, "NO_PROXY=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy round trip failed: %v\n%s", err, output)
	}
	if !tunnelled.Load() {
		t.Fatal("the relay dial did not go through the proxy")
	}
}
