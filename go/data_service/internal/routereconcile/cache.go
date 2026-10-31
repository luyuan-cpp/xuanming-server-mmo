package routereconcile

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// RedisCache 是 Cache 的生产实现:leader 独立的 go-redis 客户端(规格 scout 1b-2 cache.go 一项)。
//
// 为什么不用 go-zero 的 *redis.Redis:go-zero 按地址缓存底层客户端,熔断钩子挂在第一次建客户端时(提案 §11.2 第 1b-1 批风险 3),
// leader 的全量预热 / 推送失败会计进 Router 的熔断,把同一 mapping Redis 上的登录读一起拖下水;独立客户端的连接池与熔断都和 Router 隔离。
// 构造时不连接(go-redis 懒连接),只有当选 leader 才真正建连接。
//
// 多键脚本(发布脚本三键、挂标记脚本两键)与 INFO 身份都按**单实例**定义:MappingRedis.Type 非空且不是 node 一律拒绝。
// MaxRetries = -1 关掉 go-redis 自己的重试(v9.18.0 options.go:-1 = 不重试;核实意见已亲阅):重发由 leader 下一拍 / 下一批负责,
// pipeline 不会在新连接上被整条重放到 WAITAOF 之后。
type RedisCache struct {
	rdb         *goredis.Client
	waitAOF     bool
	numLocal    int
	numReplicas int
	waitMs      int

	publish   *goredis.Script
	readyRead *goredis.Script
	cas       *goredis.Script
	cad       *goredis.Script
}

// NewRedisCache 建 leader 的 mapping Redis 客户端。rc 必须是 Normalize 过的 RouteStore 段(池大小取 Reconcile.RedisPoolSize,
// WAITAOF 取 WaitAOFNumLocal / WaitAOFNumReplicas / WaitAOFTimeoutMs,与 W1 的发布同口径)。
func NewRedisCache(mapping redis.RedisConf, rc config.RouteStoreConfig) (*RedisCache, error) {
	if mapping.Type != "" && mapping.Type != redis.NodeType {
		return nil, fmt.Errorf("route reconcile needs a single-instance mapping redis (MappingRedis.Type=%q, want %q): "+
			"multi-key scripts and the INFO identity are defined per instance", mapping.Type, redis.NodeType)
	}
	if strings.TrimSpace(mapping.Host) == "" {
		return nil, errors.New("route reconcile: MappingRedis.Host is empty")
	}
	var tlsConfig *tls.Config
	if mapping.Tls {
		// 与 go-zero redisclientmanager.go 的 getClient 同口径(Tls=true 时 InsecureSkipVerify),同一份 MappingRedis 配置两边行为一致。
		tlsConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 与 go-zero 对同一配置项的处理一致
	}
	readTimeout := time.Duration(rc.WaitAOFTimeoutMs)*time.Millisecond + time.Second
	rdb := goredis.NewClient(&goredis.Options{
		Addr:        mapping.Host,
		Username:    mapping.User,
		Password:    mapping.Pass,
		DB:          0,
		PoolSize:    rc.Reconcile.RedisPoolSize,
		MaxRetries:  -1,
		ReadTimeout: readTimeout,
		TLSConfig:   tlsConfig,
	})
	return newRedisCacheFromClient(rdb, rc), nil
}

// newRedisCacheFromClient 用一个现成的 go-redis 客户端装配(单测接 miniredis 用)。
func newRedisCacheFromClient(rdb *goredis.Client, rc config.RouteStoreConfig) *RedisCache {
	return &RedisCache{
		rdb:         rdb,
		waitAOF:     rc.WaitsForAOF(),
		numLocal:    rc.NumLocal(),
		numReplicas: rc.NumReplicas(),
		waitMs:      rc.WaitAOFTimeoutMs,
		publish:     goredis.NewScript(placement.PublishRouteScript),
		readyRead:   goredis.NewScript(placement.RouteReadyReadScript),
		cas:         goredis.NewScript(placement.RouteReadyCompareAndSetScript),
		cad:         goredis.NewScript(placement.RouteReadyCompareAndDeleteScript),
	}
}

// Close 关客户端。
func (c *RedisCache) Close() error { return c.rdb.Close() }

// ── 身份与自检 ────────────────────────────────────────────────────────────────

