package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	importMaxFiles = 5000
	importMaxBytes = 256 << 20
)

// importChatFiles copies a chat's workspace into a new "chat-<id>" directory
// at the top of targetRoot (a project), for "move this chat to a project".
// Symlinks, special files and any .git are left behind: a copied .git would
// bring its hooks into a project, which the tools refuse to write for the same
// reason. It returns the directory's name inside the project.
func importChatFiles(chatRoot, targetRoot, sessionID string) (dir string, files int, bytes int64, err error) {
	type entry struct {
		rel  string
		mode fs.FileMode
		dir  bool
	}
	var list []entry
	err = filepath.WalkDir(chatRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(chatRoot, path)
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			list = append(list, entry{rel: rel, dir: true})
		case info.Mode().IsRegular():
			if files++; files > importMaxFiles {
				return fmt.Errorf("more than %d files", importMaxFiles)
			}
			if bytes += info.Size(); bytes > importMaxBytes {
				return fmt.Errorf("more than %d MB", importMaxBytes>>20)
			}
			list = append(list, entry{rel: rel, mode: info.Mode()})
		}
		return nil
	})
	if err != nil {
		return "", 0, 0, fmt.Errorf("workspace is too large to move: %w", err)
	}
	if files == 0 {
		return "", 0, 0, errors.New("the chat's workspace has no files")
	}

	name := "chat-" + sessionID
	dst := filepath.Join(targetRoot, name)
	for i := 2; ; i++ {
		err := os.Mkdir(dst, 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || i > 99 {
			return "", 0, 0, err
		}
		name = fmt.Sprintf("chat-%s-%d", sessionID, i)
		dst = filepath.Join(targetRoot, name)
	}
	fail := func(err error) (string, int, int64, error) {
		_ = os.RemoveAll(dst)
		return "", 0, 0, err
	}
	for _, e := range list {
		to := filepath.Join(dst, e.rel)
		if e.dir {
			if err := os.MkdirAll(to, 0o755); err != nil {
				return fail(err)
			}
			continue
		}
		perm := fs.FileMode(0o644)
		if e.mode&0o111 != 0 {
			perm = 0o755
		}
		if err := copyFile(filepath.Join(chatRoot, e.rel), to, perm); err != nil {
			return fail(err)
		}
	}
	return name, files, bytes, nil
}

func copyFile(from, to string, perm fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
