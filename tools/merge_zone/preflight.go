package main

// 合服 / 撤销的前置门禁(preflight)。全部是**拒绝执行**,不是警告。
//
// 门禁存在的理由都一样:合服的四个写入面之间没有事务,一旦开写就没有便宜的
// 回头路。所有「这次能不能开始」的判断必须在第一次写之前问完。
//
// 检查清单(顺序即执行顺序)。「那个 zone」合服时是源区,撤销时是目标区(见 preflightParams.zone):
//   P1 源 / 目标 zone 库存在              —— 目标 zone 从没部署过就不是合服目标(MySQL 侧,调用方先做)
//   P2 那个 zone 的场景节点全下线          —— scene_nodes:zone:{zone}:load 必须空
//   P3 go/db 对那个 zone topic 的积压 = 0   —— 还有没落库的存盘任务就动 = 动的是一份旧的
//   P4 kafka:retry / kafka:dead 队列为空    —— 同上,而且这两个是**已经失败**的任务
//   P5 本次涉及的玩家没有任何在持的锁      —— lock:player:* / player:{id}:__lock
//   P6 本次涉及的玩家没有在线会话          —— player:session:*
//   P7 本次涉及的玩家不在任何活队伍里      —— team:player:* 的 tid 非 0 且 team:rec:<tid> 仍在
//
// 合服与撤销共用这一套(2026-09-28,player-storage-placement.md §12 A7):撤销要把玩家改回源区、删目标库
// 的行,目标区还有节点在跑、目标 topic 还有没落库的存盘、玩家还在线,都等于把合服之后产生的写直接丢掉。
//
// P3 的可注入性:Kafka 在开发机上根本不跑,而这条检查又不能省(它保护的是
// 「玩家最后一次存盘有没有落库」)。所以 lag 的来源是一个接口:
//   - kafkaCLILagSource   跑 kafka-consumer-groups(.sh|.bat) 解析 LAG 列 —— 生产用法
//   - attestedLagSource   运维显式声明「我已经确认排空了」—— 本地 / 无 CLI 环境
//   - fakeLagSource       测试注入
// 三者都没有 = 拒绝。默认不能是「查不到就当 0」。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// ── Kafka 消费积压 ────────────────────────────────────────────

// kafkaLagSource 报告某 consumer group 在某 topic 上的总积压。
type kafkaLagSource interface {
	// TotalLag 返回该 group 在该 topic 上所有分区的 LAG 之和。
	// 返回 (0, nil) 才算通过;任何错误都让 preflight 失败。
	TotalLag(ctx context.Context, topic, group string) (int64, error)
	// Describe 一行说明,进日志与审计记录 —— 运维必须能看出这次的 0 是
	// 「真查的」还是「人肉声明的」。
	Describe() string
}

// kafkaCLILagSource 调用 Kafka 自带的 kafka-consumer-groups CLI。
// 为什么不直接连 broker:merge_zone 是独立 go.mod,拉 sarama 只为了跑一次
// describe-group 不划算,而 CLI 在任何装了 Kafka 的运维机上都有。
type kafkaCLILagSource struct {
	cmdPath   string // kafka-consumer-groups.sh / .bat 的绝对路径
	bootstrap string
}

func (s kafkaCLILagSource) Describe() string {
	return fmt.Sprintf("kafka-consumer-groups CLI (%s, bootstrap=%s)", s.cmdPath, s.bootstrap)
}

func (s kafkaCLILagSource) TotalLag(ctx context.Context, topic, group string) (int64, error) {
	cmd := exec.CommandContext(ctx, s.cmdPath,
		"--bootstrap-server", s.bootstrap, "--describe", "--group", group)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("run %s: %w (output: %s)", s.cmdPath, err, truncateForLog(string(out), 500))
	}
	return parseConsumerGroupLag(string(out), topic)
}

