package lexstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// ErrConflict 发布时指针已被别人换过(两个人同时发布)。重新读一遍再发。
var ErrConflict = errors.New("lexstore: the lexicon pointer changed while publishing; reload and publish again")

// Loaded 是从 etcd 读到的一份词库。Text 为 nil 表示 etcd 里还没有词库。
type Loaded struct {
	Pointer Pointer
	Text    []byte
	// Revision 是读指针那一刻的 etcd revision,watch 从它的下一个开始。
	Revision int64
}

// Load 读指针和它指向的那一版正文,并按指针核对。指针或分块有问题返回 ErrCorrupt。
func Load(ctx context.Context, cli *clientv3.Client, prefix string) (Loaded, error) {
	resp, err := cli.Get(ctx, PointerKey(prefix))
	if err != nil {
		return Loaded{}, fmt.Errorf("lexstore: get pointer: %w", err)
	}
	loaded := Loaded{Revision: resp.Header.Revision}
	if len(resp.Kvs) == 0 {
		return loaded, nil
	}
	pointer, err := ParsePointer(resp.Kvs[0].Value)
	if err != nil {
		return Loaded{}, err
	}
	chunks := make([][]byte, 0, pointer.Chunks)
	for i := 0; i < pointer.Chunks; i++ {
		chunk, err := cli.Get(ctx, ChunkKey(prefix, pointer.Version, i))
		if err != nil {
			return Loaded{}, fmt.Errorf("lexstore: get chunk %d of version %d: %w", i, pointer.Version, err)
		}
		if len(chunk.Kvs) == 0 {
			return Loaded{}, fmt.Errorf("%w: version %d is missing chunk %d", ErrCorrupt, pointer.Version, i)
		}
		chunks = append(chunks, chunk.Kvs[0].Value)
	}
	text, err := Decode(pointer, chunks)
	if err != nil {
		return Loaded{}, err
	}
	loaded.Pointer = pointer
	loaded.Text = text
	return loaded, nil
}

// Publish 把一份词库正文发布成新版本:写分块,再带条件换指针。返回新指针。
//
// 新版本号 = 现有最大版本号 + 1,连上次发布失败留下的分块也算在内,所以不会写进一个半成品的版本目录。
// 指针在读出之后被别人换过就不换,返回 ErrConflict;这次写的分块留着,由后面的 Prune 清掉。
// entries 只写进指针做展示;by 是发布人,必填。
func Publish(ctx context.Context, cli *clientv3.Client, prefix string, text []byte, entries int, by string, now time.Time) (Pointer, error) {
	if strings.TrimSpace(by) == "" {
		return Pointer{}, errors.New("lexstore: publisher name is required")
	}
	chunks, sum, err := Encode(text)
	if err != nil {
		return Pointer{}, err
	}
	current, err := cli.Get(ctx, PointerKey(prefix))
	if err != nil {
		return Pointer{}, fmt.Errorf("lexstore: get pointer: %w", err)
	}
	var modRevision int64
	var latest uint64
	if len(current.Kvs) > 0 {
		modRevision = current.Kvs[0].ModRevision
		// 现有指针坏了也要能发布(发布正是修复手段),所以解析失败只是不参考它的版本号。
		if previous, parseErr := ParsePointer(current.Kvs[0].Value); parseErr == nil {
			latest = previous.Version
		}
	}
	versions, err := ListVersions(ctx, cli, prefix)
	if err != nil {
		return Pointer{}, err
	}
	if n := len(versions); n > 0 && versions[n-1] > latest {
		latest = versions[n-1]
	}
	pointer := Pointer{
		Version:   latest + 1,
		Chunks:    len(chunks),
		SHA256:    sum,
		Bytes:     len(text),
		Entries:   entries,
		UpdatedBy: by,
		UpdatedMs: now.UnixMilli(),
	}
	raw, err := json.Marshal(pointer)
	if err != nil {
		return Pointer{}, err
	}
	for i, chunk := range chunks {
		if _, err = cli.Put(ctx, ChunkKey(prefix, pointer.Version, i), string(chunk)); err != nil {
			return Pointer{}, fmt.Errorf("lexstore: put chunk %d of version %d: %w", i, pointer.Version, err)
		}
	}
	swapped, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(PointerKey(prefix)), "=", modRevision)).
		Then(clientv3.OpPut(PointerKey(prefix), string(raw))).
		Commit()
	if err != nil {
		return Pointer{}, fmt.Errorf("lexstore: swap pointer: %w", err)
	}
	if !swapped.Succeeded {
		return Pointer{}, ErrConflict
	}
	return pointer, nil
}

