// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package remote_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/testtools"
	"github.com/arduino/arduino-app-cli/internal/testtools/editorclient"
	"github.com/arduino/arduino-app-cli/pkg/board/remote"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/adb"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/local"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/ssh"
	"github.com/arduino/arduino-app-cli/pkg/board/remotefs"
)

func BenchmarkRemotePush(b *testing.B) {
	b.Run("Native", func(b *testing.B) {
		runPushBenchmark(b, func(ctx context.Context, conn remote.RemoteConn, src, dst string) error {
			return conn.Push(ctx, src, dst)
		})
	})

	b.Run("Base", func(b *testing.B) {
		runPushBenchmark(b, func(ctx context.Context, conn remote.RemoteConn, src, dst string) error {
			return fsWalkPush(conn, src, dst)
		})
	})
	b.Run("Legacy", func(b *testing.B) {
		runPushBenchmark(b, legacyPush)
	})
}

// runPushBenchmark runs the given backends against the standard payload set.
func runPushBenchmark(b *testing.B, pushFunc func(ctx context.Context, conn remote.RemoteConn, src, dst string) error) {
	name, adbPort, sshPort := testtools.StartAdbDContainer(b)
	b.Cleanup(func() { testtools.StopAdbDContainer(b, name) })

	backends := []struct {
		name     string
		conn     remote.RemoteConn
		basePath string
	}{{
		"adb",
		func() remote.RemoteConn {
			conn, err := adb.FromHost("localhost:"+adbPort, "")
			require.NoError(b, err)
			return conn
		}(),
		"/home/arduino",
	}, {
		"ssh",
		func() remote.RemoteConn {
			conn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
			require.NoError(b, err)
			return conn
		}(),
		"./",
	}, {
		"local",
		func() remote.RemoteConn {
			return &local.LocalConnection{}
		}(),
		b.TempDir(),
	},
	}

	payloads := []struct {
		name  string
		build func(tb testing.TB, dir string)
	}{
		{
			// Many small files: 500 files of 1 KiB each.
			name: "ManySmallFiles",
			build: func(tb testing.TB, dir string) {
				const n = 500
				const size = 1 * 1024
				for i := range n {
					writeRandomFile(tb, filepath.Join(dir, fmt.Sprintf("small_%04d.bin", i)), size)
				}
			},
		},
		{
			// Few big files: 3 files of 8 MiB each.
			name: "FewBigFiles",
			build: func(tb testing.TB, dir string) {
				const n = 3
				const size = 8 * 1024 * 1024
				for i := range n {
					writeRandomFile(tb, filepath.Join(dir, fmt.Sprintf("big_%d.bin", i)), size)
				}
			},
		},
		{
			// Mixed: 100 small (2 KiB), 5 medium (256 KiB), 1 big (4 MiB), nested.
			name: "Mixed",
			build: func(tb testing.TB, dir string) {
				for i := range 100 {
					writeRandomFile(tb, filepath.Join(dir, "small", fmt.Sprintf("s_%03d.bin", i)), 2*1024)
				}
				for i := range 5 {
					writeRandomFile(tb, filepath.Join(dir, "medium", fmt.Sprintf("m_%d.bin", i)), 256*1024)
				}
				writeRandomFile(tb, filepath.Join(dir, "big", "b_0.bin"), 4*1024*1024)
			},
		},
	}

	for _, be := range backends {
		b.Run(be.name, func(b *testing.B) {
			for _, p := range payloads {
				b.Run(p.name, func(b *testing.B) {
					// Build payload once per case (outside the timed loop).
					srcDir := b.TempDir()
					payloadDir := filepath.Join(srcDir, "payload")
					require.NoError(b, os.MkdirAll(payloadDir, 0755))
					p.build(b, payloadDir)

					// Compute total bytes for throughput reporting.
					var totalBytes int64
					err := filepath.Walk(payloadDir, func(_ string, info os.FileInfo, err error) error {
						if err != nil {
							return err
						}
						if !info.IsDir() {
							totalBytes += info.Size()
						}
						return nil
					})
					require.NoError(b, err)

					// Prepare remote destination base dir.
					remoteBase := path.Join(be.basePath, "bench_push")
					_ = be.conn.Remove(remoteBase)
					require.NoError(b, be.conn.MkDirAll(remoteBase))
					b.Cleanup(func() { _ = be.conn.Remove(remoteBase) })

					b.SetBytes(totalBytes)
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						dst := path.Join(be.basePath, fmt.Sprintf("iter_%d", i))
						if err := pushFunc(context.Background(), be.conn, payloadDir, dst); err != nil {
							b.Fatalf("push failed: %v", err)
						}
					}

				})
			}
		})
	}
}

