package lexstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// incompressible 生成一段压不动的确定性数据,让压缩后的大小超过一个分块。
func incompressible(size int) []byte {
	data := make([]byte, size)
	rand.New(rand.NewSource(20261119)).Read(data)
	return data
}

func pointerFor(t *testing.T, text []byte) (Pointer, [][]byte) {
	t.Helper()
	chunks, sum, err := Encode(text)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return Pointer{Version: 3, Chunks: len(chunks), SHA256: sum, Bytes: len(text), UpdatedBy: "tester", UpdatedMs: 1}, chunks
}

func TestEncodeDecodeRoundTripAcrossChunks(t *testing.T) {
	text := incompressible(ChunkBytes*2 + 1000)
	pointer, chunks := pointerFor(t, text)
	if len(chunks) < 3 {
		t.Fatalf("expected the data to span at least 3 chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) == 0 || len(chunk) > ChunkBytes {
			t.Fatalf("chunk %d has %d bytes, the limit is %d", i, len(chunk), ChunkBytes)
		}
	}
	if err := pointer.Validate(); err != nil {
		t.Fatalf("pointer built from Encode must validate: %v", err)
	}
	got, err := Decode(pointer, chunks)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got, text) {
		t.Fatalf("round trip changed the text")
	}
}

func TestDecodeRejectsMismatches(t *testing.T) {
	text := []byte("#content_guard_lexicon 1\nD\tad\tsub\tfoo\n")
	pointer, chunks := pointerFor(t, text)

	missing := pointer
	missing.Chunks = len(chunks) + 1
	wrongSum := pointer
	wrongSum.SHA256 = strings.Repeat("0", 64)
	shorter := pointer
	shorter.Bytes = len(text) - 1
	longer := pointer
	longer.Bytes = len(text) + 1
	garbage := [][]byte{[]byte("not gzip at all")}

	cases := []struct {
		name    string
		pointer Pointer
		chunks  [][]byte
	}{
		{"chunk count", missing, chunks},
		{"checksum", wrongSum, chunks},
		{"text longer than declared", shorter, chunks},
		{"text shorter than declared", longer, chunks},
		{"not gzip", pointer, garbage},
	}
	for _, c := range cases {
		if _, err := Decode(c.pointer, c.chunks); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Decode error = %v, want ErrCorrupt", c.name, err)
		}
	}
}

func TestEncodeRejectsEmptyText(t *testing.T) {
	if _, _, err := Encode(nil); err == nil {
		t.Fatalf("Encode accepted empty text")
	}
}

func TestParsePointerValidation(t *testing.T) {
	good := Pointer{Version: 1, Chunks: 1, SHA256: strings.Repeat("ab", 32), Bytes: 10, UpdatedBy: "x", UpdatedMs: 1}
	raw, err := json.Marshal(good)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	parsed, err := ParsePointer(raw)
	if err != nil || parsed != good {
		t.Fatalf("ParsePointer(good) = %+v, %v", parsed, err)
	}

	mutate := func(change func(*Pointer)) []byte {
		p := good
		change(&p)
		out, _ := json.Marshal(p)
		return out
	}
	bad := map[string][]byte{
		"not json":       []byte("{"),
		"version zero":   mutate(func(p *Pointer) { p.Version = 0 }),
		"no chunks":      mutate(func(p *Pointer) { p.Chunks = 0 }),
		"too many":       mutate(func(p *Pointer) { p.Chunks = MaxChunks + 1 }),
		"zero bytes":     mutate(func(p *Pointer) { p.Bytes = 0 }),
		"oversized":      mutate(func(p *Pointer) { p.Bytes = MaxTextBytes + 1 }),
		"short sha":      mutate(func(p *Pointer) { p.SHA256 = "abcd" }),
		"uppercase sha":  mutate(func(p *Pointer) { p.SHA256 = strings.Repeat("AB", 32) }),
		"non-hex sha":    mutate(func(p *Pointer) { p.SHA256 = strings.Repeat("zz", 32) }),
		"empty document": []byte("{}"),
	}
	for name, raw := range bad {
		if _, err := ParsePointer(raw); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: ParsePointer error = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestKeyLayout(t *testing.T) {
	prefix := "/mmorpg/content_guard/lexicon/"
	if got := PointerKey(prefix); got != "/mmorpg/content_guard/lexicon/current" {
		t.Errorf("PointerKey = %q", got)
	}
	if got := ChunkPrefix(prefix, 12); got != "/mmorpg/content_guard/lexicon/v/12/" {
		t.Errorf("ChunkPrefix = %q", got)
	}
	if got := ChunkKey(prefix, 12, 3); got != "/mmorpg/content_guard/lexicon/v/12/0003" {
		t.Errorf("ChunkKey = %q", got)
	}
	// 版本 1 的前缀不能是版本 12 的前缀的前缀:按前缀删一个版本时不能连带删掉别的版本。
	if strings.HasPrefix(ChunkKey(prefix, 12, 0), ChunkPrefix(prefix, 1)) {
		t.Errorf("version 1 prefix must not cover version 12 keys")
	}
	if DefaultPrefix != prefix {
		t.Errorf("DefaultPrefix = %q, the design document and the service yaml say %q", DefaultPrefix, prefix)
	}
}
