// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package editor

import (
	"os"
	"testing"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/editor/rootpath"
)

// mkPath returns a *rootpath.Path anchored at root. Fails the test on error.
func mkPath(t *testing.T, root *os.Root, rel string) *rootpath.Path {
	t.Helper()
	p, err := rootpath.New(root, rel)
	require.NoError(t, err)
	return p
}

func TestCoalesce(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	p := func(rel string) *rootpath.Path { return mkPath(t, root, rel) }

	tests := []struct {
		name string
		in   []rawEvent
		want []changeEvent
	}{
		{
			"create+writes -> create",
			[]rawEvent{
				{Op: fsnotify.Create, Path: p("a")},
				{Op: fsnotify.Write, Path: p("a")},
				{Op: fsnotify.Write, Path: p("a")},
			},
			[]changeEvent{{Type: "create", Path: p("a")}},
		},
		{
			"writes -> update",
			[]rawEvent{
				{Op: fsnotify.Write, Path: p("a")},
				{Op: fsnotify.Write, Path: p("a")},
			},
			[]changeEvent{{Type: "update", Path: p("a")}},
		},
		{
			"create+remove cancels",
			[]rawEvent{
				{Op: fsnotify.Create, Path: p("tmp")},
				{Op: fsnotify.Remove, Path: p("tmp")},
			},
			nil,
		},
		{
			"rename pairing",
			[]rawEvent{
				{Op: fsnotify.Rename, Path: p("a")},
				{Op: fsnotify.Create, Path: p("b")},
			},
			[]changeEvent{{Type: "rename", Path: p("b"), OldPath: p("a")}},
		},
		{
			"dir rename with contents pairs and drops descendants",
			[]rawEvent{
				{Op: fsnotify.Rename, Path: p("py"), IsDir: true},
				{Op: fsnotify.Create, Path: p("py2"), IsDir: true},
				{Op: fsnotify.Create, Path: p("py2/main.py")},
			},
			[]changeEvent{{Type: "rename", Path: p("py2"), OldPath: p("py"), IsDir: true}},
		},
		{
			"unpaired delete+create in different dirs stay separate",
			[]rawEvent{
				{Op: fsnotify.Remove, Path: p("a")},
				{Op: fsnotify.Create, Path: p("sub/b")},
			},
			[]changeEvent{
				{Type: "delete", Path: p("a")},
				{Type: "create", Path: p("sub/b")},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := coalesce(tc.in)
			if tc.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMatchGlobs(t *testing.T) {
	assert.False(t, matchGlobs("node_modules/foo/bar.js", nil, []string{"node_modules/**"}))
	assert.True(t, matchGlobs("src/app.ts", nil, []string{"node_modules/**"}))
	assert.True(t, matchGlobs("src/app.ts", []string{"**/*.ts"}, nil))
	assert.False(t, matchGlobs("src/app.js", []string{"**/*.ts"}, nil))
}
