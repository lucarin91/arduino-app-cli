// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package adb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/arduino/go-paths-helper"
	"github.com/pkg/sftp"

	"github.com/arduino/arduino-app-cli/pkg/board/remote"
	"github.com/arduino/arduino-app-cli/pkg/board/remote/sftpfs"
	"github.com/arduino/arduino-app-cli/pkg/x/ports"
)

type ADBConnection struct {
	adbPath string
	host    string

	*sftpfs.SftpFS
}

var _ remote.RemoteConn = (*ADBConnection)(nil)

var (
	// ErrNotFound is returned when the ADB device is not found.
	ErrNotFound = fmt.Errorf("ADB device not found")
	// ErrDeviceOffline is returned when the ADB device is not reachable.
	// This usually requires a restart of the adbd server daemon on the device.
	ErrDeviceOffline = fmt.Errorf("ADB device is offline")
)

// FromSerial creates an ADBConnection from a device serial number.
// returns an error NotFoundErr if the device is not found, and DeviceOfflineErr if the device is offline.
func FromSerial(serial string, adbPath string) (*ADBConnection, error) {
	if adbPath == "" {
		adbPath = FindAdbPath()
	}

	isConnected := func(serial, adbPath string) (bool, error) {
		cmd, err := paths.NewProcess(nil, adbPath, "-s", serial, "get-state")
		if err != nil {
			return false, fmt.Errorf("failed to create ADB command: %w", err)
		}

		output, err := cmd.RunAndCaptureCombinedOutput(context.TODO())
		if err != nil {
			slog.Error("unable to connect to ADB device", "error", err, "output", string(output), "serial", serial)
			if bytes.Contains(output, []byte("device offline")) {
				return false, ErrDeviceOffline
			} else if bytes.Contains(output, []byte("not found")) {
				return false, ErrNotFound
			}
			return false, fmt.Errorf("failed to get ADB device state: %w: %s", err, output)
		}

		return string(bytes.TrimSpace(output)) == "device", nil
	}

	if connected, err := isConnected(serial, adbPath); err != nil {
		return nil, err
	} else if !connected {
		return nil, fmt.Errorf("device %s is not connected", serial)
	}

	conn := &ADBConnection{
		adbPath: adbPath,
		host:    serial,
	}
	conn.SftpFS = sftpfs.New(conn.dialSftp)
	return conn, nil
}

func FromHost(host string, adbPath string) (*ADBConnection, error) {
	if adbPath == "" {
		adbPath = FindAdbPath()
	}
	cmd, err := paths.NewProcess(nil, adbPath, "connect", host)
	if err != nil {
		return nil, err
	}
	if out, err := cmd.RunAndCaptureCombinedOutput(context.TODO()); err != nil {
		return nil, fmt.Errorf("failed to connect to ADB host %s: %w: %s", host, err, out)
	}
	return FromSerial(host, adbPath)
}

// TODO: ok on ubuntu/debian, may differ on other distros.
const sftpServerBin = "/usr/lib/openssh/sftp-server"

// dialSftp launches sftp-server on the device via `adb shell` and returns a
// client speaking SFTP over its stdio. The returned closer kills the process.
func (a *ADBConnection) dialSftp() (*sftp.Client, []sftpfs.CloseFunc, error) {
	cmd, err := paths.NewProcess(nil, a.adbPath, "-s", a.host, "shell", sftpServerBin)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create sftp-server command: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get sftp stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get sftp stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start sftp-server: %w", err)
	}
	client, err := sftp.NewClientPipe(stdout, stdin)
	if err != nil {
		_ = cmd.Kill()
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("failed to create sftp client: %w", err)
	}
	return client, []sftpfs.CloseFunc{func() error {
		err1 := cmd.Kill()
		err2 := cmd.Wait()
		return errors.Join(err1, err2)
	}}, nil
}