// Probe 一个 pipeline:INFO server / replication / stats、CONFIG GET maxmemory-policy / enable-debug-command。
// INFO 任一段失败即报错;CONFIG 回错误回复(被禁用 / 改名)记为 ""(未知),网络错误报错。
func (c *RedisCache) Probe(ctx context.Context) (Probe, error) {
	var srv, repl, stats *goredis.StringCmd
	var policy, debug *goredis.MapStringStringCmd
	_, pipeErr := c.rdb.Pipelined(ctx, func(p goredis.Pipeliner) error {
		srv = p.Info(ctx, "server")
		repl = p.Info(ctx, "replication")
		stats = p.Info(ctx, "stats")
		policy = p.ConfigGet(ctx, "maxmemory-policy")
		debug = p.ConfigGet(ctx, "enable-debug-command")
		return nil
	})
	if srv == nil {
		return Probe{}, fmt.Errorf("probe pipeline was not executed: %v", pipeErr)
	}
	return probeFromReplies(
		infoReply{srv.Val(), srv.Err()}, infoReply{repl.Val(), repl.Err()}, infoReply{stats.Val(), stats.Err()},
		configReply{policy.Val(), policy.Err()}, configReply{debug.Val(), debug.Err()})
}

type infoReply struct {
	text string
	err  error
}

type configReply struct {
	vals map[string]string
	err  error
}

// probeFromReplies 是 Probe 的纯解析部分(单测用捕获的 Redis 7.2 文本表驱动,miniredis 不支持 INFO server / replication)。
func probeFromReplies(srv, repl, stats infoReply, policy, debug configReply) (Probe, error) {
	for name, r := range map[string]infoReply{"server": srv, "replication": repl, "stats": stats} {
		if r.err != nil {
			return Probe{}, fmt.Errorf("INFO %s: %w", name, r.err)
		}
	}
	id, err := placement.ParseRedisInstanceIdentity(srv.text, repl.text)
	if err != nil {
		return Probe{}, fmt.Errorf("parse INFO server / replication: %w", err)
	}
	p := Probe{Identity: id, EvictedKeys: -1}
	if v, ok := infoField(stats.text, "evicted_keys"); ok {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			p.EvictedKeys = n
		}
	}
	if p.MaxmemoryPolicy, err = configValue(policy, "maxmemory-policy"); err != nil {
		return Probe{}, err
	}
	if p.DebugCommand, err = configValue(debug, "enable-debug-command"); err != nil {
		return Probe{}, err
	}
	return p, nil
}

// configValue:错误回复(CONFIG 被禁用 / 改名 / ACL 拒绝)→ ""(未知,由调用方按 fail-closed 处理);网络错误原样返回。
func configValue(r configReply, name string) (string, error) {
	if r.err != nil {
		if isRedisErrorReply(r.err) {
			return "", nil
		}
		return "", fmt.Errorf("CONFIG GET %s: %w", name, r.err)
	}
	return r.vals[name], nil
}

// infoField 取 INFO 原文里 "field:value" 一行的 value(整行匹配字段名,认 CRLF / LF)。
func infoField(info, field string) (string, bool) {
	prefix := field + ":"
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, prefix) {
			return line[len(prefix):], true
		}
	}
	return "", false
}

// SelfCheck 跑一次读方同一段 RouteReadyReadScript(只带就绪标记一把键;EVALSHA,NOSCRIPT 时回退 EVAL)。
func (c *RedisCache) SelfCheck(ctx context.Context) (placement.RouteReadyRead, error) {
	raw, err := c.readyRead.Run(ctx, c.rdb, placement.RouteReadyReadKeys()).Result()
	if err != nil {
		return placement.RouteReadyRead{}, fmt.Errorf("route ready read script: %w", err)
	}
	return placement.ParseRouteReadyRead(raw, 0)
}

// ── 控制键 ────────────────────────────────────────────────────────────────────

