package clientplayerloginlogic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/dataloader"
	"login/internal/logic/pkg/homezone"
	pbbase "proto/common/base"
	comppb "proto/common/component"
	pbdb "proto/common/database"
	dspb "proto/data_service"
	loginpb "proto/login"
	"shared/generated/pb/table"
	"shared/generated/persistschema"
	"shared/idsegment"
	"shared/ownerepoch"
	"shared/playername"
	"shared/savebarrier"
)

// 建角写新号种子(docs/design/new-player-first-enter-no-cold-read.md)的单测。harness 沿用
// createplayer_homezone_test.go / createplayer_name_test.go / player_identity_backfill_test.go,
// 不新增假客户端:种子只碰 Redis,data_service 侧的调用序列必须与开关关着时一模一样。

// newPlayerSeedTTL 与 dataloader 里冷读组装父键的 TTL 相同(包内常量不导出,这里照抄数值)。
const newPlayerSeedTTL = 24 * time.Hour

// miniredis 的 INFO 没有实例身份，只替换这两条只读命令的回包。
// 正式 Lua 的快路判定/屏障检查不变，也不关闭默认身份核验开关。
func installParentIdentityInfoForTest(mr *miniredis.Miniredis) {
	mr.Server().SetPreHook(func(peer *server.Peer, command string, args ...string) bool {
		if command != "INFO" || len(args) != 1 {
			return false
		}
		switch strings.ToLower(args[0]) {
		case "server":
			peer.WriteBulk("run_id:" + strings.Repeat("a", 40) + "\r\n")
		case "replication":
			peer.WriteBulk("role:master\r\nmaster_replid:" + strings.Repeat("b", 40) + "\r\n")
		default:
			return false
		}
		return true
	})
}

// setNewPlayerSeed 设置种子开关,用例结束恢复原值。CreatePlayer 直接读全局 config.AppConfig,
// 不恢复会把同包后续用例悄悄变成「开着」。
func setNewPlayerSeed(t *testing.T, enabled bool) {
	t.Helper()
	saved := config.AppConfig.NewPlayerSeed
	config.AppConfig.NewPlayerSeed.Enabled = enabled
	t.Cleanup(func() { config.AppConfig.NewPlayerSeed = saved })
}

// seedCount 读 login_create_player_seed_total{result} 的当前值。计数是进程级累加的,用例只比增量,
// 而且要在动作**之前**先取一次:helper 内部才 Enable 指标开关,之前的写入都被丢弃了。
// result 写字面量而不是 SeedResult.String():这几个 label 值是告警表达式的契约,要单独钉住。
func seedCount(t *testing.T, result string) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_create_player_seed_total", map[string]string{"result": result})
}

// seedParentKeyFor 与 backfill / scene 同一种拼法(PlayerAllData 全名 + ":{" + player_id + "}",玩家域 hash tag)。
// 刻意不调生产函数,独立钉住键形。
func seedParentKeyFor(playerID uint64) string {
	return fmt.Sprintf("%s:{%d}", (&pbdb.PlayerAllData{}).ProtoReflect().Descriptor().FullName(), playerID)
}

// seedSubCacheKeys 是两张子表的缓存键:冷读发过读任务时 go/db 会回写它们,快路径下必须不存在。
func seedSubCacheKeys(playerID uint64) []string {
	return []string{
		fmt.Sprintf("%s:%d", (&pbdb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName(), playerID),
		fmt.Sprintf("%s:%d", (&pbdb.PlayerDatabase_1{}).ProtoReflect().Descriptor().FullName(), playerID),
	}
}

// 独立表达正式初始提交，不复用被测构造函数；仍不携带身份和注册业务初值。
func expectedSeed(playerID uint64) *pbdb.PlayerAllData {
	return &pbdb.PlayerAllData{
		PlayerDatabaseData:   &pbdb.PlayerDatabase{PlayerId: playerID, FenceOwnerEpoch: 1, FenceSaveSeq: 1, PersistSchemaGen: persistschema.Gen},
		PlayerDatabase_1Data: &pbdb.PlayerDatabase_1{PlayerId: playerID, FenceOwnerEpoch: 1, FenceSaveSeq: 1, PersistSchemaGen: persistschema.Gen},
		LastSaveSeq:          1,
	}
}

func readSeededParent(t *testing.T, ctx context.Context, rdb *redis.Client, playerID uint64) (*pbdb.PlayerAllData, []byte) {
	t.Helper()
	raw, err := rdb.Get(ctx, seedParentKeyFor(playerID)).Bytes()
	if err != nil {
		t.Fatalf("read parent key: %v", err)
	}
	parent := &pbdb.PlayerAllData{}
	if err := proto.Unmarshal(raw, parent); err != nil {
		t.Fatalf("unmarshal parent key: %v", err)
	}
	return parent, raw
}

func requireCreated(t *testing.T, resp *loginpb.CreatePlayerResponse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("CreatePlayer: %v", err)
	}
	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}
}

// 开关关着:与本功能之前逐字节相同,不写父键。
func TestCreatePlayer_SeedOffWritesNoParentKey(t *testing.T) {
	setNewPlayerSeed(t, false)
	fake := &fakeRegisterClient{}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)

	resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	requireCreated(t, resp, err)

	if h.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("parent key written with NewPlayerSeed disabled")
	}
}