// writeRandomFile creates a file at p filled with size bytes of random data.
func writeRandomFile(tb testing.TB, p string, size int) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(filepath.Dir(p), 0755))
	f, err := os.Create(p)
	require.NoError(tb, err)
	defer f.Close()
	if size == 0 {
		return
	}
	buf := make([]byte, 64*1024)
	remaining := size
	for remaining > 0 {
		n := min(len(buf), remaining)
		_, err := rand.Read(buf[:n])
		require.NoError(tb, err)
		_, err = f.Write(buf[:n])
		require.NoError(tb, err)
		remaining -= n
	}
}

// fsWalkPush replicates the tree on the remote using only the remote.FS
// interface (MkDirAll + WriteFile), without using Push.
func fsWalkPush(rfs remote.FS, src, dst string) error {
	srcFS := os.DirFS(src)
	return fs.WalkDir(srcFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Use forward slashes for the remote path; p is already slash-separated.
		target := path.Join(dst, p)
		if d.IsDir() {
			return rfs.MkDirAll(target)
		}
		if err := rfs.MkDirAll(path.Dir(target)); err != nil {
			return err
		}
		in, err := srcFS.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		return rfs.WriteFile(in, target)
	})
}

// legacyPush replicates the old ImportFolderToAppFromPath algorithm, kept
// for benchmarking.
func legacyPush(ctx context.Context, conn remote.RemoteConn, src, dst string) error {
	const maxConcurrentUploads = 8

	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type fileEntry struct {
		localPath  string
		remotePath string
	}
	var dirs []string
	var files []fileEntry

	if err := filepath.WalkDir(src, func(currentLocalPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(src, currentLocalPath)
		if err != nil {
			return err
		}
		remoteEntryPath := path.Join(dst, relPath)
		if d.IsDir() {
			dirs = append(dirs, remoteEntryPath)
		} else {
			files = append(files, fileEntry{localPath: currentLocalPath, remotePath: remoteEntryPath})
		}
		return nil
	}); err != nil {
		return err
	}

	sem := make(chan struct{}, maxConcurrentUploads)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	// Phase 2: concurrent MkDirAll.
	for _, d := range dirs {
		if cancelCtx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := conn.MkDirAll(d); err != nil {
				setErr(err)
			}
		}(d)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// Phase 3: concurrent WriteFile.
	for _, fe := range files {
		if cancelCtx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(f fileEntry) {
			defer wg.Done()
			defer func() { <-sem }()

			localFile, err := os.Open(f.localPath)
			if err != nil {
				setErr(fmt.Errorf("failed to open local file %q: %w", f.localPath, err))
				return
			}
			defer localFile.Close()

			for range 10 {
				err = conn.WriteFile(localFile, f.remotePath)
				if err == nil {
					return
				}
			}
			_ = conn.Remove(f.remotePath)
			setErr(fmt.Errorf("failed to write remote file %q: %w", f.remotePath, err))
		}(fe)
	}
	wg.Wait()

	return firstErr
}

// listBackend is the minimal shape needed by BenchmarkRemoteList: seed +
// list + cleanup against a target directory.
type listBackend struct {
	name string
	// mkdir creates dir (idempotent).
	mkdir func(dir string) error
	// writeFile creates a file of the given size at path.
	writeFile func(p string, size int) error
	// list returns the entries directly under dir.
	list func(dir string) (int, error)
	// remove removes dir recursively.
	remove func(dir string) error
	// basePath is where seeded trees live for this backend.
	basePath string
}

// SinkList prevents the compiler from eliding the list result.
var SinkList int

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

// remoteConnBackend adapts a remote.RemoteConn to listBackend.
func remoteConnBackend(name, basePath string, conn remote.RemoteConn) listBackend {
	return listBackend{
		name:     name,
		basePath: basePath,
		mkdir:    conn.MkDirAll,
		writeFile: func(p string, size int) error {
			r, w := io.Pipe()
			go func() { w.CloseWithError(writeRandom(w, size)) }()
			return conn.WriteFile(r, p)
		},
		list: func(dir string) (int, error) {
			entries, err := conn.List(dir)
			return len(entries), err
		},
		remove: conn.Remove,
	}
}