// ReadControl 一个 pipeline:MGET ready warm watermark + TYPE ready + TYPE watermark(区分「不在」与「不是 string」)。
// 水位键也要 TYPE(核实意见 A1-8):不是 string 时 MGET 回 nil,不区分就会被当成「缺席」—— SET NX 因键在而每拍失败、M3 永久失效,
// Arm=false 下还会被当成「副本被清空」反复作废 cache_verified_ms。
func (c *RedisCache) ReadControl(ctx context.Context) (Control, error) {
	var mget *goredis.SliceCmd
	var readyType, watermarkType *goredis.StatusCmd
	_, pipeErr := c.rdb.Pipelined(ctx, func(p goredis.Pipeliner) error {
		mget = p.MGet(ctx, placement.RouteReadyKey, placement.RouteWarmKey, placement.RouteWatermarkKey)
		readyType = p.Type(ctx, placement.RouteReadyKey)
		watermarkType = p.Type(ctx, placement.RouteWatermarkKey)
		return nil
	})
	if mget == nil {
		return Control{}, fmt.Errorf("control keys pipeline was not executed: %v", pipeErr)
	}
	vals, err := mget.Result()
	if err != nil {
		return Control{}, fmt.Errorf("MGET control keys: %w", err)
	}
	if len(vals) != 3 {
		return Control{}, fmt.Errorf("MGET control keys returned %d values, want 3", len(vals))
	}
	rt, err := readyType.Result()
	if err != nil {
		return Control{}, fmt.Errorf("TYPE %s: %w", placement.RouteReadyKey, err)
	}
	wt, err := watermarkType.Result()
	if err != nil {
		return Control{}, fmt.Errorf("TYPE %s: %w", placement.RouteWatermarkKey, err)
	}
	ctl := Control{Ready: strPtr(vals[0]), Warm: strPtr(vals[1]), Watermark: strPtr(vals[2])}
	ctl.ReadyWrongType = ctl.Ready == nil && rt != "none" && rt != "string"
	ctl.WatermarkWrongType = ctl.Watermark == nil && wt != "none" && wt != "string"
	return ctl, nil
}

func strPtr(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}

// SetWarmToken 覆写预热令牌(M2:每个可能以挂标记收尾的任务开始时 SET 新令牌,无 TTL)。
func (c *RedisCache) SetWarmToken(ctx context.Context, token string) error {
	if !placement.ValidRouteWarmToken(token) {
		return fmt.Errorf("%w: warm token %q", placement.ErrMalformed, token)
	}
	return c.rdb.Set(ctx, placement.RouteWarmKey, token, 0).Err()
}

// ArmReady 挂就绪标记:同一个 pipeline(同一条连接)里 EVAL RouteReadyArmScript,紧跟 WAITAOF(配置要求时)。
// 挂上了(回 1)而 WAITAOF 报错 / 份数不足:按原值撤掉刚挂的标记并返回 error —— 没确认落盘的标记不留给读方。
// EVAL 而不是 EVALSHA:pipeline 里 NOSCRIPT 无法透明回退,挂标记又很少发生。
func (c *RedisCache) ArmReady(ctx context.Context, token string, m placement.RouteReadyMarker) (placement.RouteReadyArmOutcome, error) {
	keys, args, err := placement.RouteReadyArmKeysAndArgs(token, m)
	if err != nil {
		return 0, err
	}
	var evalCmd, waitCmd *goredis.Cmd
	_, pipeErr := c.rdb.Pipelined(ctx, func(p goredis.Pipeliner) error {
		evalCmd = p.Eval(ctx, placement.RouteReadyArmScript, keys, args...)
		if c.waitAOF {
			waitCmd = p.Do(ctx, "WAITAOF", c.numLocal, c.numReplicas, c.waitMs)
		}
		return nil
	})
	if evalCmd == nil {
		return 0, fmt.Errorf("arm pipeline was not executed: %v", pipeErr)
	}
	if err := evalCmd.Err(); err != nil {
		return 0, fmt.Errorf("route ready arm script: %w", err)
	}
	out, err := placement.ClassifyRouteReadyArmReply(evalCmd.Val())
	if err != nil {
		return 0, err
	}
	if out != placement.RouteReadyArmArmed || waitCmd == nil {
		return out, nil
	}
	acks, werr := waitCmd.Slice()
	if werr == nil {
		werr = checkWaitAOFReply(acks, c.numLocal, c.numReplicas)
	}
	if werr == nil {
		return out, nil
	}
	raw, _ := args[1].(string)
	if _, derr := c.CompareAndDeleteReady(ctx, raw); derr != nil {
		return 0, fmt.Errorf("armed but WAITAOF failed (%v); dropping the unconfirmed marker also failed: %w", werr, derr)
	}
	return 0, fmt.Errorf("armed but not confirmed durable, the marker was dropped again: %w", werr)
}

// CompareAndSetReady 按原值换标记(RouteReadyCompareAndSetScript)。
func (c *RedisCache) CompareAndSetReady(ctx context.Context, old, next string) (bool, error) {
	return c.runFlag(ctx, c.cas, placement.RouteReadyKey, old, next)
}

// CompareAndDeleteReady 按原值撤标记(RouteReadyCompareAndDeleteScript)。
func (c *RedisCache) CompareAndDeleteReady(ctx context.Context, old string) (bool, error) {
	return c.runFlag(ctx, c.cad, placement.RouteReadyKey, old)
}

