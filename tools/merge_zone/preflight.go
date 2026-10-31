package main

// 合服 / 撤销的前置门禁(preflight)。全部是**拒绝执行**,不是警告:所有「这次能不能开始」的判断必须在第一次写之前问完。
//
// 检查清单(顺序即执行顺序)。「那个 zone」合服时是源区,撤销时是目标区(见 preflightParams.zone):
//   P1 源 / 目标 zone 库、guild / trade 库表就绪 —— SQL 侧,调用方先做
//   P2 那个 zone 的场景节点全下线              —— etcd 注册表一条都没有,且 scene_nodes:zone:{zone}:load 为空
//   P6 本次涉及的玩家没有在线会话              —— player:session:*
//   P7 本次涉及的玩家不在任何活队伍里          —— 全球 SQL team_membership.team_id 必须为 0 或无原件
//
// P3~P5(go/db 的 Kafka 积压、重试 / 死信队列、存盘补投索引、玩家锁)随 2026-10-30 存盘直接提交 TiDB 删除:
// 玩家数据不再经 Kafka,也不随合服搬动,没有「最后一次存盘有没有落库」要等;编号不重排,文档与日志里的 P6 / P7 仍对得上。
//
// 「本次涉及的玩家」= home_zone 等于源区的玩家(player_route 现查,合服不改 home_zone,续跑结果不变)。
// P6 / P7 是开始之前的一次快照,没有围栏挡住之后的写:它们靠 P2 成立 —— 场景节点全下线之后,这批玩家不再有入口能上线或组队。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// sceneNodeLoadKeyFmt 是 scene_manager 的场景节点负载集。
const sceneNodeLoadKeyFmt = "scene_nodes:zone:%d:load"

// errZoneStillLive 表示要操作的 zone 还不能证明已下线。
var errZoneStillLive = errors.New("zone still has live scene nodes")

