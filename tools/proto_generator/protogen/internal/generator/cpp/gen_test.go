package cpp

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	_config "protogen/internal/config"
	"protogen/logger"

	"go.uber.org/zap"
)

func TestCollectDomainCppProtoFilesDeduplicatesSortsAndSkipsMissing(t *testing.T) {
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previousLogger })

	root := t.TempDir()
	writeProto := func(rel string) string {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("syntax = \"proto3\";\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}

	fileA := writeProto(filepath.Join("a", "alpha.proto"))
	fileB := writeProto(filepath.Join("b", "beta.proto"))

	domains := map[string]_config.DomainMeta{
		"all":     {Source: root},
		"a":       {Source: filepath.Join(root, "a")},
		"b":       {Source: filepath.Join(root, "b")},
		"missing": {Source: filepath.Join(root, "does_not_exist")},
	}

	got, err := collectDomainCppProtoFiles(domains)
	if err != nil {
		t.Fatalf("collectDomainCppProtoFiles: %v", err)
	}
	want := []string{fileA, fileB}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestShardProtoFilesBySizeBalancesAndIsDeterministic(t *testing.T) {
	if got := shardProtoFilesBySize(nil, 4, func(string) int64 { return 1 }); got != nil {
		t.Fatalf("empty input should return nil, got %v", got)
	}

	files := []string{"d.proto", "a.proto", "c.proto", "b.proto", "e.proto"}
	sizes := map[string]int64{
		"a.proto": 100,
		"b.proto": 90,
		"c.proto": 80,
		"d.proto": 70,
		"e.proto": 60,
	}
	sizeOf := func(p string) int64 { return sizes[p] }

	// 单分片保持全部文件在一个批次内且有序。
	single := shardProtoFilesBySize(files, 1, sizeOf)
	if len(single) != 1 || !reflect.DeepEqual(single[0], []string{"a.proto", "b.proto", "c.proto", "d.proto", "e.proto"}) {
		t.Fatalf("single shard mismatch: %v", single)
	}

	// maxShards 大于文件数时，分片数缩减为文件数且不产生空分片。
	capped := shardProtoFilesBySize([]string{"b.proto", "a.proto"}, 8, sizeOf)
	if len(capped) != 2 {
		t.Fatalf("expected 2 shards when only 2 files, got %v", capped)
	}

	// 2 分片 LPT 均衡：a(100)+d(70)=170 vs b(90)+c(80)+e(60)=230 或 a(100)+d(70)+e(60)=230 vs b(90)+c(80)=170；
	// 关键是无遗漏无重复、确定性输出且最大桶显著低于全量串行总重(400)。
	shards := shardProtoFilesBySize(files, 2, sizeOf)
	if len(shards) != 2 {
		t.Fatalf("expected 2 shards, got %d", len(shards))
	}
	var flattened []string
	var maxWeight int64
	for _, shard := range shards {
		var w int64
		for _, f := range shard {
			w += sizes[f]
			flattened = append(flattened, f)
		}
		if w > maxWeight {
			maxWeight = w
		}
	}
	sort.Strings(flattened)
	if !reflect.DeepEqual(flattened, []string{"a.proto", "b.proto", "c.proto", "d.proto", "e.proto"}) {
		t.Fatalf("sharded files mismatch: %v", flattened)
	}
	if maxWeight > 230 {
		t.Fatalf("LPT balance worse than expected: maxWeight=%d, shards=%v", maxWeight, shards)
	}

	// 重复调用输出必须完全一致。
	again := shardProtoFilesBySize(files, 2, sizeOf)
	if !reflect.DeepEqual(shards, again) {
		t.Fatalf("non-deterministic sharding: %v vs %v", shards, again)
	}
}
