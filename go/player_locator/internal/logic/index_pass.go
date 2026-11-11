package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"

	"shared/safego"
)

// 补建 / 审计按段分给各副本(docs/design/player-locator-gate-session-index.md §4.4、§4.5;去单点总纲 §9 的 C2)。
//
// 一遍完整扫描在 2000 万在线时要跑十几个小时,只有 leader 一个副本在扫就是容量单点。玩家域是 Cluster 时把它拆开:
//   - leader 仍然决定「什么时候跑哪一种」(就绪标记缺失 → 补建;就绪且审计到期 → 审计),也仍然在结束时设就绪标记、
//     记审计时刻(session_reconciler.go 的 runRepairPass、runAuditPass 不变)。它只是不自己扫了:fullPass 改为发布一遍、
//     等 256 段都做完、汇总各段的统计(fullPassSharded)。
//   - 每个副本按死 gate 清扫已有的段租约(gate_sweep 角色,slot_shard.go)做自己那些段:逐个 slot 列出键补漏成员(第 1 阶段),
//     再读这个 slot 的登记集清脏成员(第 2 阶段),做完一个 slot 就把这一段的进度推进到下一个(indexPassWorker)。
//   - 进度按段记在控制域:换主、副本重启、段换了主人都从断点接着做,不从头再来。
//
// 正确性不靠「同一时刻只有一个副本在做一段」:补成员、摘成员都走字节守卫脚本,两个副本短暂做同一个 slot 无害;
// 进度是比较后写入(passAdvanceScript),旧主人写不回已经被新主人推进过的进度,也写不进已经结束或被换掉的一遍。
//
// 单实例玩家域(只在 dev)没有按 slot 列键的命令,仍由 leader 自己整库扫描(fullPassLocal)。

// slotKeysListLimit 是一个 slot 一次列举的键数上限(playerredis.SlotKeys)。GETKEYSINSLOT 没有游标,超过就报错、不截断。
// 2000 万在线时一个 slot 约一千多个会话,加上同槽的其余玩家键也只有几千个;十万留了一个数量级以上的余量,一次回复约 3 MB。
// 真超过了这个 slot 的那一段停在这里,补建 / 审计的 last_success 告警会响,日志点名 slot。
const slotKeysListLimit = 100_000

// passKind 是一遍的种类。
type passKind string

const (
	passKindBackfill passKind = "backfill"
	passKindAudit    passKind = "audit"
)

// indexPass 是当前这一遍(gateIndexPassKey 的值)。gen 是 leader 发布它时读到的代次:换主后只有种类与代次都没变才接着做,
// 代次变了说明期间看到过旧写者,这一遍不能再用来宣布就绪。
type indexPass struct {
	id   string
	kind passKind
	gen  string
}

func (p indexPass) value() string { return p.id + "|" + string(p.kind) + "|" + p.gen }

func parseIndexPass(raw string) (indexPass, bool) {
	parts := strings.SplitN(raw, "|", 3)
	if len(parts) != 3 || parts[0] == "" {
		return indexPass{}, false
	}
	kind := passKind(parts[1])
	if kind != passKindBackfill && kind != passKindAudit {
		return indexPass{}, false
	}
	return indexPass{id: parts[0], kind: kind, gen: parts[2]}, true
}

// passProgress 是一段在某一遍里的进度:next 是下一个要做的 slot(等于段尾就是做完了),其余是已做完的 slot 的累计统计。
type passProgress struct {
	passID    string
	next      int
	scanned   int
	repaired  int
	skipped   int
	indexKeys int
	retries   int
	keyErrors int
}

func (p passProgress) value() string {
	return strings.Join([]string{p.passID, strconv.Itoa(p.next), strconv.Itoa(p.scanned), strconv.Itoa(p.repaired),
		strconv.Itoa(p.skipped), strconv.Itoa(p.indexKeys), strconv.Itoa(p.retries), strconv.Itoa(p.keyErrors)}, "|")
}

func parsePassProgress(raw string) (passProgress, bool) {
	parts := strings.Split(raw, "|")
	if len(parts) != 8 || parts[0] == "" {
		return passProgress{}, false
	}
	nums := make([]int, 7)
	for i, part := range parts[1:] {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return passProgress{}, false
		}
		nums[i] = n
	}
	return passProgress{passID: parts[0], next: nums[0], scanned: nums[1], repaired: nums[2], skipped: nums[3],
		indexKeys: nums[4], retries: nums[5], keyErrors: nums[6]}, true
}

