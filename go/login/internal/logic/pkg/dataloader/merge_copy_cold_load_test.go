package dataloader

// 前向 copy 合服之后的冷加载判定样例表(2026-10-22,r2-mz#1 评审跟进)。
//
// tools/merge_zone 的 -verify-merged 有一行 verify:save_barrier,在合服 Step 5 逐人预判「开服后这名玩家的第一次登录,
// login 会一致放行、采纳 SQL 放行,还是拒登」。它是独立 go.mod,不能引 shared,只能镜像这里的判定;10-22 之前两份判定
// 就漂移过一次(login 改用 savebarrier.Reconcile 后 verify 仍按旧规则判)。现在两边共用一张样例表
// go/shared/savebarrier/testdata/merge_copy_cold_load_cases.json:merge_zone 的单测用 judgeSaveBarrier 对表,这里用 login
// 的真实冷加载入口(beginParentLoad → route → SQL 读 → settle,miniredis 跑正式 Lua,只替换实例身份)对同一张表。
// 任一边改了判定而没改表,就会在那一边变红。
//
// 场景固定为前向 copy 合服之后:父快照已被合服删除;屏障整把留在原实例,身份戳等于实例当前身份(替身固定为 redis-test)。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	databasepb "proto/common/database"
	dbpb "proto/db"
	"shared/ownerepoch"
	"shared/savebarrier"
	"strconv"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// mergeCopyColdLoadCase 是样例表的一行,字段含义见表头的 doc。
type mergeCopyColdLoadCase struct {
	Name        string  `json:"name"`
	Owner       *string `json:"owner"`
	OwnerType   string  `json:"owner_type"`
	BarrierType string  `json:"barrier_type"`
	Pending     *string `json:"pending"`
	AckFirst    *string `json:"ack_first"`
	AckSecond   *string `json:"ack_second"`
	RowFirst    *string `json:"row_first"`
	RowSecond   *string `json:"row_second"`
	Verdict     string  `json:"verdict"`
	Severity    string  `json:"severity"`
	Login       string  `json:"login"`
}

// 相对本包目录(go test 的工作目录):go/login/internal/logic/pkg/dataloader → go/shared/savebarrier/testdata。
var mergeCopyColdLoadCasesPath = filepath.Join("..", "..", "..", "..", "..", "shared", "savebarrier", "testdata",
	"merge_copy_cold_load_cases.json")