// parseConsumerGroupLag 解析 kafka-consumer-groups --describe 的表格输出。
//
// 表头形如(列顺序在 2.x~3.x 稳定):
//
//	GROUP TOPIC PARTITION CURRENT-OFFSET LOG-END-OFFSET LAG CONSUMER-ID HOST CLIENT-ID
//
// 按**表头名**定位 TOPIC / LAG 两列,不按固定下标 —— 版本间会插列。
// LAG 为 "-"(消费者不在线且没有已提交位点)按错误处理:那说明这个 group
// 从没消费过这个 topic,「积压 0」的结论不成立。
func parseConsumerGroupLag(out, topic string) (int64, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	topicCol, lagCol := -1, -1
	var total int64
	matched := 0
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if topicCol < 0 {
			for i, f := range fields {
				switch strings.ToUpper(f) {
				case "TOPIC":
					topicCol = i
				case "LAG":
					lagCol = i
				}
			}
			if topicCol >= 0 && lagCol >= 0 {
				continue // 这一行是表头
			}
			topicCol, lagCol = -1, -1
			continue
		}
		if len(fields) <= topicCol || len(fields) <= lagCol {
			continue
		}
		if fields[topicCol] != topic {
			continue
		}
		matched++
		lag, err := strconv.ParseInt(fields[lagCol], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("topic %s partition row has non-numeric LAG %q — group has no committed offset; cannot prove the backlog is drained",
				topic, fields[lagCol])
		}
		total += lag
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if topicCol < 0 || lagCol < 0 {
		return 0, fmt.Errorf("could not find TOPIC/LAG columns in kafka-consumer-groups output")
	}
	if matched == 0 {
		return 0, fmt.Errorf("consumer group has no partition assigned to topic %s — cannot prove the backlog is drained", topic)
	}
	return total, nil
}

// attestedLagSource 是运维的显式声明:「我已经用别的手段确认排空了」。
// 它不查任何东西,但会在日志里留下一条刺眼的记录 —— 这条声明进事故复盘。
type attestedLagSource struct{}

func (attestedLagSource) Describe() string {
	return "OPERATOR ATTESTATION (-assume-kafka-drained): no lag was actually measured"
}

func (attestedLagSource) TotalLag(context.Context, string, string) (int64, error) { return 0, nil }

// fakeLagSource 只给测试用。
type fakeLagSource struct {
	lag int64
	err error
}

func (f fakeLagSource) Describe() string                                        { return "fake" }
func (f fakeLagSource) TotalLag(context.Context, string, string) (int64, error) { return f.lag, f.err }

