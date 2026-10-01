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
// (nil func or nil result: no reply). It records whether the reply was written.
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
			resp := reply(req.ID)
			if resp == nil {
				continue // no reply for this request
			}
			replied.Store(true)
			_ = c.WriteJSON(resp)
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

func okReply(id json.RawMessage) map[string]interface{} {
	return map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"}
}

// multiplexKeysDialer: a failed subscribe (no reply / error reply) is an error,
// frees its pacing slot, and leaves the shared socket usable for the next one.
func TestMultiplexDialerSubscribeFailureThenReuse(t *testing.T) {
	cases := map[string]func(id json.RawMessage) map[string]interface{}{
		"timeout": nil, // first request unanswered
		"error": func(id json.RawMessage) map[string]interface{} {
			return map[string]interface{}{"jsonrpc": "2.0", "id": id,
				"error": map[string]interface{}{"code": 1, "message": "too many blocks back"}}
		},
	}
	for name, firstReply := range cases {
		t.Run(name, func(t *testing.T) {
			defer withReplyTimeout(300 * time.Millisecond)()
			var n atomic.Int32
			url, _ := replyNode(t, func(id json.RawMessage) map[string]interface{} {
				if n.Add(1) == 1 {
					if firstReply == nil {
						return nil
					}
					return firstReply(id)
				}
				return okReply(id)
			})
			sub, _, cleanup := newKeysFirehoseSub(t, nil)
			defer cleanup()
			sub.wsCtx = context.Background()
			defer sub.closeSharedAddrWS()
			in := &rpc.EventSubscriptionInput{FromAddress: newTestFelt(0xB)}

			sess, err := sub.multiplexKeysDialer(context.Background(), url, in)
			if err == nil {
				sess.close()
				t.Fatal("subscribe without a usable reply must fail")
			}
			if l := len(sub.addrSubSem); l != 0 {
				t.Fatalf("pacing slot not released after failed subscribe: %d held", l)
			}
			ws := sub.addrWS

			sess, err = sub.multiplexKeysDialer(context.Background(), url, in)
			if err != nil {
				t.Fatalf("shared socket unusable after a failed subscribe: %v", err)
			}
			defer sess.close()
			if sub.addrWS != ws {
				t.Error("shared socket was redialed instead of reused")
			}
		})
	}
}
