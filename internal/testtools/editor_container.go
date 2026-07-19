// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package testtools

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arduino/arduino-app-cli/pkg/board/remote/adb"
)

// StartEditorContainer builds and starts the editor-server Docker image
// (editor.Dockerfile). The image ships adbd alongside the editor so tests
// can seed files over adb (VM-local ext4) while walking them via the editor,
// keeping the ssh vs editor comparison on the same filesystem class.
//
// Returns the container name, the editor's :8998 host port and adb's :5555
// host port. The container is not automatically removed on test failure;
// use StopEditorContainer.
func StartEditorContainer(t testing.TB) (name, editorPort, adbPort string) {
	if runtime.GOOS != "linux" && os.Getenv("CI") != "" {
		t.Skip("Skipping tests in CI that requires docker on non-Linux systems")
	}
	t.Helper()

	cmd := exec.Command("docker", "build", "-t", "editor-server", "-f", "editor.Dockerfile", ".")
	cmd.Dir = getBaseProjectPath(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build editor-server image: %v: %s", err, out)
	}

	containerName := fmt.Sprintf("editor-server-testing-%d", time.Now().UnixNano())
	for range 10 {
		editorPort = getRandPort(t)
		adbPort = getRandPort(t)
		out, err := exec.Command(
			"docker", "run", "-d", "--rm", "--name", containerName,
			"-p", editorPort+":8998",
			"-p", adbPort+":5555",
			"editor-server",
		).CombinedOutput()
		if err == nil {
			break
		}
		t.Logf("attempt to start editor-server container with editor=%q adb=%q: %s, %s",
			editorPort, adbPort, err, strings.TrimSpace(string(out)))
	}

	adbPath := adb.FindAdbPath()
	deadline := time.After(10 * time.Second)
	tick := time.Tick(500 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Fatalf("editor-server adbd did not become ready within timeout")
		case <-tick:
			out, err := exec.Command(adbPath, "connect", "localhost:"+adbPort).CombinedOutput()
			if err == nil && strings.Contains(string(out), "connected to localhost:"+adbPort) {
				return containerName, editorPort, adbPort
			}
		}
	}
}

// StopEditorContainer removes the container. Safe to call in t.Cleanup.
func StopEditorContainer(t testing.TB, name string) {
	t.Helper()
	if out, err := exec.Command("docker", "rm", "-f", name).CombinedOutput(); err != nil {
		t.Logf("editor-server stop output: %v: %v", err, string(out))
	}
}