// DeleteReadyWrongType 无条件 DEL 就绪标记键:只给「键在但不是 string」用(读方 MGET 把它当缺席,CAS / CAD 脚本对它报 WRONGTYPE)。
// 与「刚被挂上的 string 标记」赛跑时会误删它 —— 那是撤销信任的方向(fail-closed),leader 下一拍重新判。
func (c *RedisCache) DeleteReadyWrongType(ctx context.Context) error {
	return c.rdb.Del(ctx, placement.RouteReadyKey).Err()
}

// DeleteWatermarkWrongType 无条件 DEL 水位键:只给「键在但不是 string」用(核实意见 A1-8;CAS 脚本对它报 WRONGTYPE、SET NX 因键在而失败)。
// 与「刚被 SET NX 的 string 水位键」赛跑时会误删它 —— 只丢一拍的 M3 证据,leader 下一拍按库值重建。
func (c *RedisCache) DeleteWatermarkWrongType(ctx context.Context) error {
	return c.rdb.Del(ctx, placement.RouteWatermarkKey).Err()
}

// AdvanceWatermark 推进水位键:prev 为 nil 时 SET NX,否则按原值 CAS(复用就绪标记的 CAS 脚本,KEYS[1] 换成水位键)。
func (c *RedisCache) AdvanceWatermark(ctx context.Context, prev *string, next string) (bool, error) {
	if prev == nil {
		return c.rdb.SetNX(ctx, placement.RouteWatermarkKey, next, 0).Result()
	}
	return c.runFlag(ctx, c.cas, placement.RouteWatermarkKey, *prev, next)
}

