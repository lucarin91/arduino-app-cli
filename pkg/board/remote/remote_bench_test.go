// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package remote_test

import (
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/testtools"
	"github.com/arduino/arduino-app-cli/pkg/board/remote"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/adb"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/local"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/ssh"
	"github.com/arduino/arduino-app-cli/pkg/board/remotefs"
)

// connFactory builds a RemoteConn for each backend.
type connFactory struct {
	name     string
	basePath string
	build    func(tb testing.TB) remote.RemoteConn
}

func fsBackends(tb testing.TB) []connFactory {
	tb.Helper()
	name, adbPort, sshPort := testtools.StartAdbDContainer(tb)
	tb.Cleanup(func() { testtools.StopAdbDContainer(tb, name) })

	tmp := tb.TempDir()
	return []connFactory{
		{
			name:     "adb",
			basePath: "/home/arduino",
			build: func(tb testing.TB) remote.RemoteConn {
				c, err := adb.FromHost("localhost:"+adbPort, "")
				require.NoError(tb, err)
				return c
			},
		},
		{
			name:     "ssh",
			basePath: "./",
			build: func(tb testing.TB) remote.RemoteConn {
				c, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
				require.NoError(tb, err)
				return c
			},
		},
		{
			name:     "local",
			basePath: tmp,
			build: func(tb testing.TB) remote.RemoteConn {
				return &local.LocalConnection{}
			},
		},
	}
}

const benchFileSize = 4 * 1024 // 4KB

// BenchmarkRemoteWrite measures WriteFile latency for a single file, a few
// files, and many files over one persistent connection per backend.
func BenchmarkRemoteWrite(b *testing.B) {
	for _, be := range fsBackends(b) {
		b.Run(be.name, func(b *testing.B) {
			srcDir := b.TempDir()

			conn := be.build(b)
			remoteBase := path.Join(be.basePath, "bench_write_"+be.name)
			_ = conn.Remove(remoteBase)
			require.NoError(b, conn.MkDirAll(remoteBase))
			b.Cleanup(func() { _ = conn.Remove(remoteBase) })

			run := func(b *testing.B, srcs []string) {
				b.SetBytes(int64(benchFileSize * len(srcs)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for j, s := range srcs {
						in, err := os.Open(s)
						require.NoError(b, err)
						dst := path.Join(remoteBase, fmt.Sprintf("file_%d_%d", i, j))
						err = conn.WriteFile(in, dst)
						require.NoError(b, err)
						in.Close()
					}
				}
			}

			b.Run("Single", func(b *testing.B) {
				run(b, seedLocalFiles(b, srcDir, 1))
			})
			b.Run("Few", func(b *testing.B) {
				run(b, seedLocalFiles(b, srcDir, 5))
			})
			b.Run("Many", func(b *testing.B) {
				run(b, seedLocalFiles(b, srcDir, 100))
			})
		})
	}
}

var Sink []byte

// BenchmarkRemoteRead mirrors BenchmarkRemoteWrite for ReadFile.
func BenchmarkRemoteRead(b *testing.B) {
	for _, be := range fsBackends(b) {
		b.Run(be.name, func(b *testing.B) {
			conn := be.build(b)
			remoteBase := path.Join(be.basePath, "bench_read_"+be.name)
			_ = conn.Remove(remoteBase)
			require.NoError(b, conn.MkDirAll(remoteBase))
			b.Cleanup(func() { _ = conn.Remove(remoteBase) })

			run := func(b *testing.B, paths []string) {
				b.SetBytes(int64(benchFileSize * len(paths)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for _, p := range paths {
						rc, err := conn.ReadFile(p)
						require.NoError(b, err)
						Sink, err = io.ReadAll(rc)
						require.NoError(b, err)
						rc.Close()
					}
				}
			}

			b.Run("Single", func(b *testing.B) {
				run(b, seedRemoteFiles(b, conn, remoteBase, 1))
			})
			b.Run("Few", func(b *testing.B) {
				run(b, seedRemoteFiles(b, conn, remoteBase, 5))
			})
			b.Run("Many", func(b *testing.B) {
				run(b, seedRemoteFiles(b, conn, remoteBase, 100))
			})
		})
	}
}

// writeRandom writes size bytes of random data to w.
func writeRandom(w io.Writer, size int) error {
	n, err := io.CopyN(w, rand.Reader, int64(size))
	if err != nil {
		return fmt.Errorf("failed to write random data: %w", err)
	}
	if n != int64(size) {
		return fmt.Errorf("unexpected number of bytes written: got %d, want %d", n, size)
	}
	return nil
}

// seedLocalFiles creates n random files of benchFileSize under dir with the
// given name prefix and returns their local paths.
func seedLocalFiles(tb testing.TB, dir string, n int) []string {
	tb.Helper()
	files := make([]string, 0, n)
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("files_%04d.bin", i))
		f, err := os.Create(p)
		require.NoError(tb, err)
		err = writeRandom(f, benchFileSize)
		require.NoError(tb, err)
		require.NoError(tb, f.Close())
		files = append(files, p)
	}
	return files
}

