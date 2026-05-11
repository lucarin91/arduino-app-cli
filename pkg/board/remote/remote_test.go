// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package remote_test

import (
	"fmt"
	"net"
	"path"
	"slices"

	"io"
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

			err = remote.conn.Forward(t.Context(), forwardPort, pongServerPort)
			assert.NoError(t, err)

			conn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", forwardPort))
			require.NoError(t, err)

			buf := [128]byte{}
			n, err := conn.Read(buf[:])
			require.NoError(t, err)
			require.Equal(t, "pong", string(buf[:n]))

			err = conn.Close()
			require.NoError(t, err)

			err = remote.conn.ForwardKillAll(t.Context())
			assert.NoError(t, err)

			_, err = net.Dial("tcp", fmt.Sprintf("localhost:%d", forwardPort))
			require.Error(t, err)
		})
	}
}