func (a *ADBConnection) Forward(ctx context.Context, localPort int, remotePort int) error {
	localString := fmt.Sprintf("tcp:%d", localPort)
	remoteString := fmt.Sprintf("tcp:%d", remotePort)

	if !ports.IsAvailable(localPort) {
		// Check if the port is already forwarded by adb to the expected remote port
		if checkCmd, err := paths.NewProcess(nil, a.adbPath, "-s", a.host, "forward", "--list"); err == nil {
			if out, err := checkCmd.RunAndCaptureCombinedOutput(ctx); err == nil {
				scanner := bufio.NewScanner(bytes.NewReader(out))
				for scanner.Scan() {
					// Output format is typically: <serial> <local> <remote>
					fields := strings.Fields(scanner.Text())
					if len(fields) >= 3 && fields[0] == a.host && fields[1] == localString && fields[2] == remoteString {
						return nil // Port is already forwarded exactly as requested
					}
				}
			}
		}

		return remote.ErrPortAvailable
	}

	cmd, err := paths.NewProcess(nil, a.adbPath, "-s", a.host, "forward", localString, remoteString)
	if err != nil {
		return err
	}
	if out, err := cmd.RunAndCaptureCombinedOutput(ctx); err != nil {
		return fmt.Errorf(
			"failed to forward ADB port %s to %s: %w: %s",
			localString,
			remoteString,
			err,
			out,
		)
	}

	return nil
}

func (a *ADBConnection) ForwardKillAll(ctx context.Context) error {
	cmd, err := paths.NewProcess(nil, a.adbPath, "-s", a.host, "forward", "--remove-all")
	if err != nil {
		return err
	}
	if out, err := cmd.RunAndCaptureCombinedOutput(ctx); err != nil {
		return fmt.Errorf("failed to kill all ADB forwarded ports: %w: %s", err, out)
	}
	return nil
}

type ADBCommand struct {
	cmd *paths.Process
	err error
}

func (a *ADBConnection) GetCmd(cmd string, args ...string) remote.Cmder {
	for i, arg := range args {
		if strings.Contains(arg, " ") {
			args[i] = fmt.Sprintf("%q", arg)
		}
	}

	// TODO: fix command injection vulnerability
	var cmds []string
	cmds = append(cmds, a.adbPath, "-s", a.host, "shell", cmd)
	if len(args) > 0 {
		cmds = append(cmds, args...)
	}

	command, err := paths.NewProcess(nil, cmds...)
	return &ADBCommand{cmd: command, err: err}
}

func (a *ADBCommand) Run(ctx context.Context) error {
	if a.err != nil {
		return fmt.Errorf("failed to create command: %w", a.err)
	}

	return a.cmd.RunWithinContext(ctx)
}

func (a *ADBCommand) Output(ctx context.Context) ([]byte, error) {
	if a.err != nil {
		return nil, fmt.Errorf("failed to create command: %w", a.err)
	}

	return a.cmd.RunAndCaptureCombinedOutput(ctx)
}

func (a *ADBCommand) Interactive() (io.WriteCloser, io.Reader, io.Reader, remote.Closer, error) {
	if a.err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to create command: %w", a.err)
	}

	stdin, err := a.cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	stdout, err := a.cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}
	stderr, err := a.cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := a.cmd.Start(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to start command: %w", err)
	}

	return stdin, stdout, stderr, func() error {
		if err := stdout.Close(); err != nil {
			return fmt.Errorf("failed to close stdout pipe: %w", err)
		}
		if err := stderr.Close(); err != nil {
			return fmt.Errorf("failed to close stderr pipe: %w", err)
		}
		if err := a.cmd.Wait(); err != nil {
			return fmt.Errorf("command failed: %w", err)
		}
		return nil
	}, nil
}

func FindAdbPath() string {
	var adbPath = "adb"

	// Attempt to find the adb path in the Arduino15 directory
	const arduino15adbPath = "packages/arduino/tools/adb/32.0.0/adb"
	var path string
	switch runtime.GOOS {
	case "darwin":
		user, err := user.Current()
		if err != nil {
			slog.Warn("Unable to get current user", "error", err)
			break
		}
		path = filepath.Join(user.HomeDir, "/Library/Arduino15/", arduino15adbPath)
	case "linux":
		user, err := user.Current()
		if err != nil {
			slog.Warn("Unable to get current user", "error", err)
			break
		}
		path = filepath.Join(user.HomeDir, ".arduino15/", arduino15adbPath)
	case "windows":
		user, err := user.Current()
		if err != nil {
			slog.Warn("Unable to get current user", "error", err)
			break
		}
		path = filepath.Join(user.HomeDir, "AppData/Local/Arduino15/", arduino15adbPath)
		path += ".exe"
	}
	s, err := os.Stat(path)
	if err == nil && !s.IsDir() {
		adbPath = path
	}

	slog.Debug("get adb path", "path", adbPath)

	return adbPath
}
