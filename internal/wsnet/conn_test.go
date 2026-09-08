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
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"pairfob/internal/envelope"
)

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
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "pairfob-proxy-test.invalid:80" {
			http.Error(w, "unexpected proxy destination", http.StatusBadRequest)
			return
		}
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
}

func TestRecvRejectsTextWebSocketMessage(t *testing.T) {
	errs := make(chan error, 1)
	server := httptest.NewServer(httpHandler(func(ws *websocket.Conn) {
		_, err := Wrap(ws).RecvWithin(time.Second)
		errs <- err
	}))
	defer server.Close()

	client := dialTestWS(t, server.URL)
	defer client.Close()
	if err := client.WriteMessage(websocket.TextMessage, []byte("not binary")); err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "binary required") {
		t.Fatalf("Recv error = %v, want binary-required error", err)
	}
}

func TestConcurrentSendSerializesWebSocketWrites(t *testing.T) {
	const sends = 32
	done := make(chan struct{})
	server := httptest.NewServer(httpHandler(func(ws *websocket.Conn) {
		conn := Wrap(ws)
		var wg sync.WaitGroup
		wg.Add(sends)
		for i := 0; i < sends; i++ {
			go func(i int) {
				defer wg.Done()
				if err := conn.Send(envelope.JSON(envelope.TypPING, [16]byte{}, map[string]int{"i": i})); err != nil {
					t.Errorf("Send: %v", err)
				}
			}(i)
		}
		wg.Wait()
		close(done)
	}))
	defer server.Close()

	client := dialTestWS(t, server.URL)
	defer client.Close()
	for i := 0; i < sends; i++ {
		messageType, payload, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.BinaryMessage {
			t.Fatalf("message type = %d", messageType)
		}
		if _, err := envelope.Decode(payload); err != nil {
			t.Fatalf("Decode: %v", err)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent sends did not finish")
	}
}

func TestHasV1SubprotocolRequiresExplicitOffer(t *testing.T) {
	req := httptest.NewRequest("GET", "http://relay.test/v1/ws", nil)
	if HasV1Subprotocol(req) {
		t.Fatal("accepted missing subprotocol")
	}
	req.Header.Set("Sec-WebSocket-Protocol", "other, pairfob.v1")
	if !HasV1Subprotocol(req) {
		t.Fatal("did not find pairfob.v1 offer")
	}
}

func TestHasSubprotocolMatchesNamedOffer(t *testing.T) {
	req := httptest.NewRequest("GET", "http://origin.test/v2/ws", nil)
	if HasSubprotocol(req, SubprotocolV2) {
		t.Fatal("accepted missing subprotocol")
	}
	req.Header.Set("Sec-WebSocket-Protocol", "pairfob.v2")
	if !HasSubprotocol(req, SubprotocolV2) {
		t.Fatal("did not find pairfob.v2 offer")
	}
	if HasV1Subprotocol(req) {
		t.Fatal("v2 offer must not satisfy HasV1Subprotocol")
	}
}

func TestDialProtocolNegotiatesNamedSubprotocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !HasSubprotocol(r, SubprotocolV2) {
			http.Error(w, "pairfob.v2 required", http.StatusUpgradeRequired)
			return
		}
		upgrader := UpgraderFor(SubprotocolV2)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_, _, _ = ws.ReadMessage()
	}))
	defer server.Close()

	raw := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, err := DialProtocol(raw, SubprotocolV2)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := Dial(raw); err == nil {
		t.Fatal("pairfob.v1 Dial accepted a v2-only server")
	}
}

func TestCheckSameHostOrigin(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{name: "daemon without origin", want: true},
		{name: "same origin", origin: "https://relay.example:8443", want: true},
		{name: "cross origin", origin: "https://evil.example:8443", want: false},
		{name: "wrong port", origin: "https://relay.example", want: false},
		{name: "opaque origin", origin: "null", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "https://relay.example:8443/v1/ws", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if got := CheckSameHostOrigin(req); got != tt.want {
				t.Fatalf("CheckSameHostOrigin()=%v want=%v", got, tt.want)
			}
		})
	}
}

func httpHandler(handle func(*websocket.Conn)) *testWSHandler {
	return &testWSHandler{handle: handle}
}

type testWSHandler struct {
	handle func(*websocket.Conn)
}

func (h *testWSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := Upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	h.handle(ws)
}

func dialTestWS(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(serverURL, "http")
	dialer := websocket.Dialer{Subprotocols: []string{"pairfob.v1"}}
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
