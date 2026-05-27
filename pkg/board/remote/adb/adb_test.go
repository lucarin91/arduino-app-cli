// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package adb

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFindAdbPath(t *testing.T) {
	got := FindAdbPath()
	if runtime.GOOS == "windows" {
		assert.True(t, filepath.Base(got) == "adb.exe")
	} else {
		assert.True(t, filepath.Base(got) == "adb")
	}
	assert.True(t, filepath.IsAbs(got))
	t.Logf("FindAdbPath returned: %q", got)
}
