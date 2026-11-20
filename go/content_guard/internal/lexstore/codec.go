// Package lexstore 把词库存进 etcd 并从 etcd 取出来(docs/design/rename-and-content-safety.md §6.4)。
//
// 键的布局(prefix 缺省是 DefaultPrefix):
//
//	<prefix>current              指针:当前版本、块数、校验和、谁在什么时候发布的(JSON)
//	<prefix>v/<版本>/<四位序号>   这一版词库的正文:gzip 压缩后切成不超过 ChunkBytes 的块
//
// 发布 = 先写完新版本的全部分块,再用一次带条件的写入换指针。读的一方只看指针:指针没换之前,
// 新版本的分块写了一半也不会被任何人读到。指针换了之后旧版本的分块还留着几版,回滚只要把指针指回去。
//
// 本文件是纯计算(压缩、切块、校验),etcd 的读写在 store.go,监听在 watcher.go。
package lexstore

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// DefaultPrefix 是词库在 etcd 里的缺省前缀。
	DefaultPrefix = "/mmorpg/content_guard/lexicon/"

	// ChunkBytes 是单个分块的上限。etcd 单次请求缺省上限约 1.5 MiB,留足余量。
	ChunkBytes = 512 * 1024

	// MaxChunks 是一个版本的块数上限(对应压缩后约 128 MiB),只为挡住坏指针。
	MaxChunks = 256

	// MaxTextBytes 是解压后正文的上限。指针里的 bytes 超过它直接拒收,解压时也按它截断,不被压缩炸弹撑爆内存。
	MaxTextBytes = 64 * 1024 * 1024

	// KeepVersions 是发布之后保留的版本数(含当前版本),更早的分块被清掉。
	KeepVersions = 3
)

// ErrCorrupt 指针或分块与预期不符:块数不对、校验和不对、解压失败、大小越界。
var ErrCorrupt = errors.New("lexstore: lexicon in etcd is corrupt or incomplete")

// Pointer 是 <prefix>current 的内容。
type Pointer struct {
	Version   uint64 `json:"version"`    // 从 1 起,每次发布加 1
	Chunks    int    `json:"chunks"`     // 这一版的块数
	SHA256    string `json:"sha256"`     // 解压后正文的 SHA-256(小写十六进制)
	Bytes     int    `json:"bytes"`      // 解压后正文的字节数
	Entries   int    `json:"entries"`    // 词条数(拒绝词加白名单),只为展示
	UpdatedBy string `json:"updated_by"` // 发布人,发布工具的 -by
	UpdatedMs int64  `json:"updated_ms"` // 发布时刻(Unix 毫秒,发布工具所在机器的时钟)
}

// Validate 核对指针自身是否合理。
func (p Pointer) Validate() error {
	if p.Version == 0 {
		return fmt.Errorf("%w: pointer version is 0", ErrCorrupt)
	}
	if p.Chunks < 1 || p.Chunks > MaxChunks {
		return fmt.Errorf("%w: pointer chunks=%d out of [1, %d]", ErrCorrupt, p.Chunks, MaxChunks)
	}
	if p.Bytes < 1 || p.Bytes > MaxTextBytes {
		return fmt.Errorf("%w: pointer bytes=%d out of [1, %d]", ErrCorrupt, p.Bytes, MaxTextBytes)
	}
	if len(p.SHA256) != sha256.Size*2 || strings.ToLower(p.SHA256) != p.SHA256 {
		return fmt.Errorf("%w: pointer sha256 is not 64 lowercase hex characters", ErrCorrupt)
	}
	if _, err := hex.DecodeString(p.SHA256); err != nil {
		return fmt.Errorf("%w: pointer sha256 is not hex", ErrCorrupt)
	}
	return nil
}

