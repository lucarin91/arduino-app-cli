// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package editor_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/go-paths-helper"

	"github.com/arduino/arduino-app-cli/internal/editor"
)

type notif struct {
	Method string
	Params json.RawMessage
}

func dial(t *testing.T, root string) (*jsonrpc2.Conn, chan notif, func()) {
	t.Helper()
	root = paths.New(root).Canonical().String()
	h, err := editor.New(editor.Config{Root: root, MaxWatches: 16, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)

	ns := make(chan notif, 16)
	handler := jsonrpc2.HandlerWithError(func(_ context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
		if req.Notif && req.Params != nil {
			ns <- notif{Method: req.Method, Params: *req.Params}
		}
		return nil, nil
	})
	rpc := jsonrpc2.NewConn(context.Background(), wsStream{c}, handler)
	return rpc, ns, func() { _ = rpc.Close(); _ = c.Close(); srv.Close() }
}

// wsStream duplicates the tiny adapter from editor.go; kept here so tests
// don't need to import an unexported type.
type wsStream struct{ c *websocket.Conn }

func (s wsStream) WriteObject(o interface{}) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	return s.c.WriteMessage(websocket.TextMessage, b)
}

func (s wsStream) ReadObject(v interface{}) error {
	for {
		mt, d, err := s.c.ReadMessage()
		if err != nil {
			return err
		}
		if mt == websocket.TextMessage || mt == websocket.BinaryMessage {
			return json.Unmarshal(d, v)
		}
	}
}

func (s wsStream) Close() error { return s.c.Close() }

func TestServer_Hello(t *testing.T) {
	rpc, _, cleanup := dial(t, t.TempDir())
	defer cleanup()
	var res struct {
		Protocol     string   `json:"protocol"`
		Capabilities []string `json:"capabilities"`
		Limits       struct {
			MaxWatches int `json:"maxWatches"`
		} `json:"limits"`
	}
	require.NoError(t, rpc.Call(context.Background(), "hello", nil, &res))
	assert.Equal(t, "editor.v1", res.Protocol)
	assert.Contains(t, res.Capabilities, "fs.watch")
	assert.Equal(t, 16, res.Limits.MaxWatches)
}

func TestServer_WatchProducesNotification(t *testing.T) {
	root := paths.New(t.TempDir()).Canonical().String()
	rpc, notifs, cleanup := dial(t, root)
	defer cleanup()

	var wr struct {
		SubscriptionID string `json:"subscriptionId"`
	}
	require.NoError(t, rpc.Call(context.Background(), "fs.watch", map[string]any{
		"path": ".", "recursive": true, "debounceMs": 30,
	}, &wr))
	require.NotEmpty(t, wr.SubscriptionID)

	time.Sleep(80 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(root, "hello.txt"), []byte("x"), 0o600))

	select {
	case n := <-notifs:
		assert.Equal(t, "fs.changed", n.Method)
		var payload struct {
			SubscriptionID string `json:"subscriptionId"`
			Events         []struct {
				Type, Path string
			} `json:"events"`
		}
		require.NoError(t, json.Unmarshal(n.Params, &payload))
		assert.Equal(t, wr.SubscriptionID, payload.SubscriptionID)
		require.NotEmpty(t, payload.Events)
		assert.Equal(t, filepath.Join(root, "hello.txt"), payload.Events[0].Path)
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for fs.changed")
	}
}

func TestServer_UnknownSubscriptionErrors(t *testing.T) {
	rpc, _, cleanup := dial(t, t.TempDir())
	defer cleanup()
	var out struct{}
	err := rpc.Call(context.Background(), "fs.unwatch", map[string]any{"subscriptionId": "sub-nope"}, &out)
	require.Error(t, err)
	jerr, ok := err.(*jsonrpc2.Error)
	require.True(t, ok)
	assert.Equal(t, int64(-32003), jerr.Code)
}
