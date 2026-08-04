package main

import (
	"strings"
	"testing"
)

func TestReadPasswordLinesRequiresExactlyTwoMatchingLines(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "matching two lines", input: "secret\nsecret\n", want: "secret"},
		{name: "one line", input: "secret\n", wantErr: true},
		{name: "three lines", input: "secret\nsecret\nignored\n", wantErr: true},
		{name: "mismatch", input: "secret\ndifferent\n", wantErr: true},
		{name: "empty but exactly two", input: "\n\n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readPasswordLines(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("readPasswordLines error = %v, wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("password = %q, want %q", got, tt.want)
			}
		})
	}
}