// ParsePointer 解析并核对指针。
func ParsePointer(raw []byte) (Pointer, error) {
	var p Pointer
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pointer{}, fmt.Errorf("%w: pointer is not JSON: %v", ErrCorrupt, err)
	}
	if err := p.Validate(); err != nil {
		return Pointer{}, err
	}
	return p, nil
}

// PointerKey 返回指针键。
func PointerKey(prefix string) string { return prefix + "current" }

// VersionsPrefix 返回全部版本分块的公共前缀。
func VersionsPrefix(prefix string) string { return prefix + "v/" }

// ChunkPrefix 返回某个版本全部分块的前缀。
func ChunkPrefix(prefix string, version uint64) string {
	return fmt.Sprintf("%s%d/", VersionsPrefix(prefix), version)
}

// ChunkKey 返回某个版本第 index 块的键。四位定宽,按键排序就是块的顺序。
func ChunkKey(prefix string, version uint64, index int) string {
	return fmt.Sprintf("%s%04d", ChunkPrefix(prefix, version), index)
}

// Encode 把词库正文压缩并切块,返回各块和正文的 SHA-256。
func Encode(text []byte) (chunks [][]byte, sum string, err error) {
	if len(text) == 0 || len(text) > MaxTextBytes {
		return nil, "", fmt.Errorf("lexstore: lexicon text is %d bytes, must be in [1, %d]", len(text), MaxTextBytes)
	}
	var packed bytes.Buffer
	writer, err := gzip.NewWriterLevel(&packed, gzip.BestCompression)
	if err != nil {
		return nil, "", err
	}
	if _, err = writer.Write(text); err != nil {
		return nil, "", err
	}
	if err = writer.Close(); err != nil {
		return nil, "", err
	}
	data := packed.Bytes()
	for len(data) > 0 {
		size := len(data)
		if size > ChunkBytes {
			size = ChunkBytes
		}
		chunks = append(chunks, data[:size])
		data = data[size:]
	}
	if len(chunks) > MaxChunks {
		return nil, "", fmt.Errorf("lexstore: lexicon needs %d chunks, the limit is %d", len(chunks), MaxChunks)
	}
	digest := sha256.Sum256(text)
	return chunks, hex.EncodeToString(digest[:]), nil
}

// Decode 把分块拼回正文,并按指针核对块数、大小和校验和。任何一项不符都返回 ErrCorrupt。
func Decode(pointer Pointer, chunks [][]byte) ([]byte, error) {
	if len(chunks) != pointer.Chunks {
		return nil, fmt.Errorf("%w: version %d has %d chunks, pointer says %d", ErrCorrupt, pointer.Version, len(chunks), pointer.Chunks)
	}
	if pointer.Bytes < 1 || pointer.Bytes > MaxTextBytes {
		return nil, fmt.Errorf("%w: pointer bytes=%d out of [1, %d]", ErrCorrupt, pointer.Bytes, MaxTextBytes)
	}
	text, err := inflate(chunks, pointer.Bytes)
	if err != nil {
		return nil, fmt.Errorf("version %d: %w", pointer.Version, err)
	}
	if len(text) != pointer.Bytes {
		return nil, fmt.Errorf("%w: version %d decompressed to a different size than the pointer says (%d)", ErrCorrupt, pointer.Version, pointer.Bytes)
	}
	digest := sha256.Sum256(text)
	if hex.EncodeToString(digest[:]) != pointer.SHA256 {
		return nil, fmt.Errorf("%w: version %d checksum mismatch", ErrCorrupt, pointer.Version)
	}
	return text, nil
}

// inflate 把分块拼起来解压,最多读 limit+1 字节:正文比预期长也能被调用方发现,而不是悄悄截断或撑爆内存。
func inflate(chunks [][]byte, limit int) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(bytes.Join(chunks, nil)))
	if err != nil {
		return nil, fmt.Errorf("%w: not gzip: %v", ErrCorrupt, err)
	}
	defer reader.Close()
	text, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: failed to decompress: %v", ErrCorrupt, err)
	}
	return text, nil
}