// dbTaskTopic 镜像 go/db/internal/config.DbTaskTopicForGeneration。
// 改一处必须同步另一处(merge_zone 是独立 module,不引主工程包)。
func dbTaskTopic(zone, generation uint32) string {
	base := fmt.Sprintf("db_task_zone_%d", zone)
	if generation <= 1 {
		return base
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}

// dbRetryQueueKey / dbDeadQueueKey 镜像 go/db/internal/kafka/key_ordered_consumer.go
// 里 newConsumer 拼的三把 key(retryQueueKey / retryProcessingKey / retryDeadQueueKey)。
// 它们在 go/db 的 RedisClient 上,go/db/etc/db.yaml 里是 DB 0。
func dbRetryQueueKey(topic string) string      { return "kafka:retry:queue:" + topic }
func dbRetryProcessingKey(topic string) string { return "kafka:retry:processing:" + topic }
func dbDeadQueueKey(topic string) string       { return "kafka:dead:queue:" + topic }

// ── preflight 主体 ────────────────────────────────────────────

// preflightDeps 是 preflight 需要的全部句柄。任何一个为 nil 的检查会被跳过
// **并报错** —— 「没句柄所以没查」不能等于「查过了没问题」。
type preflightDeps struct {
	// sceneRdb: scene_manager 的 Redis(scene_nodes:zone:{z}:load)。
	sceneRdb redis.UniversalClient
	// mappingRdb: data_service mapping Redis(lock:player:*)。
	mappingRdb *redis.Client
	// sharedRdb: 共享 DB 0(player:session:* / kafka:retry|dead:queue:*)。
	sharedRdb *redis.Client
	// lag: Kafka 积压来源,见上。
	lag kafkaLagSource
}

// preflightParams 是本次运行的参数面。
type preflightParams struct {
	// zone 是写之前必须已经停干净的那个 zone:场景节点全下线、它的 db_task topic 无积压、重试 / 死信队列为空。
	// 合服是源区:源区玩家最后的存盘在源 topic 上,没落库就拷 = 拷走一份旧的。
	// 撤销是目标区:合服后这批玩家的存盘走目标 topic,没落库就改回路由、删目标行 = 把这些存盘丢掉。
	zone uint32
	// scope 只进日志与拒绝文案("source" / "target"),让运维一眼看出拦住的是哪一侧。
	scope           string
	kafkaGroup      string
	topicGeneration uint32
	// playerIDs 是本次要动的玩家(合服 = 收集到的源区玩家;撤销 = 清单),P5~P7 逐个查。
	playerIDs []uint64
}

// runPreflight 依次跑 P2~P7(P1 在 MySQL 侧,由调用方先做)。任何一条不过就返回错误;
// 调用方此时已立围栏、还没写任何东西,走 mergeFence.refuseAndRelease(释放围栏再非零退出)。
func runPreflight(ctx context.Context, d preflightDeps, p preflightParams) error {
	// P2 那个 zone 的场景节点全下线。
	if d.sceneRdb == nil {
		return fmt.Errorf("preflight: no scene_manager Redis handle — cannot prove the %s zone %d is down", p.scope, p.zone)
	}
	if err := assertZoneDown(ctx, d.sceneRdb, p.zone); err != nil {
		return fmt.Errorf("preflight P2 (%s zone %d): %w", p.scope, p.zone, err)
	}
	log.Printf("preflight P2 OK: %s zone %d has no live scene nodes", p.scope, p.zone)

	// P3 Kafka 消费积压。
	if d.lag == nil {
		return errors.New("preflight P3: no Kafka lag source configured — pass -kafka-consumer-groups-cmd, " +
			"or -assume-kafka-drained if you have verified the backlog by other means")
	}
	topic := dbTaskTopic(p.zone, p.topicGeneration)
	lag, err := d.lag.TotalLag(ctx, topic, p.kafkaGroup)
	if err != nil {
		return fmt.Errorf("preflight P3 (%s): %w", d.lag.Describe(), err)
	}
	if lag != 0 {
		return fmt.Errorf("preflight P3: consumer group %q still has lag %d on topic %s — "+
			"the last saves on the %s zone's topic are not in zone_%d_db yet", p.kafkaGroup, lag, topic, p.scope, p.zone)
	}
	log.Printf("preflight P3 OK: %s lag=0 on %s [%s]", p.kafkaGroup, topic, d.lag.Describe())

	// P4 retry / dead / processing 队列必须空。
	if d.sharedRdb == nil {
		return errors.New("preflight P4: no shared (DB 0) Redis handle — cannot inspect kafka retry/dead queues")
	}
	for _, k := range []string{dbRetryQueueKey(topic), dbRetryProcessingKey(topic), dbDeadQueueKey(topic)} {
		n, err := d.sharedRdb.LLen(ctx, k).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return fmt.Errorf("preflight P4: llen %s: %w", k, err)
		}
		if n > 0 {
			return fmt.Errorf("preflight P4: %s holds %d unprocessed db_task payload(s) — "+
				"those writes never reached zone_%d_db. Drain or triage them before touching these players", k, n, p.zone)
		}
	}
	log.Printf("preflight P4 OK: kafka retry/processing/dead queues for %s are empty", topic)

	// P5 玩家锁。lock:player:{id} 在 mapping Redis(data_service router.go),
	// player:{id}:__lock 在 data Redis —— 后者的地址由 -source-data-redis 决定,
	// 不一定配了,所以这里只硬查 mapping 侧,data 侧在 audit 里覆盖。
	if d.mappingRdb == nil {
		return errors.New("preflight P5: no mapping Redis handle")
	}
	locked, err := countExistingKeys(ctx, d.mappingRdb, p.playerIDs, "lock:player:")
	if err != nil {
		return fmt.Errorf("preflight P5: %w", err)
	}
	if locked > 0 {
		return fmt.Errorf("preflight P5: %d of the players in scope still hold lock:player:* in the mapping Redis — "+
			"someone is mid-write on them", locked)
	}
	log.Printf("preflight P5 OK: no lock:player:* held by the %d players in scope", len(p.playerIDs))

	// P6 在线会话。player:session:{id} 由 player_locator 维护,DB 0。
	sessions, err := countExistingKeys(ctx, d.sharedRdb, p.playerIDs, "player:session:")
	if err != nil {
		return fmt.Errorf("preflight P6: %w", err)
	}
	if sessions > 0 {
		return fmt.Errorf("preflight P6: %d of the players in scope still have player:session:* — zone-down is incomplete", sessions)
	}
	log.Printf("preflight P6 OK: no player:session:* for the %d players in scope", len(p.playerIDs))

	// P7 组队。队伍只存在 SharedRedis(DB 0,与 player:session 同库),记录里写着每个成员的 zone_id
	// 与队伍 zone_id(docs/design/team-system.md §C.1、§D.3),合服不迁移这些数据:玩家改归目标区后,
	// 跨区校验与场景跟随的 zone 守卫会按旧 zone 误判。所以要求合服前解散队伍,或等队伍 24h 空闲过期(J-18)。
	inTeam, err := countPlayersInLiveTeams(ctx, d.sharedRdb, p.playerIDs)
	if err != nil {
		return fmt.Errorf("preflight P7: %w", err)
	}
	if inTeam > 0 {
		return fmt.Errorf("preflight P7: %d of the players in scope are still members of a live team (team:player:* -> team:rec:*) — "+
			"team records carry each member's zone_id and are not migrated. Disband those teams (or wait for the 24h idle expiry) first", inTeam)
	}
	log.Printf("preflight P7 OK: none of the %d players in scope is in a live team", len(p.playerIDs))
	return nil
}