// runFlag 跑一段回 0 / 1 的单键脚本(EVALSHA,NOSCRIPT 回退 EVAL)。
func (c *RedisCache) runFlag(ctx context.Context, s *goredis.Script, key string, args ...any) (bool, error) {
	n, err := s.Run(ctx, c.rdb, []string{key}, args...).Int64()
	if err != nil {
		return false, fmt.Errorf("script on %s: %w", key, err)
	}
	switch n {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, fmt.Errorf("script on %s returned %d, want 0 or 1", key, n)
}

// ── 推送 ──────────────────────────────────────────────────────────────────────

// publishArgs 是一行要发给发布脚本的参数。
type publishArgs struct {
	idx            int
	keys           []string
	rev, home, plc string
}

// Push 推一批库行到 mapping Redis(规格 scout 1b-2 cache.go「Push(rows)」):
//  1. 墓碑行(home 与落点都为空)跳过:推了会造出只有 maprev 的键,导入 / 审计工具判它畸形;落点三列不自洽、rev 不合法 → db_invalid;
//  2. 一次 MGET 取 zone / placement / maprev,与本行值和 rev 全等的计 skipped_current(稳态下增量推送绝大多数行走这里,零 EVALSHA);
//  3. 其余在一个 pipeline 里 EVALSHA 发布脚本(三键);
//  4. 回 NOSCRIPT 的行 SCRIPT LOAD 后只重发这些行(脚本是 set-if-newer + touch,重发幂等);
//  5. 逐行用 placement.ClassifyPublishReply 分类;脚本的错误回复(畸形 maprev 等)是逐行 PushError;网络错误整批返回 error。
//
// 返回与 rows 一一对应。
func (c *RedisCache) Push(ctx context.Context, rows []store.RouteRow) ([]PushRowResult, error) {
	out := make([]PushRowResult, len(rows))
	var todo []publishArgs
	for i, row := range rows {
		out[i].PlayerID = row.PlayerID
		if row.IsTombstone() {
			out[i].Outcome = PushTombstoneSkipped
			continue
		}
		plc, err := row.PlacementValue()
		rev := placement.FormatRev(row.Rev)
		switch {
		case err != nil:
			out[i].Outcome, out[i].Detail = PushDBInvalid, err.Error()
			continue
		case row.PlayerID == 0 || !placement.ValidRev(rev):
			out[i].Outcome, out[i].Detail = PushDBInvalid, fmt.Sprintf("player_id=%d rev=%d", row.PlayerID, row.Rev)
			continue
		}
		todo = append(todo, publishArgs{idx: i, keys: placement.PublishRouteKeys(row.PlayerID), rev: rev, home: row.HomeValue(), plc: plc})
	}
	if len(todo) == 0 {
		return out, nil
	}

	mgetKeys := make([]string, 0, 3*len(todo))
	for _, a := range todo {
		mgetKeys = append(mgetKeys, a.keys...)
	}
	cached, err := c.rdb.MGet(ctx, mgetKeys...).Result()
	if err != nil {
		return nil, fmt.Errorf("MGET %d mapping keys: %w", len(mgetKeys), err)
	}
	if len(cached) != len(mgetKeys) {
		return nil, fmt.Errorf("MGET returned %d values for %d keys", len(cached), len(mgetKeys))
	}
	var evals []publishArgs
	for j, a := range todo {
		if sameString(cached[3*j], a.home) && sameString(cached[3*j+1], a.plc) && isString(cached[3*j+2], a.rev) {
			out[a.idx].Outcome = PushSkippedCurrent
			continue
		}
		evals = append(evals, a)
	}
	if len(evals) == 0 {
		return out, nil
	}

	noscript, err := c.evalPublish(ctx, evals, out)
	if err != nil {
		return nil, err
	}
	if len(noscript) == 0 {
		return out, nil
	}
	if err := c.publish.Load(ctx, c.rdb).Err(); err != nil {
		return nil, fmt.Errorf("SCRIPT LOAD the publish script after NOSCRIPT: %w", err)
	}
	again, err := c.evalPublish(ctx, noscript, out)
	if err != nil {
		return nil, err
	}
	for _, a := range again {
		out[a.idx].Outcome, out[a.idx].Detail = PushError, "NOSCRIPT again right after SCRIPT LOAD"
	}
	return out, nil
}

// evalPublish 在一个 pipeline 里对 list 逐行 EVALSHA,结果写进 out;返回回 NOSCRIPT 的行。网络错误整批返回 error。
func (c *RedisCache) evalPublish(ctx context.Context, list []publishArgs, out []PushRowResult) ([]publishArgs, error) {
	cmds := make([]*goredis.Cmd, len(list))
	_, pipeErr := c.rdb.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, a := range list {
			cmds[i] = c.publish.EvalSha(ctx, p, a.keys, a.rev, a.home, a.plc)
		}
		return nil
	})
	executed := false
	for _, cmd := range cmds {
		if cmd != nil && (cmd.Err() == nil || isRedisErrorReply(cmd.Err())) {
			executed = true
			break
		}
	}
	if !executed {
		if pipeErr == nil {
			pipeErr = errors.New("no command result")
		}
		return nil, fmt.Errorf("publish pipeline of %d rows not executed: %w", len(list), pipeErr)
	}
	var noscript []publishArgs
	for i, a := range list {
		cmd := cmds[i]
		err := cmd.Err()
		switch {
		case err == nil:
		case goredis.HasErrorPrefix(err, "NOSCRIPT"):
			noscript = append(noscript, a)
			continue
		case isRedisErrorReply(err):
			out[a.idx].Outcome, out[a.idx].Detail = PushError, err.Error()
			continue
		default:
			return nil, fmt.Errorf("publish player %d rev=%s: %w", out[a.idx].PlayerID, a.rev, err)
		}
		reply, err := placement.ClassifyPublishReply(cmd.Val(), a.rev, a.home, a.plc)
		if err != nil {
			out[a.idx].Outcome, out[a.idx].Detail = PushError, err.Error()
			continue
		}
		switch reply.Outcome {
		case placement.PublishApplied:
			out[a.idx].Outcome = PushApplied
		case placement.PublishConfirmed:
			out[a.idx].Outcome = PushConfirmed
		case placement.PublishSuperseded:
			out[a.idx].Outcome, out[a.idx].CacheRev = PushSuperseded, reply.CurRev
		case placement.PublishConflict:
			out[a.idx].Outcome = PushConflict
			out[a.idx].Detail = fmt.Sprintf("cached rev=%s home=%q placement=%q, db rev=%s home=%q placement=%q",
				reply.CurRev, reply.CurHome, reply.CurPlacement, a.rev, a.home, a.plc)
		case placement.PublishLegacyConflict:
			out[a.idx].Outcome = PushLegacyConflict
			out[a.idx].Detail = fmt.Sprintf("no maprev, cached home=%q placement=%q, db rev=%s home=%q placement=%q",
				reply.CurHome, reply.CurPlacement, a.rev, a.home, a.plc)
		default:
			out[a.idx].Outcome, out[a.idx].Detail = PushError, "unknown publish outcome "+reply.Outcome.String()
		}
	}
	return noscript, nil
}

