// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Package editorclient is a minimal JSON-RPC 2.0 client over WebSocket for
// the editor server. It is intentionally test-scoped: it uses testing.TB for
// fatal reporting and lives under internal/testtools so it can be shared
// across benchmarks and integration tests without leaking a client type into
// pkg/board.
package editorclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sourcegraph/jsonrpc2"
	"github.com/stretchr/testify/require"
)

// Client wraps a WebSocket + JSON-RPC 2.0 connection to the editor server.
type Client struct {
	conn *websocket.Conn
	rpc  *jsonrpc2.Conn
}

// Dial opens a WS connection to ws://addr/v1/edit and wraps it in a JSON-RPC
// client. The caller must invoke Close when done.
func Dial(tb testing.TB, addr string) *Client {
	tb.Helper()
	c, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/v1/edit", nil)
	require.NoError(tb, err)
	rpc := jsonrpc2.NewConn(context.Background(), wsStream{c: c}, nil)
	return &Client{conn: c, rpc: rpc}
}

// Close tears down the RPC and WS connections.
func (c *Client) Close() {
	_ = c.rpc.Close()
	_ = c.conn.Close()
}

// Walk calls fs.walk with the given params and returns the entries slice
// unchanged. Entries are returned as loosely-typed maps so callers can decide
// which fields they need without coupling to a shared wire struct.
func (c *Client) Walk(params map[string]any) ([]map[string]any, error) {
	var res struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := c.rpc.Call(context.Background(), "fs.walk", params, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}

// WaitReady polls the editor WS endpoint until the server accepts a
// connection or the 15s deadline expires. Fails the test on timeout.
func WaitReady(tb testing.TB, addr string) {
	tb.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/v1/edit", nil)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	tb.Fatalf("editor-server did not become ready at %s", addr)
}

// wsStream adapts *websocket.Conn to jsonrpc2.ObjectStream, matching the
// server's framing (one JSON message per text frame).
type wsStream struct{ c *websocket.Conn }

func (s wsStream) WriteObject(obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return s.c.WriteMessage(websocket.TextMessage, b)
}

func (s wsStream) ReadObject(v any) error {
	for {
		mt, data, err := s.c.ReadMessage()
		if err != nil {
			return err
		}
		if mt == websocket.TextMessage || mt == websocket.BinaryMessage {
			return json.Unmarshal(data, v)
		}
	}
}

func (s wsStream) Close() error { return s.c.Close() }