func loadMergeCopyColdLoadCases(t *testing.T) []mergeCopyColdLoadCase {
	t.Helper()
	raw, err := os.ReadFile(mergeCopyColdLoadCasesPath)
	if err != nil {
		t.Fatalf("读取合服冷加载样例表: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var table struct {
		Doc   string                  `json:"doc"`
		Cases []mergeCopyColdLoadCase `json:"cases"`
	}
	if err := dec.Decode(&table); err != nil {
		t.Fatalf("解析 %s: %v", mergeCopyColdLoadCasesPath, err)
	}
	if len(table.Cases) == 0 {
		t.Fatalf("%s 没有样例", mergeCopyColdLoadCasesPath)
	}
	return table.Cases
}

// parseCaseVersion 解析样例里的 "v<owner_epoch>:<save_seq>";不是这个形式时 ok 为 false(畸形样例原样写入)。
func parseCaseVersion(s string) (epoch, seq uint64, ok bool) {
	rest, found := strings.CutPrefix(s, "v")
	if !found {
		return 0, 0, false
	}
	e, q, found := strings.Cut(rest, ":")
	if !found {
		return 0, 0, false
	}
	epoch, eerr := strconv.ParseUint(e, 10, 64)
	seq, serr := strconv.ParseUint(q, 10, 64)
	return epoch, seq, eerr == nil && serr == nil
}

func caseBarrierValue(s string) string {
	if epoch, seq, ok := parseCaseVersion(s); ok {
		return savebarrier.Version(epoch, seq)
	}
	return s
}

// installMergeCopyColdLoadCase 按样例写 owner_epoch 与屏障;父快照不写(合服已删)。
func installMergeCopyColdLoadCase(t *testing.T, rc *redis.Client, c mergeCopyColdLoadCase) {
	t.Helper()
	ctx := context.Background()
	ownerKey, barrierKey := ownerepoch.OwnerEpochKey(seedTestPlayerID), savebarrier.Key(seedTestPlayerID)
	switch c.OwnerType {
	case "":
		if c.Owner != nil {
			if err := rc.Set(ctx, ownerKey, *c.Owner, 0).Err(); err != nil {
				t.Fatal(err)
			}
		}
	case "hash":
		if err := rc.HSet(ctx, ownerKey, "corrupt", "1").Err(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("未知 owner_type %q", c.OwnerType)
	}
	var fields []any
	for _, f := range []struct {
		name  string
		value *string
	}{{"pending", c.Pending}, {"player_database", c.AckFirst}, {"player_database_1", c.AckSecond}} {
		if f.value != nil {
			fields = append(fields, f.name, caseBarrierValue(*f.value))
		}
	}
	switch {
	case c.BarrierType == "string":
		if err := rc.Set(ctx, barrierKey, "corrupt", 0).Err(); err != nil {
			t.Fatal(err)
		}
	case c.BarrierType != "":
		t.Fatalf("未知 barrier_type %q", c.BarrierType)
	case len(fields) > 0:
		// 合服前后是同一实例:屏障里的身份戳等于实例当前身份。
		fields = append(fields, savebarrier.VerifiedIdentityField, "redis-test")
		if err := rc.HSet(ctx, barrierKey, fields...).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// mergeCopyRowReader 是 go/db 读两张玩家表的替身:样例没有这一行时回成功、无数据(与 go/db 的「SELECT 无行」相同)。
func mergeCopyRowReader(t *testing.T, c mergeCopyColdLoadCase) func(*dbpb.DBTask) ([]byte, error) {
	return func(task *dbpb.DBTask) ([]byte, error) {
		typ, err := protoregistry.GlobalTypes.FindMessageByURL(task.MsgType)
		if err != nil {
			return nil, err
		}
		sub := typ.New().Interface()
		if err := proto.Unmarshal(task.Body, sub); err != nil {
			return nil, err
		}
		var spec *string
		var setFence func(epoch, seq uint64)
		switch row := sub.(type) {
		case *databasepb.PlayerDatabase:
			spec = c.RowFirst
			setFence = func(epoch, seq uint64) { row.FenceOwnerEpoch, row.FenceSaveSeq = epoch, seq }
		case *databasepb.PlayerDatabase_1:
			spec = c.RowSecond
			setFence = func(epoch, seq uint64) { row.FenceOwnerEpoch, row.FenceSaveSeq = epoch, seq }
		default:
			return nil, fmt.Errorf("意外的 SQL 读 %s", task.MsgType)
		}
		if spec == nil {
			return nil, nil
		}
		epoch, seq, ok := parseCaseVersion(*spec)
		if !ok {
			t.Fatalf("行围栏样例 %q 不是 v<epoch>:<seq>", *spec)
		}
		setFence(epoch, seq)
		return proto.Marshal(sub)
	}
}

// mergeCopyBarrierState 是判「一致放行」还是「采纳放行」用的快照:采纳会改写 pending / 两表 ACK(必要时抬 owner),
// 一致路径只写父键与身份戳。读不出(键类型错)时把错误原文记进快照,前后照样可比。
func mergeCopyBarrierState(rc *redis.Client) string {
	ctx := context.Background()
	owner, ownerErr := rc.Get(ctx, ownerepoch.OwnerEpochKey(seedTestPlayerID)).Result()
	fields, barrierErr := rc.HMGet(ctx, savebarrier.Key(seedTestPlayerID), "pending", "player_database", "player_database_1").Result()
	return fmt.Sprintf("owner=%q/%v barrier=%v/%v", owner, ownerErr, fields, barrierErr)
}

// TestMergeCopyColdLoadMatchesTheSharedTable:login 冷加载对样例表逐条给出表里的结论。refuse 不写任何键;
// admit 发布父快照且不改屏障与 owner(V1 一致,不打采纳告警);adopt 发布父快照并改写屏障(V2 / V7 采纳)。
func TestMergeCopyColdLoadMatchesTheSharedTable(t *testing.T) {
	for _, c := range loadMergeCopyColdLoadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			mr, rc := newSeedTestRedis(t)
			ctx := context.Background()
			installMergeCopyColdLoadCase(t, rc, c)
			if mr.Exists(seedParentKey(seedTestPlayerID)) {
				t.Fatal("样例前提不是父快照缺失")
			}
			dump, state := mr.Dump(), mergeCopyBarrierState(rc)
			reads := &coldSQLReads{read: mergeCopyRowReader(t, c)}
			calls := 0
			var loadErr error
			ensureTestPlayerAllDataInRedisAsync(ctx, rc, reads, reads, seedTestPlayerID, 0, func(err error) {
				calls++
				loadErr = err
			})
			if calls != 1 {
				t.Fatalf("冷加载完成回调 %d 次", calls)
			}
			got := "refuse"
			switch {
			case loadErr != nil:
				if mr.Dump() != dump {
					t.Fatalf("拒绝路径改写了 Redis(err=%v)", loadErr)
				}
			case mergeCopyBarrierState(rc) == state:
				got = "admit"
			default:
				got = "adopt"
			}
			if loadErr == nil && !mr.Exists(seedParentKey(seedTestPlayerID)) {
				t.Fatal("放行却没有发布父快照")
			}
			if got != c.Login {
				t.Fatalf("login 冷加载 = %s(err=%v),样例表要求 %s(merge_zone 判 %q / %s)", got, loadErr, c.Login,
					c.Verdict, c.Severity)
			}
		})
	}
}