// editorBackend starts a dedicated editor-server container (with adbd
// alongside), seeds files over adb, and lists via fs.walk depth=1 over WS.
// The dialed client is returned so tree benchmarks can reuse it.
func editorBackend(tb testing.TB) (listBackend, *editorclient.Client) {
	tb.Helper()

	name, editorPort, adbPort := testtools.StartEditorContainer(tb)
	tb.Cleanup(func() { testtools.StopEditorContainer(tb, name) })

	adbConn, err := adb.FromHost("localhost:"+adbPort, "")
	require.NoError(tb, err)

	addr := "localhost:" + editorPort
	editorclient.WaitReady(tb, addr)
	client := editorclient.Dial(tb, addr)
	tb.Cleanup(client.Close)

	const basePath = "/home/arduino"
	seed := remoteConnBackend("editor", basePath, adbConn)
	seed.list = func(dir string) (int, error) {
		entries, err := client.Walk(map[string]any{"path": dir, "depth": 1})
		if err != nil {
			return 0, err
		}
		// fs.walk includes "." for the root itself; List does not.
		return len(entries) - 1, nil
	}
	return seed, client
}

// BenchmarkRemoteList measures directory-listing latency across backends.
// The editor backend calls fs.walk with depth=1 to match List semantically.
func BenchmarkRemoteList(b *testing.B) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	name, adbPort, sshPort := testtools.StartAdbDContainer(b)
	b.Cleanup(func() { testtools.StopAdbDContainer(b, name) })

	adbConn, err := adb.FromHost("localhost:"+adbPort, "")
	require.NoError(b, err)
	sshConn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
	require.NoError(b, err)

	editorBE, _ := editorBackend(b)
	backends := []listBackend{
		remoteConnBackend("adb", "/home/arduino", adbConn),
		remoteConnBackend("ssh", "./", sshConn),
		remoteConnBackend("local", b.TempDir(), &local.LocalConnection{}),
		editorBE,
	}

	for _, be := range backends {
		b.Run(be.name, func(b *testing.B) {
			remoteBase := path.Join(be.basePath, "bench_list_"+be.name)
			_ = be.remove(remoteBase)
			require.NoError(b, be.mkdir(remoteBase))
			b.Cleanup(func() { _ = be.remove(remoteBase) })

			for _, count := range []int{10, 50, 200} {
				b.Run(fmt.Sprintf("%dentries", count), func(b *testing.B) {
					dir := path.Join(remoteBase, fmt.Sprintf("entries_%d", count))
					require.NoError(b, be.mkdir(dir))
					for i := range count {
						p := path.Join(dir, fmt.Sprintf("file_%04d.bin", i))
						require.NoError(b, be.writeFile(p, 1024))
					}

					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						n, err := be.list(dir)
						require.NoError(b, err)
						require.Equal(b, count, n)
						SinkList = n
					}
				})
			}
		})
	}
}

// legacyBuildFileTreeRecursive vendors the client-side walker from
// cloud-editor-mono (standalone-apps/app-lab-desktop/internal/fs/filetree.go),
// with light trimming (no mime, no ignore) and error propagation fixed.
// Kept as one self-contained function for easy reference in the tree bench.
func legacyBuildFileTreeRecursive(fss fs.FS, currentPath string) (int, error) {
	const maxConcurrentDirReads = 8

	type treeNode struct {
		Name       string
		Size       int64
		IsDir      bool
		ModifiedAt string
		Children   []treeNode
	}

	var buildFileTreeRecursive func(currentPath string, entry fs.DirEntry, sem chan struct{}) (*treeNode, error)
	buildFileTreeRecursive = func(currentPath string, entry fs.DirEntry, sem chan struct{}) (*treeNode, error) {
		if entry != nil && !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return nil, err
			}
			return &treeNode{
				Name:       info.Name(),
				Size:       info.Size(),
				ModifiedAt: info.ModTime().Format(time.RFC3339),
			}, nil
		}

		sem <- struct{}{}
		f, err := fss.Open(currentPath)
		if err != nil {
			<-sem
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			<-sem
			_ = f.Close()
			return nil, err
		}
		node := treeNode{
			Name:       info.Name(),
			Size:       info.Size(),
			IsDir:      true,
			ModifiedAt: info.ModTime().Format(time.RFC3339),
		}
		entries, err := f.(fs.ReadDirFile).ReadDir(0)
		_ = f.Close()
		<-sem
		if err != nil {
			return nil, err
		}

		type nodeResult struct {
			node *treeNode
			err  error
		}
		results := make([]nodeResult, len(entries))
		var wg sync.WaitGroup
		for i, e := range entries {
			childPath := path.Join(currentPath, e.Name())
			if e.IsDir() {
				wg.Add(1)
				go func(idx int, de fs.DirEntry, cp string) {
					defer wg.Done()
					child, err := buildFileTreeRecursive(cp, de, sem)
					results[idx] = nodeResult{node: child, err: err}
				}(i, e, childPath)
			} else {
				child, err := buildFileTreeRecursive(childPath, e, sem)
				results[i] = nodeResult{node: child, err: err}
			}
		}
		wg.Wait()

		for _, r := range results {
			if r.err != nil {
				return nil, r.err
			}
			if r.node != nil {
				node.Children = append(node.Children, *r.node)
			}
		}
		return &node, nil
	}

	var countTree func(*treeNode) int
	countTree = func(n *treeNode) int {
		if n == nil {
			return 0
		}
		total := 1
		for i := range n.Children {
			total += countTree(&n.Children[i])
		}
		return total
	}

	sem := make(chan struct{}, maxConcurrentDirReads)
	root, err := buildFileTreeRecursive(currentPath, nil, sem)
	if err != nil {
		return 0, err
	}
	return countTree(root), nil
}