// advance 是做完 next 这个 slot 之后的进度。
func (p passProgress) advance(rep passReport) passProgress {
	p.next++
	p.scanned += rep.scannedKeys
	p.repaired += rep.repaired
	p.skipped += rep.skippedCandidates
	p.indexKeys += rep.gates
	p.retries += rep.pageRetries
	p.keyErrors += rep.keyErrors.count
	return p
}

func segmentStart(seg int) int { return seg * slotsPerSegment }
func segmentEnd(seg int) int   { return (seg + 1) * slotsPerSegment }

// beginPassScript 发布一遍:当前这一遍的种类与代次都和这次一样就原样返回它(换主后接着做),否则换一个新编号、清空各段进度。
// KEYS[1]=gateIndexPassKey,KEYS[2]=gateIndexPassSeqKey,KEYS[3]=gateIndexPassProgressKey;ARGV[1]=种类,ARGV[2]=代次。返回这一遍的值。
var beginPassScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if cur then
    local id, kind, gen = string.match(cur, "^([^|]+)|([^|]+)|(.*)$")
    if id and kind == ARGV[1] and gen == ARGV[2] then
        return cur
    end
end
local id = redis.call("INCR", KEYS[2])
local value = tostring(id) .. "|" .. ARGV[1] .. "|" .. ARGV[2]
redis.call("SET", KEYS[1], value)
redis.call("DEL", KEYS[3])
return value
`)

// endPassScript 结束一遍:还是这一遍才删描述和进度。KEYS[1]=gateIndexPassKey,KEYS[2]=gateIndexPassProgressKey;ARGV[1]=编号。
var endPassScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if not cur or string.sub(cur, 1, string.len(ARGV[1]) + 1) ~= ARGV[1] .. "|" then
    return 0
end
redis.call("DEL", KEYS[1], KEYS[2])
return 1
`)

// passAdvanceScript 推进一段的进度:这一遍还在,且这一段的进度还是调用方读到的那一份,才写入新进度。
// KEYS[1]=gateIndexPassKey,KEYS[2]=gateIndexPassProgressKey;ARGV[1]=编号,ARGV[2]=段号,ARGV[3]=读到的旧值("" = 没有),ARGV[4]=新值。
var passAdvanceScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
if not cur or string.sub(cur, 1, string.len(ARGV[1]) + 1) ~= ARGV[1] .. "|" then
    return 0
end
local old = redis.call("HGET", KEYS[2], ARGV[2])
if not old then
    old = ""
end
if old ~= ARGV[3] then
    return 0
