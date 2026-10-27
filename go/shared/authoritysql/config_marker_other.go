//go:build !windows

package authoritysql

import (
	"errors"
	"os"
	"path/filepath"
)

// 非Windows保留原目录fsync提交点；失败后不删除或覆盖已创建的marker。
func consumeGrantMarker(dir, name string, content []byte) error {
	if err := checkPrivateControlDirectory(dir); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = writeGrantMarker(file, content); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := parent.Sync()
	return errors.Join(syncErr, parent.Close())
}

func checkPrivateControlDirectory(dir string) error {
	for current := dir; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("控制目录含symlink或不是目录")
		}
		// /tmp的sticky父目录可承载本账号私有子目录；最终目录本身绝不允许共享写。
		if info.Mode().Perm()&0022 != 0 && (current == dir || info.Mode()&os.ModeSticky == 0) {
			return errors.New("控制目录可被其他主体改写")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func writePrivateControlFile(dir, name string, content []byte) error {
	if err := checkPrivateControlDirectory(dir); err != nil {
		return err
	}
	return consumeGrantMarker(dir, name, content)
}
func readPrivateControlFile(dir, name string, maxBytes int64) ([]byte, error) {
	if err := checkPrivateControlDirectory(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("控制文件必须为本账号私有普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !os.SameFile(info, actual) || actual.Mode().Perm()&0077 != 0 {
		return nil, errors.Join(errors.New("控制文件打开时发生替换"), file.Close())
	}
	return readControlBytes(file, maxBytes)
}
