// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package remote

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
)

func ParseChage(r io.Reader) (bool, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Last password change") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) < 2 {
				return false, fmt.Errorf("unexpected output from chage command: %s", line)
			}
			value := strings.TrimSpace(parts[1])
			return value != "password must be changed", nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, fmt.Errorf("unexpected output from chage command")
}

// ParseLsOutput parses the output of the `ls -laQ` command and returns a slice of FileInfo.
func ParseLsOutput(out io.Reader) ([]FileInfo, error) {
	// skip the first line which contains the total size
	r := bufio.NewReader(out)
	if _, err := r.ReadBytes('\n'); err != nil {
		return nil, err
	}

	var files []FileInfo
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		first := strings.IndexByte(string(line), '"')
		last := strings.LastIndexByte(string(line), '"')
		name := string(line[first+1 : last])
		if name == "." || name == ".." {
			continue
		}
		files = append(files, FileInfo{
			Name:  name,
			IsDir: line[0] == 'd',
		})
	}

	return files, nil
}