end
redis.call("HSET", KEYS[2], ARGV[2], ARGV[4])
return 1
`)

// errPassReplaced:等的这一遍已经不在了(另一个短暂重叠的 leader 换了一遍,或有人手工删了),这一遍作废。
var errPassReplaced = errors.New("index pass was replaced or removed while it ran")

// ---------------------------------------------------------------------------
// leader:发布一遍、等它做完
// ---------------------------------------------------------------------------

// fullPassSharded 发布一遍(或接着上一任没做完的那一遍),每拍查一次各段的进度,256 段都做完就结束这一遍、返回汇总。
// 只报统计,不改就绪标记与审计时刻:那是调用方(runRepairPass、runAuditPass)的事,判据和单机扫描一样。
// 任期结束(ctx 取消)时直接返回,这一遍和各段进度留着,下一任接着做。
func (r *sessionReconciler) fullPassSharded(ctx context.Context, audit bool) (passReport, error) {
	kind := passKindBackfill
	if audit {
		kind = passKindAudit
	}
	gen, err := readGateIndexGen(ctx, r.ctrl)
	if err != nil {
		return passReport{actions: map[string]int{}}, fmt.Errorf("read gate index gen: %w", err)
	}
	raw, err := beginPassScript.Run(ctx, r.ctrl, []string{gateIndexPassKey, gateIndexPassSeqKey, gateIndexPassProgressKey},
		string(kind), gen).Text()
	if err != nil {
		return passReport{actions: map[string]int{}}, fmt.Errorf("begin index pass: %w", err)
	}
	pass, ok := parseIndexPass(raw)
	if !ok {
		return passReport{actions: map[string]int{}}, fmt.Errorf("begin index pass returned a malformed value %q", raw)
	}
	logx.Infof("SessionReconciler: index pass %s (%s, gen=%q) running on the gate sweep segments", pass.id, pass.kind, pass.gen)
	defer gateIndexPassSegmentsDone.Set(0)
	for {
		done, rep, err := readPassCompletion(ctx, r.ctrl, pass)
		switch {
		case errors.Is(err, errPassReplaced):
			return rep, err
		case err != nil:
			// 读不到进度只是这一拍看不清,下一拍再看;各副本照常在做。
			if ctx.Err() == nil {
				logx.Errorf("SessionReconciler: read index pass %s progress failed, retry next interval: %v", pass.id, err)
			}
		default:
			gateIndexPassSegmentsDone.Set(float64(done))
			if done == slotSegmentCount {
				ended, err := endPassScript.Run(ctx, r.ctrl, []string{gateIndexPassKey, gateIndexPassProgressKey}, pass.id).Int()
				if err != nil {
					return rep, fmt.Errorf("end index pass %s: %w", pass.id, err)
				}
				if ended != 1 {
					return rep, errPassReplaced
				}
				return rep, nil
			}
		}
		if err := r.deps.sleep(ctx, r.interval); err != nil {
			return rep, err
		}
	}
}

// readPassCompletion 读这一遍做完了几段,并汇总这一遍各段的统计。这一遍已经不是当前那一遍时返回 errPassReplaced。
func readPassCompletion(ctx context.Context, ctrl *redis.Client, pass indexPass) (int, passReport, error) {
	rep := passReport{actions: map[string]int{}}
	pipe := ctrl.Pipeline()
	current := pipe.Get(ctx, gateIndexPassKey)
	progress := pipe.HGetAll(ctx, gateIndexPassProgressKey)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return 0, rep, err
	}
	if raw, err := current.Result(); errors.Is(err, redis.Nil) || (err == nil && raw != pass.value()) {
		return 0, rep, errPassReplaced
	} else if err != nil {
		return 0, rep, err
	}
	done := 0
	for field, raw := range progress.Val() {
		seg, err := strconv.Atoi(field)
		if err != nil || seg < 0 || seg >= slotSegmentCount {
			continue
		}
		p, ok := parsePassProgress(raw)
		if !ok || p.passID != pass.id {
			continue
		}
		rep.scannedKeys += p.scanned
		rep.repaired += p.repaired
		rep.skippedCandidates += p.skipped
		rep.gates += p.indexKeys
		rep.pageRetries += p.retries
		rep.keyErrors.count += p.keyErrors
		if p.next >= segmentEnd(seg) {
			done++
		}
	}
	return done, rep, nil
}

// ---------------------------------------------------------------------------
// 每个副本:做自己那些段
// ---------------------------------------------------------------------------

// segmentOwner 是做一遍时对分片的依赖:当前分给本副本的段号(升序)。生产为死 gate 清扫的 *slotShard。
type segmentOwner interface {
	OwnedSegments() []int
}

// slotKeyLister 列出一个 slot 的全部键,生产为 playerredis.SlotKeys。
type slotKeyLister func(ctx context.Context, c redis.UniversalClient, slot uint16, limit int) ([]string, error)

// indexPassWorker 每拍读一次当前这一遍,把分给本副本、还没做完的段逐个 slot 做完。
// 扫描与守卫脚本借用 sessionReconciler 的实现(它的 player / ctrl / cfg / deps / scanPacer,与 leader 的那一份互不相干)。
type indexPassWorker struct {
	r     *sessionReconciler
	owner segmentOwner
	list  slotKeyLister
}

func newIndexPassWorker(r *sessionReconciler, owner segmentOwner, list slotKeyLister) *indexPassWorker {
	return &indexPassWorker{r: r, owner: owner, list: list}
}

// run 每拍做一步,直到 ctx 结束。每一步各自 recover。
func (w *indexPassWorker) run(ctx context.Context) {
	for ctx.Err() == nil {
		safego.Run("player_locator.index_pass.step", func() { w.step(ctx) })
		if err := w.r.deps.sleep(ctx, w.r.interval); err != nil {
			return
		}
	}
}

// step 读当前这一遍,逐个持有的段往下做。一段出错只停这一段(下一拍从断点重试),不影响别的段。
func (w *indexPassWorker) step(ctx context.Context) {
	raw, err := w.r.ctrl.Get(ctx, gateIndexPassKey).Result()
	if errors.Is(err, redis.Nil) {
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			logx.Errorf("SessionReconciler: read index pass failed, retry next interval: %v", err)
		}
		return
	}
	pass, ok := parseIndexPass(raw)
	if !ok {
		logx.Errorf("SessionReconciler: malformed index pass %q, waiting for the leader to replace it", raw)
		return
	}
	for _, seg := range w.owner.OwnedSegments() {
		if ctx.Err() != nil {
			return
		}
		if err := w.runSegment(ctx, pass, seg); err != nil && ctx.Err() == nil {
			logx.Errorf("SessionReconciler: index pass %s (%s) stopped at segment %d, retry next interval: %v", pass.id, pass.kind, seg, err)
		}
	}
}

// runSegment 从这一段的断点做到段尾。段不再归本副本、或进度被别人推进过 / 这一遍已经结束时停下,不算错误。
func (w *indexPassWorker) runSegment(ctx context.Context, pass indexPass, seg int) error {
	field := strconv.Itoa(seg)
	raw, err := w.r.ctrl.HGet(ctx, gateIndexPassProgressKey, field).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("read progress: %w", err)
	}
	prog, ok := parsePassProgress(raw)
	if !ok || prog.passID != pass.id || prog.next < segmentStart(seg) || prog.next > segmentEnd(seg) {
		prog = passProgress{passID: pass.id, next: segmentStart(seg)}
	}
	if prog.next >= segmentEnd(seg) {
		return nil
	}
	for prog.next < segmentEnd(seg) {
		if !w.owns(seg) {
			return nil
		}
		slot := uint16(prog.next)
		rep := passReport{actions: map[string]int{}}
		if err := w.processSlot(ctx, pass.kind == passKindAudit, slot, &rep); err != nil {
			reconcilePassSlotsTotal.Inc(reconcileResultRedisError)
			return fmt.Errorf("slot %d: %w", slot, err)
		}
		next := prog.advance(rep)
		advanced, err := passAdvanceScript.Run(ctx, w.r.ctrl, []string{gateIndexPassKey, gateIndexPassProgressKey},
			pass.id, field, raw, next.value()).Int()
		if err != nil {
			reconcilePassSlotsTotal.Inc(reconcileResultRedisError)
			return fmt.Errorf("advance progress past slot %d: %w", slot, err)
		}
		if advanced != 1 {
			return nil // 这一遍结束或换了,或这一段被别的副本推进过:让给它
		}
		reconcilePassSlotsTotal.Inc(reconcileResultOK)
		raw, prog = next.value(), next
	}
	logx.Infof("SessionReconciler: index pass %s (%s) segment %d done: scanned_keys=%d repaired=%d index_keys=%d skipped_candidates=%d key_type_errors=%d page_retries=%d",
		pass.id, pass.kind, seg, prog.scanned, prog.repaired, prog.indexKeys, prog.skipped, prog.keyErrors, prog.retries)
	return nil
}

func (w *indexPassWorker) owns(seg int) bool {
	owned := w.owner.OwnedSegments()
	i := sort.SearchInts(owned, seg)
	return i < len(owned) && owned[i] == seg
}

// processSlot 做一个 slot:第 1 阶段列出它的全部键、按页补漏成员;第 2 阶段读它的登记集、逐把索引键清脏成员。
// 和单机扫描(backfillKeyspace、pruneAllGateIndexes)用的是同一组页处理函数,限速与单页重试也一样。
func (w *indexPassWorker) processSlot(ctx context.Context, audit bool, slot uint16, rep *passReport) error {
	r := w.r
	keys, err := w.list(ctx, r.player, slot, slotKeysListLimit)
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}
	pageSize := int(r.cfg.ScanCount)
	for start := 0; start < len(keys); start += pageSize {
		page := keys[start:min(start+pageSize, len(keys))]
		if err := r.scanPacer.Wait(ctx); err != nil {
			return err
		}
		err := r.retryPassPage(ctx, rep, func() string { return fmt.Sprintf("backfill a page of %d keys of slot %d", len(page), slot) }, func() error {
			return r.backfillPage(ctx, page, audit, rep)
		})
		if err != nil {
			return err
		}
		rep.scannedKeys += len(page)
	}

	var pairs []gateSlot
	err = r.retryPassPage(ctx, rep, func() string { return fmt.Sprintf("read the gate registry of slot %d", slot) }, func() error {
		if err := r.scanPacer.Wait(ctx); err != nil {
			return err
		}
		var err error
		pairs, err = readGateRegistries(ctx, r.player, []uint16{slot}, rep.actions, &rep.keyErrors)
		return err
	})
	if err != nil {
		return err
	}
	rep.gates += len(pairs)
	for i, p := range pairs {
		if i > 0 && int64(i)%r.cfg.ScanCount == 0 {
			if err := r.scanPacer.Wait(ctx); err != nil {
				return err
			}
		}
		if err := r.pruneGateIndex(ctx, p, rep); err != nil {
			return err
		}
	}
	return nil
}
