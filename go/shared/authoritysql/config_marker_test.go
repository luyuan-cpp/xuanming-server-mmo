package authoritysql

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type markerFileFixture struct {
	n                           int
	writeErr, syncErr, closeErr error
	calls                       string
}

func (f *markerFileFixture) Write([]byte) (int, error) {
	f.calls += "写"
	return f.n, f.writeErr
}

func (f *markerFileFixture) Sync() error {
	f.calls += "同步"
	return f.syncErr
}

func (f *markerFileFixture) Close() error {
	f.calls += "关闭"
	return f.closeErr
}

func TestGrantMarkerRequiresCompleteWriteSyncAndClose(t *testing.T) {
	writeFailure, syncFailure, closeFailure := errors.New("写失败"), errors.New("同步失败"), errors.New("关闭失败")
	for _, test := range []struct {
		name string
		file markerFileFixture
		want []error
	}{
		{"完整提交", markerFileFixture{n: 3}, nil},
		{"短写", markerFileFixture{n: 2}, []error{io.ErrShortWrite}},
		{"写失败仍收尾", markerFileFixture{n: 0, writeErr: writeFailure}, []error{writeFailure, io.ErrShortWrite}},
		{"同步失败", markerFileFixture{n: 3, syncErr: syncFailure}, []error{syncFailure}},
		{"关闭失败", markerFileFixture{n: 3, closeErr: closeFailure}, []error{closeFailure}},
		{"所有错误保留", markerFileFixture{n: 1, writeErr: writeFailure, syncErr: syncFailure, closeErr: closeFailure}, []error{writeFailure, io.ErrShortWrite, syncFailure, closeFailure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := writeGrantMarker(&test.file, []byte("id\n"))
			if (err == nil) != (len(test.want) == 0) {
				t.Fatalf("提交结局=%v，期望错误=%v", err, test.want)
			}
			for _, want := range test.want {
				if !errors.Is(err, want) {
					t.Errorf("缺少错误%v，实际=%v", want, err)
				}
			}
			if test.file.calls != "写同步关闭" {
				t.Fatalf("资源未完整收尾：%s", test.file.calls)
			}
		})
	}
}

func TestPrivateControlFileKeepsOriginalAndBoundsReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original.bin")
	if err := WritePrivateControlFile(path, []byte("binding")); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivateControlFile(path, []byte("changed")); err == nil {
		t.Fatal("第二次创建覆盖了原件")
	}
	raw, err := ReadPrivateControlFile(path, 7)
	if err != nil || string(raw) != "binding" {
		t.Fatalf("原件变化: %q %v", raw, err)
	}
	if raw, err = ReadPrivateControlFile(path, 6); err == nil || raw != nil {
		t.Fatal("越过读取预算")
	}
	if err = WritePrivateControlFile(filepath.Join(filepath.Dir(path), "empty.bin"), nil); err == nil {
		t.Fatal("接受空控制原件")
	}
	if _, err = os.Stat(filepath.Join(filepath.Dir(path), "empty.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("校验失败仍创建文件")
	}
}
func TestPrivateControlPathRejectsRelativeAndAlternateStream(t *testing.T) {
	for _, path := range []string{"relative.bin", filepath.Join(t.TempDir(), "file:stream"), filepath.Join(t.TempDir(), "file.")} {
		if _, _, err := privateControlPath(path); err == nil {
			t.Fatalf("接受歧义路径: %q", path)
		}
	}
}

func TestUnixGrantConsumptionRejectsSharedWritableDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	} // Windows的真实DACL检查由同平台专门用例覆盖。
	dir := t.TempDir()
	if err := os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if err := consumeGrantMarker(dir, "grant.consumed", []byte("fixed")); err == nil {
		t.Fatal("同组可替换消费目录，允许删除marker后重放")
	}
	if _, err := os.Stat(filepath.Join(dir, "grant.consumed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("目录校验失败仍创建消费原件")
	}
}