// 开关开着：SQL 目录提交后才缓存完整初始提交，保留原名字与路由调用。
func TestCreatePlayer_SeedOnWritesCompleteCommitAfterDirectory(t *testing.T) {
	setNewPlayerSeed(t, true)
	fake := &fakeRegisterClient{}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
	writtenBefore := seedCount(t, "written")

	resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	requireCreated(t, resp, err)

	parent, _ := readSeededParent(t, h.ctx, h.rdb, hzPlayerID)
	if !proto.Equal(parent, expectedSeed(hzPlayerID)) {
		t.Fatalf("seed = %v, want the complete initial SQL commit", parent)
	}
	if h.rdb.Get(h.ctx, ownerepoch.OwnerEpochKey(hzPlayerID)).Val() != "1" {
		t.Fatal("缺初始 owner 地板")
	}
	for _, field := range []string{"pending", "player_database", "player_database_1"} {
		if h.rdb.HGet(h.ctx, savebarrier.Key(hzPlayerID), field).Val() != savebarrier.Version(1, 1) {
			t.Fatal("初始提交屏障不完整")
		}
	}
	if ttl := h.mr.TTL(seedParentKeyFor(hzPlayerID)); ttl <= 0 || ttl > newPlayerSeedTTL {
		t.Fatalf("seed TTL = %v, want (0, %v]", ttl, newPlayerSeedTTL)
	}
	if players := storedPlayers(t, h.ctx, h.rdb); len(players) != 1 || players[0].GetPlayerId() != hzPlayerID {
		t.Fatalf("persisted players = %v, want exactly player %d", players, hzPlayerID)
	}
	if got := seedCount(t, "written") - writtenBefore; got != 1 {
		t.Fatalf("seed_total{written} delta = %v, want 1", got)
	}
	if got := strings.Join(fake.events, ","); got != "reserve,register" {
		t.Fatalf("data_service call order = %q, want reserve,register (the seed must not touch data_service)", got)
	}
}

// 归属登记失败(fail-closed):建角整体失败,种子根本不写。
func TestCreatePlayer_SeedSkippedWhenHomeZoneRegistrationFails(t *testing.T) {
	setNewPlayerSeed(t, true)
	fake := &fakeRegisterClient{err: errors.New("down")}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)

	resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	if err != nil {
		t.Fatalf("CreatePlayer must fail via tip, not transport error: %v", err)
	}
	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)

	if h.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("seed written although player:zone registration failed")
	}
}

// 入口锁在名字登记后丢失，目录提交点拒绝；名字补偿保留，不得留下可信种子。
func TestCreatePlayer_LostCreateLockNeverWritesUncommittedSeed(t *testing.T) {
	setNewPlayerSeed(t, true)
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		if err := n.rdb.Del(n.ctx, nameCreateKey).Err(); err != nil {
			t.Errorf("drop create lock: %v", err)
		}
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginInProgress)
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) although the fenced write was refused", len(players))
	}
	if n.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("SQL 尚未提交时写了可信种子")
	}
	if len(n.fake.releaseReqs) != 1 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 1/0", len(n.fake.releaseReqs), len(n.scheduled))
	}
}

// idProbeCount 读 login_create_player_id_probe_total{result} 的当前值,口径同 seedCount:只比增量,
// 动作之前先取一次;result 写字面量,钉住告警表达式的契约。
func idProbeCount(t *testing.T, result string) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_create_player_id_probe_total", map[string]string{"result": result})
}

// seedResultLabels 是 login_create_player_seed_total 的全部 result 取值,用来断言「种子根本没跑」。
var seedResultLabels = []string{"written", "idempotent", "parent_exists", "owner_epoch_exists", "error"}

// existingParentBlob 是一份「真实存档」形状的父键(有等级、不是种子骨架)。
func existingParentBlob(t *testing.T) []byte {
	t.Helper()
	existing, err := proto.Marshal(&pbdb.PlayerAllData{
		PlayerDatabaseData: &pbdb.PlayerDatabase{
			PlayerId:       hzPlayerID,
			LevelComponent: &comppb.LevelComp{Level: 30},
		},
	})
	if err != nil {
		t.Fatalf("marshal existing parent: %v", err)
	}
	return existing
}

// plantInSeedWindow 让 Reserve(6c)在回包之前执行 plant:状态出现在「6b' 探测之后、6d' 种子之前」的窗口里,
// 探测看不到它,只有种子脚本的写时复核能拦下。Reserve 本身照常回成功。
func plantInSeedWindow(t *testing.T, fake *fakeRegisterClient, plant func() error) {
	t.Helper()
	fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		if err := plant(); err != nil {
			t.Errorf("plant state in the probe→seed window: %v", err)
		}
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}
}

