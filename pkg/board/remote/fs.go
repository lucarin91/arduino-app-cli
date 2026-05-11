package remote

import (
	"fmt"
	"io/fs"
	"path"
)

// CopyFS copies the file system fsys into a remote directory dir using the provided RemoteConn.
//
// Copying stops at and returns the first error encountered.
func CopyFS(conn RemoteConn, dir string, fsys fs.FS) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		newPath := path.Join(dir, p)
		switch d.Type() {
		case fs.ModeDir:
			return conn.MkDirAll(newPath)
		case fs.ModeSymlink:
			// FIXME: implement symbolic link support
			return fmt.Errorf("CopyFS: symbolic links not supported")
		case 0:
			r, err := fsys.Open(p)
			if err != nil {
				return err
			}
			defer r.Close()
			// FIXME: support permission fowarding
			return conn.WriteFile(r, newPath)
		default:
			return &fs.PathError{Op: "CopyFS", Path: p, Err: fs.ErrInvalid}
		}
	})
}