// seedRemoteFiles streams n random files of benchFileSize directly to the
// remote under remoteBase and returns their remote paths.
func seedRemoteFiles(tb testing.TB, conn remote.RemoteConn, remoteBase string, n int) []string {
	tb.Helper()
	files := make([]string, 0, n)
	for i := range n {
		p := path.Join(remoteBase, fmt.Sprintf("files_%04d.bin", i))
		r, w := io.Pipe()
		go func() {
			w.CloseWithError(writeRandom(w, benchFileSize))
		}()
		require.NoError(tb, conn.WriteFile(r, p))
		files = append(files, p)
	}
	return files
}

var SinkFiles, SinkDirs int

// BenchmarkRemoteWalk measures fs.WalkDir latency over a remote directory
// tree exposed via remotefs.RemoteFS, for a few tree shapes.
func BenchmarkRemoteWalk(b *testing.B) {
	for _, be := range fsBackends(b) {
		b.Run(be.name, func(b *testing.B) {
			conn := be.build(b)
			remoteBase := path.Join(be.basePath, "bench_walk_"+be.name)
			_ = conn.Remove(remoteBase)
			require.NoError(b, conn.MkDirAll(remoteBase))
			b.Cleanup(func() { _ = conn.Remove(remoteBase) })

			rfs := remotefs.New(remoteBase, conn)

			run := func(b *testing.B, root string, files, dirs int) {
				b.SetBytes(int64(benchFileSize * files))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					SinkFiles, SinkDirs = 0, 0
					err := fs.WalkDir(rfs, root, func(_ string, d fs.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if d.IsDir() {
							SinkDirs++
						} else {
							SinkFiles++
						}
						return nil
					})
					require.NoError(b, err)
					require.Equal(b, files, SinkFiles)
					require.Equal(b, dirs, SinkDirs)
				}
			}

			b.Run("Flat", func(b *testing.B) {
				files, dirs := seedRemoteTree(b, conn, path.Join(remoteBase, "flat"), 1, 100)
				run(b, "flat", files, dirs)
			})
			b.Run("Deep", func(b *testing.B) {
				files, dirs := seedRemoteTree(b, conn, path.Join(remoteBase, "deep"), 5, 2)
				run(b, "deep", files, dirs)
			})
		})
	}
}

// seedRemoteTree creates a directory tree rooted at root with the given depth
// and branching factor. At every level it creates `branch` subdirectories and
// `branch` files. Returns the root, total number of files and total number of
// directories (including the root).
func seedRemoteTree(tb testing.TB, conn remote.RemoteConn, root string, depth, branch int) (int, int) {
	tb.Helper()
	require.NoError(tb, conn.MkDirAll(root))

	totalFiles, totalDirs := 0, 1
	var build func(dir string, d int)
	build = func(dir string, d int) {
		for i := range branch {
			p := path.Join(dir, fmt.Sprintf("file_%04d.bin", i))
			r, w := io.Pipe()
			go func() { w.CloseWithError(writeRandom(w, benchFileSize)) }()
			require.NoError(tb, conn.WriteFile(r, p))
			totalFiles++
		}
		if d == 0 {
			return
		}
		for i := range branch {
			sub := path.Join(dir, fmt.Sprintf("dir_%04d", i))
			require.NoError(tb, conn.MkDirAll(sub))
			totalDirs++
			build(sub, d-1)
		}
	}
	build(root, depth-1)
	return totalFiles, totalDirs
}
