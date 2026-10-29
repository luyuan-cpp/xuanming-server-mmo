package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidPrivateFileCommandsHaveNoOutputOrSideEffect(t *testing.T) {
	target := filepath.Join(t.TempDir(), "must-not-exist.bin")
	for _, args := range [][]string{
		{"--command", "delete-file", "--path", target, "--max-bytes", "3"},
		{"--command", "create-file", "--path", target, "--max-bytes", "3"},
		{"--command", "create-file", "--path", target, "--max-bytes", "0"},
		{"--command", "create-file", "--path", "relative", "--max-bytes", "3"},
		{"--command", "create-file", "--path", target, "--max-bytes", "3", "extra"},
	} {
		var out bytes.Buffer
		if err := run(args, bytes.NewBufferString("four"), &out); err == nil {
			t.Fatalf("非法操作成功: %v", args)
		}
		if out.Len() != 0 {
			t.Fatal("失败响应泄漏正文")
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatal("失败操作产生了文件")
		}
	}
}