// requireRefusedAtSeedAndCompensated 是种子报 id 复用(设计 §10 P2,2026-10-15 定为拒绝)之后的共同断言:
// 建角以 kLoginDataSerializeFailed 拒绝;名字(6c 确定已登记)按 reserve 时的 id / 名字释放恰好一次
// (同步一次,且没有经 afterFunc 排出的延迟二次释放);
// 账号 blob 没有新角色、反查映射不写;建角锁已释放。6d 的 player:zone 已登记(撤不掉,与第 7 步失败同类),
// 体现在调用序列里的 register 上。
//
// 2026-10-16:改收 *nameHarness。调用方原先用 newCreatePlayerHarnessWithNames,afterFunc 仍是真定时器,
// 只比对同步那一刻的调用序列,看不到排到 10s 之后的那次释放;newNameHarness 把 afterFunc 换成同步执行并记进
// n.scheduled,这里才断言得了「没有延迟二次释放」(同 TestCreatePlayerName_RegisterFailureReleasesOnce)。
func requireRefusedAtSeedAndCompensated(t *testing.T, n *nameHarness, resp *loginpb.CreatePlayerResponse, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("CreatePlayer must fail via tip, not transport error: %v", err)
	}
	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if got := strings.Join(n.fake.events, ","); got != "reserve,register,release" {
		t.Fatalf("data_service call order = %q, want reserve,register,release", got)
	}
	if len(n.scheduled) != 0 {
		t.Fatalf("delayed releases scheduled = %v, want none (the name was definitely reserved)", n.scheduled)
	}
	reserved, released := n.fake.reserveReqs[0], n.fake.releaseReqs[0]
	if released.GetPlayerId() != reserved.GetPlayerId() || released.GetName() != reserved.GetName() {
		t.Fatalf("released %d/%q, want the reservation %d/%q",
			released.GetPlayerId(), released.GetName(), reserved.GetPlayerId(), reserved.GetName())
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) although the create was refused", len(players))
	}
	if n.mr.Exists(constants.PlayerToAccountKey(hzPlayerID)) {
		t.Fatal("reverse mapping written although the create was refused")
	}
	if n.mr.Exists(nameCreateKey) {
		t.Fatal("create lock still held after a refused create")
	}
}

// 回归(设计 §10 P2,负责人 2026-10-15 按「最标准」定为拒绝):父键在「6b' 探测之后、6d' 种子之前」才出现
// (Reserve 回调里写入一份真实存档):种子脚本作为写时的原子复核,不覆盖、不加 TTL,计 parent_exists;
// 建角随之拒绝,名字释放一次。
//
// 期望改写(2026-10-15),逐条:
//   - 注入时机:原用例在建角之前就预置父键。现在 6b' 探测会在登记名字之前拒绝,种子根本跑不到,原写法测不到
//     种子脚本的「不覆盖」;所以挪到窗口里注入。建角之前就在的情形见 TestCreatePlayer_IDProbeRefusesMintedIDWithExistingState;
//   - 建角结果:原期望「建角照常成功」(只告警),改为 kLoginDataSerializeFailed 拒绝;
//   - 新增补偿断言:名字释放恰好一次、账号 blob 不变、反查映射不写、锁已释放;
//   - 保留不变:父键字节不变、TTL 仍为 0、seed_total{parent_exists} +1。
//
// 修复前:没有 6b' 探测,id_probe_total{clear} 不增长,前提断言处即红;即便去掉前提断言,种子报 parent_exists
// 也只告警、建角成功,requireTip 处同样红。
//
// 2026-10-16:改用 newNameHarness(发号源换成同 id 的 countingSegment,其余不变),为的是让
// requireRefusedAtSeedAndCompensated 断言得了「没有延迟二次释放」。把种子拒绝分支的 releaseName 改成
// uncertain=true,本用例即红:同步执行的延迟释放让调用序列多出一次 release,n.scheduled 也不再为空
// (此前照样绿,延迟那次排在真定时器上,断言时还没发生)。
func TestCreatePlayer_SeedNeverOverwritesExistingParent(t *testing.T) {
	setNewPlayerSeed(t, true)
	n := newNameHarness(t)
	existing := existingParentBlob(t)
	plantInSeedWindow(t, n.fake, func() error {
		return n.rdb.Set(n.ctx, seedParentKeyFor(hzPlayerID), existing, 0).Err()
	})
	before := seedCount(t, "parent_exists")
	probeClearBefore := idProbeCount(t, "clear")

	resp, err := n.l.CreatePlayer(&loginpb.CreatePlayerRequest{})

	// 先钉住前提:探测确实放行了,拒绝来自种子那一步。
	if got := idProbeCount(t, "clear") - probeClearBefore; got != 1 {
		t.Fatalf("id_probe_total{clear} delta = %v, want 1 (the parent key must appear after the probe)", got)
	}
	requireRefusedAtSeedAndCompensated(t, n, resp, err)
	if _, after := readSeededParent(t, n.ctx, n.rdb, hzPlayerID); !bytes.Equal(existing, after) {
		t.Fatal("seed overwrote an existing parent key")
	}
	if ttl := n.mr.TTL(seedParentKeyFor(hzPlayerID)); ttl != 0 {
		t.Fatalf("existing parent TTL = %v after create, want none", ttl)
	}
	if got := seedCount(t, "parent_exists") - before; got != 1 {
		t.Fatalf("seed_total{parent_exists} delta = %v, want 1", got)
	}
}

