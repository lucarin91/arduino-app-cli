// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package remote_test

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arduino/arduino-app-cli/internal/testtools"
	"github.com/arduino/arduino-app-cli/pkg/board/remote"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/adb"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/local"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/ssh"
	"github.com/arduino/arduino-app-cli/pkg/x/ports"
)

func TestRemoteFS(t *testing.T) {
	name, adbPort, sshPort := testtools.StartAdbDContainer(t)
	t.Cleanup(func() { testtools.StopAdbDContainer(t, name) })

	tests := []struct {
		name string
		conn remote.FS
	}{{
		"adb",
		func() remote.FS {
			conn, err := adb.FromHost("localhost:"+adbPort, "")
			require.NoError(t, err)
			return conn
		}(),
	}, {
		"ssh",
		func() remote.FS {
			conn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
			require.NoError(t, err)
			return conn
		}(),
	}, {
		"local",
		func() remote.FS {
			return &local.LocalConnection{}
		}(),
	},
	}

	files := []string{
		"testdir/testfile.txt",
		"testdir/testfile with spaces.txt",
		"testdir with space/testfile.txt",
		"testdir with space/testfile with spaces.txt",
	}

	dirs := func() []string {
		dirs := make([]string, 0, len(files))
		for _, file := range files {
			dirs = append(dirs, path.Dir(file))
		}
		slices.Sort(dirs)
		return slices.Compact(dirs)
	}()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("Mkdir", func(t *testing.T) {
				for _, dir := range dirs {
					err := tc.conn.MkDirAll("./" + dir)
					require.NoError(t, err)
					info, err := tc.conn.Stats("./" + dir)
					require.NoError(t, err)
					assert.Equal(t, remote.FileInfo{
						Name:  dir,
						IsDir: true,
					}, info)
				}
			})

			t.Run("WriteFile/ReadFile", func(t *testing.T) {
				for _, file := range files {
					t.Run(file, func(t *testing.T) {
						err := tc.conn.WriteFile(strings.NewReader("Hello, World!"), "./"+file)
						require.NoError(t, err)
						info, err := tc.conn.Stats("./" + file)
						require.NoError(t, err)
						assert.Equal(t, remote.FileInfo{
							Name:  path.Base(file),
							IsDir: false,
						}, info)

						r, err := tc.conn.ReadFile("./" + file)
						require.NoError(t, err)
						data, err := io.ReadAll(r)
						require.NoError(t, err)
						require.Equal(t, "Hello, World!", string(data))
					})
				}
			})

			t.Run("List", func(t *testing.T) {
				gotFiles, err := tc.conn.List("./")
				require.NoError(t, err)
				for _, dir := range dirs {
					assert.Contains(t, gotFiles, remote.FileInfo{Name: dir, IsDir: true})
				}

				for _, dir := range dirs {
					gotFiles, err = tc.conn.List("./" + dir)
					require.NoError(t, err)
					assert.Len(t, gotFiles, 2)
					for _, gotFile := range gotFiles {
						assert.Contains(t, files, path.Join(dir, gotFile.Name))
					}
				}
			})

			t.Run("Remove", func(t *testing.T) {
				for _, file := range files {
					err := tc.conn.Remove("./" + file)
					require.NoError(t, err)
					_, err = tc.conn.Stats("./" + file)
					assert.Error(t, err)
				}

				for _, dir := range dirs {
					err := tc.conn.Remove("./" + dir)
					require.NoError(t, err)
					_, err = tc.conn.Stats("./" + dir)
					assert.Error(t, err)
				}
			})
		})
	}
}