// sameString:MGET 的值(nil = 不在 / 不是 string)等于 want(want 为空串 = 应当不在)。
func sameString(v any, want string) bool {
	if v == nil {
		return want == ""
	}
	s, ok := v.(string)
	return ok && s == want
}

// isString:v 是 string 且等于 want(maprev 必须真的存在)。
func isString(v any, want string) bool {
	s, ok := v.(string)
	return ok && s == want
}

// ── 合走标记 ────────────────────────────────────────────────────────

// MirrorMergedInto 把库里的合走标记镜像进 Redis(只增):SET NX;没写成就 GET 解析,同 dst 同 run_id 计 present,否则计冲突
// (不覆盖、不删除;db_first 下读方读库)。单行编码 / 解码失败计 Errors 并接着做其它行;网络错误整体返回 error(已有统计照样返回)。
func (c *RedisCache) MirrorMergedInto(ctx context.Context, rows []store.MergedIntoRow) (MergedIntoMirrorResult, error) {
	var res MergedIntoMirrorResult
	type item struct {
		row   store.MergedIntoRow
		key   string
		value string
	}
	items := make([]item, 0, len(rows))
	for _, r := range rows {
		v, err := placement.EncodeMergedIntoValue(r.DstZone, r.RunID)
		if err != nil {
			res.Errors++
			res.ConflictDetails = append(res.ConflictDetails, fmt.Sprintf("src=%d: zone_merged_into row cannot be encoded: %v", r.SrcZone, err))
			continue
		}
		items = append(items, item{row: r, key: placement.MergedIntoKey(r.SrcZone), value: v})
	}
	if len(items) == 0 {
		return res, nil
	}
	setCmds := make([]*goredis.BoolCmd, len(items))
	if _, err := c.rdb.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, it := range items {
			setCmds[i] = p.SetNX(ctx, it.key, it.value, 0)
		}
		return nil
	}); err != nil && !isRedisErrorReply(err) {
		return res, fmt.Errorf("SET NX merge:merged_into: %w", err)
	}
	for i, it := range items {
		set, err := setCmds[i].Result()
		switch {
		case err != nil:
			res.Errors++
			continue
		case set:
			res.Added++
			continue
		}
		cur, err := c.rdb.Get(ctx, it.key).Result()
		if err != nil {
			if errors.Is(err, goredis.Nil) {
				res.Errors++ // SET NX 没写成、随后又读不到:被并发删掉了,下一轮再补
				continue
			}
			if isRedisErrorReply(err) {
				res.Conflicts++
				res.ConflictDetails = append(res.ConflictDetails, fmt.Sprintf("%s is not a string: %v", it.key, err))
				continue
			}
			return res, fmt.Errorf("GET %s: %w", it.key, err)
		}
		dst, runID, derr := placement.DecodeMergedIntoValue(it.row.SrcZone, cur)
		if derr == nil && dst == it.row.DstZone && runID == it.row.RunID {
			res.Present++
			continue
		}
		res.Conflicts++
		res.ConflictDetails = append(res.ConflictDetails, fmt.Sprintf("%s: db dst=%d run_id=%s, cache %q", it.key, it.row.DstZone, it.row.RunID, cur))
	}
	return res, nil
}

// ── 小工具 ────────────────────────────────────────────────────────────────────

// isRedisErrorReply 判断 err 是不是 Redis 回的错误回复(确定性错误);网络错误、ctx 错误都不是。
func isRedisErrorReply(err error) bool {
	var re goredis.Error
	return errors.As(err, &re) && !errors.Is(err, goredis.Nil)
}

// checkWaitAOFReply 判断 WAITAOF 的回复 [numlocal, numreplicas] 是否满足要求的份数。
// 与 internal/routing/route_publish.go 的 checkWaitAOFReply 同一判据(那个未导出,这里复制一份纯函数,口径变更两处同改)。
func checkWaitAOFReply(vals []any, numLocal, numReplicas int) error {
	if len(vals) != 2 {
		return fmt.Errorf("unexpected WAITAOF reply: %d elements %v, want [numlocal, numreplicas]", len(vals), vals)
	}
	local, localOK := vals[0].(int64)
	replicas, replicasOK := vals[1].(int64)
	if !localOK || !replicasOK || local < 0 || replicas < 0 {
		return fmt.Errorf("unexpected WAITAOF reply: %v", vals)
	}
	if local < int64(numLocal) || replicas < int64(numReplicas) {
		return fmt.Errorf("WAITAOF acked numlocal=%d numreplicas=%d, want at least %d and %d", local, replicas, numLocal, numReplicas)
	}
	return nil
}