// 回归(设计 §10 P2,同上):owner_epoch 在「6b' 探测之后、6d' 种子之前」才被铸出(玩家被派入过场景):
// 父键此刻不在也不写,计 owner_epoch_exists;建角随之拒绝,名字释放一次。
//
// 期望改写(2026-10-15),逐条:
//   - 注入时机:原用例在建角之前就预置 owner_epoch,现在会被 6b' 探测先拦下,挪到窗口里注入;
//   - 建角结果:原期望「建角照常成功」,改为 kLoginDataSerializeFailed 拒绝;
//   - 新增补偿断言:同 TestCreatePlayer_SeedNeverOverwritesExistingParent;
//   - 保留不变:父键不存在、seed_total{owner_epoch_exists} +1。
//
// 修复前:没有 6b' 探测,前提断言处即红;即便去掉它,种子报 owner_epoch_exists 也只告警、建角成功,
// requireTip 处同样红。
//
// 2026-10-16:改用 newNameHarness,理由同 TestCreatePlayer_SeedNeverOverwritesExistingParent。
func TestCreatePlayer_SeedRefusedAfterOwnerEpochMinted(t *testing.T) {
	setNewPlayerSeed(t, true)
	n := newNameHarness(t)
	plantInSeedWindow(t, n.fake, func() error {
		return n.rdb.Set(n.ctx, ownerepoch.OwnerEpochKey(hzPlayerID), "3", 0).Err()
	})
	before := seedCount(t, "owner_epoch_exists")
	probeClearBefore := idProbeCount(t, "clear")

	resp, err := n.l.CreatePlayer(&loginpb.CreatePlayerRequest{})

	// 先钉住前提:探测确实放行了,拒绝来自种子那一步。
	if got := idProbeCount(t, "clear") - probeClearBefore; got != 1 {
		t.Fatalf("id_probe_total{clear} delta = %v, want 1 (owner_epoch must appear after the probe)", got)
	}
	requireRefusedAtSeedAndCompensated(t, n, resp, err)
	if n.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("seed written although player:{id}:owner_epoch exists")
	}
	if got := seedCount(t, "owner_epoch_exists") - before; got != 1 {
		t.Fatalf("seed_total{owner_epoch_exists} delta = %v, want 1", got)
	}
}

// 回归(设计 §10 P7,负责人 2026-10-15 按「最标准」定为采用):新铸 id 上建角之前就已有既有状态(父键 /
// owner_epoch)时,建角在登记名字**之前**拒绝(fail-closed):名字不登记、player:zone 不登记、种子不跑、
// 账号 blob 不变、反查映射不写,建角锁照常释放,计 id_probe_total{result} +1。种子开关开、关都要拒:
// id 复用在关着种子时同样会让新账号首次入场读到那份旧缓存。
//
// 修复前:没有探测。关着种子时建角直接成功;开着种子时种子报 parent_exists / owner_epoch_exists 只告警、
// 建角照常成功,名字与 player:zone 都已登记 —— 两种都在 requireTip 处即红。
func TestCreatePlayer_IDProbeRefusesMintedIDWithExistingState(t *testing.T) {
	plantParent := func(t *testing.T, h *createPlayerHarness) {
		t.Helper()
		if err := h.rdb.Set(h.ctx, seedParentKeyFor(hzPlayerID), existingParentBlob(t), 0).Err(); err != nil {
			t.Fatalf("plant existing parent: %v", err)
		}
	}
	plantOwnerEpoch := func(t *testing.T, h *createPlayerHarness) {
		t.Helper()
		if err := h.rdb.Set(h.ctx, ownerepoch.OwnerEpochKey(hzPlayerID), "3", 0).Err(); err != nil {
			t.Fatalf("plant owner_epoch: %v", err)
		}
	}
	cases := []struct {
		name   string
		seed   bool // NewPlayerSeed.Enabled
		plant  func(*testing.T, *createPlayerHarness)
		result string // id_probe_total 的 result
	}{
		{"父键已在,种子开", true, plantParent, "parent_exists"},
		{"父键已在,种子关", false, plantParent, "parent_exists"},
		{"owner_epoch 已在,种子开", true, plantOwnerEpoch, "owner_epoch_exists"},
		{"owner_epoch 已在,种子关", false, plantOwnerEpoch, "owner_epoch_exists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setNewPlayerSeed(t, tc.seed)
			fake := &fakeRegisterClient{}
			h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
			tc.plant(t, h)
			seedKey := seedParentKeyFor(hzPlayerID)
			parentBefore, parentErrBefore := h.mr.Get(seedKey)
			probeBefore := idProbeCount(t, tc.result)
			seedBefore := make(map[string]float64, len(seedResultLabels))
			for _, label := range seedResultLabels {
				seedBefore[label] = seedCount(t, label)
			}

			resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})

			if err != nil {
				t.Fatalf("CreatePlayer must fail via tip, not transport error: %v", err)
			}
			requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
			// 名字、player:zone 一个 RPC 都没发,也就没有释放。
			if got := strings.Join(fake.events, ","); got != "" {
				t.Fatalf("data_service calls = %q, want none (refused before reserving the name)", got)
			}
			if got := idProbeCount(t, tc.result) - probeBefore; got != 1 {
				t.Fatalf("id_probe_total{%s} delta = %v, want 1", tc.result, got)
			}
			for _, label := range seedResultLabels {
				if got := seedCount(t, label) - seedBefore[label]; got != 0 {
					t.Fatalf("seed_total{%s} delta = %v, want 0 (the seed must not run)", label, got)
				}
			}
			if parentAfter, parentErrAfter := h.mr.Get(seedKey); parentAfter != parentBefore ||
				(parentErrBefore == nil) != (parentErrAfter == nil) {
				t.Fatalf("parent key changed: %q/%v -> %q/%v", parentBefore, parentErrBefore, parentAfter, parentErrAfter)
			}
			if ttl := h.mr.TTL(seedKey); ttl != 0 {
				t.Fatalf("parent TTL = %v after a refused create, want none", ttl)
			}
			if players := storedPlayers(t, h.ctx, h.rdb); len(players) != 0 {
				t.Fatalf("account blob persisted %d player(s) although the create was refused", len(players))
			}
			if h.mr.Exists(constants.PlayerToAccountKey(hzPlayerID)) {
				t.Fatal("reverse mapping written although the create was refused")
			}
			if h.mr.Exists(nameCreateKey) {
				t.Fatal("create lock still held after a refused create")
			}
		})
	}
}

