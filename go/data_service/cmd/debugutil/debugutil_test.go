package debugutil

import (
	"context"
	"strings"
	"testing"

	databasepb "proto/common/database"

	"shared/ownerepoch"
	"shared/savebarrier"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func TestCheckLocalWriteTarget(t *testing.T) {
	for _, target := range []string{"127.0.0.1:3306", "localhost:6379", "[::1]:6379", "127.0.0.2:3306", "127.0.0.1"} {
		if err := CheckLocalWriteTarget("t", target, ""); err != nil {
			t.Fatalf("%s should be allowed: %v", target, err)
		}
	}
	for _, target := range []string{"mysql.mmorpg-infra:3306", "10.0.0.5:6379", "", "prod-redis:6379"} {
		if err := CheckLocalWriteTarget("t", target, ""); err == nil {
			t.Fatalf("%s should be rejected", target)
		}
	}
	if err := CheckLocalWriteTarget("t", "mysql:3306", "redis, mysql"); err != nil {
		t.Fatalf("explicitly allowed host rejected: %v", err)
	}
	if err := CheckLocalWriteTarget("t", "mysql-prod:3306", "mysql"); err == nil {
		t.Fatal("allow list must match the whole host name")
	}
}

// 钉住工具这份键名副本的字面量。只有 shared/* 的键(savebarrier、ownerepoch)与按 proto 全名派生的键是直接调写者
// 同源函数得到的;login / player_locator / C++ 的键是照抄,写者改格式时本测试不会变红(见 bundle.go 键函数上方的注释)。
func TestPlayerKeyFormats(t *testing.T) {
	const id = 42
	cases := []struct{ got, want string }{
		{ParentKey(id), "PlayerAllData:{42}"},
		{SubTableCacheKey(&databasepb.PlayerDatabase{}, id), "player_database:42"},
		{SubTableCacheKey(&databasepb.PlayerDatabase_1{}, id), "player_database_1:42"},
		{SavePendingIndexKey(3, 130), "player:save_pending:3:2"},
		{PlayerToAccountKey(id), "player_to_account:42"},
		{AccountKey("dev_a"), "account:dev_a"},
		{LocatorSessionKey(id), "player:session:42"},
		{savebarrier.Key(id), "player:{42}:save_barrier"},
		{ownerepoch.OwnerEpochKey(id), "player:{42}:owner_epoch"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Fatalf("key = %q, want %q", c.got, c.want)
		}
	}

	specs := PlayerRedisKeySpecs(id, 0)
	seen := map[string]bool{}
	for _, spec := range specs {
		if seen[spec.Key] {
			t.Fatalf("duplicate key spec %s", spec.Key)
		}
		seen[spec.Key] = true
		if spec.Kind >= RedisKindCount || spec.Purpose == "" {
			t.Fatalf("spec %s incomplete", spec.Key)
		}
		if spec.Kind == RedisKindZSetMember {
			t.Fatalf("home zone 0 must not produce home-scoped key %s", spec.Key)
		}
	}
	withHome := PlayerRedisKeySpecs(id, 7)
	if len(withHome) != len(specs)+2 {
		t.Fatalf("home zone keys missing: %d vs %d", len(withHome), len(specs))
	}
}

func TestReadRedisKeysAtomicSnapshot(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	parent := &databasepb.PlayerAllData{PlayerDatabaseData: &databasepb.PlayerDatabase{PlayerId: 9}, LastSaveSeq: 5}
	raw, err := proto.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	mr.Set(ParentKey(9), string(raw))
	mr.Set(ownerepoch.OwnerEpochKey(9), "3")
	mr.HSet(savebarrier.Key(9), "pending", savebarrier.Version(3, 5))
	mr.ZAdd(SavePendingIndexKey(1, 9), 0, "9")
	mr.HSet("wrong:type", "f", "v")

	specs := []RedisKeySpec{
		{Key: ParentKey(9), Kind: RedisKindString, Decode: func() proto.Message { return &databasepb.PlayerAllData{} }},
		{Key: ownerepoch.OwnerEpochKey(9), Kind: RedisKindString},
		{Key: savebarrier.Key(9), Kind: RedisKindHash},
		{Key: SavePendingIndexKey(1, 9), Kind: RedisKindZSetMember, Member: "9"},
		{Key: SavePendingIndexKey(1, 10), Kind: RedisKindZSetMember, Member: "10"},
		{Key: "wrong:type", Kind: RedisKindString},
		{Key: "missing", Kind: RedisKindString},
	}
	entries, err := ReadRedisKeys(context.Background(), client, specs)
	if err != nil {
		t.Fatalf("ReadRedisKeys: %v", err)
	}
	if entries[0].Type != "string" || entries[0].Value == nil || entries[0].Value.Kind != "protobuf" || entries[0].Value.RawBase64 == "" {
		t.Fatalf("parent entry not decoded with raw bytes: %+v", entries[0])
	}
	if EntryText(&entries[1]) != "3" || entries[1].PTTLMs != -1 {
		t.Fatalf("owner epoch entry wrong: %+v", entries[1])
	}
	if HashText(&entries[2], "pending") != savebarrier.Version(3, 5) {
		t.Fatalf("barrier hash wrong: %+v", entries[2])
	}
	if !entries[3].MemberPresent || entries[4].MemberPresent {
		t.Fatalf("zset membership wrong: %+v / %+v", entries[3], entries[4])
	}
	if entries[5].Type != "hash" || entries[5].Error == "" || entries[5].Value != nil {
		t.Fatalf("type mismatch must be reported on that key only: %+v", entries[5])
	}
	if entries[6].Type != "none" || entries[6].PTTLMs != -2 {
		t.Fatalf("missing key wrong: %+v", entries[6])
	}
}

func TestRowFenceHelpers(t *testing.T) {
	row := &databasepb.PlayerDatabase{PlayerId: 1}
	if RowFenceVersion(row) != "" {
		t.Fatal("zero fence must render as empty")
	}
	if err := SetRowFence(row, 4, 77); err != nil {
		t.Fatal(err)
	}
	if epoch, seq, ok := RowFence(row); !ok || epoch != 4 || seq != 77 {
		t.Fatalf("fence = %d %d %v", epoch, seq, ok)
	}
	if RowFenceVersion(row) != savebarrier.Version(4, 77) {
		t.Fatalf("version = %s", RowFenceVersion(row))
	}
	if err := SetRowFence(&databasepb.PlayerCentreDatabase{}, 1, 1); err == nil {
		t.Fatal("table without fence columns must be rejected")
	}
}

func TestRenderProtoRedactDropsRawBytes(t *testing.T) {
	raw, err := proto.Marshal(&databasepb.UserAccounts{Account: "dev_a", Password: "secret-hash"})
	if err != nil {
		t.Fatal(err)
	}
	spec := AccountRedisKeySpec("dev_a")
	value := RenderProto(raw, spec.Decode, spec.Redact)
	if value.RawBase64 != "" {
		t.Fatal("redacted value must not carry raw bytes")
	}
	decoded, ok := value.ProtoJSON.(map[string]any)
	if !ok || decoded["password"] != "" || decoded["account"] != "dev_a" {
		t.Fatalf("password not redacted: %#v", value.ProtoJSON)
	}
}

func TestPhysicalTableNames(t *testing.T) {
	want := []string{"player_centre_database", "player_database", "player_database_1"}
	for i, table := range PlayerTables {
		name, err := PhysicalTableName(table.New())
		if err != nil || name != want[i] {
			t.Fatalf("table %d = %q, %v; want %q (PlayerTables 须按物理表名升序)", i, name, err, want[i])
		}
	}
}

// 字符集塞进 Params 会被驱动原样拼成 `SET charset = utf8mb4`,每条连接都建连失败;必须走 mysql.Charset 选项。
func TestMySQLConfigCharsetNotInParams(t *testing.T) {
	cfg, err := mysqlConfig(MySQLEndpoint{Hosts: "127.0.0.1:3306", User: "u", Password: "p", DBName: "zone_1_db"})
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := cfg.Params["charset"]; bad {
		t.Fatal("charset must not be a session SET param")
	}
	if dsn := cfg.FormatDSN(); !strings.Contains(dsn, "charset=utf8mb4") || !strings.Contains(dsn, "/zone_1_db") {
		t.Fatalf("dsn = %s", dsn)
	}
}
