// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

// Package rootpath is a jailed filesystem-path helper. Every Path is bound to
// an *os.Root; all filesystem operations go through that root and cannot
// escape it (no absolute paths, no "..", no symlinks pointing outside).
//
// The API mirrors github.com/arduino/go-paths-helper (paths.Path / PathList)
// method names and shapes on purpose, so it can be proposed upstream later —
// either as a sibling type or, if the upstream is willing, as one of several
// implementations behind a shared Path interface.
//
// Wire format: Path.String() returns the relative slash-separated path (POSIX
// style), which is exactly what the editor protocol carries on the wire.
package rootpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// ErrOutsideRoot is returned when a supplied path is absolute, contains ".."
// escapes, or resolves via symlink to a target outside the root.
var ErrOutsideRoot = errors.New("path outside root")

// ErrDifferentRoot is returned by cross-path operations (Rename, RelFrom)
// when the two paths are bound to different roots.
var ErrDifferentRoot = errors.New("paths bound to different roots")

// Path is a relative path anchored to an *os.Root. The zero value is not
// usable; construct with New or RootOf.
type Path struct {
	root *os.Root
	rel  string // slash-separated, cleaned, never absolute, never leading ".."; "." means the root itself
}

// RootOf returns the Path representing the root directory itself.
func RootOf(root *os.Root) *Path {
	return &Path{root: root, rel: "."}
}

// New validates rel and returns a Path anchored at root. rel must be relative,
// slash-separated, and stay within the root ("../" escapes are refused).
// Returns ErrOutsideRoot on validation failure.
func New(root *os.Root, rel string) (*Path, error) {
	if root == nil {
		return nil, errors.New("rootpath: nil root")
	}
	cleaned, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	return &Path{root: root, rel: cleaned}, nil
}

// FromOSPath converts an OS-absolute path emitted by an external source
// (e.g. fsnotify events) back into a Path anchored at root. Returns
// ErrOutsideRoot if the OS path is not under root.Name().
func FromOSPath(root *os.Root, osPath string) (*Path, error) {
	if root == nil {
		return nil, errors.New("rootpath: nil root")
	}
	rel, err := filepath.Rel(root.Name(), osPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOutsideRoot, err)
	}
	cleaned, err := cleanRel(filepath.ToSlash(rel))
	if err != nil {
		return nil, err
	}
	return &Path{root: root, rel: cleaned}, nil
}

func cleanRel(rel string) (string, error) {
	if rel == "" {
		return ".", nil
	}
	if filepath.IsAbs(rel) || path.IsAbs(rel) {
		return "", fmt.Errorf("%w: absolute path %q", ErrOutsideRoot, rel)
	}
	cleaned := path.Clean(rel)
	if !fs.ValidPath(cleaned) {
		return "", fmt.Errorf("%w: invalid path %q", ErrOutsideRoot, rel)
	}
	return cleaned, nil
}

// String returns the cleaned relative slash-separated path. "." denotes the
// root itself.
func (p *Path) String() string { return p.rel }

// MarshalJSON emits the same string String() returns.
func (p *Path) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.rel + `"`), nil
}

// Root returns the *os.Root this path is bound to.
func (p *Path) Root() *os.Root { return p.root }

// OSPath returns the OS-absolute path for interop with APIs that don't
// understand *os.Root (e.g. fsnotify.Watcher.Add). This is the only sanctioned
// way to obtain the absolute path — use sparingly.
func (p *Path) OSPath() string {
	return filepath.Join(p.root.Name(), filepath.FromSlash(p.rel))
}

// IsRoot reports whether this path is the root directory itself.
func (p *Path) IsRoot() bool { return p.rel == "." }

// Base returns the last path element.
func (p *Path) Base() string { return path.Base(p.rel) }

// Ext returns the extension including the leading dot.
func (p *Path) Ext() string { return path.Ext(p.rel) }

// Parent returns the parent directory. Parent of the root is the root itself.
func (p *Path) Parent() *Path {
	if p.rel == "." {
		return p
	}
	return &Path{root: p.root, rel: path.Dir(p.rel)}
}

// Join appends elements. The result must stay inside the root.
func (p *Path) Join(elems ...string) (*Path, error) {
	joined := path.Join(append([]string{p.rel}, elems...)...)
	cleaned, err := cleanRel(joined)
	if err != nil {
		return nil, err
	}
	return &Path{root: p.root, rel: cleaned}, nil
}

// RelFrom returns the path of p relative to base. Both must share the same
// root, otherwise ErrDifferentRoot.
func (p *Path) RelFrom(base *Path) (string, error) {
	if p.root != base.root {
		return "", ErrDifferentRoot
	}
	rel, err := filepath.Rel(base.rel, p.rel)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// EquivalentTo reports whether two paths refer to the same location under the
// same root. Pure string equality on the cleaned form; no filesystem lookup.
func (p *Path) EquivalentTo(other *Path) bool {
	return p.root == other.root && p.rel == other.rel
}

// IsInsideDir reports whether p is inside dir (strict — dir itself is not
// inside itself). Both must share the same root.
func (p *Path) IsInsideDir(dir *Path) bool {
	if p.root != dir.root {
		return false
	}
	if dir.rel == "." {
		return p.rel != "."
	}
	return len(p.rel) > len(dir.rel) &&
		p.rel[:len(dir.rel)] == dir.rel &&
		p.rel[len(dir.rel)] == '/'
}

// --- Filesystem I/O (all via *os.Root) ---

func (p *Path) osRel() string { return filepath.FromSlash(p.rel) }

func (p *Path) Stat() (fs.FileInfo, error)  { return p.root.Stat(p.osRel()) }
func (p *Path) Lstat() (fs.FileInfo, error) { return p.root.Lstat(p.osRel()) }

func (p *Path) Exist() bool {
	_, err := p.Stat()
	return err == nil
}

func (p *Path) IsDir() bool {
	fi, err := p.Stat()
	return err == nil && fi.IsDir()
}

func (p *Path) Open() (*os.File, error) { return p.root.Open(p.osRel()) }

func (p *Path) OpenFile(flag int, perm fs.FileMode) (*os.File, error) {
	return p.root.OpenFile(p.osRel(), flag, perm)
}

func (p *Path) Create() (*os.File, error)         { return p.root.Create(p.osRel()) }
func (p *Path) Mkdir(perm fs.FileMode) error      { return p.root.Mkdir(p.osRel(), perm) }
func (p *Path) MkdirAll(perm fs.FileMode) error   { return p.root.MkdirAll(p.osRel(), perm) }
func (p *Path) Remove() error                     { return p.root.Remove(p.osRel()) }
func (p *Path) RemoveAll() error                  { return p.root.RemoveAll(p.osRel()) }
func (p *Path) ReadFile() ([]byte, error)         { return p.root.ReadFile(p.osRel()) }
func (p *Path) WriteFile(data []byte, perm fs.FileMode) error {
	return p.root.WriteFile(p.osRel(), data, perm)
}

// Rename wraps *os.Root.Rename. Both paths must share the same root.
func (p *Path) Rename(to *Path) error {
	if p.root != to.root {
		return ErrDifferentRoot
	}
	return p.root.Rename(p.osRel(), to.osRel())
}

// ReadDir lists the directory and returns entries as Paths under the same
// root.
func (p *Path) ReadDir() (PathList, error) {
	f, err := p.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make(PathList, 0, len(entries))
	for _, e := range entries {
		child, err := p.Join(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, child)
	}
	return out, nil
}