// mintHookSegment 发出号之后立刻跑一次 after:把故障注入卡在「发号之后、6b' 探测之前」。
type mintHookSegment struct {
	id    uint64
	after func()
}

func (s *mintHookSegment) Next(context.Context) (uint64, error) {
	s.after()
	return s.id, nil
}

// 回归(设计 §10 P7 的 fail-closed 一半):探测本身出错(Redis 在发号之后整个不可用)⇒ 拒绝建角,名字、
// player:zone 一个都不登记,不记孤儿,计 id_probe_total{error} +1。探不清不等于没有:放行就可能把一个
// 在役 id 交给新账号。
//
// 修复前:没有探测,名字与 player:zone 照常登记;随后围栏写与回读都失败,按「分不清」保留名字登记、记一个孤儿,
// tip 是 kLoginRedisSetFailed —— requireTip 处即红(调用序列也会是 reserve,register)。
func TestCreatePlayer_IDProbeErrorRefusesCreate(t *testing.T) {
	n := newNameHarness(t)
	n.l.svcCtx.PlayerIDMinter = &idsegment.Minter{Name: "player_id", Segment: &mintHookSegment{id: hzPlayerID, after: func() {
		n.mr.SetError("simulated redis outage")
	}}}
	t.Cleanup(func() { n.mr.SetError("") })
	errorsBefore := idProbeCount(t, "error")
	orphansBefore := orphanCount(t)

	resp := n.create(t, "云中君")
	// 后面的断言要经 go-redis 读回账号 blob。建角锁的释放发生在故障期间,本用例不对它断言。
	n.mr.SetError("")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if got := strings.Join(n.fake.events, ","); got != "" {
		t.Fatalf("data_service calls = %q, want none (refused before reserving the name)", got)
	}
	if got := idProbeCount(t, "error") - errorsBefore; got != 1 {
		t.Fatalf("id_probe_total{error} delta = %v, want 1", got)
	}
	if got := orphanCount(t) - orphansBefore; got != 0 {
		t.Fatalf("orphan_total delta = %v, want 0 (no name was reserved)", got)
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) although the probe failed", len(players))
	}
	if n.mr.Exists(constants.PlayerToAccountKey(hzPlayerID)) || n.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("reverse mapping or seed written although the probe failed")
	}
}

// probePipelineHook 只拦 6b' 探测的那条 pipeline(参数里带新铸 id 的父键;拨号时 initConn 发的 HELLO /
// CLIENT SETINFO 等 pipeline 不带它,不会误中):before 在交给 go-redis 之前跑,afterOK 在 pipeline 成功返回之后跑。
// 单条命令与拨号原样放行 —— 种子脚本是单条 EVALSHA / EVAL,不经过这里。钩子与 CreatePlayer 在同一个 goroutine 里
// 同步执行,hits 不需要锁。
type probePipelineHook struct {
	key     string
	before  func(ctx context.Context)
	afterOK func()
	hits    int
}

func (h *probePipelineHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (h *probePipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *probePipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if !pipelineTouchesKey(cmds, h.key) {
			return next(ctx, cmds)
		}
		h.hits++
		if h.before != nil {
			h.before(ctx)
		}
		err := next(ctx, cmds)
		if err == nil && h.afterOK != nil {
			h.afterOK()
		}
		return err
	}
}

func pipelineTouchesKey(cmds []redis.Cmder, key string) bool {
	for _, cmd := range cmds {
		for _, a := range cmd.Args() {
			if fmt.Sprint(a) == key {
				return true
			}
		}
	}
	return false
}