// assertZoneDown 是前置门禁,两条都满足才放行:
//   - etcd 场景节点注册表里该 zone 一条注册都没有(正面证据,scene_registry.go);registry 为 nil 或读失败一律拒绝;
//   - 负载集 scene_nodes:zone:{S}:load 为空(辅助条件:注册已消失、scene_manager 还没摘干净负载集时不抢跑)。
//
// 只看负载集不够:负载键丢失 / 被淘汰时 ZCARD 读作 0,活着的 zone 会被当成已下线(cross-zone-scene-travel.md §13.10)。
func assertZoneDown(ctx context.Context, rdb redis.UniversalClient, registry sceneRegistry, zone uint32) error {
	if registry == nil {
		return fmt.Errorf("%w: no etcd scene-node registry to prove zone %d is down (pass -etcd-endpoints)", errZoneStillLive, zone)
	}
	registered, err := registry.SceneNodesInZone(ctx, zone)
	if err != nil {
		return fmt.Errorf("%w: cannot prove zone %d is down: %v", errZoneStillLive, zone, err)
	}
	if registered > 0 {
		return fmt.Errorf("%w: etcd still registers %d scene node(s) of zone %d — finish zone-down first", errZoneStillLive, registered, zone)
	}
	key := fmt.Sprintf(sceneNodeLoadKeyFmt, zone)
	n, err := rdb.ZCard(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("zcard %s: %w", key, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %s has %d members — finish zone-down first", errZoneStillLive, key, n)
	}
	return nil
}

// preflightDeps 是 preflight 需要的全部句柄。任何一个为 nil 的检查会被跳过
// **并报错** —— 「没句柄所以没查」不能等于「查过了没问题」。
type preflightDeps struct {
	// sceneRdb: scene_manager 的 Redis(scene_nodes:zone:{z}:load)。
	sceneRdb redis.UniversalClient
	// sceneNodes: etcd 场景节点注册表(scene_registry.go)。P2 的正面证据,nil 即拒绝。
	sceneNodes sceneRegistry
	// sharedRdb: 共享 DB 0(player:session:*)。
	sharedRdb     *redis.Client
	teamAuthority teamAuthorityReader
}

// preflightParams 是本次运行的参数面。
type preflightParams struct {
	// readOnly只表达预览。实际写仍需独立TOOL和NORMAL冻结原件。
	readOnly bool
	// zone 是写之前必须已经停干净的那个 zone:合服是源区,撤销是目标区(此刻源区玩家解析到目标区)。
	zone uint32
	// scope 只进日志与拒绝文案("source" / "target"),让运维一眼看出拦住的是哪一侧。
	scope string
	// playerIDs 是本次涉及的玩家(home_zone = 源区),P6 / P7 逐个查。
	playerIDs []uint64
}

// runPreflight 依次跑 P2 / P6 / P7(P1 在 SQL 侧,由调用方先做)。任何一条不过就返回错误;调用方此时还没写任何东西。
func runPreflight(ctx context.Context, d preflightDeps, p preflightParams) (err error) {
	finish, err := enterMaintenanceWork(ctx)
	if err != nil {
		return err
	}
	returned := false
	defer func() {
		if !returned || err != nil {
			holdMaintenanceTool(ctx)
		}
		finish()
	}()
	if !p.readOnly {
		if normalFreezeFromContext(ctx) == nil {
			return errNormalFreezeProof
		}
		if err := checkMaintenanceTool(ctx); err != nil {
			return err
		}
	}
	// P2 那个 zone 的场景节点全下线。
	if d.sceneRdb == nil {
		return fmt.Errorf("preflight: no scene_manager Redis handle — cannot prove the %s zone %d is down", p.scope, p.zone)
	}
	if err := assertZoneDown(ctx, d.sceneRdb, d.sceneNodes, p.zone); err != nil {
		return fmt.Errorf("preflight P2 (%s zone %d): %w", p.scope, p.zone, err)
	}
	log.Printf("preflight P2 OK: %s zone %d has no registered scene node in etcd and an empty load set", p.scope, p.zone)

	// P6 在线会话。player:session:{id} 由 player_locator 维护,DB 0。
	if d.sharedRdb == nil {
		return errors.New("preflight P6: no shared (DB 0) Redis handle — cannot inspect player:session")
	}
	sessions, err := countExistingKeys(ctx, d.sharedRdb, p.playerIDs, "player:session:")
	if err != nil {
		return fmt.Errorf("preflight P6: %w", err)
	}
	if sessions > 0 {
		return fmt.Errorf("preflight P6: %d of the players in scope still have player:session:* — zone-down is incomplete", sessions)
	}
	log.Printf("preflight P6 OK: no player:session:* for the %d players in scope", len(p.playerIDs))

	// P7 全局SQL成员原件。队伍记录写着成员的 zone,合服不迁移;清单之外也没有别的办法把它们改对。
	if d.teamAuthority == nil {
		return errors.New("preflight P7: 缺少全球Team SQL权威读者")
	}
	inTeam, err := d.teamAuthority.PlayersInTeams(ctx, p.playerIDs)
	if err != nil {
		return fmt.Errorf("preflight P7: %w", err)
	}
	if inTeam > 0 {
		return fmt.Errorf("preflight P7: %d名玩家仍有SQL队伍成员关系，必须先安全离队或解散；Redis缺失不能解除此门禁", inTeam)
	}
	log.Printf("preflight P7 OK: none of the %d players in scope is in a live team", len(p.playerIDs))
	returned = true
	return nil
}

// countExistingKeys 数 prefix+id 里有多少存在。按 pipeline 分批,不用 SCAN:
// 我们要查的是**确定的一组 id**,SCAN 只会更慢且可能漏。
func countExistingKeys(ctx context.Context, rdb *redis.Client, ids []uint64, prefix string) (int, error) {
	if rdb == nil {
		return 0, errors.New("nil redis handle")
	}
	found := 0
	for _, batch := range chunkUint64(ids, 500) {
		pipe := rdb.Pipeline()
		cmds := make([]*redis.IntCmd, 0, len(batch))
		for _, id := range batch {
			cmds = append(cmds, pipe.Exists(ctx, prefix+strconv.FormatUint(id, 10)))
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return found, fmt.Errorf("exists %s*: %w", prefix, err)
		}
		for _, c := range cmds {
			if err := c.Err(); err != nil {
				holdMaintenanceTool(ctx)
				return found, fmt.Errorf("exists %s*: %w", prefix, err)
			}
			if c.Val() > 0 {
				found++
			}
		}
	}
	return found, nil
}