func TestRemoteShell(t *testing.T) {
	name, adbPort, sshPort := testtools.StartAdbDContainer(t)
	t.Cleanup(func() { testtools.StopAdbDContainer(t, name) })

	remotes := []remote.RemoteShell{
		func() remote.RemoteShell {
			conn, err := adb.FromHost("localhost:"+adbPort, "")
			require.NoError(t, err)
			return conn
		}(),
		func() remote.RemoteShell {
			conn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
			require.NoError(t, err)
			return conn
		}(),
		func() remote.RemoteShell {
			return &local.LocalConnection{}
		}(),
	}

	for _, conn := range remotes {
		tests := []func(string, ...string) remote.Cmder{
			func(cmd string, args ...string) remote.Cmder {
				return conn.GetCmd(cmd, args...)
			},
		}

		for _, cmder := range tests {
			t.Run("Run", func(t *testing.T) {
				cmd := cmder("echo", "Hello, World!")
				err := cmd.Run(t.Context())
				require.NoError(t, err)
			})

			t.Run("Output", func(t *testing.T) {
				cmd := cmder("echo", "Hello, World!")
				output, err := cmd.Output(t.Context())
				require.NoError(t, err)
				assert.True(t, strings.HasPrefix(string(output), "Hello, World!"))
			})

			t.Run("Interactive", func(t *testing.T) {
				cmd := cmder("cat")
				stdin, stdout, stderr, closer, err := cmd.Interactive()
				require.NoError(t, err)

				_, err = stdin.Write([]byte("Hello, Interactive World!\n"))
				require.NoError(t, err)
				stdin.Close() // Close stdin to signal EOF

				output, err := io.ReadAll(stdout)
				require.NoError(t, err)
				assert.True(t, strings.HasPrefix(string(output), "Hello, Interactive World!"))
				stderrOutput, err := io.ReadAll(stderr)
				require.NoError(t, err)
				require.Empty(t, stderrOutput)

				require.NoError(t, closer())
			})
		}
	}

}

func TestRemoteForwarder(t *testing.T) {
	name, adbPort, sshPort := testtools.StartAdbDContainer(t)
	t.Cleanup(func() { testtools.StopAdbDContainer(t, name) })

	const pongServerPort = 9999

	remotes := []struct {
		name        string
		conn        remote.Forwarder
		forwardPort int
	}{
		{
			name: "adb",
			conn: func() remote.Forwarder {
				conn, err := adb.FromHost("localhost:"+adbPort, "")
				require.NoError(t, err)
				return conn
			}(),
			forwardPort: func() int {
				port, err := ports.GetAvailable()
				require.NoError(t, err)
				return port
			}(),
		},
		{
			name: "ssh",
			conn: func() remote.Forwarder {
				conn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
				require.NoError(t, err)
				return conn
			}(),
		},

		// We are skipping the local forwarder test, which is just an no op in this case.
	}

	for _, remote := range remotes {
		t.Run(remote.name, func(t *testing.T) {
			forwardPort, err := ports.GetAvailable()
			require.NoError(t, err)

			for i := range 3 {
				t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
					err = remote.conn.Forward(t.Context(), forwardPort, pongServerPort)
					assert.NoError(t, err)

					conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", forwardPort))
					require.NoError(t, err)

					buf := [128]byte{}
					n, err := conn.Read(buf[:])
					require.NoError(t, err, "failed to read from forwarded port %d", forwardPort)
					require.Equal(t, "pong", string(buf[:n]))

					err = conn.Close()
					require.NoError(t, err)
				})
			}

			err = remote.conn.ForwardKillAll(t.Context())
			assert.NoError(t, err)

			_, err = net.Dial("tcp", fmt.Sprintf("localhost:%d", forwardPort))
			require.Error(t, err)
		})
	}
}