// 回归(2026-10-16,设计 §9 第 1 条的 2026-10-16 更正注):6b' 探测不得另设比请求 ctx 更短的截止。
// login 的 go-redis 没开 ContextTimeoutEnabled,ctx 截止不约束 socket 读写,但取连接(连接池排队)与拨号认 ctx;
// 探测又是 fail-closed,另设 1s 子预算时,连接池等满超过 1s 的建角会被拒(handler 还剩约 3s,后续步骤本来做得完),
// 还被记成一次探测出错。直接测「池等待在 1s 与 handler 剩余预算之间时建角成功」要真等墙钟,所以钉住它的确定性代理:
// 交给 go-redis 的 ctx 截止时间必须就是请求 ctx 的截止时间(连接池排队与拨号认的正是它)。
//
// 修复前:probeMintedID 套 context.WithTimeout(l.ctx, 1s),钩子看到的截止约为发探测时刻 + 1s,
// 在 probe ctx deadline 断言处即红。
func TestCreatePlayer_IDProbeUsesTheRequestDeadline(t *testing.T) {
	n := newNameHarness(t)
	requestDeadline := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(n.l.ctx, requestDeadline)
	defer cancel()
	n.l.ctx = ctx
	var probeDeadline time.Time
	var probeHasDeadline bool
	hook := &probePipelineHook{key: seedParentKeyFor(hzPlayerID), before: func(probeCtx context.Context) {
		probeDeadline, probeHasDeadline = probeCtx.Deadline()
	}}
	n.rdb.AddHook(hook)
	clearBefore := idProbeCount(t, "clear")

	resp := n.create(t, "云中君")

	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}
	if hook.hits != 1 {
		t.Fatalf("probe pipelines seen = %d, want 1", hook.hits)
	}
	if !probeHasDeadline || !probeDeadline.Equal(requestDeadline) {
		t.Fatalf("probe ctx deadline = %v (set=%v), want the request deadline %v (no shorter sub-budget)",
			probeDeadline, probeHasDeadline, requestDeadline)
	}
	if got := idProbeCount(t, "clear") - clearBefore; got != 1 {
		t.Fatalf("id_probe_total{clear} delta = %v, want 1", got)
	}
}

// 回归(2026-10-16):请求 ctx 在探测途中结束 —— 探测已越过 probeMintedID 开头的前置检查、交给了 go-redis,
// go-redis 取连接时看到 ctx 已结束,以 context.Canceled 返回。探测没有结论,也不是 Redis 故障:仍拒绝建角
// (kLoginDataSerializeFailed),但不计 id_probe_total(error、clear 都不动),与前置检查同一口径;名字、
// player:zone 一个 RPC 都不发,不记孤儿,建角锁照常释放(走 WithoutCancel)。
// 生产上对应:连接池排队或拨号期间请求截止 / 客户端断连。钩子在交给 go-redis 之前取消,走的是连接池排队那同一处
// ctx 检查,不依赖墙钟,也不需要真把连接池占满。
//
// 修复前:探测出错一律计 error 并打 ERROR「probe failed」,在 id_probe_total{error} delta 断言处即红。
func TestCreatePlayer_IDProbeRequestDoneMidProbeIsNotProbeError(t *testing.T) {
	n := newNameHarness(t)
	ctx, cancel := context.WithCancel(n.l.ctx)
	defer cancel()
	n.l.ctx = ctx
	hook := &probePipelineHook{key: seedParentKeyFor(hzPlayerID), before: func(context.Context) { cancel() }}
	n.rdb.AddHook(hook)
	errorBefore := idProbeCount(t, "error")
	clearBefore := idProbeCount(t, "clear")
	orphansBefore := orphanCount(t)

	resp := n.create(t, "云中君")

	// 先钉住前提:探测越过了前置检查、真的交给了 go-redis,请求 ctx 是在那之后才结束的。
	if hook.hits != 1 {
		t.Fatalf("probe pipelines seen = %d, want 1 (the probe must get past its pre-check)", hook.hits)
	}
	if ctx.Err() == nil {
		t.Fatal("scenario never ended the request ctx")
	}
	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if got := idProbeCount(t, "error") - errorBefore; got != 0 {
		t.Fatalf("id_probe_total{error} delta = %v, want 0 (the request ended mid-probe; that is not a Redis failure)", got)
	}
	if got := idProbeCount(t, "clear") - clearBefore; got != 0 {
		t.Fatalf("id_probe_total{clear} delta = %v, want 0 (the probe reached no verdict)", got)
	}
	if got := strings.Join(n.fake.events, ","); got != "" {
		t.Fatalf("data_service calls = %q, want none (refused before reserving the name)", got)
	}
	if got := orphanCount(t) - orphansBefore; got != 0 {
		t.Fatalf("orphan_total delta = %v, want 0 (no name was reserved)", got)
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) although the create was refused", len(players))
	}
	if n.mr.Exists(constants.PlayerToAccountKey(hzPlayerID)) || n.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("reverse mapping or seed written although the create was refused")
	}
	if n.mr.Exists(nameCreateKey) {
		t.Fatal("create lock still held after CreatePlayer returned on an ended request ctx")
	}
}

