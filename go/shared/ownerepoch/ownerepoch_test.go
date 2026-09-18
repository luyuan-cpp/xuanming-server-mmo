package ownerepoch

import (
	"errors"
	"testing"
)

// 键名是与 C++ 的跨语言契约,拼错一个字符两边就各写各的、且没有任何报错。
func TestKeyFormats(t *testing.T) {
	if got := OwnerEpochKey(12345); got != "player:12345:owner_epoch" {
		t.Fatalf("OwnerEpochKey = %q", got)
	}
	if got := HandoffKey(12345); got != "player:12345:handoff" {
		t.Fatalf("HandoffKey = %q", got)
	}
}

func TestParseEpoch(t *testing.T) {
	cases := []struct {
		raw     string
		want    uint64
		wantErr bool
	}{
		{raw: "", want: 0},
		{raw: "0", want: 0},
		{raw: "7", want: 7},
		{raw: "18446744073709551615", want: 18446744073709551615},
		{raw: "-1", wantErr: true},
		{raw: "abc", wantErr: true},
		{raw: "1:2", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseEpoch(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseEpoch(%q) expected error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEpoch(%q) unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseEpoch(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestParseHandoffRoundTrip(t *testing.T) {
	h := Handoff{Epoch: 42, SavedAtMs: 1757000000123}
	raw := h.String()
	if raw != "42:1757000000123" {
		t.Fatalf("Handoff.String = %q", raw)
	}
	parsed, err := ParseHandoff(raw)
	if err != nil {
		t.Fatalf("ParseHandoff(%q) error: %v", raw, err)
	}
	if parsed != h {
		t.Fatalf("ParseHandoff(%q) = %+v, want %+v", raw, parsed, h)
	}
}

// 「没写」与「写坏」必须能分开:前者是正常的进行中状态,后者是需要排障的异常。
// 两者对放行判定的结论一样(都不放行),但日志级别与指标不同。
func TestParseHandoffDistinguishesMissingFromMalformed(t *testing.T) {
	if _, err := ParseHandoff(""); !errors.Is(err, ErrNoHandoff) {
		t.Fatalf("empty marker: err = %v, want ErrNoHandoff", err)
	}
	for _, raw := range []string{"42", "42:", ":123", "a:b", "42:x", "-1:5", "1:2:3"} {
		_, err := ParseHandoff(raw)
		if !errors.Is(err, ErrMalformedHandoff) {
			t.Errorf("ParseHandoff(%q): err = %v, want ErrMalformedHandoff", raw, err)
		}
		if errors.Is(err, ErrNoHandoff) {
			t.Errorf("ParseHandoff(%q) must not be reported as missing", raw)
		}
	}
}