// ListVersions 返回 etcd 里有分块的版本号,升序。
func ListVersions(ctx context.Context, cli *clientv3.Client, prefix string) ([]uint64, error) {
	root := VersionsPrefix(prefix)
	resp, err := cli.Get(ctx, root, clientv3.WithPrefix(), clientv3.WithKeysOnly())
	if err != nil {
		return nil, fmt.Errorf("lexstore: list versions: %w", err)
	}
	seen := map[uint64]bool{}
	for _, kv := range resp.Kvs {
		rest := strings.TrimPrefix(string(kv.Key), root)
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			continue
		}
		version, parseErr := strconv.ParseUint(rest[:slash], 10, 64)
		if parseErr != nil || version == 0 {
			continue
		}
		seen[version] = true
	}
	versions := make([]uint64, 0, len(seen))
	for version := range seen {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(a, b int) bool { return versions[a] < versions[b] })
	return versions, nil
}

// Prune 删掉比 current 早 KeepVersions 版以上的分块,以及比 current 新的分块(发布失败留下的半成品)。
// current 是刚发布成功的版本号。返回删掉的版本号。
func Prune(ctx context.Context, cli *clientv3.Client, prefix string, current uint64) ([]uint64, error) {
	versions, err := ListVersions(ctx, cli, prefix)
	if err != nil {
		return nil, err
	}
	var removed []uint64
	for _, version := range versions {
		if version <= current && version+KeepVersions > current {
			continue
		}
		if _, err = cli.Delete(ctx, ChunkPrefix(prefix, version), clientv3.WithPrefix()); err != nil {
			return removed, fmt.Errorf("lexstore: delete version %d: %w", version, err)
		}
		removed = append(removed, version)
	}
	return removed, nil
}

// ReadVersion 读出某个还留着分块的旧版本的正文(回滚用)。旧版本没有自己的指针,只靠 gzip 自带的校验。
func ReadVersion(ctx context.Context, cli *clientv3.Client, prefix string, version uint64) ([]byte, error) {
	resp, err := cli.Get(ctx, ChunkPrefix(prefix, version), clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("lexstore: get version %d: %w", version, err)
	}
	if len(resp.Kvs) == 0 {
		return nil, fmt.Errorf("lexstore: version %d has no chunks in etcd (pruned, or never published)", version)
	}
	// 带前缀的 Get 缺省按键升序返回;序号四位定宽,所以就是块的顺序。逐个核对序号,缺块就报错。
	chunks := make([][]byte, 0, len(resp.Kvs))
	for i, kv := range resp.Kvs {
		if string(kv.Key) != ChunkKey(prefix, version, i) {
			return nil, fmt.Errorf("%w: version %d chunk %d is missing", ErrCorrupt, version, i)
		}
		chunks = append(chunks, kv.Value)
	}
	text, err := inflate(chunks, MaxTextBytes)
	if err != nil {
		return nil, fmt.Errorf("version %d: %w", version, err)
	}
	if len(text) == 0 || len(text) > MaxTextBytes {
		return nil, fmt.Errorf("%w: version %d decompressed to %d bytes", ErrCorrupt, version, len(text))
	}
	return text, nil
}