// 回归(docs/design/client-request-ordering.md §6.1):请求 ctx 恰好在 6b' 探测的 Redis 往返期间结束。login 的
// go-redis 没开 ContextTimeoutEnabled,已发出的 pipeline 照样读回 clear,建角于是进入 6c;6c 必须在发 Reserve
// 之前查 ctx 放弃:一个 data_service RPC 都不发,不释放,不排延迟释放。
// TestCreatePlayer_RequestDoneMidCreateCommitsNothing 的「发号之后」子用例自 6b' 落地起停在探测开头的 ctx 预检,
// 钉不住 tryReserveName 开头的预检,由本用例补上。
//
// 那条预检早已存在,本用例不属于「修复前红」,而是守住它:去掉 tryReserveName 开头的 ctx 预检(或挪到
// reserveNameOnce 之后),假 data_service 不看 ctx、照样回 OK,6c” 随后放弃并释放,调用序列变成
// reserve,release,在 data_service calls 断言处即红。
func TestCreatePlayer_IDProbeClearThenDeadlineSendsNoReserve(t *testing.T) {
	n := newNameHarness(t)
	accountKey := constants.GetAccountDataKey(hzAccount)
	before, err := n.mr.Get(accountKey)
	if err != nil {
		t.Fatalf("read seeded account blob: %v", err)
	}
	ctx, cancel := context.WithCancel(n.l.ctx)
	defer cancel()
	n.l.ctx = ctx
	hook := &probePipelineHook{key: seedParentKeyFor(hzPlayerID), afterOK: cancel}
	n.rdb.AddHook(hook)
	clearBefore := idProbeCount(t, "clear")

	resp := n.create(t, "云中君")

	// 先钉住前提:探测确实放行了,请求 ctx 是在那之后才结束的。前提不成立时下面的断言说明不了问题。
	if got := idProbeCount(t, "clear") - clearBefore; got != 1 {
		t.Fatalf("id_probe_total{clear} delta = %v, want 1 (the probe must have let the create through)", got)
	}
	if ctx.Err() == nil {
		t.Fatal("scenario never ended the request ctx")
	}
	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if got := strings.Join(n.fake.events, ","); got != "" {
		t.Fatalf("data_service calls = %q, want none (the request ctx ended before the name reservation was sent)", got)
	}
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("releases = %d, delayed = %v, want none (nothing was reserved)", len(n.fake.releaseReqs), n.scheduled)
	}
	if after, err := n.mr.Get(accountKey); err != nil || after != before {
		t.Fatalf("account blob changed (err=%v) although the create was abandoned before reserving the name", err)
	}
	if n.mr.Exists(constants.PlayerToAccountKey(hzPlayerID)) || n.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("reverse mapping or seed written although the create was abandoned")
	}
	if n.mr.Exists(nameCreateKey) {
		t.Fatal("create lock still held after CreatePlayer returned on an ended request ctx")
	}
}

// 种子脚本报错(父键是个 list,脚本里的 GET 回 WRONGTYPE):建角照常成功,账号 blob 与反查索引照写,
// 计 error,父键原样不动。证明「种子结果不改变建角结果」对 error 分支同样成立。
// list 键只在 SQL 已提交回执后出现，不能由提交前的只读身份探测代替此回归。
func TestCreatePlayer_SeedErrorDoesNotFailCreate(t *testing.T) {
	setNewPlayerSeed(t, true)
	fake := &fakeRegisterClient{}
	h := newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake)
	key := seedParentKeyFor(hzPlayerID)
	fixtureDirectory(t, h.rdb).afterCommit = func() {
		if _, err := h.mr.Lpush(key, "x"); err != nil {
			t.Fatal(err)
		}
	}
	before := seedCount(t, "error")

	resp, err := h.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	requireCreated(t, resp, err)

	if players := storedPlayers(t, h.ctx, h.rdb); len(players) != 1 || players[0].GetPlayerId() != hzPlayerID {
		t.Fatalf("persisted players = %v, want exactly player %d", players, hzPlayerID)
	}
	if owned, err := h.l.svcCtx.AccountDirectory.Lookup(h.ctx, hzPlayerID); err != nil || owned.Account != hzAccount {
		t.Fatalf("SQL ownership = %v/%v, want %q", owned, err, hzAccount)
	}
	if got := seedCount(t, "error") - before; got != 1 {
		t.Fatalf("seed_total{error} delta = %v, want 1", got)
	}
	if typ := h.mr.Type(key); typ != "list" {
		t.Fatalf("parent key type = %q after a failed seed, want the original list untouched", typ)
	}
	if len(fake.releaseReqs) != 0 {
		t.Fatal("缓存失败释放了已提交角色的在役名字")
	}
}