// seedTree creates a synthetic tree using the backend's mkdir/writeFile.
// filesPerDir=5, dirsPerLevel=4, depth=3 → 85 dirs, 425 files.
func seedTree(b *testing.B, be listBackend, root string, dirsPerLevel, depth, filesPerDir, fileSize int) {
	b.Helper()
	require.NoError(b, be.mkdir(root))
	for i := 0; i < filesPerDir; i++ {
		require.NoError(b, be.writeFile(path.Join(root, fmt.Sprintf("file_%02d.bin", i)), fileSize))
	}
	if depth == 0 {
		return
	}
	for i := 0; i < dirsPerLevel; i++ {
		child := path.Join(root, fmt.Sprintf("dir_%02d", i))
		seedTree(b, be, child, dirsPerLevel, depth-1, filesPerDir, fileSize)
	}
}

// BenchmarkRemoteTree measures a full recursive tree walk. ssh/adb/local
// use the vendored client-side walker; editor does the whole walk in a
// single fs.walk request.
func BenchmarkRemoteTree(b *testing.B) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	name, adbPort, sshPort := testtools.StartAdbDContainer(b)
	b.Cleanup(func() { testtools.StopAdbDContainer(b, name) })

	adbConn, err := adb.FromHost("localhost:"+adbPort, "")
	require.NoError(b, err)
	sshConn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
	require.NoError(b, err)
	editorBE, editorCli := editorBackend(b)

	type treeBackend struct {
		name     string
		seed     listBackend
		basePath string
		walkAll  func(dir string) (int, error)
	}

	walkViaConn := func(conn remote.RemoteConn) func(string) (int, error) {
		return func(dir string) (int, error) {
			return legacyBuildFileTreeRecursive(remotefs.New(dir, conn), ".")
		}
	}

	backends := []treeBackend{
		{
			name:     "adb",
			seed:     remoteConnBackend("adb", "/home/arduino", adbConn),
			basePath: "/home/arduino",
			walkAll:  walkViaConn(adbConn),
		},
		{
			name:     "ssh",
			seed:     remoteConnBackend("ssh", "./", sshConn),
			basePath: "./",
			walkAll:  walkViaConn(sshConn),
		},
		{
			name:     "editor",
			seed:     editorBE,
			basePath: editorBE.basePath,
			walkAll: func(dir string) (int, error) {
				entries, err := editorCli.Walk(map[string]any{"path": dir})
				if err != nil {
					return 0, err
				}
				return len(entries), nil
			},
		},
	}

	const (
		dirsPerLevel = 4
		depth        = 3
		filesPerDir  = 5
		fileSize     = 1024
	)

	for _, be := range backends {
		b.Run(be.name, func(b *testing.B) {
			root := path.Join(be.basePath, "bench_tree_"+be.name)
			_ = be.seed.remove(root)
			seedTree(b, be.seed, root, dirsPerLevel, depth, filesPerDir, fileSize)
			b.Cleanup(func() { _ = be.seed.remove(root) })

			// Sanity: all backends must see the same tree.
			want, err := be.walkAll(root)
			require.NoError(b, err)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n, err := be.walkAll(root)
				require.NoError(b, err)
				require.Equal(b, want, n)
				SinkList = n
			}
		})
	}
}
