// This file is part of arduino-app-cli.
//
// SPDX-FileCopyrightText: Arduino s.r.l. and/or its affiliated companies
// SPDX-License-Identifier: GPL-3.0-or-later

package ssh

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

type ScpClient struct {
	Client *ssh.Client
}

func NewScpClient(client *ssh.Client) *ScpClient {
	return &ScpClient{Client: client}
}

const remoteBinary = "scp"

func (c *ScpClient) PushDir(fsys fs.FS, remote string, override bool) error {
	// If override is true, the remote path is treated as a directory where the contents of fsys will be copied.
	base := filepath.Base(remote)
	if override {
		remote = filepath.Dir(remote)
	}

	session, err := c.Client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	r, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	w, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer w.Close()

	cmd := fmt.Sprintf("%s -rt %q", remoteBinary, remote)
	if err := session.Start(cmd); err != nil {
		return err
	}

	rw := &scpSession{r: r, w: w}
	if err := pushDir(rw, fsys, base); err != nil {
		return err
	}
	_ = rw.Close()

	return session.Wait()
}

func (c *ScpClient) PushFile(local, remote string) error {
	session, err := c.Client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	r, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	w, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer w.Close()

	cmd := fmt.Sprintf("%s -t %q", remoteBinary, remote)
	if err := session.Start(cmd); err != nil {
		return err
	}

	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()

	rw := &scpSession{r: r, w: w}
	if err := pushFile(rw, f); err != nil {
		return err
	}
	_ = rw.Close()

	return session.Wait()
}

const enableDebug = false

type scpSession struct {
	r io.Reader
	w io.WriteCloser
}

func (s *scpSession) Read(p []byte) (n int, err error) {
	if enableDebug {
		fmt.Printf("Got: %q\n", string(p)) // nolint:forbidigo
	}
	return s.r.Read(p)
}

func (s *scpSession) Write(p []byte) (n int, err error) {
	if enableDebug {
		fmt.Printf("Sent: %q\n", string(p)) // nolint:forbidigo
	}
	return s.w.Write(p)
}

func (s *scpSession) Close() error {
	return s.w.Close()
}

func pushFile(rw io.ReadWriter, f fs.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}

	fmt.Fprintf(rw, "C%04o %d %s\n", info.Mode().Perm(), info.Size(), info.Name())

	if err := checkErr(rw); err != nil {
		return err
	}

	if _, err := io.Copy(rw, f); err != nil {
		return err
	}
	fmt.Fprint(rw, "\x00")

	return checkErr(rw)
}

func pushDir(rw io.ReadWriter, fsys fs.FS, remoteBase string) error {
	info, err := fs.Stat(fsys, ".")
	if err != nil {
		return err
	}
	return pusDirRec(rw, fsys, ".", remoteBase, info)
}

func pusDirRec(rw io.ReadWriter, fsys fs.FS, name, remote string, info os.FileInfo) error {
	switch info.Mode().Type() {
	case fs.ModeDir:
		fmt.Fprintf(rw, "D%04o 0 %s\n", info.Mode().Perm(), remote)
		if err := checkErr(rw); err != nil {
			return err
		}

		dirs, err := fs.ReadDir(fsys, name)
		if err != nil {
			return err
		}
		for _, d1 := range dirs {
			name1 := filepath.Join(name, d1.Name())
			info1, err := d1.Info()
			if err != nil {
				return err
			}
			if err := pusDirRec(rw, fsys, name1, d1.Name(), info1); err != nil {
				return err
			}
		}
		fmt.Fprint(rw, "E\n")
		if err := checkErr(rw); err != nil {
			return err
		}
	case 0:
		f, err := fsys.Open(name)
		if err != nil {
			return err
		}
		if err := pushFile(rw, f); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported file type: %s", info.Mode().Type())
	}

	return nil
}

func checkErr(r io.Reader) error {
	buf := make([]byte, 1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if buf[0] != 0 {
		return fmt.Errorf("received error code: %d", buf[0])
	}
	return nil
}
