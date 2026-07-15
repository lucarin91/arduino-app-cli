// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package editor

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/arduino/go-paths-helper"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
	"go.bug.st/f"
)

const defaultDebounce = 50 * time.Millisecond

type rawEvent struct {
	Op    fsnotify.Op
	Path  string
	IsDir bool
}

type watchSub struct {
	root, path *paths.Path
	includes   []string
	excludes   []string
	log        *slog.Logger

	fsw    *fsnotify.Watcher
	events chan []changeEvent
	errors chan error

	mu      sync.Mutex
	watched map[string]struct{}
}

func newWatchSub(ctx context.Context, root string, target *paths.Path, p watchParams, log *slog.Logger) (*watchSub, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	deb := time.Duration(p.DebounceMs) * time.Millisecond
	if deb <= 0 {
		deb = defaultDebounce
	}
	s := &watchSub{
		root: paths.New(root), path: target,
		includes: p.Includes, excludes: p.Excludes, log: log,
		fsw:    fw,
		events: make(chan []changeEvent, 16), errors: make(chan error, 4),
		watched: map[string]struct{}{},
	}
	switch {
	case !s.path.IsDir():
		err = s.addDir(s.path.Parent())
	case !p.Recursive:
		err = s.addDir(s.path)
	default:
		err = s.walkAdd(s.path, map[string]struct{}{}, nil)
	}
	if err != nil {
		_ = fw.Close()
		return nil, err
	}
	go s.loop(ctx, p.Recursive, deb)
	return s, nil
}

// walkAdd installs watches under dir (canonical, cycle-safe). When emit is
// non-nil it also appends synthesized Create events for every entry found —
// used to close the recursive-watch race on `mkdir -p ... && touch ...`.
func (s *watchSub) walkAdd(dir *paths.Path, visited map[string]struct{}, emit *[]rawEvent) error {
	canon := dir.Canonical()
	key := canon.String()
	if _, seen := visited[key]; seen {
		return nil
	}
	visited[key] = struct{}{}
	if inside, err := canon.IsInsideDir(s.root); err != nil {
		return nil
	} else if !inside && !canon.EquivalentTo(s.root) {
		return nil
	}
	if err := s.addDir(canon); err != nil {
		return err
	}
	entries, err := canon.ReadDir()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if emit != nil {
			*emit = append(*emit, rawEvent{Op: fsnotify.Create, Path: e.String(), IsDir: e.IsDir()})
		}
		if !e.IsDir() {
			continue
		}
		if rel, err := e.RelFrom(s.root); err == nil && !matchGlobs(filepath.ToSlash(rel.String()), nil, s.excludes) {
			continue
		}
		if err := s.walkAdd(e, visited, emit); err != nil {
			s.log.Debug("editor: walkAdd", slog.String("path", e.String()), slog.String("err", err.Error()))
		}
	}
	return nil
}

func (s *watchSub) addDir(dir *paths.Path) error {
	key := dir.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.watched[key]; ok {
		return nil
	}
	if err := s.fsw.Add(key); err != nil {
		return fmt.Errorf("watch %q: %w", key, err)
	}
	s.watched[key] = struct{}{}
	return nil
}

func (s *watchSub) loop(ctx context.Context, recursive bool, debounce time.Duration) {
	defer close(s.events)
	defer close(s.errors)

	var buf []rawEvent
	var timer *time.Timer
	timerC := func() <-chan time.Time {
		if timer == nil {
			return nil
		}
		return timer.C
	}
	flush := func() {
		if len(buf) == 0 {
			return
		}
		out := f.Filter(coalesce(s.root.String(), buf), func(e changeEvent) bool {
			return matchGlobs(e.Path, s.includes, s.excludes)
		})
		buf = buf[:0]
		if len(out) == 0 {
			return
		}
		select {
		case s.events <- out:
		case <-ctx.Done():
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-s.fsw.Events:
			if !ok {
				flush()
				return
			}
			isDir := paths.New(ev.Name).IsDir()
			buf = append(buf, rawEvent{Op: ev.Op, Path: ev.Name, IsDir: isDir})
			if recursive && isDir && ev.Op&fsnotify.Create != 0 {
				if err := s.walkAdd(paths.New(ev.Name), map[string]struct{}{}, &buf); err != nil {
					s.log.Debug("editor: recursive add", slog.String("path", ev.Name), slog.String("err", err.Error()))
				}
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				s.mu.Lock()
				if _, ok := s.watched[ev.Name]; ok {
					_ = s.fsw.Remove(ev.Name)
					delete(s.watched, ev.Name)
				}
				s.mu.Unlock()
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
			}
		case err, ok := <-s.fsw.Errors:
			if !ok {
				return
			}
			select {
			case s.errors <- err:
			default:
			}
		case <-timerC():
			timer = nil
			flush()
		}
	}
}

// coalesce collapses a debounce window: CREATE+WRITE→create, multi-WRITE→
// update, CREATE+REMOVE cancels; a lone remove + lone create in the same
// parent dir becomes a rename.
func coalesce(root string, batch []rawEvent) []changeEvent {
	type state struct{ created, written, removed, isDir bool }
	byPath := map[string]*state{}
	var order []string
	for _, e := range batch {
		st, ok := byPath[e.Path]
		if !ok {
			st = &state{isDir: e.IsDir}
			byPath[e.Path] = st
			order = append(order, e.Path)
		}
		if e.IsDir {
			st.isDir = true
		}
		switch {
		case e.Op&fsnotify.Create != 0:
			st.created = true
		case e.Op&fsnotify.Write != 0:
			st.written = true
		case e.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
			st.removed = true
		}
	}

	var rm, cr string
	rmN, crN := 0, 0
	for p, st := range byPath {
		switch {
		case st.removed && !st.created:
			rmN++
			rm = p
		case st.created && !st.removed:
			crN++
			cr = p
		}
	}
	renamePair := rmN == 1 && crN == 1 &&
		paths.New(rm).Parent().EquivalentTo(paths.New(cr).Parent())

	rootP := paths.New(root)
	out := make([]changeEvent, 0, len(order))
	rel := func(p string) (string, bool) {
		r, err := paths.New(p).RelFrom(rootP)
		if err != nil {
			return "", false
		}
		return filepath.ToSlash(r.String()), true
	}
	for _, p := range order {
		st := byPath[p]
		r, ok := rel(p)
		if !ok {
			continue
		}
		switch {
		case renamePair && p == cr:
			old, ok := rel(rm)
			if !ok {
				continue
			}
			out = append(out, changeEvent{Type: "rename", Path: r, OldPath: old, IsDir: st.isDir})
		case renamePair && p == rm:
		case st.removed && !st.created:
			out = append(out, changeEvent{Type: "delete", Path: r, IsDir: st.isDir})
		case st.created && st.removed:
		case st.created:
			out = append(out, changeEvent{Type: "create", Path: r, IsDir: st.isDir})
		case st.written:
			out = append(out, changeEvent{Type: "update", Path: r, IsDir: st.isDir})
		}
	}
	slices.SortStableFunc(out, func(a, b changeEvent) int {
		if a.Path < b.Path {
			return -1
		} else if a.Path > b.Path {
			return 1
		}
		return 0
	})
	return out
}

func matchGlobs(rel string, includes, excludes []string) bool {
	match := func(pat string) bool { ok, _ := doublestar.PathMatch(pat, rel); return ok }
	if slices.ContainsFunc(excludes, match) {
		return false
	}
	return len(includes) == 0 || slices.ContainsFunc(includes, match)
}
