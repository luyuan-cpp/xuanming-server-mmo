package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteNewDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := writeNew(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(path, []byte("second")); err == nil {
		t.Fatal("must not overwrite an existing file")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "first" {
		t.Fatalf("file = %q, %v", got, err)
	}
}

// 不带 -authority-phase 一律拒绝,不做任何事。
func TestRunRefusesWithoutAuthorityPhase(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), options{}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(errOut.String(), "-authority-phase is required") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunAuthorityPhaseRequiresAbsoluteOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), options{authorityPhase: "read", authorityOutput: "relative.json"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d", code)
	}
}
