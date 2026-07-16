// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Package editor exposes a WebSocket + JSON-RPC 2.0 endpoint for remote
// file-watching. Only Config and New are public and no other internal/...
// package is imported, so the module can be extracted later.
package editor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/gorilla/websocket"
	"github.com/sourcegraph/jsonrpc2"
)

const (
	protocolVersion = "editor.v1"
	serverName      = "arduino-app-cli editor"
	serverVersion   = "0.1.0"

	mHello, mWatch, mUnwatch  = "hello", "fs.watch", "fs.unwatch"
	nChanged, nWatchErr       = "fs.changed", "fs.watchError"
	pingInterval, pongTimeout = 30 * time.Second, 10 * time.Second

	codeWatchLimit    = -32002
	codeNotSubscribed = -32003
	codeWatcherFail   = -32004
)

type Config struct {
	MaxWatches  int
	Logger      *slog.Logger
	CheckOrigin func(*http.Request) bool // nil = accept any
}

func New(cfg Config) (http.Handler, error) {
	if cfg.Logger == nil {
		return nil, errors.New("editor: Logger required")
	}
	if cfg.MaxWatches <= 0 {
		cfg.MaxWatches = 1024
	}

	up := websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool { return cfg.CheckOrigin == nil || cfg.CheckOrigin(r) },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			cfg.Logger.Error("editor: upgrade failed", slog.String("err", err.Error()))
			return
		}
		(&session{cfg: cfg, conn: conn, subs: map[string]*subscription{}}).run(r.Context())
	}), nil
}

type session struct {
	cfg  Config
	conn *websocket.Conn
	rpc  *jsonrpc2.Conn
	mu   sync.Mutex
	subs map[string]*subscription
}

type subscription struct {
	id     string
	w      *watchSub
	cancel context.CancelFunc
}

func (s *session) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	_ = s.conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(pingInterval + pongTimeout))
	})
	go func() {
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pongTimeout)); err != nil {
					return
				}
			}
		}
	}()

	s.rpc = jsonrpc2.NewConn(ctx, wsStream{s.conn}, jsonrpc2.AsyncHandler(jsonrpc2.HandlerWithError(s.handle)))
	select {
	case <-ctx.Done():
	case <-s.rpc.DisconnectNotify():
	}

	s.mu.Lock()
	subs := s.subs
	s.subs = nil
	s.mu.Unlock()
	for _, sub := range subs {
		sub.cancel()
		_ = sub.w.fsw.Close()
	}
	_ = s.rpc.Close()
}

func (s *session) handle(ctx context.Context, _ *jsonrpc2.Conn, req *jsonrpc2.Request) (interface{}, error) {
	switch req.Method {
	case mHello:
		return map[string]any{
			"serverName": serverName, "serverVersion": serverVersion,
			"protocol": protocolVersion, "capabilities": []string{"fs.watch"},
			"limits": map[string]int{"maxWatches": s.cfg.MaxWatches},
		}, nil

	case mWatch:
		var p watchParams
		if req.Params != nil {
			if err := json.Unmarshal(*req.Params, &p); err != nil {
				return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: err.Error()}
			}
		}
		if p.Path == "" {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "path is required"}
		}
		target := paths.New(p.Path)
		if !target.IsAbs() {
			return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeInvalidParams, Message: "path must be absolute"}
		}
		s.mu.Lock()
		full := len(s.subs) >= s.cfg.MaxWatches
		s.mu.Unlock()
		if full {
			return nil, &jsonrpc2.Error{Code: codeWatchLimit, Message: "watch limit reached"}
		}

		subCtx, cancel := context.WithCancel(ctx)
		w, err := newWatchSub(subCtx, target, p, s.cfg.Logger)
		if err != nil {
			cancel()
			return nil, &jsonrpc2.Error{Code: codeWatcherFail, Message: err.Error()}
		}
		var idb [8]byte
		_, _ = rand.Read(idb[:])
		id := "sub-" + hex.EncodeToString(idb[:])
		sub := &subscription{id: id, w: w, cancel: cancel}
		s.mu.Lock()
		s.subs[id] = sub
		s.mu.Unlock()
		go s.pump(subCtx, sub)
		return map[string]string{"subscriptionId": id}, nil

	case mUnwatch:
		var p struct {
			SubscriptionID string `json:"subscriptionId"`
		}
		if req.Params != nil {
			_ = json.Unmarshal(*req.Params, &p)
		}
		s.mu.Lock()
		sub, ok := s.subs[p.SubscriptionID]
		delete(s.subs, p.SubscriptionID)
		s.mu.Unlock()
		if !ok {
			return nil, &jsonrpc2.Error{Code: codeNotSubscribed, Message: "unknown subscriptionId"}
		}
		sub.cancel()
		_ = sub.w.fsw.Close()
		return struct{}{}, nil
	}
	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: "unknown method: " + req.Method}
}

func (s *session) pump(ctx context.Context, sub *subscription) {
	defer func() {
		_ = sub.w.fsw.Close()
		s.mu.Lock()
		delete(s.subs, sub.id)
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case events, ok := <-sub.w.events:
			if !ok {
				return
			}
			_ = s.rpc.Notify(ctx, nChanged, map[string]any{"subscriptionId": sub.id, "events": events})
		case err, ok := <-sub.w.errors:
			if !ok {
				return
			}
			_ = s.rpc.Notify(ctx, nWatchErr, map[string]any{
				"subscriptionId": sub.id, "message": err.Error(), "fatal": false,
			})
		}
	}
}

type watchParams struct {
	Path       string   `json:"path"`
	Recursive  bool     `json:"recursive,omitempty"`
	Includes   []string `json:"includes,omitempty"`
	Excludes   []string `json:"excludes,omitempty"`
	DebounceMs int      `json:"debounceMs,omitempty"`
}

type changeEvent struct {
	Type    string      `json:"type"` // create|update|delete|rename
	Path    *paths.Path `json:"path"`
	IsDir   bool        `json:"isDir"`
	OldPath *paths.Path `json:"oldPath,omitempty"`
}

// wsStream adapts *websocket.Conn to jsonrpc2.ObjectStream (one JSON per text frame).
type wsStream struct{ c *websocket.Conn }

func (s wsStream) WriteObject(obj interface{}) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	return s.c.WriteMessage(websocket.TextMessage, b)
}

func (s wsStream) ReadObject(v interface{}) error {
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