func TestCreatePlayer_LateCommittedSeedKeepsOwnerAndRegisteredName(t *testing.T) {
	setNewPlayerSeed(t, true)
	n := newNameHarness(t)
	ownerKey := ownerepoch.OwnerEpochKey(hzPlayerID)
	fixtureDirectory(t, n.rdb).afterCommit = func() {
		if err := n.rdb.Set(n.ctx, ownerKey, "3", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := n.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	requireCreated(t, resp, err)
	if n.rdb.Get(n.ctx, ownerKey).Val() != "3" || n.mr.Exists(seedParentKeyFor(hzPlayerID)) {
		t.Fatal("迟到初始种子覆盖了更高 owner 或写出旧父快照")
	}
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatal("迟到种子补偿释放了已提交角色的名字")
	}
	if len(storedPlayers(t, n.ctx, n.rdb)) != 1 {
		t.Fatal("缓存拒写删除了已提交目录")
	}
}

func TestCreatePlayer_UnknownCommitRecoversDirectoryWithoutSeedReceipt(t *testing.T) {
	setNewPlayerSeed(t, true)
	n := newNameHarness(t)
	fixtureDirectory(t, n.rdb).afterCreateErr = errors.New("SQL 已提交但应答丢失")
	before := make(map[string]float64)
	for _, label := range seedResultLabels {
		before[label] = seedCount(t, label)
	}
	resp, err := n.l.CreatePlayer(&loginpb.CreatePlayerRequest{})
	requireCreated(t, resp, err)
	if n.mr.Exists(seedParentKeyFor(hzPlayerID)) || n.mr.Exists(ownerepoch.OwnerEpochKey(hzPlayerID)) {
		t.Fatal("未知提交经 Lookup 恢复时制造了初始凭据")
	}
	if len(storedPlayers(t, n.ctx, n.rdb)) != 1 || len(n.fake.releaseReqs) != 0 {
		t.Fatal("未知提交恢复未保留目录与名字")
	}
	for _, label := range seedResultLabels {
		if seedCount(t, label) != before[label] {
			t.Fatal("Lookup 恢复触发了缓存种子")
		}
	}
}

// 种子 → EnterGame:预加载走 EXISTS 快路径(svc 里 KafkaClient / TaskResultDispatcher 为 nil,
// 任何冷读分支都会 panic,链就走不到 SetSession),身份补齐写进种子、保留种子的 TTL,
// 没有子表缓存回写。
func TestEnterGame_SeededNewPlayerTakesFastPath(t *testing.T) {
	h := newEnterWiringHarness(t, []*pbbase.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}}, nil)
	ctx := context.Background()
	// harness 默认种下一份老存档;换成建角种子,模拟「刚建完角、第一次进场」。
	if err := h.rdb.Del(ctx, identityBlobKey(), ownerepoch.OwnerEpochKey(identityPlayerID), savebarrier.Key(identityPlayerID)).Err(); err != nil {
		t.Fatalf("drop harness parent key: %v", err)
	}
	if got, err := dataloader.SeedCommittedNewPlayerAllData(ctx, h.rdb, expectedSeed(identityPlayerID)); err != nil || got != dataloader.SeedWritten {
		t.Fatalf("SeedNewPlayerAllData = %v / err %v, want written / nil", got, err)
	}

	resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID})
	if err != nil {
		t.Fatalf("EnterGame: %v", err)
	}
	if resp.GetErrorMessage() != nil {
		t.Fatalf("EnterGame rejected with tip %d, want the async chain started", resp.GetErrorMessage().GetId())
	}
	h.waitChain(t)

	if got := h.locator.setSessionCalls.Load(); got != 1 {
		t.Fatalf("SetSession calls = %d, want 1 (the chain must take the fast path and get past identity backfill)", got)
	}
	raw, player := readIdentityBlob(t, h.rdb)
	if got := player.GetUint32PbComponent().GetClass(); got != 3 {
		t.Fatalf("class = %d, want 3", got)
	}
	if got := player.GetProfileComponent().GetName(); got != "云中君" {
		t.Fatalf("profile name = %q, want 云中君", got)
	}
	parent := &pbdb.PlayerAllData{}
	if err := proto.Unmarshal(raw, parent); err != nil {
		t.Fatalf("unmarshal parent key: %v", err)
	}
	if got := parent.GetPlayerDatabase_1Data().GetPlayerId(); got != identityPlayerID {
		t.Fatalf("player_database_1 player_id = %d, want %d (backfill must keep the seed's second sub table)", got, identityPlayerID)
	}
	if ttl, err := h.rdb.PTTL(ctx, identityBlobKey()).Result(); err != nil || ttl <= 0 || ttl > newPlayerSeedTTL {
		t.Fatalf("parent PTTL = %v / err %v, want (0, %v] (backfill keeps the seed TTL)", ttl, err, newPlayerSeedTTL)
	}
	if n, err := h.rdb.Exists(ctx, seedSubCacheKeys(identityPlayerID)...).Result(); err != nil || n != 0 {
		t.Fatalf("sub cache keys present = %d / err %v, want 0 (no DB read on the fast path)", n, err)
	}
}

// 身份CAS之后、SceneManager铸造owner之前中断，不得把仍无任何资产的新号永久锁死。
func TestSeededPlayerCanRetryAfterIdentityBackfillBeforeFirstOwner(t *testing.T) {
	l, rc, mr, _ := newIdentityBackfillHarness(t, &pbdb.PlayerDatabase{PlayerId: identityPlayerID})
	installParentIdentityInfoForTest(mr)
	ctx := context.Background()
	if err := rc.Del(ctx, identityBlobKey()).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err := dataloader.SeedCommittedNewPlayerAllData(ctx, rc, expectedSeed(identityPlayerID)); err != nil || result != dataloader.SeedWritten {
		t.Fatalf("正式建角种子失败: %v/%v", result, err)
	}
	state := identityState(3, "云中君")
	state.appearanceID, state.gender = "appearance-verified", 2
	if err := l.backfillPlayerIdentity(ctx, nil, state); err != nil {
		t.Fatal(err)
	}
	before, player := readIdentityBlob(t, rc)
	if player.GetUint32PbComponent().GetClass() != 3 || player.GetProfileComponent().GetAppearanceId() != state.appearanceID {
		t.Fatal("正式身份补齐未执行")
	}
	// 未执行SetSession/EnterScene，重试必须走正式Ensure且不发SQL读、不重建父键。
	calls := 0
	dataloader.EnsurePlayerAllDataInRedisAsync(ctx, rc, nil, nil, identityPlayerID, 0, func(err error) {
		calls++
		if err != nil {
			t.Errorf("首次owner之前的身份补齐种子被拒: %v", err)
		}
	})
	after, _ := readIdentityBlob(t, rc)
	if calls != 1 || !bytes.Equal(before, after) {
		t.Fatal("重试改写了已有身份或没有完成一次")
	}
}