func TestRemoteTransfer(t *testing.T) {
	name, adbPort, sshPort := testtools.StartAdbDContainer(t)
	t.Cleanup(func() { testtools.StopAdbDContainer(t, name) })

	tests := []struct {
		name     string
		conn     remote.RemoteConn
		basePath string
	}{{
		"adb",
		func() remote.RemoteConn {
			conn, err := adb.FromHost("localhost:"+adbPort, "")
			require.NoError(t, err)
			return conn
		}(),
		"/home/arduino", // FIXME: adb push seems to not work with relative paths
	}, {
		"ssh",
		func() remote.RemoteConn {
			conn, err := ssh.FromHost("arduino", "arduino", "127.0.0.1:"+sshPort)
			require.NoError(t, err)
			return conn
		}(),
		"./",
	}, {
		"local",
		func() remote.RemoteConn {
			return &local.LocalConnection{}
		}(),
		"./",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := t.TempDir()

			// Prepare a single source file.
			srcFile := filepath.Join(srcDir, "pushfile.txt")
			fileContent := "Hello, Push!"
			require.NoError(t, os.WriteFile(srcFile, []byte(fileContent), 0600))

			// Prepare a source directory with nested files.
			srcSubDir := filepath.Join(srcDir, "pushdir")
			require.NoError(t, os.MkdirAll(filepath.Join(srcSubDir, "nested"), 0755))
			nestedContent := "Nested Hello!"
			require.NoError(t, os.WriteFile(filepath.Join(srcSubDir, "a.txt"), []byte(fileContent), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(srcSubDir, "nested", "b.txt"), []byte(nestedContent), 0600))

			require.NoError(t, tc.conn.MkDirAll("./testdir"))
			t.Cleanup(func() { _ = tc.conn.Remove("./testdir") })

			t.Run("PushFile", func(t *testing.T) {
				err := tc.conn.Push(t.Context(), srcFile, path.Join(tc.basePath, "testdir/pushfile.txt"))
				require.NoError(t, err)

				info, err := tc.conn.Stats("./testdir/pushfile.txt")
				require.NoError(t, err)
				assert.Equal(t, remote.FileInfo{
					Name:  "pushfile.txt",
					IsDir: false,
				}, info)

				r, err := tc.conn.ReadFile("./testdir/pushfile.txt")
				require.NoError(t, err)
				data, err := io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, fileContent, string(data))
			})

			t.Run("PushDir", func(t *testing.T) {
				err := tc.conn.Push(t.Context(), srcSubDir, path.Join(tc.basePath, "testdir/pushdir"))
				require.NoError(t, err)

				info, err := tc.conn.Stats("./testdir/pushdir")
				require.NoError(t, err)
				assert.Equal(t, remote.FileInfo{
					Name:  "pushdir",
					IsDir: true,
				}, info)

				info, err = tc.conn.Stats("./testdir/pushdir/a.txt")
				require.NoError(t, err)
				assert.Equal(t, remote.FileInfo{Name: "a.txt", IsDir: false}, info)

				info, err = tc.conn.Stats("./testdir/pushdir/nested")
				require.NoError(t, err)
				assert.Equal(t, remote.FileInfo{Name: "nested", IsDir: true}, info)

				r, err := tc.conn.ReadFile("./testdir/pushdir/a.txt")
				require.NoError(t, err)
				data, err := io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, fileContent, string(data))

				r, err = tc.conn.ReadFile("./testdir/pushdir/nested/b.txt")
				require.NoError(t, err)
				data, err = io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, nestedContent, string(data))
			})

			t.Run("PushFileOverride", func(t *testing.T) {
				// Overwrite source content and push again to the same destination as PushFile.
				newContent := "Overridden Push!"
				require.NoError(t, os.WriteFile(srcFile, []byte(newContent), 0600))

				err := tc.conn.Push(t.Context(), srcFile, path.Join(tc.basePath, "testdir/pushfile.txt"))
				require.NoError(t, err)

				info, err := tc.conn.Stats("./testdir/pushfile.txt")
				require.NoError(t, err)
				assert.Equal(t, remote.FileInfo{
					Name:  "pushfile.txt",
					IsDir: false,
				}, info)

				r, err := tc.conn.ReadFile("./testdir/pushfile.txt")
				require.NoError(t, err)
				data, err := io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, newContent, string(data))

				// Restore original content for subsequent subtests.
				require.NoError(t, os.WriteFile(srcFile, []byte(fileContent), 0600))
			})

			t.Run("PushDirOverride", func(t *testing.T) {
				// Modify source: change a.txt content and add a new file.
				overriddenContent := "Overridden Hello!"
				require.NoError(t, os.WriteFile(filepath.Join(srcSubDir, "a.txt"), []byte(overriddenContent), 0600))
				newContent := "Brand new file!"
				require.NoError(t, os.WriteFile(filepath.Join(srcSubDir, "c.txt"), []byte(newContent), 0600))

				// Push again over the same destination as PushDir.
				err := tc.conn.Push(t.Context(), srcSubDir, path.Join(tc.basePath, "testdir/pushdir"))
				require.NoError(t, err)

				// Existing file is overridden.
				r, err := tc.conn.ReadFile("./testdir/pushdir/a.txt")
				require.NoError(t, err)
				data, err := io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, overriddenContent, string(data))

				// New file is present.
				r, err = tc.conn.ReadFile("./testdir/pushdir/c.txt")
				require.NoError(t, err)
				data, err = io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, newContent, string(data))

				// Nested file is preserved.
				r, err = tc.conn.ReadFile("./testdir/pushdir/nested/b.txt")
				require.NoError(t, err)
				data, err = io.ReadAll(r)
				require.NoError(t, err)
				require.Equal(t, nestedContent, string(data))
			})
		})
	}
}
