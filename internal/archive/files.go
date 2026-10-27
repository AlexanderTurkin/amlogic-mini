package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func openRegular(path string) (*os.File, error) {
	if err := EnsureRegular(path); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.Base(path))
}

func HashFile(ctx context.Context, path string) (string, int64, error) {
	f, err := openRegular(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	buf := make([]byte, 256<<10)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", size, err
		}
		n, err := f.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			size += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", size, err
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), size, nil
}

func WriteFile(root, name string, data []byte) error {
	if !SafeName(name) {
		return fmt.Errorf("unsafe backup filename")
	}
	f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

func ReplaceFile(root, name string, data []byte) (err error) {
	if !SafeName(name) {
		return fmt.Errorf("unsafe backup filename")
	}
	target := filepath.Join(root, name)
	if _, e := os.Lstat(target); e == nil {
		if err := EnsureRegular(target); err != nil {
			return err
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, err := os.CreateTemp(root, ".amlogic-mini-*")
	if err != nil {
		return err
	}
	defer func() {
		removeErr := os.Remove(f.Name())
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), target); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	} // Directory fsync is unavailable through os.File on Windows.
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ReadExact captures a fixed-size snapshot and checks EOF so growth is rejected.
func ReadExact(ctx context.Context, source io.Reader, size int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, fmt.Errorf("invalid snapshot size")
	}
	data := make([]byte, size+1)
	n, err := io.ReadFull(source, data)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(n) != size {
		return nil, fmt.Errorf("snapshot size changed: got %d bytes, expected %d", n, size)
	}
	return data[:n:n], nil
}