// teamPlayerIndexKey / teamRecordKey 镜像组队模块(go/match/internal/team/keys.go)写在 SharedRedis 的两类 key
// (docs/design/team-system.md §C.1):team:player:<player_id> 是 hash{tid, epoch},离队后 tid 置 "0" 但 key 保留;
// team:rec:<team_id> 是队伍权威记录。merge_zone 是独立 module,不引主工程包;改一处必须同步另一处。
func teamPlayerIndexKey(playerID uint64) string {
	return "team:player:" + strconv.FormatUint(playerID, 10)
}

func teamRecordKey(teamID string) string { return "team:rec:" + teamID }

// teamIndexNoTeam 是 team:player:* 里 tid 字段"无队伍"的取值。
const teamIndexNoTeam = "0"

// countPlayersInLiveTeams 数给定玩家里有多少人的组队索引指向仍存在的队伍记录。
//
// 判定口径与组队模块一致:索引 key 不存在或 tid 为 "0" 即无队;tid 非 0 但 team:rec 已不存在是孤儿索引
// (队伍已过期,组队模块下次读到会自愈为无队,§C.6),不阻塞合服。索引类型不对等读错误一律返回错误(fail-closed)。
// 按 pipeline 分批,理由同 countExistingKeys。
func countPlayersInLiveTeams(ctx context.Context, rdb *redis.Client, ids []uint64) (int, error) {
	if rdb == nil {
		return 0, errors.New("nil redis handle")
	}
	found := 0
	for _, batch := range chunkUint64(ids, 500) {
		pipe := rdb.Pipeline()
		tidCmds := make([]*redis.StringCmd, 0, len(batch))
		for _, id := range batch {
			tidCmds = append(tidCmds, pipe.HGet(ctx, teamPlayerIndexKey(id), "tid"))
		}
		// Exec 在任一命令返回 redis.Nil(key 或字段不存在)时也会报 Nil;逐条判错,不能整批当失败。
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return found, fmt.Errorf("hget team:player:* tid: %w", err)
		}
		tids := make([]string, 0, len(batch))
		for _, cmd := range tidCmds {
			tid, err := cmd.Result()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return found, fmt.Errorf("hget team:player:* tid: %w", err)
			}
			if tid == "" || tid == teamIndexNoTeam {
				continue
			}
			tids = append(tids, tid)
		}
		if len(tids) == 0 {
			continue
		}

		pipe = rdb.Pipeline()
		existsCmds := make([]*redis.IntCmd, 0, len(tids))
		for _, tid := range tids {
			existsCmds = append(existsCmds, pipe.Exists(ctx, teamRecordKey(tid)))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return found, fmt.Errorf("exists team:rec:*: %w", err)
		}
		for _, cmd := range existsCmds {
			if cmd.Val() > 0 {
				found++
			}
		}
	}
	return found, nil
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
			if c.Val() > 0 {
				found++
			}
		}
	}
	return found, nil
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
