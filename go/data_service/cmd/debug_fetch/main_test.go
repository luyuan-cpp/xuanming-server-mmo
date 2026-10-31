package main

import (
	"encoding/base64"
	"testing"
)

func TestJSONValueKeepsBinaryReadable(t *testing.T) {
	if got := jsonValue([]byte("seller")); got != "seller" {
		t.Fatalf("text column: %v", got)
	}
	binary := []byte{0xff, 0x00, 0x01}
	if got := jsonValue(binary); got != base64.StdEncoding.EncodeToString(binary) {
		t.Fatalf("binary column: %v", got)
	}
	if got := jsonValue(int64(7)); got != int64(7) {
		t.Fatalf("number column: %v", got)
	}
}

func TestRunRefusesMissingArguments(t *testing.T) {
	for name, o := range map[string]options{
		"no dsn":    {player: 1, reason: "r", out: "o"},
		"no player": {dsnEnv: "D", reason: "r", out: "o"},
		"no reason": {dsnEnv: "D", player: 1, out: "o"},
		"no out":    {dsnEnv: "D", player: 1, reason: "r"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(o); err == nil {
				t.Fatal("must be refused")
			}
		})
	}
	t.Run("empty dsn variable", func(t *testing.T) {
		t.Setenv("DEBUG_FETCH_TEST_EMPTY", "")
		if err := run(options{dsnEnv: "DEBUG_FETCH_TEST_EMPTY", player: 1, reason: "r", out: "o"}); err == nil {
			t.Fatal("must be refused")
		}
	})
}
