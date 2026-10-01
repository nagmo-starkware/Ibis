package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NethermindEth/starknet.go/rpc"
	"github.com/gorilla/websocket"
)

// replyNode is a WSS server answering starknet_subscribeEvents via reply
// (nil: never). It records whether the reply was written.
func replyNode(t *testing.T, reply func(id json.RawMessage) map[string]interface{}) (url string, replied *atomic.Bool) {
	t.Helper()
	replied = &atomic.Bool{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(msg, &req) != nil || reply == nil {
				continue
			}
			time.Sleep(100 * time.Millisecond)
			replied.Store(true)
			_ = c.WriteJSON(reply(req.ID))
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), replied
}

func withReplyTimeout(d time.Duration) func() {
	old := subscribeReplyTimeout
	subscribeReplyTimeout = d
	return func() { subscribeReplyTimeout = old }
}

// The dial returns only after the node's reply carrying the subscription id.
func TestDefaultDialerWaitsForSubscribeReply(t *testing.T) {
	defer withReplyTimeout(5 * time.Second)()
	url, replied := replyNode(t, func(id json.RawMessage) map[string]interface{} {
		return map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"}
	})
	sess, err := defaultWSSDialer(context.Background(), url, &rpc.EventSubscriptionInput{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer sess.close()
	if !replied.Load() {
		t.Fatal("dial returned before the node replied to the subscribe")
	}
}

// No reply within subscribeReplyTimeout: a failed dial, not an active session.
func TestDefaultDialerSubscribeReplyTimeout(t *testing.T) {
	defer withReplyTimeout(300 * time.Millisecond)()
	url, _ := replyNode(t, nil)
	start := time.Now()
	sess, err := defaultWSSDialer(context.Background(), url, &rpc.EventSubscriptionInput{})
	if err == nil {
		sess.close()
		t.Fatal("dial succeeded without a subscribe reply")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("dial took %v, want ~%v", d, subscribeReplyTimeout)
	}
}

// A JSON-RPC error reply is a failed dial.
func TestDefaultDialerSubscribeErrorReply(t *testing.T) {
	defer withReplyTimeout(5 * time.Second)()
	url, _ := replyNode(t, func(id json.RawMessage) map[string]interface{} {
		return map[string]interface{}{"jsonrpc": "2.0", "id": id,
			"error": map[string]interface{}{"code": 1, "message": "too many blocks back"}}
	})
	if sess, err := defaultWSSDialer(context.Background(), url, &rpc.EventSubscriptionInput{}); err == nil {
		sess.close()
		t.Fatal("dial succeeded on a JSON-RPC error reply")
	}
}
