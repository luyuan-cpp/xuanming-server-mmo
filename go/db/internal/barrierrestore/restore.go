// Package barrierrestore 在停写维护期间从权威 SQL 恢复历史玩家的提交屏障。
// 旧恢复模式不改归属/缓存；显式promotion仅在审定撤销计划中提升围栏并清同SQL的缓存。
// 两种模式都不能替代停旧写者与审定 Kafka/retry 的运维门禁。
package barrierrestore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/encoding/protojson"
	"io"
	"net"
	"os"
	"path/filepath"
	assetpb "proto/common/asset"
	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"sort"
	"strconv"
	"strings"
	"time"

	databasepb "proto/common/database"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/kafkautil"
	"shared/ownerepoch"
	"shared/placement"
	"shared/savebarrier"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const Format = "player-save-barrier-restore-v1"
const FormatV2 = "player-save-barrier-restore-v2"
const RoutingModeRedis = "redis-same-instance"
const RoutingModeSplit = "redis-split"
const RoutingModeDBFirst = "db_first"
const PromotionFormat = "player-save-fence-promotion-v1"

// Plan 只包含身份与摘要。它不是停写证明，执行时仍须独立维持维护窗口。
//
// AllowHomeFallback(2026-10-20 修复，r2-mz#1)：显式允许「没有落点记录、home == Zone、Schema 就是本区 zone 库」的玩家
// 按 go/db 的有效落点规则(记录 ?? home 的 zone 库)回退，不再要求先钉稳定落点；copy 合服 / 撤销后的玩家天然如此。
// 只在计划阶段由运维开启并随计划进 SHA-256；缺省 false 时行为与之前完全一致，旧计划照常可读。见 homeZoneFallback。
type Plan struct {
	Format                 string   `json:"format"`
	Schema                 string   `json:"schema"`
	Zone                   uint32   `json:"home_zone"`
	MaintenanceID          string   `json:"maintenance_id"`
	AllowLegacy            bool     `json:"allow_legacy"`
	RedisAddr              string   `json:"redis_addr"`
	RedisDB                int      `json:"redis_db"`
	RoutingMode            string   `json:"routing_mode"`
	LocalMySQL             bool     `json:"local_mysql"`
	MappingAddr            string   `json:"mapping_addr,omitempty"`
	MappingDB              int      `json:"mapping_db,omitempty"`
	RouteSchema            string   `json:"route_schema,omitempty"`
	AuditedOwnerUpperBound string   `json:"audited_owner_upper_bound,omitempty"`
	AllowHomeFallback      bool     `json:"allow_home_fallback,omitempty"`
	Players                []Player `json:"players"`
}

type Player struct {
	ID              string    `json:"player_id"`
	Owner           string    `json:"owner_epoch"`
	Original        string    `json:"original_version"`
	Target          string    `json:"target_version"`
	Rows            [2]string `json:"asset_row_sha256"`
	HomeZone        string    `json:"home_zone_value"`
	Placement       string    `json:"placement_value"`
	RouteGeneration string    `json:"route_generation,omitempty"`
	RouteWatermark  string    `json:"route_watermark,omitempty"`
	RouteRevision   string    `json:"route_revision,omitempty"`
}

// Restorer 的两个依赖均为维护专用连接，不能与正在接流量的业务池共用。
type Restorer struct {
	// 历史字段名保留；现在表示运维预置maintenance-recovery目标，不含loopback/调试授权语义。
	LocalTargetID          string
	RedisTargetID          string
	MappingTargetID        string
	MaintenanceSQLAddr     string
	DB                     *sql.DB
	Redis                  *redis.Client
	RoutingMode            string
	LocalMySQL             bool
	MappingRedis           *redis.Client
	RouteSchema            string
	AuditedOwnerUpperBound string
	// AllowHomeFallback 只由 Prepare 读取并写进计划(Plan.AllowHomeFallback);执行以审定计划为准,CLI 在 apply 模式拒绝该开关。
	AllowHomeFallback bool
}

// PreparePromotion 只读生成单玩家审定原件。停写证据摘要必须独立审定，不能由当前SQL猜U。
// 新维护分支仅支持同一TiDB上的db_first；SQL事务内不访问Redis/Scene/Kafka。
func (r Restorer) PreparePromotion(ctx context.Context, schema string, zone uint32, maintenanceID, id string, evidence []byte) (*databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan, error) {
	pid, err := positive(id)
	if err != nil || r.DB == nil || r.Redis == nil || r.RoutingMode != RoutingModeDBFirst || len(evidence) != 32 || !promotionTargetID(r.LocalTargetID) || !promotionTargetID(r.RedisTargetID) {
		return nil, errors.New("fence-only提升范围、本地实例或审核证据无效")
	}
	p := Plan{Format: FormatV2, Schema: schema, Zone: zone, MaintenanceID: maintenanceID, RoutingMode: RoutingModeDBFirst, RedisAddr: r.Redis.Options().Addr, RedisDB: r.Redis.Options().DB, MappingAddr: r.mapping().Options().Addr, MappingDB: r.mapping().Options().DB, RouteSchema: r.RouteSchema}
	if err := p.ValidateTarget(); err != nil {
		return nil, err
	}
	if err := r.checkPromotionRedisTargets(ctx, p); err != nil {
		return nil, err
	}
	u, err := auditedBound(r.AuditedOwnerUpperBound)
	if err != nil {
		return nil, err
	}
	ownerRaw, err := r.Redis.Get(ctx, ownerepoch.OwnerEpochKey(pid)).Result()
	if err != nil {
		return nil, errors.New("原owner缺失")
	}
	owner, err := positive(ownerRaw)
	if err != nil {
		return nil, err
	}
	prior, err := r.Redis.HGet(ctx, savebarrier.Key(pid), "handoff_fence").Result()
	if errors.Is(err, redis.Nil) {
		prior = ""
	} else if err != nil {
		return nil, err
	}
	cache, err := r.readPromotionCache(ctx, zone, pid)
	if err != nil {
		return nil, err
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := r.checkPromotionSQLTarget(ctx, tx, p); err != nil {
		return nil, err
	}
	row, err := r.lockRoute(ctx, tx, p, id)
	if err != nil {
		return nil, err
	}
	pair, err := readPair(ctx, tx, schema, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Rollback(); err != nil {
		return nil, err
	}
	if pair[0].schemaGen != pair[1].schemaGen || owner < pair[0].epoch {
		return nil, errors.New("原SQL世代不同或owner落后")
	}
	if err := verifyPromotionCache(cache, recoveryKeys(zone, pid), pair, pid); err != nil {
		return nil, err
	}
	max := owner
	if u > max {
		max = u
	}
	if max == ^uint64(0) {
		return nil, errors.New("owner提升溢出")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(nonce[:])
	revoked, err := promotedRevocation(prior, owner, max+1, token)
	if err != nil {
		return nil, err
	}
	storage, _ := placement.StorageIDFromDBName(schema)
	placementVersion := uint64(0)
	if row.Placement != "" {
		record, err := placement.Parse(row.Placement)
		if err != nil {
			return nil, err
		}
		placementVersion = record.Version
	}
	rev, _ := positive(row.RouteRevision)
	watermark, _ := positive(row.RouteWatermark)
	one, _ := hex.DecodeString(pair[0].digest)
	two, _ := hex.DecodeString(pair[1].digest)
	plan := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{Format: PromotionFormat, MaintenanceId: maintenanceID, PlayerId: pid, RowSchema: schema, RouteSchema: r.RouteSchema, HomeZone: zone, StorageId: storage, RouteRevision: rev, RouteGeneration: row.RouteGeneration, RouteWatermark: watermark, PlacementVersion: placementVersion, AuditedOwnerUpperBound: u, OriginalOwner: owner, TargetOwner: max + 1, OriginalSaveEpoch: pair[0].epoch, OriginalSaveSeq: pair[0].seq, TargetSaveSeq: 1, SchemaGen: pair[0].schemaGen, FirstBusinessSha256: one, SecondBusinessSha256: two, OriginalFence: prior, RevokedFence: revoked, ReceiptId: []byte(token), RedisAddr: p.RedisAddr, RedisDb: int32(p.RedisDB), MappingAddr: p.MappingAddr, MappingDb: int32(p.MappingDB), AuditEvidenceSha256: bytes.Clone(evidence), WritersStopped: true, QueuesDrained: true}
	for _, key := range promotionCacheKeys(zone, pid) {
		value, present := cache[key]
		kind := "string"
		if key == recoveryKeys(zone, pid)[5] {
			kind = "hash"
		}
		observation := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan_CacheObservation{Key: key, Present: present, RedisType: kind}
		if present {
			sum := sha256.Sum256(value)
			observation.Sha256 = sum[:]
		}
		plan.Caches = append(plan.Caches, observation)
	}
	if err := ValidatePromotion(plan); err != nil {
		return nil, err
	}
	if err := r.inspectMapping(ctx, p, row); err != nil {
		return nil, err
	}
	if err := r.runPromotionScript(ctx, plan, cache, "check"); err != nil {
		return nil, err
	}
	return plan, nil
}

func promotionCacheKeys(zone uint32, player uint64) []string {
	keys := recoveryKeys(zone, player)
	return []string{keys[2], keys[3], keys[4], keys[5]}
}

func promotedRevocation(prior string, originalOwner, target uint64, token string) (string, error) {
	validNonce := func(s string, zero bool) bool {
		return len(s) == 32 && strings.Trim(s, "0123456789abcdef") == "" && (zero || s != strings.Repeat("0", 32))
	}
	if !validNonce(token, false) {
		return "", errors.New("撤销nonce无效")
	}
	if prior == "" {
		return fmt.Sprintf("%d||0|%s|R|%s", target, strings.Repeat("0", 32), token), nil
	}
	original, err := savebarrier.ParseHandoffFence(prior)
	if err != nil || original.OwnerEpoch > originalOwner || original.Token == token {
		return "", errors.New("原handoff围栏不规范")
	}
	return fmt.Sprintf("%d|%s|%d|%s|R|%s", original.OwnerEpoch, original.SceneInstance, original.Generation, original.AdmissionToken, token), nil
}

func promotionRoute(p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan) (Plan, Player) {
	row := Player{ID: strconv.FormatUint(p.PlayerId, 10), HomeZone: strconv.FormatUint(uint64(p.HomeZone), 10), RouteRevision: strconv.FormatUint(p.RouteRevision, 10), RouteGeneration: p.RouteGeneration, RouteWatermark: strconv.FormatUint(p.RouteWatermark, 10)}
	if p.PlacementVersion != 0 {
		row.Placement = placement.StableValue(p.StorageId, p.PlacementVersion)
	}
	return Plan{Format: FormatV2, Schema: p.RowSchema, Zone: p.HomeZone, MaintenanceID: p.MaintenanceId, RoutingMode: RoutingModeDBFirst, RedisAddr: p.RedisAddr, RedisDB: int(p.RedisDb), MappingAddr: p.MappingAddr, MappingDB: int(p.MappingDb), RouteSchema: p.RouteSchema}, row
}

// ValidatePromotion 用同一wire类型校验CLI与执行计划；cache条目按完整键核唯一和全集，顺序无意义。
func ValidatePromotion(p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan) error {
	if p == nil || len(p.ProtoReflect().GetUnknown()) != 0 || p.Format != PromotionFormat || p.PlayerId == 0 || len(p.MaintenanceId) == 0 || len(p.MaintenanceId) > 128 || !p.WritersStopped || !p.QueuesDrained || len(p.AuditEvidenceSha256) != 32 || len(p.FirstBusinessSha256) != 32 || len(p.SecondBusinessSha256) != 32 {
		return errors.New("提升计划契约无效")
	}
	max := p.OriginalOwner
	if p.AuditedOwnerUpperBound > max {
		max = p.AuditedOwnerUpperBound
	}
	if max == ^uint64(0) || p.TargetOwner != max+1 || p.OriginalOwner == 0 || p.OriginalSaveEpoch > p.OriginalOwner || (p.OriginalSaveEpoch == 0) != (p.OriginalSaveSeq == 0) || p.TargetSaveSeq != 1 {
		return errors.New("提升计划版本无效")
	}
	target, row := promotionRoute(p)
	if err := target.ValidateTarget(); err != nil {
		return err
	}
	if err := validatePlannedRoute(target, row); err != nil {
		return err
	}
	storage, ok := placement.StorageIDFromDBName(p.RowSchema)
	if !ok || storage != p.StorageId {
		return errors.New("提升计划落点不符")
	}
	revoked, err := promotedRevocation(p.OriginalFence, p.OriginalOwner, p.TargetOwner, string(p.ReceiptId))
	if err != nil || revoked != p.RevokedFence {
		return errors.New("提升计划R原件不符")
	}
	keys := map[string]bool{}
	for _, key := range promotionCacheKeys(p.HomeZone, p.PlayerId) {
		keys[key] = false
	}
	if len(p.Caches) != len(keys) {
		return errors.New("缓存观察全集不符")
	}
	assetKey := recoveryKeys(p.HomeZone, p.PlayerId)[5]
	for _, entry := range p.Caches {
		if entry == nil || len(entry.ProtoReflect().GetUnknown()) != 0 {
			return errors.New("缓存观察损坏")
		}
		seen, ok := keys[entry.Key]
		kind := "string"
		if entry.Key == assetKey {
			kind = "hash"
		}
		if !ok || seen || entry.RedisType != kind || entry.Present && len(entry.Sha256) != 32 || !entry.Present && len(entry.Sha256) != 0 {
			return errors.New("缓存观察键/类型/摘要不符")
		}
		keys[entry.Key] = true
	}
	return nil
}

// PromotionDigest 对规范PB原件做域分离摘要；SQL永久收据存的正是这些bytes。
func PromotionDigest(p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan) ([]byte, []byte, error) {
	if err := ValidatePromotion(p); err != nil {
		return nil, nil, err
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	if err != nil {
		return nil, nil, err
	}
	h := sha256.New()
	writePromotionPart(h, []byte(PromotionFormat))
	writePromotionPart(h, raw)
	return raw, h.Sum(nil), nil
}

func writePromotionPart(h interface{ Write([]byte) (int, error) }, v []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(v)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(v)
}

func (r Restorer) readPromotionCache(ctx context.Context, zone uint32, player uint64) (map[string][]byte, error) {
	keys := recoveryKeys(zone, player)
	out := map[string][]byte{}
	for _, key := range keys[2:5] {
		raw, err := r.Redis.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 || len(raw) > 16<<20 {
			return nil, errors.New("缓存bytes预算或形状无效")
		}
		out[key] = raw
	}
	kind, err := r.Redis.Type(ctx, keys[5]).Result()
	if err != nil {
		return nil, err
	}
	if kind != "none" {
		if kind != "hash" {
			return nil, errors.New("资产证据类型无效")
		}
		evidence, err := r.Redis.HGetAll(ctx, keys[5]).Result()
		if err != nil {
			return nil, err
		}
		if len(evidence) != 2 || len(evidence["version"]) != 41 || len(evidence["data"]) == 0 || len(evidence["data"]) > 16<<20 {
			return nil, errors.New("资产证据字段不完整")
		}
		out[keys[5]] = []byte(evidence["version"] + evidence["data"])
	}
	return out, nil
}

func verifyPromotionCache(cache map[string][]byte, keys []string, rows [2]sqlRow, player uint64) error {
	check := func(raw []byte, msg proto.Message, row sqlRow) error {
		if proto.Unmarshal(raw, msg) != nil || len(msg.ProtoReflect().GetUnknown()) != 0 {
			return errors.New("缓存PB未知顶层字段或损坏，不能证明无丢内容")
		}
		fields := msg.ProtoReflect().Descriptor().Fields()
		for name, rawSQL := range row.values {
			if name == "fence_owner_epoch" || name == "fence_save_seq" {
				continue
			}
			field := fields.ByName(protoreflect.Name(name))
			if field == nil {
				return errors.New("SQL存在缓存模型无法解释的列，拒绝清缓存")
			}
			value := msg.ProtoReflect().Get(field)
			switch field.Kind() {
			case protoreflect.MessageKind:
				fromSQL := value.Message().New().Interface()
				if proto.Unmarshal(rawSQL, fromSQL) != nil || (!msg.ProtoReflect().Has(field) && proto.Size(fromSQL) != 0) || (msg.ProtoReflect().Has(field) && !proto.Equal(fromSQL, value.Message().Interface())) {
					return errors.New("缓存与SQL消息内容不同")
				}
			case protoreflect.Uint64Kind, protoreflect.Uint32Kind, protoreflect.Fixed64Kind, protoreflect.Fixed32Kind:
				if string(rawSQL) != strconv.FormatUint(value.Uint(), 10) {
					return errors.New("缓存与SQL标量不同")
				}
			case protoreflect.StringKind:
				if string(rawSQL) != value.String() {
					return errors.New("缓存与SQL字符串不同")
				}
			case protoreflect.BytesKind:
				if !bytes.Equal(rawSQL, value.Bytes()) {
					return errors.New("缓存与SQL原bytes不同")
				}
			default:
				return errors.New("缓存SQL列类型尚无安全比较契约")
			}
		}
		// 不允许SQL缺已知业务列；NULL消息是空消息，不是漏列。
		for i := 0; i < fields.Len(); i++ {
			if _, ok := row.values[string(fields.Get(i).Name())]; !ok {
				return errors.New("缓存字段无对应SQL列")
			}
		}
		return nil
	}
	if raw, ok := cache[keys[2]]; ok {
		parent := &databasepb.PlayerAllData{}
		if proto.Unmarshal(raw, parent) != nil || len(parent.ProtoReflect().GetUnknown()) != 0 || parent.GetLastSaveSeq() != rows[0].seq || parent.PlayerDatabaseData == nil || parent.PlayerDatabase_1Data == nil || parent.GetPlayerDatabaseData().GetPlayerId() != player || parent.GetPlayerDatabase_1Data().GetPlayerId() != player {
			return errors.New("父缓存身份/版本无法证明")
		}
		one, _ := proto.Marshal(parent.PlayerDatabaseData)
		two, _ := proto.Marshal(parent.PlayerDatabase_1Data)
		if err := check(one, &databasepb.PlayerDatabase{}, rows[0]); err != nil {
			return err
		}
		if err := check(two, &databasepb.PlayerDatabase_1{}, rows[1]); err != nil {
			return err
		}
		if parent.MailData != nil || parent.BagData != nil && !proto.Equal(parent.BagData, parent.PlayerDatabaseData.BagComponent) || parent.QuestData != nil && !proto.Equal(parent.QuestData, parent.PlayerDatabaseData.MissionComponent) {
			return errors.New("父缓存含未承载SQL的额外内容")
		}
	}
	for i, key := range keys[3:5] {
		if raw, ok := cache[key]; ok {
			var msg proto.Message = &databasepb.PlayerDatabase{}
			if i == 1 {
				msg = &databasepb.PlayerDatabase_1{}
			}
			if err := check(raw, msg, rows[i]); err != nil {
				return err
			}
		}
	}
	if raw, ok := cache[keys[5]]; ok {
		evidence := &assetpb.PlayerAssetSQLCommit{}
		if len(raw) < 41 || string(raw[:41]) != savebarrier.Version(rows[0].epoch, rows[0].seq) || proto.Unmarshal(raw[41:], evidence) != nil || len(evidence.ProtoReflect().GetUnknown()) != 0 || evidence.PlayerId != player || evidence.OwnerEpoch != rows[0].epoch || evidence.SaveSeq != rows[0].seq {
			return errors.New("资产证据不是原SQL同版")
		}
		// 账本bytes来自同一SQL列；不重新盖旧版本证据，也不丢弃未知账本字段。
		ledger, ok := rows[0].values["asset_op_ledger"]
		if !ok || !bytes.Equal(ledger, evidence.LedgerProto) {
			return errors.New("资产证据账本与SQL原bytes不同")
		}
	}
	return nil
}

func (r Restorer) runPromotionScript(ctx context.Context, p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan, cache map[string][]byte, mode string) error {
	keys := recoveryKeys(p.HomeZone, p.PlayerId)
	gen := p.SchemaGen
	if gen == 0 {
		gen = 1
	}
	args := []any{p.MaintenanceId, strconv.FormatUint(p.OriginalOwner, 10), strconv.FormatUint(p.TargetOwner, 10), savebarrier.Version(p.OriginalSaveEpoch, p.OriginalSaveSeq), savebarrier.Version(p.TargetOwner, p.TargetSaveSeq), strconv.FormatUint(uint64(gen), 10), p.OriginalFence, p.RevokedFence, mode}
	observations := map[string]*databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan_CacheObservation{}
	for _, entry := range p.Caches {
		observations[entry.Key] = entry
	}
	for _, key := range keys[2:5] {
		raw, present := cache[key]
		entry := observations[key]
		flag := "0"
		if entry.Present {
			flag = "1"
		}
		if present {
			sum := sha256.Sum256(raw)
			if !entry.Present || !bytes.Equal(sum[:], entry.Sha256) {
				return errors.New("缓存偏离审定原件")
			}
		}
		args = append(args, flag, string(raw))
	}
	asset, present := cache[keys[5]]
	entry := observations[keys[5]]
	flag := "0"
	if entry.Present {
		flag = "1"
	}
	if present {
		sum := sha256.Sum256(asset)
		if !entry.Present || len(asset) < 41 || !bytes.Equal(sum[:], entry.Sha256) {
			return errors.New("资产证据偏离审定原件")
		}
		args = append(args, flag, string(asset[:41]), string(asset[41:]))
	} else {
		args = append(args, flag, "", "")
	}
	// 永久实例nonce在实际Lua提交点复核；run_id/replid只作CLI当次观测，AOF重启不换计划。
	markerKey, markerScope, err := MaintenanceRedisScope("promotion", "core", p.HomeZone, p.RouteSchema, p.RowSchema, r.Redis.Options().Addr, r.Redis.Options().DB)
	if err != nil {
		return err
	}
	keys = append(keys[:72:72], markerKey, markerKey+":nonce")
	args = append(args, r.RedisTargetID, markerScope)
	if r.Redis.Options().Addr == r.mapping().Options().Addr && r.Redis.Options().DB == r.mapping().Options().DB {
		mappingKey, mappingScope, err := MaintenanceRedisScope("promotion", "mapping", p.HomeZone, p.RouteSchema, p.RowSchema, r.mapping().Options().Addr, r.mapping().Options().DB)
		if err != nil {
			return err
		}
		keys = append(keys, mappingKey, mappingKey+":nonce")
		args = append(args, r.MappingTargetID, mappingScope)
	}
	n, err := promotionScript.Run(ctx, r.Redis, keys, args...).Int()
	if err != nil {
		return fmt.Errorf("维护撤销或屏障确认拒绝: %w", err)
	}
	if n != 1 {
		return errors.New("维护脚本返回未知结果")
	}
	return nil
}

// ApplyPromotion 的各SQL阶段均不调用Redis。永久R+更高owner先撤销旧B5资格，
// 再以SQL永久阶段原件引用撤销证明；双行只改围栏元数据。未知提交保持维护并重放同一nonce。
func (r Restorer) ApplyPromotion(ctx context.Context, p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan) (*databasepb.SaveBarrierMaintenanceReceipt, error) {
	raw, digest, err := PromotionDigest(p)
	if err != nil {
		return nil, err
	}
	if r.DB == nil || r.Redis == nil || !promotionTargetID(r.LocalTargetID) || !promotionTargetID(r.RedisTargetID) || r.RoutingMode != RoutingModeDBFirst || r.RouteSchema != p.RouteSchema || r.Redis.Options().Addr != p.RedisAddr || r.Redis.Options().DB != int(p.RedisDb) || r.mapping().Options().Addr != p.MappingAddr || r.mapping().Options().DB != int(p.MappingDb) || strconv.FormatUint(p.AuditedOwnerUpperBound, 10) != r.AuditedOwnerUpperBound {
		return nil, errors.New("实际维护目标或旧写上界偏离审定计划")
	}
	target, _ := promotionRoute(p)
	if err := r.checkPromotionRedisTargets(ctx, target); err != nil {
		return nil, err
	}
	if _, _, err = r.promotionSQL(ctx, p, raw, digest, databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED); err != nil {
		return nil, err
	}
	phase, pair, err := r.promotionSQL(ctx, p, raw, digest, databasepb.SaveBarrierMaintenanceReceipt_PHASE_UNKNOWN)
	if err != nil {
		return nil, err
	}
	// SQL永久阶段不能证明Redis崩溃后仍保有撤销；续跑也必须在SQL锁外重放原计划同一R。
	if phase == databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED || phase == databasepb.SaveBarrierMaintenanceReceipt_WRITERS_REVOKED || phase == databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED {
		cache, err := r.readPromotionCache(ctx, p.HomeZone, p.PlayerId)
		if err != nil {
			return nil, err
		}
		if phase == databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED {
			if err := verifyPromotionCache(cache, recoveryKeys(p.HomeZone, p.PlayerId), pair, p.PlayerId); err != nil {
				return nil, err
			}
		}
		target, row := promotionRoute(p)
		if err := r.inspectMapping(ctx, target, row); err != nil {
			return nil, err
		}
		if err := r.runPromotionScript(ctx, p, cache, "revoke"); err != nil {
			return nil, err
		}
		if phase == databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED {
			phase, _, err = r.promotionSQL(ctx, p, raw, digest, databasepb.SaveBarrierMaintenanceReceipt_WRITERS_REVOKED)
			if err != nil {
				return nil, err
			}
		}
	}
	if phase == databasepb.SaveBarrierMaintenanceReceipt_WRITERS_REVOKED {
		// 原R已在本次调用现场复核；随后只做SQL操作，不在角色/路由/双行锁内调网络。
		phase, _, err = r.promotionSQL(ctx, p, raw, digest, databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED)
		if err != nil {
			return nil, err
		}
	}
	if phase == databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED {
		cache, err := r.readPromotionCache(ctx, p.HomeZone, p.PlayerId)
		if err != nil {
			return nil, err
		}
		target, row := promotionRoute(p)
		if err := r.inspectMapping(ctx, target, row); err != nil {
			return nil, err
		}
		if err := r.runPromotionScript(ctx, p, cache, "finish"); err != nil {
			return nil, err
		}
		phase, _, err = r.promotionSQL(ctx, p, raw, digest, databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED)
		if err != nil {
			return nil, err
		}
	}
	if phase != databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED {
		return nil, errors.New("维护收据阶段未知")
	}
	out := &databasepb.SaveBarrierMaintenanceReceipt{ReceiptId: bytes.Clone(p.ReceiptId), PlayerId: p.PlayerId, RequestDigest: digest, PlanProto: raw, Phase: phase}
	if err := r.DB.QueryRowContext(ctx, "SELECT updated_ms FROM `"+p.RouteSchema+"`.save_barrier_maintenance_receipt WHERE receipt_id=?", p.ReceiptId).Scan(&out.UpdatedMs); err != nil {
		return nil, err
	}
	return out, nil
}

func promotionTargetID(value string) bool {
	return len(value) == 32 && strings.Trim(value, "0123456789abcdef") == "" && value != strings.Repeat("0", 32)
}

// next=UNKNOWN只读取；阶段只允许相邻推进，原件永不改写。无隐式重试、无事务内外部I/O。
func (r Restorer) promotionSQL(ctx context.Context, p *databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan, raw, digest []byte, next databasepb.SaveBarrierMaintenanceReceipt_Phase) (databasepb.SaveBarrierMaintenanceReceipt_Phase, [2]sqlRow, error) {
	unknown := databasepb.SaveBarrierMaintenanceReceipt_PHASE_UNKNOWN
	var pair [2]sqlRow
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return unknown, pair, err
	}
	defer tx.Rollback()
	target, row := promotionRoute(p)
	if err := r.checkPromotionSQLTarget(ctx, tx, target); err != nil {
		return unknown, pair, err
	}
	if err := r.recheckRoute(ctx, tx, target, row); err != nil {
		return unknown, pair, err
	}
	pair, err = readPair(ctx, tx, p.RowSchema, row.ID)
	if err != nil {
		return unknown, pair, err
	}
	for i, want := range [][]byte{p.FirstBusinessSha256, p.SecondBusinessSha256} {
		if pair[i].digest != hex.EncodeToString(want) || pair[i].schemaGen != p.SchemaGen {
			return unknown, pair, errors.New("原业务全列或世代发生变化")
		}
	}
	table := "`" + p.RouteSchema + "`.save_barrier_maintenance_receipt"
	var player uint64
	var savedRaw, savedDigest []byte
	var phase databasepb.SaveBarrierMaintenanceReceipt_Phase
	err = tx.QueryRowContext(ctx, "SELECT player_id,request_digest,plan_proto,phase FROM "+table+" WHERE receipt_id=? FOR UPDATE", p.ReceiptId).Scan(&player, &savedDigest, &savedRaw, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		if next != databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED || pair[0].epoch != p.OriginalSaveEpoch || pair[0].seq != p.OriginalSaveSeq {
			return unknown, pair, errors.New("维护原件缺席或原SQL版本变化")
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO "+table+"(receipt_id,player_id,request_digest,plan_proto,phase,updated_ms) VALUES(?,?,?,?,?,CAST(UNIX_TIMESTAMP(NOW(3))*1000 AS UNSIGNED))", p.ReceiptId, p.PlayerId, digest, raw, databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED)
		if err != nil {
			return unknown, pair, err
		}
		if err = tx.Commit(); err != nil {
			return unknown, pair, fmt.Errorf("维护计划提交结局未知，保持维护并仅原计划重读: %w", err)
		}
		return databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED, pair, nil
	}
	if err != nil {
		return unknown, pair, err
	}
	if player != p.PlayerId || !bytes.Equal(savedRaw, raw) || !bytes.Equal(savedDigest, digest) || phase < databasepb.SaveBarrierMaintenanceReceipt_PLAN_FIXED || phase > databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED {
		return unknown, pair, errors.New("维护永久原件冲突或未知阶段")
	}
	expectE, expectQ := p.OriginalSaveEpoch, p.OriginalSaveSeq
	if phase >= databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED {
		expectE, expectQ = p.TargetOwner, p.TargetSaveSeq
	}
	if pair[0].epoch != expectE || pair[0].seq != expectQ {
		return unknown, pair, errors.New("维护SQL版本不符合永久阶段")
	}
	if next == unknown || next <= phase {
		return phase, pair, tx.Rollback()
	}
	if next != phase+1 {
		return unknown, pair, errors.New("维护阶段不可跳跃")
	}
	if next == databasepb.SaveBarrierMaintenanceReceipt_SQL_PROMOTED {
		for _, name := range []string{"player_database", "player_database_1"} {
			result, err := tx.ExecContext(ctx, "UPDATE `"+p.RowSchema+"`."+name+" SET fence_owner_epoch=?,fence_save_seq=? WHERE player_id=? AND fence_owner_epoch=? AND fence_save_seq=?", p.TargetOwner, p.TargetSaveSeq, p.PlayerId, p.OriginalSaveEpoch, p.OriginalSaveSeq)
			if err != nil {
				return unknown, pair, err
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return unknown, pair, errors.New("双表围栏提升CAS失败")
			}
		}
		updated, err := readPair(ctx, tx, p.RowSchema, row.ID)
		if err != nil {
			return unknown, pair, err
		}
		for i := range updated {
			if updated[i].digest != pair[i].digest || updated[i].schemaGen != pair[i].schemaGen || updated[i].epoch != p.TargetOwner || updated[i].seq != p.TargetSaveSeq {
				return unknown, pair, errors.New("提升改变业务bytes或世代")
			}
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE "+table+" SET phase=?,updated_ms=CAST(UNIX_TIMESTAMP(NOW(3))*1000 AS UNSIGNED) WHERE receipt_id=? AND phase=?", next, p.ReceiptId, phase)
	if err != nil {
		return unknown, pair, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return unknown, pair, errors.New("维护阶段CAS失败")
	}
	if err := tx.Commit(); err != nil {
		return unknown, pair, fmt.Errorf("维护阶段提交结局未知，仅原计划重读: %w", err)
	}
	return next, pair, nil
}

// 专用维护标记只由运维预置；不能使debug_import获得写权限。
// 每个实际SQL事务按固定库名序持标记行锁，不能以事务外探针代替提交点复核。
func (r Restorer) checkPromotionSQLTarget(ctx context.Context, tx *sql.Tx, p Plan) error {
	return r.checkMaintenanceSQLTargets(ctx, tx, "promotion", p.Zone, []string{p.RouteSchema, p.Schema})
}

func maintenanceAddress(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	n, e := strconv.ParseUint(port, 10, 16)
	return err == nil && host != "" && !strings.ContainsAny(host, " /\\@|\r\n\t") && e == nil && n != 0 && strconv.FormatUint(n, 10) == port
}

func (r Restorer) checkMaintenanceSQLTargets(ctx context.Context, tx *sql.Tx, purpose string, zone uint32, schemas []string) error {
	if !promotionTargetID(r.LocalTargetID) || !maintenanceAddress(r.MaintenanceSQLAddr) || zone == 0 || zone > placement.MaxZoneStorageID || purpose != "promotion" && purpose != "release" {
		return errors.New("维护SQL目标scope/nonce/物理地址非法")
	}
	ordered := append([]string{}, schemas...)
	sort.Strings(ordered)
	last := ""
	for _, schema := range ordered {
		if !validSchema(schema) {
			return errors.New("维护SQL schema非法")
		}
		if schema == last {
			continue
		}
		last = schema
		var nonce, address string
		if err := tx.QueryRowContext(ctx, "SELECT target_nonce,physical_addr FROM `"+schema+"`.maintenance_recovery_target WHERE purpose=? AND zone_id=? AND schema_name=? FOR UPDATE", purpose, zone, []byte(schema)).Scan(&nonce, &address); err != nil {
			return fmt.Errorf("专用维护SQL标记缺失或不可读: %w", err)
		}
		if nonce != r.LocalTargetID || address != r.MaintenanceSQLAddr {
			return errors.New("维护SQL目标身份或物理地址变化")
		}
	}
	return nil
}

// MaintenanceRedisScope是CLI与提交实现共用的固定键合同，不查询或创建任何标记。
func MaintenanceRedisScope(purpose, role string, zone uint32, routeSchema, schema, addr string, db int) (string, string, error) {
	if purpose != "promotion" && purpose != "release" || role != "core" && role != "mapping" || zone == 0 || zone > placement.MaxZoneStorageID || !validSchema(routeSchema) || !validSchema(schema) || !maintenanceAddress(addr) || db < 0 {
		return "", "", errors.New("维护Redis目标scope非法")
	}
	key := fmt.Sprintf("maintenance_recovery:target:%s:%d:%s:%s", purpose, zone, schema, role)
	value := fmt.Sprintf("maintenance-recovery-v1|%s|%s|%d|%s|%s|%s|%d", purpose, role, zone, routeSchema, schema, addr, db)
	return key, value, nil
}

type MaintenanceRedisMarker struct {
	Role  string `json:"role"`
	Key   string `json:"key"`
	Scope string `json:"scope"`
	Nonce string `json:"target_nonce"`
}

// 只读预检不代替Lua内的原子复核；绝不创建或修补任一标记。
func ReadMaintenanceRedisMarker(ctx context.Context, client *redis.Client, purpose, role string, zone uint32, routeSchema, schema string) (MaintenanceRedisMarker, error) {
	if client == nil {
		return MaintenanceRedisMarker{}, errors.New("缺显式Redis目标")
	}
	key, scope, err := MaintenanceRedisScope(purpose, role, zone, routeSchema, schema, client.Options().Addr, client.Options().DB)
	if err != nil {
		return MaintenanceRedisMarker{}, err
	}
	for _, want := range []struct{ key, value string }{{key, scope}, {key + ":nonce", ""}} {
		kind, err := client.Type(ctx, want.key).Result()
		if err != nil || kind != "string" {
			return MaintenanceRedisMarker{}, errors.New("专用维护标记缺失或类型错误")
		}
		ttl, err := client.PTTL(ctx, want.key).Result()
		if err != nil || ttl != -1 {
			return MaintenanceRedisMarker{}, errors.New("专用维护标记必须永久无TTL")
		}
	}
	value, err := client.Get(ctx, key).Result()
	if err != nil || value != scope {
		return MaintenanceRedisMarker{}, errors.New("维护Redis role/zone/schema/物理scope不符")
	}
	nonce, err := client.Get(ctx, key+":nonce").Result()
	if err != nil || !promotionTargetID(nonce) {
		return MaintenanceRedisMarker{}, errors.New("缺永久维护Redis nonce")
	}
	return MaintenanceRedisMarker{Role: role, Key: key, Scope: scope, Nonce: nonce}, nil
}

func (r Restorer) checkPromotionRedisTargets(ctx context.Context, p Plan) error {
	for _, t := range []struct {
		role   string
		client *redis.Client
		nonce  string
	}{{"core", r.Redis, r.RedisTargetID}, {"mapping", r.mapping(), r.MappingTargetID}} {
		marker, err := ReadMaintenanceRedisMarker(ctx, t.client, "promotion", t.role, p.Zone, p.RouteSchema, p.Schema)
		if err != nil {
			return err
		}
		if marker.Nonce != t.nonce {
			return errors.New("维护Redis nonce偏离原审定")
		}
	}
	if r.Redis.Options().Addr == r.mapping().Options().Addr && r.Redis.Options().DB == r.mapping().Options().DB && r.RedisTargetID != r.MappingTargetID {
		return errors.New("同物理Redis不能声明不同实例nonce")
	}
	return nil
}

// 首版只恢复 home zone 自己的 zone 库。异区钉落点/player_store 必须另验权威 placement，不能猜。
func validTarget(schema string, zone uint32, addr string, db int) bool {
	host, port, err := net.SplitHostPort(addr)
	p, pErr := strconv.ParseUint(port, 10, 16)
	return zone > 0 && zone <= 999999 && schema == fmt.Sprintf("zone_%d_db", zone) &&
		err == nil && pErr == nil && p > 0 && host != "" && !strings.ContainsAny(host, " /\\@\r\n\t") && db == 0
}

func (p Plan) Validate() error {
	if p.ValidateTarget() != nil ||
		len(p.MaintenanceID) == 0 || len(p.MaintenanceID) > 128 || len(p.Players) == 0 || len(p.Players) > 1000 {
		return errors.New("维护计划格式、范围或工单号无效")
	}
	seen := make(map[string]bool, len(p.Players))
	var audited uint64
	if p.Format == FormatV2 {
		var err error
		audited, err = auditedBound(p.AuditedOwnerUpperBound)
		if err != nil {
			return err
		}
	} else if p.AuditedOwnerUpperBound != "" {
		return errors.New("v1 不接受新增恢复证明")
	}
	for _, row := range p.Players {
		if _, err := positive(row.ID); err != nil || seen[row.ID] {
			return errors.New("维护计划玩家号无效或重复")
		}
		seen[row.ID] = true
		if err := validatePlannedRoute(p, row); err != nil {
			return err
		}
		owner, err := positive(row.Owner)
		if err != nil {
			return errors.New("维护计划归属计数器无效")
		}
		if p.Format == FormatV2 && owner <= audited {
			return errors.New("当前 owner 未严格高于独立审核的旧代上界")
		}
		e, s, err := savebarrier.Parse(row.Original)
		if err != nil || (e == 0) != (s == 0) || e > owner {
			return errors.New("维护计划原始围栏无效")
		}
		want := row.Original
		if e == 0 {
			if !p.AllowLegacy {
				return errors.New("零围栏需要显式允许历史迁移")
			}
			want = savebarrier.Version(owner, 1)
		}
		if row.Target != want {
			return errors.New("维护计划目标围栏无效")
		}
		for _, digest := range row.Rows {
			b, err := hex.DecodeString(digest)
			if err != nil || len(b) != sha256.Size || hex.EncodeToString(b) != digest {
				return errors.New("维护计划 SQL 行摘要无效")
			}
		}
	}
	return nil
}

func positive(raw string) (uint64, error) {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 || strconv.FormatUint(v, 10) != raw {
		return 0, errors.New("非规范正整数")
	}
	return v, nil
}

// Prepare 在两行点锁下生成计划，只读业务数据；不写 SQL、Redis 或资产明文。
func (r Restorer) Prepare(ctx context.Context, schema string, zone uint32, maintenanceID string, ids []string, allowLegacy bool) (Plan, error) {
	p := Plan{Format: Format, Schema: schema, Zone: zone, MaintenanceID: maintenanceID, AllowLegacy: allowLegacy, RedisAddr: r.Redis.Options().Addr, RedisDB: r.Redis.Options().DB, RoutingMode: r.RoutingMode, LocalMySQL: r.LocalMySQL}
	p.AllowHomeFallback = r.AllowHomeFallback
	if r.MappingRedis != nil || r.RouteSchema != "" || r.RoutingMode != RoutingModeRedis || schema != fmt.Sprintf("zone_%d_db", zone) || p.RedisDB != 0 || r.AuditedOwnerUpperBound != "" {
		p.Format, p.RouteSchema = FormatV2, r.RouteSchema
		p.AuditedOwnerUpperBound = r.AuditedOwnerUpperBound
		p.MappingAddr, p.MappingDB = r.mapping().Options().Addr, r.mapping().Options().DB
	}
	// 先校验范围，避免非法输入触发连接或读取。
	if p.ValidateTarget() != nil || len(ids) == 0 || len(ids) > 1000 || len(maintenanceID) == 0 || len(maintenanceID) > 128 {
		return p, errors.New("维护范围无效")
	}
	seen := map[string]bool{}
	if p.Format == FormatV2 {
		if _, err := auditedBound(p.AuditedOwnerUpperBound); err != nil {
			return p, err
		}
	}
	for _, id := range ids {
		if _, err := positive(id); err != nil || seen[id] {
			return p, errors.New("玩家号无效或重复")
		}
		seen[id] = true
	}
	for _, id := range ids {
		row, err := r.prepareOne(ctx, p, id)
		if err != nil {
			return p, fmt.Errorf("玩家 %s 预检失败: %w", id, err)
		}
		p.Players = append(p.Players, row)
	}
	return p, p.Validate()
}

func (r Restorer) prepareOne(ctx context.Context, p Plan, id string) (Player, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Player{}, err
	}
	defer tx.Rollback()
	row, err := r.lockRoute(ctx, tx, p, id)
	if err != nil {
		return Player{}, err
	}
	rows, err := readPair(ctx, tx, p.Schema, id)
	if err != nil {
		return Player{}, err
	}
	pid, _ := positive(id)
	// GET 仅获取计数器；Lua 在同一原子段复核它，不能把此预检当成发布点。
	owner, err := r.Redis.Get(ctx, ownerepoch.OwnerEpochKey(pid)).Result()
	if err != nil {
		return Player{}, errors.New("归属计数器缺失或无法读取，需要先单独恢复计数器")
	}
	n, err := positive(owner)
	if err != nil || n < rows[0].epoch {
		return Player{}, errors.New("归属计数器缺失、畸形或落后于 SQL")
	}
	row.Owner, row.Original, row.Rows = owner, savebarrier.Version(rows[0].epoch, rows[0].seq), [2]string{rows[0].digest, rows[1].digest}
	row.Target = row.Original
	if rows[0].epoch == 0 {
		if !p.AllowLegacy {
			return Player{}, errors.New("历史零围栏未获迁移选项")
		}
		row.Target = savebarrier.Version(n, 1)
	}
	if err := r.inspect(ctx, p, row, false, row.Original == row.Target, restoredSchemaGen(rows)); err != nil {
		return Player{}, err
	}
	return row, nil
}

// ApplyOne 每次只提交一名计划内玩家。失败保留维护标记，已成功的前缀可按原计划重入。
// 零围栏先原子提交双表元数据，再重新点锁强读复核，锁内才发布 Redis；SQL 提交失败绝不发布 ACK。
func (r Restorer) ApplyOne(ctx context.Context, p Plan, index int) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.RedisAddr != r.Redis.Options().Addr || p.RedisDB != r.Redis.Options().DB || p.RoutingMode != r.RoutingMode || p.LocalMySQL != r.LocalMySQL || p.RouteSchema != r.RouteSchema || (p.Format == FormatV2 && (p.MappingAddr != r.mapping().Options().Addr || p.MappingDB != r.mapping().Options().DB)) {
		return errors.New("实际 Redis 连接或路由模式偏离审定计划")
	}
	if p.Format == Format && (r.mapping().Options().Addr != p.RedisAddr || r.mapping().Options().DB != 0) {
		return errors.New("v1 实际映射必须与父缓存同实例 DB0")
	}
	if p.AuditedOwnerUpperBound != r.AuditedOwnerUpperBound {
		return errors.New("独立审核的旧代上界偏离计划")
	}
	if index < 0 || index >= len(p.Players) {
		return errors.New("维护计划玩家索引越界")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	row := p.Players[index]
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err := r.recheckRoute(ctx, tx, p, row); err != nil {
		return err
	}
	current, err := readPair(ctx, tx, p.Schema, row.ID)
	if err != nil {
		return err
	}
	if err := match(row, current); err != nil {
		return err
	}
	if err := r.inspect(ctx, p, row, false, savebarrier.Version(current[0].epoch, current[0].seq) == row.Target, restoredSchemaGen(current)); err != nil {
		return err
	}
	if savebarrier.Version(current[0].epoch, current[0].seq) != row.Target {
		e, s, _ := savebarrier.Parse(row.Target)
		for _, table := range []string{"player_database", "player_database_1"} {
			result, err := tx.ExecContext(ctx, "UPDATE `"+p.Schema+"`."+table+" SET fence_owner_epoch=?,fence_save_seq=? WHERE player_id=? AND fence_owner_epoch=0 AND fence_save_seq=0", e, s, row.ID)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return errors.New("历史围栏迁移 CAS 失败")
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("双表围栏提交失败，未发布屏障: %w", err)
		}
		tx, err = r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			return err
		}
		if err := r.recheckRoute(ctx, tx, p, row); err != nil {
			return err
		}
		current, err = readPair(ctx, tx, p.Schema, row.ID)
		if err != nil {
			return err
		}
		if err := match(row, current); err != nil {
			return err
		}
		if savebarrier.Version(current[0].epoch, current[0].seq) != row.Target {
			return errors.New("已提交围栏在重锁期间改变")
		}
	}
	if err := r.inspect(ctx, p, row, true, true, restoredSchemaGen(current)); err != nil {
		return err
	}
	// 本事务只有锁定读。Redis 已确认真实提交的 SQL，结束读事务不改变资产结局。
	return tx.Rollback()
}

type sqlRow struct {
	epoch, seq uint64
	schemaGen  uint32
	digest     string
	values     map[string][]byte
}

// 恢复以 SQL 为准，屏障 hash 的 schema_gen 写成恢复之后玩家实际会加载到的数据的世代：同一事务已点锁两行的
// 最小戳，未盖戳 0 按 1。与 scene C3、login C4 的"加载世代 = 两表戳最小值"同一口径
// (docs/design/mixed-version-persist-field-protection.md §3.3、§13 第 8 条)：取最大值的话，两行戳不同(单表进过死信)
// 时冷加载出的数据加载世代低于 hash，恢复后的第一次进游戏与存盘都会被判倒退，enforce 下这名玩家既进不来也存不了。
// 世代仍来自 SQL 原件且纳入原全列摘要；它不进入可编辑计划，也不修改 SQL 列。
func restoredSchemaGen(rows [2]sqlRow) uint32 {
	gen := uint32(0)
	for _, row := range rows {
		effective := row.schemaGen
		if effective == 0 {
			effective = 1
		}
		if gen == 0 || effective < gen {
			gen = effective
		}
	}
	return gen
}

func readPair(ctx context.Context, tx *sql.Tx, schema, id string) ([2]sqlRow, error) {
	var pair [2]sqlRow
	for i, table := range []string{"player_database", "player_database_1"} {
		row, err := readRow(ctx, tx, schema, table, id)
		if err != nil {
			return pair, err
		}
		pair[i] = row
	}
	if pair[0].epoch != pair[1].epoch || pair[0].seq != pair[1].seq || (pair[0].epoch == 0) != (pair[0].seq == 0) {
		return pair, errors.New("两张 SQL 行围栏不同版或不完整")
	}
	return pair, nil
}

func readRow(ctx context.Context, tx *sql.Tx, schema, table, id string) (sqlRow, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM `"+schema+"`."+table+" WHERE player_id=? FOR UPDATE", id)
	if err != nil {
		return sqlRow{}, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return sqlRow{}, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return sqlRow{}, err
		}
		return sqlRow{}, errors.New("SQL 玩家行缺失")
	}
	values := make([]sql.RawBytes, len(columns))
	args := make([]any, len(columns))
	for i := range values {
		args[i] = &values[i]
	}
	if err := rows.Scan(args...); err != nil {
		return sqlRow{}, err
	}
	var row sqlRow
	row.values = make(map[string][]byte, len(columns))
	if len(columns) == 0 || len(columns) > 256 {
		return row, errors.New("SQL玩家列预算无效")
	}
	found := [4]bool{}
	h := sha256.New()
	totalBytes := 0
	for i, col := range columns {
		v := values[i]
		totalBytes += len(v)
		if totalBytes > 16<<20 {
			return row, errors.New("SQL玩家原件单行超过16MiB预算")
		}
		if _, duplicate := row.values[col]; duplicate {
			return row, errors.New("SQL玩家列重复")
		}
		row.values[col] = bytes.Clone(v)
		switch col {
		case "fence_owner_epoch", "fence_save_seq":
			n, err := strconv.ParseUint(string(v), 10, 64)
			if err != nil || strconv.FormatUint(n, 10) != string(v) {
				return row, errors.New("SQL 围栏值无效")
			}
			if col == "fence_owner_epoch" {
				row.epoch, found[1] = n, true
			} else {
				row.seq, found[2] = n, true
			}
			continue
		case "player_id":
			if string(v) != id {
				return row, errors.New("SQL 行玩家身份不符")
			}
			found[0] = true
		case "persist_schema_gen":
			// 正常迁移先补NOT NULL DEFAULT 0列；缺列、NULL与坏值不能静默当历史0。
			n, err := strconv.ParseUint(string(v), 10, 32)
			if err != nil || strconv.FormatUint(n, 10) != string(v) {
				return row, errors.New("SQL 持久化世代不是规范 uint32")
			}
			row.schemaGen, found[3] = uint32(n), true
			// 不continue：原始戳字节与所有其它非围栏列一起进入原计划摘要。
		}
		// 长度前缀与 NULL 标记避免字段边界或 NULL/空串出现相同摘要。
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(col)))
		h.Write(size[:])
		h.Write([]byte(col))
		if v == nil {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{1})
			binary.BigEndian.PutUint64(size[:], uint64(len(v)))
			h.Write(size[:])
			h.Write(v)
		}
	}
	if found != [4]bool{true, true, true, true} {
		return row, errors.New("SQL 玩家身份、围栏或持久化世代列缺失，须先完成正常迁移")
	}
	if rows.Next() {
		return row, errors.New("SQL 玩家主键不唯一")
	}
	if err := rows.Err(); err != nil {
		return row, err
	}
	row.digest = hex.EncodeToString(h.Sum(nil))
	return row, nil
}

func match(want Player, current [2]sqlRow) error {
	v := savebarrier.Version(current[0].epoch, current[0].seq)
	if v != want.Original && v != want.Target {
		return errors.New("SQL 围栏偏离审定计划")
	}
	for i := range current {
		if current[i].digest != want.Rows[i] {
			return errors.New("SQL 资产行偏离审定计划")
		}
	}
	return nil
}

func validRoute(zone uint32, home, raw string, allowHomeFallback bool) error {
	homeZone, present, err := placement.ParseHomeZone(home)
	if err != nil || !present || homeZone != zone || home != strconv.FormatUint(uint64(zone), 10) {
		return errors.New("玩家 home zone 与计划不符")
	}
	if homeZoneFallback(allowHomeFallback, raw) {
		// v1 的 schema 已由 validTarget 钉为 zone_<home>_db,正是 go/db 给无记录玩家选的库;发布时 Lua 再原子复核记录仍不存在。
		return nil
	}
	rec, err := placement.Parse(raw)
	if err != nil || rec.Frozen || rec.StorageID != zone || rec.Encode() != raw {
		return errors.New("玩家不在本区稳定落点，须走有权威路由证明的维护流程")
	}
	return nil
}

// homeZoneFallback 报告这名玩家是否按「无落点记录 → home 的 zone 库」恢复(2026-10-20 修复，r2-mz#1)。
//
// 只在计划显式允许且落点原值为空时成立；调用方另行保证 home == 计划 zone、schema 的库号 == home。放宽的边界：
//   - 与 go/db 选库同一规则(placement.EffectiveStorage：记录 ?? home)，恢复出的屏障指向的正是运行期冷加载要读的两行；
//     有记录的玩家(钉落点、搬库、pin 合服)仍走原来的稳定落点校验，不受影响。
//   - 屏障只按当前 SQL 两行的围栏重建，不会把任何旧屏障恢复回来：已有屏障仍须与目标版本逐字段相同，否则照旧拒绝。
//   - 现有原子校验一条不少：永久维护标记、owner、home 原值、无合服围栏、缓存 / 证据 / 交接标记不存在、64 桶为空；
//     同实例时落点记录「仍不存在」在写屏障的同一段 Lua 里复核(scripts.go),拆实例时由 mapping 维护检查前后各核一次。
func homeZoneFallback(allow bool, rawPlacement string) bool {
	return allow && rawPlacement == ""
}

func MaintenanceKey(zone uint32) string {
	return fmt.Sprintf("player:save_barrier:maintenance:%d", zone)
}

func recoveryKeys(zone uint32, id uint64) []string {
	first := string((&databasepb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName())
	second := string((&databasepb.PlayerDatabase_1{}).ProtoReflect().Descriptor().FullName())
	keys := []string{MaintenanceKey(zone), ownerepoch.OwnerEpochKey(id), savebarrier.ParentKey(id), fmt.Sprintf("%s:%d", first, id), fmt.Sprintf("%s:%d", second, id), fmt.Sprintf("player:{%d}:asset_sql_commit", id), ownerepoch.HandoffKey(id), savebarrier.Key(id)}
	for bucket := 0; bucket < 64; bucket++ {
		keys = append(keys, fmt.Sprintf("player:save_pending:%d:%d", zone, bucket))
	}
	keys = append(keys, placement.HomeZoneKey(id), placement.Key(id), placement.MergeFenceKey(zone))
	return keys
}

func (r Restorer) inspect(ctx context.Context, p Plan, row Player, publish, committed bool, schemaGen uint32) error {
	id, _ := positive(row.ID)
	write := "0"
	if publish {
		write = "1"
	}
	allowExisting := "0"
	if committed {
		allowExisting = "1"
	}
	keys := recoveryKeys(p.Zone, id)
	if p.RoutingMode != RoutingModeRedis {
		if err := r.inspectMapping(ctx, p, row); err != nil {
			return err
		}
		keys = keys[:72]
	}
	// 首次读取的原件只作为本次 Lua CAS 输入；不能缺省空字段冒缺键。
	prior, err := r.Redis.HGet(ctx, savebarrier.Key(id), "handoff_fence").Result()
	if errors.Is(err, redis.Nil) {
		prior = ""
	} else if err != nil {
		return fmt.Errorf("读取交接资格下限: %w", err)
	} else if prior == "" {
		return errors.New("交接资格下限为空值")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fmt.Errorf("轮换交接撤销令牌: %w", err)
	}
	n, err := restoreScript.Run(ctx, r.Redis, keys, p.MaintenanceID, row.Owner, row.Target, write, allowExisting, row.HomeZone, row.Placement, strconv.FormatUint(uint64(schemaGen), 10), prior, hex.EncodeToString(token[:])).Int()
	if err != nil {
		return fmt.Errorf("维护屏障检查拒绝: %w", err)
	}
	if n != 1 {
		return errors.New("维护屏障回复无效")
	}
	if p.RoutingMode != RoutingModeRedis {
		return r.inspectMapping(ctx, p, row)
	}
	return nil
}

// MaintenanceReleasePlan 是显式维护动作的封套，不取代任何业务 proto。
// 成员及外部审定原件固定；每次执行的短排他窗独立输入，不能改原计划续期。
type MaintenanceReleasePlan struct {
	Format        string                     `json:"format"`
	Intent        string                     `json:"intent_id"`
	Zone          uint32                     `json:"zone"`
	MaintenanceID string                     `json:"maintenance_id"`
	LocalTarget   string                     `json:"local_target_id"`
	SQLAddr       string                     `json:"physical_sql_addr"`
	RouteSchema   string                     `json:"route_schema"`
	Review        ReleaseFile                `json:"review"`
	Members       []MaintenanceReleaseMember `json:"members"`
	Targets       []MaintenanceReleaseTarget `json:"targets"`
	SQLSHA256     string                     `json:"sql_snapshot_sha256"`
}
type MaintenanceReleaseMember struct {
	PlayerID       uint64      `json:"player_id"`
	RowSchema      string      `json:"row_schema"`
	PromotionProto []byte      `json:"promotion_proto,omitempty"`
	RecoveryProtos [][]byte    `json:"recovery_protos,omitempty"`
	CancelAction   ReleaseFile `json:"cancel_action"`
	// 来自A原全列观察；普通未恢复成员也须钉业务摘要/原版本，不能只从当前SQL重新取基线。
	FirstBusinessSHA256  string `json:"first_business_sha256"`
	SecondBusinessSHA256 string `json:"second_business_sha256"`
	Owner                uint64 `json:"owner_epoch"`
	SaveSeq              uint64 `json:"save_seq"`
	Fence                string `json:"handoff_fence"`
}
type MaintenanceReleaseTarget struct {
	Role          string                    `json:"role"`
	Addr          string                    `json:"addr"`
	DB            int                       `json:"db"`
	InstanceNonce string                    `json:"instance_nonce"`
	Guards        []MaintenanceReleaseGuard `json:"guards"`
	Markers       []MaintenanceRedisMarker  `json:"markers"`
}
type MaintenanceReleaseGuard struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
type ReleaseFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

const maintenanceReleaseFormat = "player-save-explicit-maintenance-release-v2"

// PrepareMaintenanceRelease 只读SQL/Redis，不写收据、guard或业务状态。
// permit须由同一审定review和本次新排他窗构造；SQL不宣称能保证进程/操作者排他。
func (r Restorer) PrepareMaintenanceRelease(ctx context.Context, zone uint32, maintenance string, members []MaintenanceReleaseMember, permit *MaintenanceReleasePermit) (MaintenanceReleasePlan, error) {
	p := MaintenanceReleasePlan{Format: maintenanceReleaseFormat, Zone: zone, MaintenanceID: maintenance, LocalTarget: r.LocalTargetID, SQLAddr: r.MaintenanceSQLAddr, RouteSchema: r.RouteSchema, Members: members}
	if permit == nil || permit.planSHA != "" {
		return p, errors.New("只读release-plan需要未绑定计划的新审定窗口")
	}
	p.Review = permit.review
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return p, err
	}
	p.Intent = hex.EncodeToString(nonce)
	var err error
	p.Targets, err = r.releaseTargets(ctx, zone, maintenance)
	if err != nil {
		return p, err
	}
	if err = r.validateMaintenanceRelease(p); err != nil {
		return p, err
	}
	if err = permit.check(p, ""); err != nil {
		return p, err
	}
	p.SQLSHA256, err = r.releaseSQLSnapshot(ctx, p)
	if err != nil {
		return p, err
	}
	// 计划创建只准全部原guard在、收据缺席；已提交前缀必须用原计划，不重造intent。
	for _, target := range p.Targets {
		state, err := r.releaseTarget(ctx, p, target, "", false, permit.expiresMS)
		if err != nil || state != "pending" {
			return p, errors.New("release-plan发现已提交/损坏目标，必须保原计划消歧")
		}
	}
	return p, permit.check(p, "")
}

// ApplyMaintenanceRelease 先复核全部物理目标，再逐目标原子提交。未知应答不自动重发。
// 同原计划重入从入口即接受精确committed前缀；仍不unfreeze SQL或重启业务。
func (r Restorer) ApplyMaintenanceRelease(ctx context.Context, p MaintenanceReleasePlan, planSHA string, permit *MaintenanceReleasePermit) error {
	if err := r.validateMaintenanceRelease(p); err != nil {
		return err
	}
	if !releaseHex(planSHA, 64) || permit == nil {
		return errors.New("必须原计划全文件SHA和新的短排他窗")
	}
	original, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	original = append(original, '\n')
	fileSum := sha256.Sum256(original)
	if hex.EncodeToString(fileSum[:]) != planSHA {
		return errors.New("typed计划与原CLI规范整文件SHA不符，禁止换intent/正文")
	}
	if err := permit.check(p, planSHA); err != nil {
		return err
	}
	snapshot, err := r.releaseSQLSnapshot(ctx, p)
	if err != nil || snapshot != p.SQLSHA256 {
		return errors.New("解除前SQL全列/成员/永久原件发生变化")
	}
	for _, target := range p.Targets {
		if _, err = r.releaseTarget(ctx, p, target, planSHA, false, permit.expiresMS); err != nil {
			return err
		}
	}
	for _, target := range p.Targets {
		if err = permit.check(p, planSHA); err != nil {
			return err
		}
		// 网络I/O均发生在SQL事务之外。SQL永久冻结和外部禁止新writer是显式前置。
		if _, err = r.releaseTarget(ctx, p, target, planSHA, true, permit.expiresMS); err != nil {
			return fmt.Errorf("解除结果未知/失败，保原计划及已提交前缀，SQL仍冻结: %w", err)
		}
	}
	for _, target := range p.Targets {
		state, e := r.releaseTarget(ctx, p, target, planSHA, false, permit.expiresMS)
		if e != nil || state != "committed" {
			return errors.New("解除尾核未确认完整永久收据")
		}
	}
	snapshot, err = r.releaseSQLSnapshot(ctx, p)
	if err != nil || snapshot != p.SQLSHA256 {
		return errors.New("解除后SQL全列改变；禁止unfreeze/重启，保全部原件")
	}
	return permit.check(p, planSHA)
}

func releaseHex(v string, n int) bool {
	return len(v) == n && strings.Trim(v, "0123456789abcdef") == "" && v != strings.Repeat("0", n)
}
func maintenanceReleaseReceiptKey(zone uint32, maintenance string) string {
	h := sha256.Sum256([]byte(maintenance))
	return fmt.Sprintf("player:save_barrier:maintenance_release:%d:%x", zone, h)
}
func (r Restorer) releaseTargets(ctx context.Context, zone uint32, maintenance string) ([]MaintenanceReleaseTarget, error) {
	if r.Redis == nil || r.mapping() == nil {
		return nil, errors.New("缺两个维护专用Redis连接")
	}
	targets := []MaintenanceReleaseTarget{}
	for _, item := range []struct {
		role   string
		client *redis.Client
		guards []MaintenanceReleaseGuard
	}{
		{"core", r.Redis, []MaintenanceReleaseGuard{{MaintenanceKey(zone), maintenance}}},
		{"mapping", r.mapping(), []MaintenanceReleaseGuard{{MaintenanceKey(zone), maintenance}, {placement.MergeFenceKey(zone), routeGuardValue(maintenance)}}},
	} {
		c := item.client
		marker, err := ReadMaintenanceRedisMarker(ctx, c, "release", item.role, zone, r.RouteSchema, r.RouteSchema)
		if err != nil {
			return nil, err
		}
		nonce := marker.Nonce
		merged := -1
		for i, t := range targets {
			if t.Addr == c.Options().Addr && t.DB == c.Options().DB {
				merged = i
			}
		}
		if merged >= 0 {
			t := &targets[merged]
			if t.InstanceNonce != nonce {
				return nil, errors.New("同一物理目标nonce观察冲突")
			}
			t.Markers = append(t.Markers, marker)
			for _, g := range item.guards {
				found := false
				for _, prior := range t.Guards {
					if prior.Key == g.Key {
						if prior.Value != g.Value {
							return nil, errors.New("同key不同原guard")
						}
						found = true
					}
				}
				if !found {
					t.Guards = append(t.Guards, g)
				}
			}
			continue
		}
		targets = append(targets, MaintenanceReleaseTarget{Role: item.role, Addr: c.Options().Addr, DB: c.Options().DB, InstanceNonce: nonce, Guards: item.guards, Markers: []MaintenanceRedisMarker{marker}})
	}
	return targets, nil
}
func (r Restorer) validateMaintenanceRelease(p MaintenanceReleasePlan) error {
	if p.Format != maintenanceReleaseFormat || !releaseHex(p.Intent, 64) || p.Zone == 0 || p.Zone > placement.MaxZoneStorageID || p.MaintenanceID == "" || len(p.MaintenanceID) > 128 || p.LocalTarget != r.LocalTargetID || !promotionTargetID(p.LocalTarget) || p.SQLAddr != r.MaintenanceSQLAddr || !maintenanceAddress(p.SQLAddr) || p.RouteSchema != r.RouteSchema || !validSchema(p.RouteSchema) || r.RoutingMode != RoutingModeDBFirst || r.LocalMySQL || r.DB == nil || r.Redis == nil || r.mapping() == nil || len(p.Members) == 0 || len(p.Members) > 1000 {
		return errors.New("显式解除仅限原TiDB/db_first/维护目标/完整具名成员")
	}
	seen := map[uint64]bool{}
	recovered := 0
	for _, m := range p.Members {
		if m.PlayerID == 0 || seen[m.PlayerID] || !validSchema(m.RowSchema) || m.Owner == 0 || m.SaveSeq == 0 || !releaseHex(m.FirstBusinessSHA256, 64) || !releaseHex(m.SecondBusinessSHA256, 64) {
			return errors.New("解除成员重复/非法/缺A原资产业务摘要与版本")
		}
		seen[m.PlayerID] = true
		if len(m.PromotionProto) == 0 {
			if len(m.RecoveryProtos) != 0 || m.CancelAction.Path != "" {
				return errors.New("无promotion成员不能伪带领域恢复原件")
			}
			continue
		}
		recovered++
		source := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
		if proto.Unmarshal(m.PromotionProto, source) != nil || ValidatePromotion(source) != nil || source.PlayerId != m.PlayerID || source.RowSchema != m.RowSchema || source.RouteSchema != p.RouteSchema || source.HomeZone != p.Zone || source.MaintenanceId != p.MaintenanceID || len(m.RecoveryProtos) == 0 {
			return errors.New("promotion不属于原维护/成员，或缺永久退休原件")
		}
		raw, _, err := PromotionDigest(source)
		if err != nil || !bytes.Equal(raw, m.PromotionProto) {
			return errors.New("promotion不是原规范PB bytes")
		}
		if m.Owner != source.TargetOwner || m.SaveSeq != source.TargetSaveSeq || m.Fence != source.RevokedFence || m.FirstBusinessSHA256 != hex.EncodeToString(source.FirstBusinessSha256) || m.SecondBusinessSHA256 != hex.EncodeToString(source.SecondBusinessSha256) {
			return errors.New("恢复成员业务/双版本/永久R不等原promotion")
		}
	}
	if recovered == 0 {
		return errors.New("显式恢复解除至少需要本维护的真实永久恢复原件")
	}
	// 物理目标/guard集合只由固定core/mapping连接派生，不接受任意key。
	if len(p.Targets) < 1 || len(p.Targets) > 2 {
		return errors.New("解除物理目标数非法")
	}
	if p.Targets[0].Addr != r.Redis.Options().Addr || p.Targets[0].DB != r.Redis.Options().DB {
		return errors.New("提交顺序必须固定core先，不能重排物理前缀")
	}

	expected := map[string]map[string]string{}
	expectedMarkers := map[string]map[string]MaintenanceRedisMarker{}
	// go-redis把构造参数MaxRetries:-1规范为Options().MaxRetries==0；默认构造会规范为3。
	for i, c := range []*redis.Client{r.Redis, r.mapping()} {
		if c == nil || c.Options().MaxRetries != 0 {
			return errors.New("解除连接必须关闭自动命令重试")
		}
		key := fmt.Sprintf("%s/%d", c.Options().Addr, c.Options().DB)
		if expected[key] == nil {
			expected[key] = map[string]string{MaintenanceKey(p.Zone): p.MaintenanceID}
			expectedMarkers[key] = map[string]MaintenanceRedisMarker{}
		}
		role := []string{"core", "mapping"}[i]
		mk, scope, err := MaintenanceRedisScope("release", role, p.Zone, p.RouteSchema, p.RouteSchema, c.Options().Addr, c.Options().DB)
		if err != nil {
			return err
		}
		expectedMarkers[key][role] = MaintenanceRedisMarker{Role: role, Key: mk, Scope: scope}
	}
	mk := fmt.Sprintf("%s/%d", r.mapping().Options().Addr, r.mapping().Options().DB)
	expected[mk][placement.MergeFenceKey(p.Zone)] = routeGuardValue(p.MaintenanceID)
	for _, t := range p.Targets {
		key := fmt.Sprintf("%s/%d", t.Addr, t.DB)
		guards, ok := expected[key]
		role := "mapping"
		if t.Addr == r.Redis.Options().Addr && t.DB == r.Redis.Options().DB {
			role = "core"
		}
		if !ok || t.Role != role || !promotionTargetID(t.InstanceNonce) || len(guards) != len(t.Guards) || len(t.Markers) != len(expectedMarkers[key]) {
			return errors.New("目标/guard不是固定全集")
		}
		for _, marker := range t.Markers {
			want, exists := expectedMarkers[key][marker.Role]
			if !exists || marker.Key != want.Key || marker.Scope != want.Scope || marker.Nonce != t.InstanceNonce {
				return errors.New("维护角色marker不是原物理目标全集")
			}
			delete(expectedMarkers[key], marker.Role)
		}
		if len(expectedMarkers[key]) != 0 {
			return errors.New("维护角色marker遗漏")
		}
		for _, g := range t.Guards {
			if guards[g.Key] != g.Value {
				return errors.New("guard不匹配")
			}
			delete(guards, g.Key)
		}
		if len(guards) != 0 {
			return errors.New("guard重复/遗漏")
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		return errors.New("物理目标遗漏")
	}
	return nil
}
func (r Restorer) releaseTarget(ctx context.Context, p MaintenanceReleasePlan, t MaintenanceReleaseTarget, planSHA string, apply bool, expiresMS int64) (string, error) {
	var client *redis.Client
	for _, c := range []*redis.Client{r.Redis, r.mapping()} {
		if c != nil && c.Options().Addr == t.Addr && c.Options().DB == t.DB {
			client = c
			break
		}
	}
	if client == nil {
		return "", errors.New("目标连接已变")
	}
	mode := "check"
	if apply {
		mode = "apply"
	}
	body, _ := json.Marshal(p)
	inner := sha256.Sum256(body)
	guardRaw, _ := json.Marshal(t.Guards)
	gd := sha256.Sum256(guardRaw)
	receipt := ""
	if planSHA != "" {
		receipt = fmt.Sprintf("release-v2|%s|%s|%x|%s|%x", p.Intent, planSHA, inner, t.InstanceNonce, gd)
	}
	keys := []string{}
	args := []any{mode, receipt, strconv.Itoa(len(t.Markers)), strconv.Itoa(len(t.Guards)), "0", strconv.FormatInt(expiresMS, 10)}
	for _, marker := range t.Markers {
		keys = append(keys, marker.Key, marker.Key+":nonce")
		args = append(args, marker.Scope, marker.Nonce)
	}
	keys = append(keys, maintenanceReleaseReceiptKey(p.Zone, p.MaintenanceID))
	for _, g := range t.Guards {
		keys = append(keys, g.Key)
		args = append(args, g.Value)
	}
	if t.Addr == r.Redis.Options().Addr && t.DB == r.Redis.Options().DB {
		args[4] = strconv.Itoa(len(p.Members))
		for _, m := range p.Members {
			keys = append(keys, ownerepoch.OwnerEpochKey(m.PlayerID), savebarrier.Key(m.PlayerID))
			args = append(args, strconv.FormatUint(m.Owner, 10), savebarrier.Version(m.Owner, m.SaveSeq), m.Fence)
		}
	}
	// EVAL一次，不用Script.Run的NOSCRIPT隐式重发路径；MaxRetries=-1也在入口强制。
	return client.Eval(ctx, maintenanceReleaseLua, keys, args...).Text()
}

// releaseSQLSnapshot只读一致事务；全部列含NULL/blob入摘要。无事务内Redis/Kafka/Scene调用。
// 验证的是正式领域终态和永久原件，不重新实现Cancel的业务裁决或更新任何P/state。
func (r Restorer) releaseSQLSnapshot(ctx context.Context, p MaintenanceReleasePlan) (string, error) {
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	evidence := []releaseSQLRecord{}
	read := func(label, q string, args ...any) ([]releaseSQLRecord, error) {
		rows, e := readMaintenanceReleaseRows(ctx, tx, label, q, args...)
		if e == nil {
			evidence = append(evidence, rows...)
		}
		return rows, e
	}
	schemas := append([]string{p.RouteSchema}, releaseMemberSchemas(p.Members)...)
	if err := r.checkMaintenanceSQLTargets(ctx, tx, "release", p.Zone, schemas); err != nil {
		return "", err
	}
	for _, schema := range schemas {
		rows, e := read("marker/"+schema, "SELECT * FROM `"+schema+"`.maintenance_recovery_target WHERE purpose=? AND zone_id=? AND schema_name=? FOR UPDATE", "release", p.Zone, []byte(schema))
		if e != nil || len(rows) != 1 || string(rows[0].value("target_nonce")) != p.LocalTarget || string(rows[0].value("physical_addr")) != r.MaintenanceSQLAddr {
			return "", errors.New("专用维护SQL目标原件不符")
		}
	}
	meta, e := read("route_meta", "SELECT * FROM `"+p.RouteSchema+"`.route_meta WHERE id=1")
	if e != nil || len(meta) != 1 || string(meta[0].value("writable")) != "0" {
		return "", errors.New("全局SQL路由必须仍冻结")
	}
	reconcile, e := read("route_reconcile", "SELECT * FROM `"+p.RouteSchema+"`.route_reconcile WHERE id=1")
	if e != nil || len(reconcile) != 1 {
		return "", errors.New("路由对账水位缺失")
	}
	if rows, e := read("merged_into", "SELECT * FROM `"+p.RouteSchema+"`.zone_merged_into WHERE src_zone=?", p.Zone); e != nil || len(rows) != 0 {
		return "", errors.New("home zone已合走")
	}
	if rows, e := read("active_merge", "SELECT * FROM `"+p.RouteSchema+"`.zone_merge_fence WHERE zone_id=? AND run_id IS NOT NULL AND expires_at_ms>CAST(UNIX_TIMESTAMP(NOW(3))*1000 AS UNSIGNED)", p.Zone); e != nil || len(rows) != 0 {
		return "", errors.New("仍有SQL合服围栏")
	}
	routeByPlayer := map[uint64]releaseSQLRecord{}
	// 全zone真实路由成员与审定成员逐键等集；不允许仅恢复子集后解除整zone。
	roster, e := read("zone_members", "SELECT * FROM `"+p.RouteSchema+"`.player_route WHERE home_zone=? ORDER BY route_shard,player_id", p.Zone)
	if e != nil || len(roster) != len(p.Members) {
		return "", errors.New("当前zone完整成员数不符")
	}
	selected := map[uint64]MaintenanceReleaseMember{}
	for _, m := range p.Members {
		selected[m.PlayerID] = m
	}
	for _, row := range roster {
		id, e := positive(string(row.value("player_id")))
		m, ok := selected[id]
		storage, known := placement.StorageIDFromDBName(m.RowSchema)
		home, hasHome := row.cell("home_zone")
		place, hasPlace := row.cell("storage_id")
		version, hasVersion := row.cell("placement_version")
		frozen, hasFrozen := row.cell("frozen_run_id")
		if e != nil || !ok || !known || !hasHome || home.Null || !hasPlace || !hasVersion || !hasFrozen || place.Null != version.Null || string(home.Bytes) != strconv.FormatUint(uint64(p.Zone), 10) || !frozen.Null && len(frozen.Bytes) != 0 {
			return "", errors.New("当前路由身份/搬库状态/NULL配对不符")
		}
		if place.Null {
			if storage != p.Zone {
				return "", errors.New("无placement只允许本home库")
			}
		} else {
			actual, aerr := positive(string(place.Bytes))
			_, verr := positive(string(version.Bytes))
			if aerr != nil || verr != nil || actual != uint64(storage) {
				return "", errors.New("实际落点与成员SQL库不符")
			}
		}
		routeByPlayer[id] = row
		delete(selected, id)
	}
	if len(selected) != 0 {
		return "", errors.New("实际成员遗漏/重复")
	}
	promotions, e := read("maintenance_all", "SELECT * FROM `"+p.RouteSchema+"`.save_barrier_maintenance_receipt ORDER BY receipt_id")
	if e != nil {
		return "", e
	}
	matching := map[uint64]releaseSQLRecord{}
	for _, row := range promotions {
		src := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
		if proto.Unmarshal(row.value("plan_proto"), src) != nil {
			return "", errors.New("永久promotion原件损坏")
		}
		if src.MaintenanceId == p.MaintenanceID {
			if _, dup := matching[src.PlayerId]; dup {
				return "", errors.New("本维护重复成员promotion")
			}
			matching[src.PlayerId] = row
		}
	}
	for _, m := range p.Members {
		id := strconv.FormatUint(m.PlayerID, 10)
		for _, table := range []string{"player_database", "player_database_1"} {
			rows, e := read(table+"/"+id, "SELECT * FROM `"+m.RowSchema+"`."+table+" WHERE player_id=?", m.PlayerID)
			if e != nil || len(rows) != 1 {
				return "", errors.New("原双SQL不完整")
			}
			want := m.FirstBusinessSHA256
			if table == "player_database_1" {
				want = m.SecondBusinessSHA256
			}
			if string(rows[0].value("player_id")) != id || string(rows[0].value("fence_owner_epoch")) != strconv.FormatUint(m.Owner, 10) || string(rows[0].value("fence_save_seq")) != strconv.FormatUint(m.SaveSeq, 10) || releaseBusinessDigest(rows[0]) != want {
				return "", errors.New("SQL业务/NULL/未知blob/版本不等A原件")
			}
		}
		// 原完整zone中未做promotion的成员也不能留下开放P/旧admission；不以恢复子集冒全zone可解除。
		roleRows, e := read("role/"+id, "SELECT * FROM `"+p.RouteSchema+"`.account_role WHERE player_id=?", m.PlayerID)
		if e != nil || len(roleRows) != 1 || string(roleRows[0].value("trade_listing_id")) != "0" {
			return "", errors.New("全zone仍有未完成P")
		}
		admissionRows, e := read("admissions/"+id, "SELECT * FROM `"+p.RouteSchema+"`.account_role_admission WHERE player_id=? ORDER BY scene_instance_id", m.PlayerID)
		if e != nil {
			return "", e
		}
		for _, row := range admissionRows {
			if !bytes.Equal(row.value("highest_generation"), row.value("closed_through")) {
				return "", errors.New("全zone仍有开放admission")
			}
		}
		if len(m.PromotionProto) == 0 {
			if _, exists := matching[m.PlayerID]; exists {
				return "", errors.New("遗漏本维护永久promotion")
			}
			continue
		}
		src := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
		_ = proto.Unmarshal(m.PromotionProto, src)
		_, digest, _ := PromotionDigest(src)
		route := routeByPlayer[m.PlayerID]
		place, _ := route.cell("storage_id")
		if string(meta[0].value("generation")) != src.RouteGeneration || string(reconcile[0].value("watermark_ms")) != strconv.FormatUint(src.RouteWatermark, 10) || string(route.value("rev")) != strconv.FormatUint(src.RouteRevision, 10) {
			return "", errors.New("当前路由generation/watermark/rev偏离原promotion")
		}
		if place.Null {
			if src.StorageId != src.HomeZone || src.PlacementVersion != 0 {
				return "", errors.New("原promotion无落点语义不符")
			}
		} else if string(place.Bytes) != strconv.FormatUint(uint64(src.StorageId), 10) || string(route.value("placement_version")) != strconv.FormatUint(src.PlacementVersion, 10) {
			return "", errors.New("当前placement/version偏离原promotion")
		}
		if src.RedisAddr != r.Redis.Options().Addr || int(src.RedisDb) != r.Redis.Options().DB || src.MappingAddr != r.mapping().Options().Addr || int(src.MappingDb) != r.mapping().Options().DB {
			return "", errors.New("promotion不是当前原Redis目标")
		}

		rec, ok := matching[m.PlayerID]
		if !ok || !bytes.Equal(rec.value("receipt_id"), src.ReceiptId) || !bytes.Equal(rec.value("request_digest"), digest) || !bytes.Equal(rec.value("plan_proto"), m.PromotionProto) || string(rec.value("phase")) != strconv.Itoa(int(databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED)) {
			return "", errors.New("永久promotion未确认或换原件")
		}
		delete(matching, m.PlayerID)
		recoveryRows, e := read("recoveries/"+id, "SELECT * FROM `"+p.RouteSchema+"`.account_role_admission_recovery WHERE maintenance_receipt_id=? ORDER BY request_id", src.ReceiptId)
		if e != nil || len(recoveryRows) != len(m.RecoveryProtos) {
			return "", errors.New("永久退休全集不符")
		}
		recoverySet := map[string]releaseSQLRecord{}
		for _, row := range recoveryRows {
			key := hex.EncodeToString(row.value("request_id"))
			if len(key) != 64 {
				return "", errors.New("SQL退休request_id损坏")
			}
			if _, duplicate := recoverySet[key]; duplicate {
				return "", errors.New("SQL退休request_id重复")
			}
			recoverySet[key] = row
		}
		for _, raw := range m.RecoveryProtos {
			expected := &databasepb.AccountRoleAdmissionRecovery{}
			if proto.Unmarshal(raw, expected) != nil || expected.PlayerId != m.PlayerID || !bytes.Equal(expected.MaintenanceReceiptId, src.ReceiptId) || !bytes.Equal(expected.MaintenanceRequestDigest, digest) {
				return "", errors.New("退休PB不属于原promotion")
			}
			key := hex.EncodeToString(expected.RequestId)
			row, found := recoverySet[key]
			if !found {
				return "", errors.New("退休原件重复/遗漏")
			}
			actual, err := releaseRecoveryProto(row)
			if err != nil || !proto.Equal(actual, expected) {
				return "", errors.New("永久退休原件已变")
			}
			delete(recoverySet, key)
		}
		if len(recoverySet) != 0 {
			return "", errors.New("仍有未消费的永久退休原件")
		}

		cancelRequest, e := readMaintenanceReleaseCancel(m.CancelAction)
		if e != nil || cancelRequest.PlayerId != m.PlayerID {
			return "", errors.New("缺正式原Cancel调用/实际回包")
		}
		for _, raw := range m.RecoveryProtos {
			recovery := &databasepb.AccountRoleAdmissionRecovery{}
			_ = proto.Unmarshal(raw, recovery)
			if recovery.ListingId != cancelRequest.ListingId || recovery.OwnerRevision != cancelRequest.ExpectedOwnerRevision || recovery.RequiredOwnerEpoch != m.Owner || recovery.RequiredSaveSeq != m.SaveSeq {
				return "", errors.New("退休原件与正式Cancel的listing/revision/原SQL围栏不符")
			}
		}
		rows := roleRows
		if string(rows[0].value("account")) != cancelRequest.SellerAccount || string(rows[0].value("owner_revision")) != strconv.FormatUint(cancelRequest.ExpectedOwnerRevision, 10) {
			return "", errors.New("正式Cancel后原归属不符")
		}
		rows, e = read("trade/"+id, "SELECT * FROM `"+p.RouteSchema+"`.account_role_trade WHERE listing_id=?", cancelRequest.ListingId)
		if e != nil || len(rows) != 1 || string(rows[0].value("player_id")) != id || string(rows[0].value("state")) != strconv.Itoa(int(dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED)) || string(rows[0].value("seller_account")) != cancelRequest.SellerAccount || string(rows[0].value("seller_owner_revision")) != strconv.FormatUint(cancelRequest.ExpectedOwnerRevision, 10) {
			return "", errors.New("当前SQL不是原listing的正式CANCELLED")
		}
		rows, e = read("holders/"+id, "SELECT * FROM `"+p.RouteSchema+"`.account_role_trade_holder WHERE listing_id=? ORDER BY scene_instance_id", cancelRequest.ListingId)
		if e != nil || len(rows) == 0 {
			return "", errors.New("原holder全集缺失")
		}
		for _, row := range rows {
			if string(row.value("drained")) != "1" {
				return "", errors.New("仍有未退休holder")
			}
		}

	}
	if len(matching) != 0 {
		return "", errors.New("本维护存在未列出的promotion成员")
	}
	if err = tx.Rollback(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), nil
}

type releaseSQLCell struct {
	Name  string `json:"name"`
	Null  bool   `json:"null"`
	Bytes []byte `json:"bytes"`
}
type releaseSQLRecord struct {
	Scope string           `json:"scope"`
	Cells []releaseSQLCell `json:"cells"`
}

func (row releaseSQLRecord) value(name string) []byte {
	for _, c := range row.Cells {
		if c.Name == name {
			return c.Bytes
		}
	}
	return nil
}
func (row releaseSQLRecord) cell(name string) (releaseSQLCell, bool) {
	for _, c := range row.Cells {
		if c.Name == name {
			return c, true
		}
	}
	return releaseSQLCell{}, false
}
func readMaintenanceReleaseRows(ctx context.Context, tx *sql.Tx, label, q string, args ...any) ([]releaseSQLRecord, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) == 0 || len(columns) > 256 {
		return nil, errors.New("SQL列预算无效")
	}
	seen := map[string]bool{}
	for _, c := range columns {
		if seen[c] {
			return nil, errors.New("SQL重复列")
		}
		seen[c] = true
	}
	out := []releaseSQLRecord{}
	total := 0
	for rows.Next() {
		if len(out) >= 10000 {
			return nil, errors.New("SQL成员预算超限")
		}
		values := make([]sql.RawBytes, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		r := releaseSQLRecord{Scope: label}
		for i, c := range columns {
			total += len(values[i])
			if total > 64<<20 {
				return nil, errors.New("SQL原件预算超限")
			}
			r.Cells = append(r.Cells, releaseSQLCell{Name: c, Null: values[i] == nil, Bytes: bytes.Clone(values[i])})
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func releaseMemberSchemas(members []MaintenanceReleaseMember) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range members {
		if !seen[m.RowSchema] {
			seen[m.RowSchema] = true
			out = append(out, m.RowSchema)
		}
	}
	sort.Strings(out)
	return out
}
func releaseBusinessDigest(row releaseSQLRecord) string {
	h := sha256.New()
	// 与本文件readRow的长度前缀/NULL规则完全一致，保物理列序；只排除两个fence列。
	for _, cell := range row.Cells {
		if cell.Name == "fence_owner_epoch" || cell.Name == "fence_save_seq" {
			continue
		}
		writePromotionPart(h, []byte(cell.Name))
		if cell.Null {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{1})
			writePromotionPart(h, cell.Bytes)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
func releaseRecoveryProto(row releaseSQLRecord) (*databasepb.AccountRoleAdmissionRecovery, error) {
	r := &databasepb.AccountRoleAdmissionRecovery{RequestId: bytes.Clone(row.value("request_id")), RequestDigest: bytes.Clone(row.value("request_digest")), RequestProto: bytes.Clone(row.value("request_proto")), MaintenanceReceiptId: bytes.Clone(row.value("maintenance_receipt_id")), MaintenanceRequestDigest: bytes.Clone(row.value("maintenance_request_digest")), SqlProofDigest: bytes.Clone(row.value("sql_proof_digest")), SceneInstanceId: string(row.value("scene_instance_id"))}
	for name, dest := range map[string]*uint64{"listing_id": &r.ListingId, "player_id": &r.PlayerId, "through_generation": &r.ThroughGeneration, "owner_revision": &r.OwnerRevision, "required_owner_epoch": &r.RequiredOwnerEpoch, "required_save_seq": &r.RequiredSaveSeq, "recovered_ms": &r.RecoveredMs} {
		n, e := positive(string(row.value(name)))
		if e != nil {
			return nil, e
		}
		*dest = n
	}
	request := &databasepb.AccountRoleAdmissionRecovery_Request{}
	// 与DS正式readAdmissionRecovery合同同义，校原件自身，而非只比较调用方提供的PB。
	if len(r.RequestId) != 32 || len(r.RequestProto) > 4096 || proto.Unmarshal(r.RequestProto, request) != nil || len(r.RequestDigest) != 32 || len(r.SqlProofDigest) != 32 ||
		!bytes.Equal(request.RequestId, r.RequestId) || request.GetSeal().GetListingId() != r.ListingId || request.GetSeal().GetPlayerId() != r.PlayerId || request.GetSeal().GetSceneInstanceId() != r.SceneInstanceId || request.GetSeal().GetThroughGeneration() != r.ThroughGeneration || request.GetSeal().GetOwnerRevision() != r.OwnerRevision ||
		!bytes.Equal(request.MaintenanceReceiptId, r.MaintenanceReceiptId) || !bytes.Equal(request.MaintenanceRequestDigest, r.MaintenanceRequestDigest) || !accountdirectory.ValidSaveFloor(r.RequiredOwnerEpoch, r.RequiredSaveSeq) || r.RequiredOwnerEpoch == 0 {
		return nil, errors.New("永久退休请求/SQL列/围栏或摘要长度不自洽")
	}
	h := sha256.New()
	writePromotionPart(h, []byte("account-role-admission-recovery-v1"))
	writePromotionPart(h, r.RequestProto)
	if !bytes.Equal(h.Sum(nil), r.RequestDigest) {
		return nil, errors.New("退休请求域摘要损坏")
	}
	return r, nil
}

// MaintenanceReleasePermit由稳定运维原件构造。权限来自显式维护执行与受限凭据，
// SHA只固定原件完整性；本模块不认证操作人、不解释验收PE或操作系统进程格式。
type MaintenanceReleasePermit struct {
	review       ReleaseFile
	input        ReleaseFile
	members      MaintenanceReleaseInput
	planSHA      string
	runID        string
	targetID     string
	sqlAddr      string
	routeSchema  string
	deadline     time.Time
	expiresMS    int64
	redisTargets []maintenanceReleaseReviewedRedisTarget
}
type MaintenanceReleaseInput struct {
	Format        string                     `json:"format"`
	Zone          uint32                     `json:"zone"`
	MaintenanceID string                     `json:"maintenance_id"`
	Members       []MaintenanceReleaseMember `json:"members"`
}
type maintenanceReleaseFact struct {
	Role   string      `json:"role"`
	Actual ReleaseFile `json:"actual"`
}
type maintenanceReleaseReview struct {
	Format          string                                  `json:"schema"`
	RunID           string                                  `json:"operation_id"`
	TargetID        string                                  `json:"target_nonce"`
	SQLAddr         string                                  `json:"physical_sql_addr"`
	RouteSchema     string                                  `json:"route_schema"`
	MembersInput    ReleaseFile                             `json:"members_input"`
	IssuerInventory ReleaseFile                             `json:"issuer_inventory"`
	StableFacts     []maintenanceReleaseFact                `json:"stable_facts"`
	KafkaInventory  ReleaseFile                             `json:"kafka_inventory"`
	RedisTargets    []maintenanceReleaseReviewedRedisTarget `json:"redis_targets"`
}
type maintenanceReleaseAttempt struct {
	Format     string                   `json:"schema"`
	Review     ReleaseFile              `json:"review"`
	RunID      string                   `json:"operation_id"`
	PlanSHA256 string                   `json:"plan_sha256"`
	Window     ReleaseFile              `json:"window"`
	Facts      []maintenanceReleaseFact `json:"facts"`
}
type maintenanceReleaseIssuer struct {
	ID       string `json:"issuer_id"`
	Role     string `json:"role"`
	Instance string `json:"instance_id"`
}
type maintenanceReleaseIssuerInventory struct {
	Format      string                     `json:"schema"`
	RunID       string                     `json:"operation_id"`
	TargetID    string                     `json:"target_nonce"`
	Zone        uint32                     `json:"zone"`
	RouteSchema string                     `json:"route_schema"`
	Issuers     []maintenanceReleaseIssuer `json:"issuers"`
}
type maintenanceReleaseRetirement struct {
	Format              string                   `json:"schema"`
	RunID               string                   `json:"operation_id"`
	Issuer              maintenanceReleaseIssuer `json:"issuer"`
	PublicationClosedMS int64                    `json:"publication_closed_ms"`
	StoppedMS           int64                    `json:"stopped_ms"`
	RevivalFenceID      string                   `json:"revival_fence_id"`
	Evidence            []ReleaseFile            `json:"source_evidence"`
}
type maintenanceReleaseIssuerWindow struct {
	Issuer                maintenanceReleaseIssuer `json:"issuer"`
	Retirement            ReleaseFile              `json:"retirement"`
	ObservedMS            int64                    `json:"observed_ms"`
	RevivalBlockedUntilMS int64                    `json:"revival_blocked_until_ms"`
	RevivalFenceID        string                   `json:"revival_fence_id"`
	Evidence              []ReleaseFile            `json:"source_evidence"`
}
type maintenanceReleaseWindow struct {
	Format     string                           `json:"schema"`
	RunID      string                           `json:"operation_id"`
	PlanSHA256 string                           `json:"plan_sha256"`
	Review     ReleaseFile                      `json:"review"`
	IssuedMS   int64                            `json:"issued_ms"`
	ExpiresMS  int64                            `json:"expires_ms"`
	Issuers    []maintenanceReleaseIssuerWindow `json:"issuers"`
}

func ReadMaintenanceReleaseMembers(pin ReleaseFile) (MaintenanceReleaseInput, error) {
	var result MaintenanceReleaseInput
	err := readReleaseJSON(pin, &result)
	if err != nil || result.Format != "maintenance-release-member-input-v1" || result.Zone == 0 || result.MaintenanceID == "" || len(result.Members) == 0 || len(result.Members) > 1000 {
		return result, errors.New("解除成员输入缺完整审定原件")
	}
	return result, nil
}
func maintenanceLabel(s string) bool {
	return s != "" && len(s) <= 256 && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.:/@-") == ""
}
func releaseSourceEvidence(pins []ReleaseFile) error {
	if len(pins) == 0 || len(pins) > 128 {
		return errors.New("缺实际采集源原件或超预算")
	}
	seen := map[ReleaseFile]bool{}
	for _, p := range pins {
		if seen[p] {
			return errors.New("采集源原件重复")
		}
		seen[p] = true
		raw, err := readReleaseBound(p, 16<<20)
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			return errors.New("采集源原件为空")
		}
	}
	return nil
}
func releaseIssuerSet(review maintenanceReleaseReview, input MaintenanceReleaseInput) (map[string]maintenanceReleaseIssuer, error) {
	var inv maintenanceReleaseIssuerInventory
	if readReleaseJSON(review.IssuerInventory, &inv) != nil || inv.Format != "maintenance-release-issuer-inventory-v1" || inv.RunID != review.RunID || inv.TargetID != review.TargetID || inv.Zone != input.Zone || inv.RouteSchema != review.RouteSchema || len(inv.Issuers) == 0 || len(inv.Issuers) > 1024 {
		return nil, errors.New("缺原审定发行方完整目录")
	}
	set := map[string]maintenanceReleaseIssuer{}
	for _, i := range inv.Issuers {
		if !maintenanceLabel(i.ID) || !maintenanceLabel(i.Role) || !maintenanceLabel(i.Instance) || set[i.ID].ID != "" {
			return nil, errors.New("发行方原身份重复或非法")
		}
		set[i.ID] = i
	}
	return set, nil
}
func readReleaseRetirements(f maintenanceReleaseFact, review maintenanceReleaseReview, expected map[string]maintenanceReleaseIssuer, w maintenanceReleaseWindow) (map[string]ReleaseFile, error) {
	var actual struct {
		Format      string        `json:"schema"`
		RunID       string        `json:"operation_id"`
		Inventory   ReleaseFile   `json:"issuer_inventory"`
		Retirements []ReleaseFile `json:"retirements"`
	}
	if readReleaseJSON(f.Actual, &actual) != nil || actual.Format != "maintenance-release-issuer-retirements-v1" || actual.RunID != review.RunID || actual.Inventory != review.IssuerInventory || len(actual.Retirements) != len(expected) {
		return nil, errors.New("发行停止事实与原全集不等")
	}
	set := map[string]ReleaseFile{}
	for _, pin := range actual.Retirements {
		var r maintenanceReleaseRetirement
		if readReleaseJSON(pin, &r) != nil || r.Format != "maintenance-release-issuer-retirement-v1" || r.RunID != review.RunID || expected[r.Issuer.ID].ID == "" || expected[r.Issuer.ID] != r.Issuer || set[r.Issuer.ID].Path != "" || r.PublicationClosedMS <= 0 || r.StoppedMS < r.PublicationClosedMS || r.StoppedMS > w.IssuedMS || !maintenanceLabel(r.RevivalFenceID) {
			return nil, errors.New("原发行方停止/关闭发布事实不完整或换身份")
		}
		if err := releaseSourceEvidence(r.Evidence); err != nil {
			return nil, err
		}
		set[r.Issuer.ID] = pin
	}
	if len(w.Issuers) != len(set) {
		return nil, errors.New("新维护窗缺原发行方等集")
	}
	seen := map[string]bool{}
	for _, i := range w.Issuers {
		if expected[i.Issuer.ID].ID == "" || expected[i.Issuer.ID] != i.Issuer || seen[i.Issuer.ID] || i.Retirement != set[i.Issuer.ID] || i.ObservedMS < w.IssuedMS || i.ObservedMS > w.ExpiresMS || i.RevivalBlockedUntilMS < w.ExpiresMS {
			return nil, errors.New("新窗换发行方/原停止原件或未覆盖禁止复活区间")
		}
		seen[i.Issuer.ID] = true
		var r maintenanceReleaseRetirement
		if readReleaseJSON(i.Retirement, &r) != nil || r.RevivalFenceID != i.RevivalFenceID {
			return nil, errors.New("新窗未沿原撤销范围")
		}
		if err := releaseSourceEvidence(i.Evidence); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// 这里只核稳定审核原件，不把本地文件视作权限，也不宣称代采跨主机事实。
// 实际停止/撤凭据采集由有权运维负责；私有验收先核原句柄/创建全集再转换。
func ReadMaintenanceReleasePermit(ctx context.Context, reviewPin, attemptPin ReleaseFile, expectedPlanSHA string) (*MaintenanceReleasePermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if expectedPlanSHA != "" && !releaseHex(expectedPlanSHA, 64) {
		return nil, errors.New("解除计划摘要非法")
	}
	var review maintenanceReleaseReview
	var attempt maintenanceReleaseAttempt
	if readReleaseJSON(reviewPin, &review) != nil || readReleaseJSON(attemptPin, &attempt) != nil || review.Format != "maintenance-release-operator-review-v1" || attempt.Format != "maintenance-release-attempt-v1" || !maintenanceLabel(review.RunID) || attempt.RunID != review.RunID || attempt.Review != reviewPin || attempt.PlanSHA256 != expectedPlanSHA || !promotionTargetID(review.TargetID) || !maintenanceAddress(review.SQLAddr) || !validSchema(review.RouteSchema) {
		return nil, errors.New("原运维审核/本次尝试/计划或物理目标不符")
	}
	input, err := ReadMaintenanceReleaseMembers(review.MembersInput)
	if err != nil {
		return nil, err
	}
	issuers, err := releaseIssuerSet(review, input)
	if err != nil {
		return nil, err
	}
	var window maintenanceReleaseWindow
	if readReleaseJSON(attempt.Window, &window) != nil || window.Format != "maintenance-release-window-v1" || window.RunID != review.RunID || window.Review != reviewPin || window.PlanSHA256 != expectedPlanSHA || window.IssuedMS <= 0 || window.ExpiresMS <= window.IssuedMS || window.ExpiresMS-window.IssuedMS > 60000 {
		return nil, errors.New("缺原计划的新短维护窗")
	}
	now := time.Now()
	if window.IssuedMS > now.UnixMilli() || window.ExpiresMS <= now.UnixMilli() {
		return nil, errors.New("维护窗未开始或已过期")
	}
	stable := map[string]ReleaseFile{}
	for _, f := range review.StableFacts {
		if stable[f.Role].Path != "" {
			return nil, errors.New("稳定事实重复")
		}
		stable[f.Role] = f.Actual
		if f.Role == "issuer-cutoff" {
			if _, err = readReleaseRetirements(f, review, issuers, window); err != nil {
				return nil, err
			}
		} else if !strings.HasPrefix(f.Role, "trade-cancel/") {
			return nil, errors.New("稳定事实包含未知角色或复用旧队列观察")
		}
	}
	if stable["issuer-cutoff"].Path == "" {
		return nil, errors.New("缺实际全发行方停止事实")
	}
	expectedStable := map[string]bool{"issuer-cutoff": true}
	for _, m := range input.Members {
		if len(m.PromotionProto) > 0 {
			role := "trade-cancel/" + strconv.FormatUint(m.PlayerID, 10)
			if expectedStable[role] || stable[role] != m.CancelAction {
				return nil, errors.New("恢复成员缺原正式Cancel或同COUNT换成员")
			}
			expectedStable[role] = true
			if _, err = readMaintenanceReleaseCancel(m.CancelAction); err != nil {
				return nil, err
			}
		}
	}
	if len(stable) != len(expectedStable) {
		return nil, errors.New("稳定事实与原成员全集不等")
	}
	seen := map[string]bool{}
	for _, f := range attempt.Facts {
		if seen[f.Role] || f.Role != "redis-queues" && f.Role != "kafka-queues" {
			return nil, errors.New("本次队列观察重复或未知")
		}
		seen[f.Role] = true
		if err = verifyMaintenanceReleaseFreshFact(f, review.RunID, window); err != nil {
			return nil, err
		}
		if f.Role == "redis-queues" {
			err = verifyMaintenanceReleaseRedisClosure(review, f)
		} else {
			err = verifyMaintenanceReleaseKafkaClosure(review, f)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(seen) != 2 {
		return nil, errors.New("每次新窗必须两类完整队列原始观察")
	}
	return &MaintenanceReleasePermit{review: reviewPin, input: review.MembersInput, members: input, planSHA: expectedPlanSHA, runID: review.RunID, targetID: review.TargetID, sqlAddr: review.SQLAddr, routeSchema: review.RouteSchema, deadline: now.Add(time.Duration(window.ExpiresMS-now.UnixMilli()) * time.Millisecond), expiresMS: window.ExpiresMS, redisTargets: review.RedisTargets}, nil
}

func verifyMaintenanceReleaseFreshFact(f maintenanceReleaseFact, operation string, w maintenanceReleaseWindow) error {
	v, err := readReleaseObject(f.Actual)
	if err != nil {
		return err
	}
	want := "maintenance-release-kafka-queues-v1"
	keys := []string{"schema", "operation_id", "observed_start_ms", "observed_end_ms", "source_evidence", "inventory", "topic_names", "group_names", "partition_rows"}
	if f.Role == "redis-queues" {
		want = "maintenance-release-redis-queues-v1"
		keys = []string{"schema", "operation_id", "observed_start_ms", "observed_end_ms", "source_evidence", "definition", "targets"}
	}
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range v {
		if !allowed[k] {
			return errors.New("队列原件包含未知或私有验收字段")
		}
	}
	var start, end int64
	var evidence []ReleaseFile
	if releaseString(v, "schema") != want || releaseString(v, "operation_id") != operation || json.Unmarshal(v["observed_start_ms"], &start) != nil || json.Unmarshal(v["observed_end_ms"], &end) != nil || start < w.IssuedMS || end < start || end > w.ExpiresMS || json.Unmarshal(v["source_evidence"], &evidence) != nil {
		return errors.New("队列实际观察不在本次维护窗")
	}
	return releaseSourceEvidence(evidence)
}

func (p *MaintenanceReleasePermit) check(plan MaintenanceReleasePlan, sha string) error {
	if p == nil || p.deadline.IsZero() || time.Until(p.deadline) <= 0 || p.planSHA != sha || p.review != plan.Review || p.members.Zone != plan.Zone || p.members.MaintenanceID != plan.MaintenanceID || p.targetID != plan.LocalTarget || p.sqlAddr != plan.SQLAddr || p.routeSchema != plan.RouteSchema {
		return errors.New("解除短窗/原计划/审定维护已失效")
	}
	a, _ := json.Marshal(p.members.Members)
	b, _ := json.Marshal(plan.Members)
	if !bytes.Equal(a, b) {
		return errors.New("解除计划成员不是原审定完整输入")
	}
	if len(p.redisTargets) != len(plan.Targets) {
		return errors.New("计划物理target不是审定全集")
	}
	seen := map[string]bool{}
	for _, target := range plan.Targets {
		found := false
		for _, expected := range p.redisTargets {
			if expected.Addr == target.Addr && expected.DB == target.DB && expected.InstanceNonce == target.InstanceNonce {
				for _, zone := range expected.Zones {
					if zone == plan.Zone {
						found = true
					}
				}
			}
		}
		key := fmt.Sprintf("%s/%d", target.Addr, target.DB)
		if !found || seen[key] {
			return errors.New("计划addr/DB/nonce/zone偏离原审定")
		}
		seen[key] = true
	}
	return nil
}
func readReleaseBound(pin ReleaseFile, max int64) ([]byte, error) {
	if !filepath.IsAbs(pin.Path) || !releaseHex(pin.SHA256, 64) {
		return nil, errors.New("原件必须绝对路径和小写完整SHA")
	}
	clean := filepath.Clean(pin.Path)
	for path := clean; ; path = filepath.Dir(path) {
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("原件或祖先缺失/软链，拒绝越界替身")
		}
		if path == filepath.Dir(path) {
			break
		}
	}
	f, err := os.Open(clean)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, errors.New("原件不是预算内普通文件")
	}
	raw, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, errors.New("原件读取超预算")
	}
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != pin.SHA256 {
		return nil, errors.New("原件SHA不符")
	}
	return raw, nil
}
func readReleaseJSON(pin ReleaseFile, out any) error {
	raw, err := readReleaseBound(pin, 64<<20)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("JSON尾部有第二正文")
	}
	return nil
}
func readReleaseObject(pin ReleaseFile) (map[string]json.RawMessage, error) {
	raw, err := readReleaseBound(pin, 64<<20)
	if err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, errors.New("实际原件不是JSON对象")
	}
	return result, nil
}
func releaseString(v map[string]json.RawMessage, key string) string {
	var out string
	_ = json.Unmarshal(v[key], &out)
	return out
}
func readMaintenanceReleaseCancel(pin ReleaseFile) (*dspb.CancelAccountRoleTradeRequest, error) {
	v, err := readReleaseObject(pin)
	if err != nil {
		return nil, err
	}
	if releaseString(v, "schema") != "maintenance-release-trade-cancel-v1" || releaseString(v, "action") != "cancel" {
		return nil, errors.New("不是正式原Cancel action")
	}
	var requestRaw []byte
	if json.Unmarshal(v["unsigned_request_proto"], &requestRaw) != nil {
		return nil, errors.New("缺原unsigned请求")
	}
	sum := sha256.Sum256(requestRaw)
	if hex.EncodeToString(sum[:]) != releaseString(v, "unsigned_request_sha256") {
		return nil, errors.New("原Cancel请求摘要不符")
	}
	in := &dspb.CancelAccountRoleTradeRequest{}
	if proto.Unmarshal(requestRaw, in) != nil || in.Auth != nil || len(in.ProtoReflect().GetUnknown()) != 0 || in.PlayerId == 0 || in.ListingId == 0 || in.ExpectedOwnerRevision == 0 || !accountdirectory.ValidAccount(in.SellerAccount) {
		return nil, errors.New("原Cancel请求不是原tuple/有Auth泄漏")
	}
	var originalPin, replyPin, statePin, observationPin ReleaseFile
	for key, out := range map[string]*ReleaseFile{"original_begin_receipt": &originalPin, "reply": &replyPin, "post_state": &statePin, "original_observation": &observationPin} {
		if json.Unmarshal(v[key], out) != nil {
			return nil, errors.New("Cancel缺完整下游pins")
		}
	}
	begin := &dspb.AccountRoleTradeReceipt{}
	reply := &dspb.AccountRoleTradeReceipt{}
	for _, item := range []struct {
		pin ReleaseFile
		msg proto.Message
	}{{originalPin, begin}, {replyPin, reply}} {
		raw, e := readReleaseBound(item.pin, 1<<20)
		if e != nil || (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, item.msg) != nil {
			return nil, errors.New("原正式Begin/Cancel回包损坏")
		}
	}
	if begin.PlayerId != in.PlayerId || begin.ListingId != in.ListingId || begin.SellerAccount != in.SellerAccount || begin.SellerOwnerRevision != in.ExpectedOwnerRevision || reply.PlayerId != begin.PlayerId || reply.ListingId != begin.ListingId || reply.SellerAccount != begin.SellerAccount || reply.SellerOwnerRevision != begin.SellerOwnerRevision || reply.State != dspb.AccountRoleTradeState_ACCOUNT_ROLE_TRADE_STATE_CANCELLED || len(begin.Holders) == 0 || len(reply.Holders) != len(begin.Holders) {
		return nil, errors.New("正式Cancel换主体/原holder全集或非终态")
	}
	holders := map[string]*base.RoleTradeSeal{}
	for _, h := range begin.Holders {
		if h == nil || h.Seal == nil || holders[h.Seal.SceneInstanceId] != nil {
			return nil, errors.New("Begin holder损坏/重复")
		}
		holders[h.Seal.SceneInstanceId] = h.Seal
	}
	for _, h := range reply.Holders {
		if h == nil || h.Seal == nil || !h.Drained || !proto.Equal(holders[h.Seal.SceneInstanceId], h.Seal) {
			return nil, errors.New("Cancel未排空原holder/更换上界")
		}
		delete(holders, h.Seal.SceneInstanceId)
	}
	if len(holders) != 0 {
		return nil, errors.New("Cancel遗漏原holder")
	}
	// producer已对原全列业务/归属做真实只读对照；本core随后再从当前SQL独立复核终态/全列。
	if _, err = readReleaseObject(statePin); err != nil {
		return nil, err
	}
	if _, err = readReleaseObject(observationPin); err != nil {
		return nil, err
	}
	return in, nil
}

type maintenanceReleaseReviewedRedisTarget struct {
	Role          string   `json:"role"`
	Addr          string   `json:"addr"`
	DB            int      `json:"db"`
	InstanceNonce string   `json:"instance_nonce"`
	Zones         []uint32 `json:"zones"`
}
type maintenanceReleaseKafkaInventory struct {
	Schema string `json:"schema"`
	Topics []struct {
		Name       string `json:"name"`
		Partitions int    `json:"partitions"`
	} `json:"topics"`
	Subscriptions []struct {
		Group string `json:"group"`
		Topic string `json:"topic"`
	} `json:"subscriptions"`
	Internal []string `json:"internal_topics"`
}

func releaseInventory(pin ReleaseFile) (maintenanceReleaseKafkaInventory, error) {
	var inv maintenanceReleaseKafkaInventory
	raw, err := readReleaseBound(pin, 1<<20)
	if err != nil || json.Unmarshal(raw, &inv) != nil || inv.Schema != "maintenance-release-kafka-inventory-v1" || len(inv.Topics) == 0 || len(inv.Topics) > 128 || len(inv.Subscriptions) == 0 {
		return inv, errors.New("缺审定Kafka真实历史全集")
	}
	return inv, nil
}
func topicsForReleaseInventory(inv maintenanceReleaseKafkaInventory) map[string]int {
	topics := map[string]int{}
	for _, t := range inv.Topics {
		topics[t.Name] = t.Partitions
	}
	return topics
}
func verifyMaintenanceReleaseKafkaClosure(review maintenanceReleaseReview, f maintenanceReleaseFact) error {
	inv, err := releaseInventory(review.KafkaInventory)
	if err != nil {
		return err
	}
	v, err := readReleaseObject(f.Actual)
	if err != nil {
		return err
	}
	var pin ReleaseFile
	if json.Unmarshal(v["inventory"], &pin) != nil || pin != review.KafkaInventory {
		return errors.New("Kafka actual未用原审定inventory")
	}
	var names, groups []string
	if json.Unmarshal(v["topic_names"], &names) != nil || json.Unmarshal(v["group_names"], &groups) != nil {
		return errors.New("Kafka实际catalogue缺失")
	}
	topics := map[string]int{}
	expectedTopics := []string{}
	partitions := 0
	for _, t := range inv.Topics {
		if t.Name == "" || t.Partitions < 1 || t.Partitions > 4096 || topics[t.Name] != 0 {
			return errors.New("审定Kafka topic/partition重复或非法")
		}
		topics[t.Name] = t.Partitions
		expectedTopics = append(expectedTopics, t.Name)
		partitions += t.Partitions
	}
	if partitions > 8192 {
		return errors.New("Kafka分区预算超限")
	}
	for _, name := range inv.Internal {
		if name != "__consumer_offsets" {
			return errors.New("不能将未知业务topic伪装internal")
		}
		expectedTopics = append(expectedTopics, name)
	}
	if !releaseExactStrings(names, expectedTopics) {
		return errors.New("Kafka实际topic不是原审定全历史全集")
	}
	expectedRows := map[string]bool{}
	groupSet := map[string]bool{}
	for _, s := range inv.Subscriptions {
		n := topics[s.Topic]
		if s.Group == "" || n == 0 {
			return errors.New("审定订阅非法")
		}
		groupSet[s.Group] = true
		for p := 0; p < n; p++ {
			key := fmt.Sprintf("%s/%s/%d", s.Group, s.Topic, p)
			if expectedRows[key] {
				return errors.New("审定订阅重复")
			}
			expectedRows[key] = true
		}
	}
	if len(expectedRows) > 16384 {
		return errors.New("订阅分区预算超限")
	}
	expectedGroups := []string{}
	for g := range groupSet {
		expectedGroups = append(expectedGroups, g)
	}
	if !releaseExactStrings(groups, expectedGroups) {
		return errors.New("Kafka实际group全集不等")
	}
	var rows []struct {
		Group     string `json:"group"`
		Topic     string `json:"topic"`
		Partition int    `json:"partition"`
		Earliest  int64  `json:"earliest"`
		Committed int64  `json:"committed"`
		End       int64  `json:"end"`
		Lag       int64  `json:"lag"`
	}
	if json.Unmarshal(v["partition_rows"], &rows) != nil || len(rows) != len(expectedRows) {
		return errors.New("Kafka实际分区全集缺失")
	}
	for _, r := range rows {
		key := fmt.Sprintf("%s/%s/%d", r.Group, r.Topic, r.Partition)
		if !expectedRows[key] || r.Earliest < 0 || r.Committed < r.Earliest || r.Committed != r.End || r.Lag != 0 {
			return errors.New("Kafka分区重复/漏项/积压")
		}
		delete(expectedRows, key)
	}
	if len(expectedRows) != 0 {
		return errors.New("Kafka分区全集未消费")
	}
	return nil
}
func releaseExactStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, v := range b {
		if seen[v] {
			return false
		}
		seen[v] = true
	}
	for _, v := range a {
		if !seen[v] {
			return false
		}
		delete(seen, v)
	}
	return len(seen) == 0
}

type maintenanceReleaseRedisKeyObservation struct {
	Key         string   `json:"key"`
	KeySHA      string   `json:"key_sha256"`
	Kind        string   `json:"kind"`
	Topic       string   `json:"topic,omitempty"`
	Type        string   `json:"type"`
	DumpSHA     string   `json:"dump_sha256"`
	PTTL        int64    `json:"pttl_ms"`
	Count       int64    `json:"count"`
	OutboxTopic string   `json:"outbox_topic,omitempty"`
	Members     []string `json:"members"`
	Values      []string `json:"values"`
}

// 原key及原成员/字段值是机核事实；摘要仅防传输替换，不能代替内容和等集检查。
func releaseRedisObservation(o maintenanceReleaseRedisKeyObservation, topics map[string]int, zones []uint32) error {
	h := sha256.Sum256([]byte(o.Key))
	if o.Key == "" || hex.EncodeToString(h[:]) != o.KeySHA || o.Count < 0 || len(o.Members) > 16384 || len(o.Values) > 16384 {
		return errors.New("Redis原键/条目不规范")
	}
	wantType := ""
	known := false
	for _, p := range []struct{ prefix, kind, typ string }{{"kafka:retry:queue:", "retry", "list"}, {"kafka:dead:queue:", "dead", "list"}, {"kafka:retry:processing:", "processing", "list"}, {"kafka:retry:instances:", "instances", "zset"}, {"kafka:retry:instance_mode:", "modes", "hash"}} {
		if !strings.HasPrefix(o.Key, p.prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Key, p.prefix)
		parts := strings.SplitN(rest, ":", 2)
		if topics[parts[0]] == 0 || o.Topic != parts[0] || o.Kind != p.kind {
			return errors.New("Redis未知topic/键角色")
		}
		if len(parts) == 2 {
			if p.kind == "retry" {
				part := strings.TrimPrefix(parts[1], "p")
				n, e := strconv.Atoi(part)
				if e != nil || n < 0 || strconv.Itoa(n) != part || parts[1] != "p"+part || n >= topics[parts[0]] {
					return errors.New("Redis未知retry分区")
				}
			} else if p.kind != "processing" || parts[1] == "" || len(parts[1]) > 512 || strings.ContainsAny(parts[1], "\x00\r\n") {
				return errors.New("Redis未知队列后缀")
			}
		}
		wantType = p.typ
		known = true
		break
	}
	if o.Kind == "pending" && o.Topic == "" {
		for _, z := range zones {
			for b := 0; b < 64; b++ {
				if o.Key == fmt.Sprintf("player:save_pending:%d:%d", z, b) {
					known = true
					wantType = "zset"
				}
			}
		}
	}
	if o.Kind == "barrier" && o.Topic == "" {
		id := strings.TrimSuffix(strings.TrimPrefix(o.Key, "player:{"), "}:save_barrier")
		n, e := strconv.ParseUint(id, 10, 64)
		if e == nil && n > 0 && savebarrier.Key(n) == o.Key {
			known = true
			wantType = "hash"
		}
	}
	if !known || (o.Type != "none" && o.Type != wantType) || !releaseHex(o.DumpSHA, 64) {
		return errors.New("Redis未定义键或原类型错误")
	}
	if o.Type == "none" {
		empty := sha256.Sum256(nil)
		if o.PTTL != -2 || o.Count != 0 || len(o.Members) != 0 || len(o.Values) != 0 || o.DumpSHA != hex.EncodeToString(empty[:]) {
			return errors.New("缺键原事实冲突")
		}
		return nil
	}
	if o.PTTL != -1 || int64(len(o.Members)) != o.Count || (wantType == "hash" || wantType == "zset") && len(o.Values) != len(o.Members) {
		return errors.New("队列原条目数/永久性不符")
	}
	seen := map[string]string{}
	for i, m := range o.Members {
		if _, ok := seen[m]; ok {
			return errors.New("Redis原成员重复")
		}
		seen[m] = ""
		if len(o.Values) > i {
			seen[m] = o.Values[i]
		}
	}
	switch o.Kind {
	case "retry", "dead", "processing", "pending":
		if o.Count != 0 {
			return errors.New("Redis仍有待投原条目")
		}
	case "modes":
		for _, v := range seen {
			if v != "owner" && v != "shared" {
				return errors.New("未知retry实例模式")
			}
		}
	case "barrier":
		p, a, b := seen["pending"], seen["player_database"], seen["player_database_1"]
		_, pPresent := seen["pending"]
		_, aPresent := seen["player_database"]
		_, bPresent := seen["player_database_1"]
		if pPresent || aPresent || bPresent {
			e, q, err := savebarrier.Parse(p)
			if err != nil || e == 0 || q == 0 || p != a || p != b {
				return errors.New("原屏障双ACK未收敛")
			}
		}
		for f := range seen {
			if strings.HasPrefix(f, "outbox_") {
				return errors.New("原屏障存在待投")
			}
		}
	}
	return nil
}

func verifyMaintenanceReleaseRedisClosure(review maintenanceReleaseReview, f maintenanceReleaseFact) error {
	inv, err := releaseInventory(review.KafkaInventory)
	if err != nil {
		return err
	}
	v, err := readReleaseObject(f.Actual)
	if err != nil {
		return err
	}
	var defPin ReleaseFile
	if json.Unmarshal(v["definition"], &defPin) != nil {
		return errors.New("Redis实际观察缺原definition")
	}
	var def struct {
		Schema  string `json:"schema"`
		Targets []struct {
			Role  string   `json:"role"`
			Addr  string   `json:"addr"`
			DB    int      `json:"db"`
			RunID string   `json:"expected_run_id"`
			Zones []uint32 `json:"zones"`
		} `json:"targets"`
		Topics []struct {
			Name       string `json:"name"`
			Partitions int    `json:"partitions"`
		} `json:"topics"`
	}
	raw, err := readReleaseBound(defPin, 1<<20)
	if err != nil || json.Unmarshal(raw, &def) != nil || def.Schema != "maintenance-release-redis-inventory-v1" {
		return errors.New("Redis实际definition不是已知producer contract")
	}
	// definition的非策略字段由原producer强验；这里另核审定target/zone/topic全集，不能只看有若干观察。
	approved := map[string]maintenanceReleaseReviewedRedisTarget{}
	for _, target := range review.RedisTargets {
		key := fmt.Sprintf("%s/%d", target.Addr, target.DB)
		if target.Role != "core" && target.Role != "mapping" || !maintenanceAddress(target.Addr) || target.DB < 0 || approved[key].Role != "" || !promotionTargetID(target.InstanceNonce) || len(target.Zones) == 0 {
			return errors.New("审定Redis全集重复/非法")
		}
		seenZones := map[uint32]bool{}
		for _, zone := range target.Zones {
			if zone == 0 || zone > placement.MaxZoneStorageID || seenZones[zone] {
				return errors.New("审定Redis zone全集非法/重复")
			}
			seenZones[zone] = true
		}
		approved[key] = target
	}
	if len(approved) == 0 || len(def.Targets) != len(approved) {
		return errors.New("Redis实际target全集数不等")
	}
	expectedTopics := map[string]int{}
	for _, t := range inv.Topics {
		expectedTopics[t.Name] = t.Partitions
	}
	if len(def.Topics) != len(expectedTopics) {
		return errors.New("Redis topic全集与Kafka历史世代不等")
	}
	for _, t := range def.Topics {
		if expectedTopics[t.Name] != t.Partitions {
			return errors.New("Redis topic分区合同不符")
		}
		delete(expectedTopics, t.Name)
	}
	if len(expectedTopics) != 0 {
		return errors.New("Redis漏历史topic")
	}
	var actual []struct {
		Role           string                                  `json:"role"`
		Addr           string                                  `json:"addr"`
		DB             int                                     `json:"db"`
		RunID          string                                  `json:"run_id"`
		KeyCount       int                                     `json:"key_count"`
		KeyNames       []string                                `json:"key_names"`
		PendingBuckets int                                     `json:"pending_buckets"`
		Observations   []maintenanceReleaseRedisKeyObservation `json:"observations"`
		Digest         string                                  `json:"observations_sha256"`
		Blockers       []string                                `json:"blockers"`
	}
	if json.Unmarshal(v["targets"], &actual) != nil || len(actual) != len(approved) {
		return errors.New("Redis实际targets遗漏")
	}
	defs := map[string]int{}
	for i, t := range def.Targets {
		key := fmt.Sprintf("%s/%d", t.Addr, t.DB)
		a, ok := approved[key]
		if !ok || a.Role != t.Role || defs[key] != 0 || !releaseEqualZones(a.Zones, t.Zones) || len(t.RunID) != 40 || strings.Trim(t.RunID, "0123456789abcdef") != "" {
			return errors.New("Redis definition偏离原addr/DB/zone全集或epoch")
		}
		defs[key] = i + 1
	}
	for _, t := range actual {
		key := fmt.Sprintf("%s/%d", t.Addr, t.DB)
		index, ok := defs[key]
		if !ok {
			return errors.New("Redis实际target重复/未知")
		}
		definition := def.Targets[index-1]
		if t.Role != definition.Role || t.RunID != definition.RunID || t.PendingBuckets != 64*len(definition.Zones) || t.KeyCount != len(t.Observations) || len(t.Blockers) != 0 || len(t.Observations) > 16384 || len(t.KeyNames) != len(t.Observations) {
			return errors.New("Redis实际epoch/64桶/数量或阻断不符")
		}
		delete(defs, key)
		required := map[string]string{}
		add := func(key, kind string) { h := sha256.Sum256([]byte(key)); required[hex.EncodeToString(h[:])] = kind }
		for _, z := range definition.Zones {
			for b := 0; b < 64; b++ {
				add(fmt.Sprintf("player:save_pending:%d:%d", z, b), "pending")
			}
		}
		for _, topic := range inv.Topics {
			for _, pair := range []struct{ prefix, kind string }{{"kafka:retry:queue:", "retry"}, {"kafka:dead:queue:", "dead"}, {"kafka:retry:processing:", "processing"}, {"kafka:retry:instances:", "instances"}, {"kafka:retry:instance_mode:", "modes"}} {
				add(pair.prefix+topic.Name, pair.kind)
			}
			for p := 0; p < topic.Partitions; p++ {
				add(fmt.Sprintf("kafka:retry:queue:%s:p%d", topic.Name, p), "retry")
			}
		}
		seen := map[string]bool{}
		actualNames := []string{}
		stable := append([]maintenanceReleaseRedisKeyObservation{}, t.Observations...)
		for i, o := range t.Observations {
			if err := releaseRedisObservation(o, topicsForReleaseInventory(inv), definition.Zones); err != nil {
				return err
			}
			actualNames = append(actualNames, o.Key)
			if !releaseHex(o.KeySHA, 64) || seen[o.KeySHA] || !releaseHex(o.DumpSHA, 64) || o.Count < 0 || o.OutboxTopic != "" {
				return errors.New("Redis原key摘要重复/未知待投")
			}
			seen[o.KeySHA] = true
			if kind, known := required[o.KeySHA]; known {
				if kind != o.Kind {
					return errors.New("必读Redis key类型角色不符")
				}
				delete(required, o.KeySHA)
			}
			switch o.Kind {
			case "retry", "dead", "processing", "pending":
				if o.Count != 0 {
					return errors.New("Redis仍有待办")
				}
			case "barrier", "instances", "modes":
			default:
				return errors.New("Redis未知队列分类")
			}
			stable[i].PTTL = 0
		}
		if len(required) != 0 {
			return errors.New("Redis缺固定64桶/topic/processing必读全集")
		}
		if !releaseExactStrings(t.KeyNames, actualNames) {
			return errors.New("Redis原扫描目录与逐键原件非等集")
		}
		encoded, _ := json.Marshal(stable)
		sum := sha256.Sum256(encoded)
		if hex.EncodeToString(sum[:]) != t.Digest {
			return errors.New("Redis逐key观察摘要不符")
		}
	}
	if len(defs) != 0 {
		return errors.New("Redis实际target未闭合")
	}
	return nil
}
func releaseEqualZones(a, b []uint32) bool {
	aa := []string{}
	bb := []string{}
	for _, z := range a {
		aa = append(aa, strconv.FormatUint(uint64(z), 10))
	}
	for _, z := range b {
		bb = append(bb, strconv.FormatUint(uint64(z), 10))
	}
	return releaseExactStrings(aa, bb)
}

// ReadAuthorityProto读取v2正式typed原件；v1本地恢复协议保留，不以v1较窄观察代替全局排空。
func ReadAuthorityProto(pin ReleaseFile, out proto.Message) error {
	digest, err := hex.DecodeString(pin.SHA256)
	if err != nil || len(digest) != 32 {
		return errors.New("authority原件摘要非法")
	}
	raw, err := drainproof.ReadPin(&databasepb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: pin.Path, Sha256: digest}, 16<<20)
	if err != nil {
		return err
	}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, out); err != nil {
		return err
	}
	if len(out.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("authority原件包含未知字段")
	}
	return nil
}

// ValidateAuthorityPlan核完整typed ManifestProto，不能按当前配置topic或COUNT缩成子集。
func ValidateAuthorityPlan(plan *databasepb.SqlWriterMaintenanceReceipt_Plan) error {
	if plan == nil || len(plan.RunId) != 16 || len(plan.PhysicalClusterIdentity) != 32 || !validSchema(plan.AuthoritySchema) || len(plan.ProtectedSchemas) == 0 || plan.BeforeEpoch == 0 || plan.CatalogRevision == 0 || len(plan.ActiveWriterKeys) == 0 || len(plan.RootIssuers) == 0 || len(plan.RootIssuers) > 1024 || len(plan.QueueMutators) == 0 || len(plan.QueueMutators) > 1024 || len(plan.KafkaPartitions) == 0 || len(plan.KafkaPartitions) > 16384 || len(plan.RedisTargets) == 0 || len(plan.RedisTargets) > 128 || len(plan.SourcePins) == 0 || len(plan.SourcePins) > 128 || len(plan.Launchers) == 0 || len(plan.Launchers) > 1024 {
		return errors.New("authority完整目录缺失或超预算")
	}
	if len(plan.ProtoReflect().GetUnknown()) != 0 {
		return errors.New("authority manifest未知字段")
	}
	for i, key := range plan.ActiveWriterKeys {
		if len(key) != 16 || i > 0 && bytes.Compare(plan.ActiveWriterKeys[i-1], key) >= 0 {
			return errors.New("ACTIVE物理writer全集不规范")
		}
	}
	for i, schema := range plan.ProtectedSchemas {
		if !validSchema(schema) || i > 0 && plan.ProtectedSchemas[i-1] >= schema {
			return errors.New("受保护schema全集不规范")
		}
	}
	issuers := map[string]bool{}
	for _, roles := range [][]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{plan.RootIssuers, plan.QueueMutators, plan.MaintenanceIssuers} {
		for _, role := range roles {
			if role == nil || len(role.IssuerId) != 16 || len(role.InstanceId) != 16 || len(role.PermanentRevivalFenceId) != 16 || role.LogicalRole == "" || len(role.SourceEvidence) == 0 {
				return errors.New("逻辑角色seed身份缺失")
			}
			key := string(role.IssuerId)
			if issuers[key] {
				return errors.New("逻辑角色issuer重复")
			}
			issuers[key] = true
		}
	}
	if len(plan.MaintenanceIssuers) > 1024 {
		return errors.New("冻结维护工具目录超预算")
	}
	for _, tool := range plan.MaintenanceIssuers {
		if tool.LogicalRole != "MAINTENANCE" || len(tool.PhysicalWriterKey) != 0 || tool.AcceptingClosedMs != 0 || tool.AcceptedSettledMs != 0 || len(tool.AcceptedFrontierDigest) != 0 || tool.UnresolvedPublications != 0 {
			return errors.New("维护工具必须是P0固定单次boot，不得预填闭合或复用SQL写者")
		}
	}
	partitions := map[string]bool{}
	for _, row := range plan.KafkaPartitions {
		if row == nil || len(row.ClusterId) == 0 || len(row.TopicId) != 16 || row.TopicName == "" || row.Group == "" || row.InitialEarliest < 0 || row.ProtectedCommittedFloor < row.InitialEarliest {
			return errors.New("历史Kafka物理身份/保护floor缺失")
		}
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		if partitions[key] {
			return errors.New("历史Kafka订阅重复")
		}
		partitions[key] = true
	}
	targets := map[string]bool{}
	for _, target := range plan.RedisTargets {
		if target == nil || !maintenanceAddress(target.Address) || target.Db < 0 || len(target.ServerRunId) != 40 || strings.Trim(string(target.ServerRunId), "0123456789abcdef") != "" || bytes.Equal(target.ServerRunId, bytes.Repeat([]byte{'0'}, 40)) || len(target.MaintenanceNonce) != 16 || bytes.Equal(target.MaintenanceNonce, make([]byte, 16)) || len(target.Zones) == 0 {
			return errors.New("Redis目标原runid/nonce缺失")
		}
		key := fmt.Sprintf("%s/%d", target.Address, target.Db)
		if targets[key] {
			return errors.New("Redis目标重复")
		}
		targets[key] = true
		zones := map[uint32]bool{}
		for _, zone := range target.Zones {
			if zone == 0 || zone > placement.MaxZoneStorageID || zones[zone] {
				return errors.New("Redis目标zone非法或重复")
			}
			zones[zone] = true
		}
	}
	for _, pin := range plan.SourcePins {
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	launchers := map[string]bool{}
	for _, launcher := range plan.Launchers {
		if launcher == nil || len(launcher.LauncherId) != 16 || len(launcher.PermanentFenceId) != 16 || len(launcher.SourcePins) == 0 || launchers[string(launcher.LauncherId)] {
			return errors.New("启动者永久禁止复活目录缺失或重复")
		}
		launchers[string(launcher.LauncherId)] = true
		for _, pin := range launcher.SourcePins {
			if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
	}
	if err := verifyAuthorityRedisBaseline(plan); err != nil {
		return err
	}
	_, err := authorityKafkaBaseline(plan)
	return err
}
func authoritySameRole(a, b *databasepb.SqlWriterMaintenanceReceipt_IssuerClosure) bool {
	return a != nil && b != nil && bytes.Equal(a.IssuerId, b.IssuerId) && bytes.Equal(a.InstanceId, b.InstanceId) && bytes.Equal(a.PhysicalWriterKey, b.PhysicalWriterKey) && a.LogicalRole == b.LogicalRole && bytes.Equal(a.PermanentRevivalFenceId, b.PermanentRevivalFenceId)
}
func verifyAuthorityRoles(expected, actual []*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure) error {
	if len(actual) != len(expected) || len(actual) == 0 || len(actual) > 1024 {
		return errors.New("逻辑角色闭合不是审定全集")
	}
	set := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, seed := range expected {
		set[string(seed.IssuerId)] = seed
	}
	for _, role := range actual {
		if role == nil || !authoritySameRole(set[string(role.IssuerId)], role) {
			return errors.New("逻辑角色身份/禁止复活范围变化")
		}
		if err := drainproof.VerifyClosure(role); err != nil {
			return err
		}
		seed := set[string(role.IssuerId)]
		if !authorityPinSetEqual(seed.SourceEvidence, role.SourceEvidence) {
			return errors.New("逻辑角色换P0构建/监督源pins")
		}
		delete(set, string(role.IssuerId))
	}
	if len(set) != 0 {
		return errors.New("逻辑角色闭合遗漏")
	}
	return nil
}

// VerifyAuthorityEvidence是v2唯一portable事实owner。调用方必须把按原ID强读的SQL阶段收据传入；
// 文件/Redis/Kafka采集在控制事务外完成，本函数不把未知提交重放成新动作。
func VerifyAuthorityEvidence(ctx context.Context, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, manifest []byte, fact *databasepb.SqlWriterMaintenanceReceipt, rootReceipt, relayReceipt *databasepb.SqlWriterMaintenanceReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateAuthorityPlan(plan); err != nil {
		return err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil || !bytes.Equal(encoded, manifest) {
		return errors.New("manifest不是原完整确定性proto")
	}
	sum := sha256.Sum256(manifest)
	if fact == nil || fact.Evidence == nil || !bytes.Equal(fact.RunId, plan.RunId) || !bytes.Equal(fact.Evidence.OperationId, plan.RunId) || !bytes.Equal(fact.Evidence.ManifestDigest, sum[:]) || fact.BeforeEpoch != plan.BeforeEpoch || fact.CatalogRevision < plan.CatalogRevision || fact.CatalogRevision-plan.CatalogRevision > uint64(len(plan.ActiveWriterKeys)) {
		return errors.New("阶段原件偏离run/manifest/epoch")
	}
	if publication := fact.Evidence.GetPublication(); publication != nil {
		expected := plan.RootIssuers
		if fact.Kind == databasepb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED {
			expected = plan.QueueMutators
		} else if fact.Kind != databasepb.SqlWriterMaintenanceReceipt_ROLE_CLOSED {
			return errors.New("角色事实kind不符")
		}
		if err := verifyAuthorityPublicationOriginals(expected, publication); err != nil {
			return err
		}
		return verifyAuthorityRoles(expected, publication.LogicalRoles)
	}
	closure := fact.Evidence.GetNonSqlClosure()
	if closure == nil {
		return errors.New("当前阶段缺正式nonSQL闭包")
	}
	if rootReceipt == nil || relayReceipt == nil || rootReceipt.Kind != databasepb.SqlWriterMaintenanceReceipt_ROLE_CLOSED || rootReceipt.RunPhase != databasepb.SqlWriterDrainRun_ROOTS_CLOSED || relayReceipt.Kind != databasepb.SqlWriterMaintenanceReceipt_RELAY_QUIESCED || relayReceipt.RunPhase != databasepb.SqlWriterDrainRun_DRAINING || !bytes.Equal(closure.RootClosedReceiptId, rootReceipt.RequestId) || !bytes.Equal(closure.RootClosedReceiptDigest, rootReceipt.ReceiptDigest) || !bytes.Equal(closure.RelayQuiescedReceiptId, relayReceipt.RequestId) || !bytes.Equal(closure.RelayQuiescedReceiptDigest, relayReceipt.ReceiptDigest) || rootReceipt.AttemptRevision == 0 || rootReceipt.AttemptRevision > fact.AttemptRevision || relayReceipt.AttemptRevision == 0 || relayReceipt.AttemptRevision > fact.AttemptRevision {
		return errors.New("nonSQL未绑定同run已永久封闭的原root/relay SQL收据")
	}
	for _, receipt := range []*databasepb.SqlWriterMaintenanceReceipt{rootReceipt, relayReceipt} {
		if receipt.Evidence == nil || !bytes.Equal(receipt.RunId, plan.RunId) || !bytes.Equal(receipt.Evidence.ManifestDigest, sum[:]) || receipt.Generation != fact.Generation || !bytes.Equal(receipt.PlanProto, manifest) || receipt.BeforeEpoch != plan.BeforeEpoch || receipt.AfterEpoch != plan.BeforeEpoch {
			return errors.New("root/relay原收据换manifest")
		}
	}
	if err := verifyAuthorityPublicationOriginals(plan.RootIssuers, rootReceipt.Evidence.GetPublication()); err != nil {
		return err
	}
	if err := verifyAuthorityRoles(plan.RootIssuers, rootReceipt.Evidence.GetPublication().GetLogicalRoles()); err != nil {
		return err
	}
	if err := verifyAuthorityPublicationOriginals(plan.QueueMutators, relayReceipt.Evidence.GetPublication()); err != nil {
		return err
	}
	if err := verifyAuthorityRoles(plan.QueueMutators, relayReceipt.Evidence.GetPublication().GetLogicalRoles()); err != nil {
		return err
	}
	if err := verifyAuthorityRoles(plan.QueueMutators, closure.QueueMutators); err != nil {
		return err
	}
	if closure.ObservedStartMs == 0 || closure.ObservedEndMs < closure.ObservedStartMs || closure.ObservedEndMs-closure.ObservedStartMs > 60000 || closure.ObservedEndMs > uint64(time.Now().UnixMilli()) {
		return errors.New("nonSQL观察不在60秒新窗口")
	}
	for _, role := range append(append([]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}, rootReceipt.Evidence.GetPublication().LogicalRoles...), closure.QueueMutators...) {
		if role.AcceptedSettledMs > closure.ObservedStartMs {
			return errors.New("队列观察早于发行者实际settled")
		}
	}
	if len(closure.SourceEvidence) == 0 || len(closure.SourceEvidence) > 128 {
		return errors.New("nonSQL缺原始采集源")
	}
	for _, pin := range closure.SourceEvidence {
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	if err := verifyAuthorityKafka(plan, closure); err != nil {
		return err
	}
	return verifyAuthorityRedis(plan, closure)
}

type authorityManualLeader struct {
	Schema             string            `json:"schema"`
	ObservedStartMS    uint64            `json:"observed_start_ms"`
	ObservedEndMS      uint64            `json:"observed_end_ms"`
	CatalogFirstSHA256 []byte            `json:"catalog_first_sha256"`
	CatalogLastSHA256  []byte            `json:"catalog_last_sha256"`
	Consumers          []json.RawMessage `json:"consumers"`
}

func verifyManualClosure(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, actual *databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) error {
	if len(plan.ManualAssignments) == 0 {
		if len(actual.ManualConsumers) != 0 {
			return errors.New("P4出现P0未列manual owner")
		}
		return nil
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	sources := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, row := range plan.ManualAssignments {
		expected[manualAssignmentKey(row)] = row
	}
	for _, source := range plan.RootIssuers {
		sources[string(source.IssuerId)] = source
	}
	manifest, err := (proto.MarshalOptions{Deterministic: true}).Marshal(plan)
	if err != nil {
		return err
	}
	manifestDigest := sha256.Sum256(manifest)
	var leader *authorityManualLeader
	var leaderPin *databasepb.SqlWriterMaintenanceReceipt_SourcePin
	catalogs := map[string]bool{}
	for _, pin := range actual.SourceEvidence {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) != nil {
			continue
		}
		if marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" {
			sum := sha256.Sum256(raw)
			catalogs[string(sum[:])] = true
		}
		if marker.Schema != "SQL_DRAIN_MANUAL_KAFKA_LEADER_V1" {
			continue
		}
		if leader != nil {
			return errors.New("manual P4 leader原件重复")
		}
		leader = &authorityManualLeader{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(leader) != nil {
			return errors.New("manual P4 leader原件格式无效")
		}
		leaderPin = pin
	}
	if leader == nil || leader.ObservedStartMS < actual.ObservedStartMs || leader.ObservedEndMS < leader.ObservedStartMS || leader.ObservedEndMS > actual.ObservedEndMs || leader.ObservedEndMS-leader.ObservedStartMS > 60000 || len(leader.Consumers) == 0 || len(leader.Consumers) != len(actual.ManualConsumers) || !catalogs[string(leader.CatalogFirstSHA256)] || !catalogs[string(leader.CatalogLastSHA256)] {
		return errors.New("manual P4缺真实采样窗口或对应首尾物理目录")
	}
	observed := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{}
	for _, entry := range actual.ManualConsumers {
		if entry == nil || len(entry.Assignments) == 0 || len(entry.SourcePins) != 2 {
			return errors.New("manual P4条目缺P3/leader原件")
		}
		key := string(entry.Assignments[0].IssuerId) + string(entry.Assignments[0].InstanceId)
		if observed[key] != nil {
			return errors.New("manual P4 boot重复")
		}
		observed[key] = entry
	}
	seen := map[string]bool{}
	seenP3 := map[string]bool{}
	for _, raw := range leader.Consumers {
		sampled, err := parseManualEvidence(raw)
		if err != nil {
			return err
		}
		if len(sampled.SourcePins) != 1 || !bytes.Equal(sampled.OperationId, plan.RunId) || !bytes.Equal(sampled.ManifestDigest, manifestDigest[:]) || !sampled.AcceptingClosed || !sampled.CallbacksJoined || sampled.Accepted != sampled.Settled || sampled.UnresolvedPublications != 0 {
			return errors.New("manual P4不是同run已关闭的零UNKNOWN原件")
		}
		key := string(sampled.Assignments[0].IssuerId) + string(sampled.Assignments[0].InstanceId)
		published := observed[key]
		if published == nil {
			return errors.New("manual P4发布遗漏采样boot")
		}
		comparison := proto.Clone(published).(*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence)
		withoutLeader := []*databasepb.SqlWriterMaintenanceReceipt_SourcePin{}
		leaderCount := 0
		for _, pin := range comparison.SourcePins {
			if proto.Equal(pin, leaderPin) {
				leaderCount++
			} else {
				withoutLeader = append(withoutLeader, pin)
			}
		}
		comparison.SourcePins = withoutLeader
		if leaderCount != 1 || !proto.Equal(comparison, sampled) {
			return errors.New("manual P4正文不是实际leader原件")
		}
		delete(observed, key)
		p3Pin := sampled.SourcePins[0]
		if p3Pin == nil || filepath.Base(p3Pin.AbsolutePath) != "manual-closure.report.json" || seenP3[p3Pin.AbsolutePath] {
			return errors.New("manual P3文件不是固定唯一原件")
		}
		seenP3[p3Pin.AbsolutePath] = true
		p3Raw, err := drainproof.ReadPin(p3Pin, 16<<20)
		if err != nil {
			return err
		}
		closed, err := parseManualEvidence(p3Raw)
		if err != nil {
			return err
		}
		if !bytes.Equal(closed.OperationId, sampled.OperationId) || !bytes.Equal(closed.ManifestDigest, sampled.ManifestDigest) || !closed.AcceptingClosed || !closed.CallbacksJoined || closed.Accepted != sampled.Accepted || closed.Settled != sampled.Settled || closed.UnresolvedPublications != 0 || len(closed.Assignments) != len(sampled.Assignments) {
			return errors.New("manual P3原件没有同boot真实闭合")
		}
		source := sources[string(closed.Assignments[0].IssuerId)]
		if source == nil || !bytes.Equal(source.InstanceId, closed.Assignments[0].InstanceId) || !authorityPinSetEqual(source.SourceEvidence, closed.SourcePins) {
			return errors.New("manual P3身份/配置证据不属于P0 SOURCE")
		}
		fixedDirectory := false
		for _, pin := range source.SourceEvidence {
			if filepath.Clean(filepath.Dir(pin.AbsolutePath)) == filepath.Clean(filepath.Dir(p3Pin.AbsolutePath)) {
				fixedDirectory = true
			}
			if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
		if !fixedDirectory {
			return errors.New("manual P3离开P0固定私有目录")
		}
		priorRows := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
		for _, row := range closed.Assignments {
			rowKey := manualAssignmentKey(row)
			if priorRows[rowKey] != nil {
				return errors.New("manual P3分区重复")
			}
			priorRows[rowKey] = row
		}
		for _, row := range sampled.Assignments {
			rowKey := manualAssignmentKey(row)
			base, p3 := expected[rowKey], priorRows[rowKey]
			if base == nil || p3 == nil || seen[rowKey] || !bytes.Equal(p3.IssuerId, source.IssuerId) || !bytes.Equal(p3.InstanceId, source.InstanceId) || p3.ObservedMs < base.ObservedMs || !sameManualConfiguration(base, p3) || !bytes.Equal(base.InstanceId, p3.InstanceId) || p3.AssignmentRevision != base.AssignmentRevision || p3.StartOffset != base.StartOffset || p3.Position < base.Position || p3.Position != p3.ExclusiveEnd || p3.Earliest < base.Earliest || p3.Earliest > base.Position || p3.ObservedMs > leader.ObservedStartMS || row.ObservedMs < leader.ObservedStartMS || row.ObservedMs > leader.ObservedEndMS || row.Earliest < p3.Earliest || row.Earliest > base.Position || row.Position != row.ExclusiveEnd {
				return errors.New("manual P3/P4物理身份、位置、保留范围或观察窗口不符")
			}
			stable := proto.Clone(row).(*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
			stable.Earliest = p3.Earliest
			stable.ExclusiveEnd = p3.ExclusiveEnd
			stable.ObservedMs = p3.ObservedMs
			if !proto.Equal(stable, p3) {
				return errors.New("manual P4修改了P3原生赋值/位置")
			}
			seen[rowKey] = true
		}
	}
	if len(observed) != 0 || len(seen) != len(expected) {
		return errors.New("manual P4没有覆盖P0全部owner")
	}
	return nil
}

func verifyAuthorityKafka(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, actual *databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	topics := map[string]bool{}
	groups := map[string]bool{}
	for _, row := range plan.KafkaPartitions {
		expected[fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)] = row
		topics[row.TopicName] = true
		groups[row.Group] = true
	}
	for _, row := range plan.ManualAssignments {
		if row == nil {
			return errors.New("manual P0空分区")
		}
		topics[row.TopicName] = true
	}
	if len(actual.KafkaPartitions) != len(expected) {
		return errors.New("Kafka历史分区数量不等")
	}
	for _, row := range actual.KafkaPartitions {
		if row == nil {
			return errors.New("Kafka空分区原件")
		}
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		base := expected[key]
		if base == nil || !bytes.Equal(base.ClusterId, row.ClusterId) || !bytes.Equal(base.TopicId, row.TopicId) || base.InitialEarliest != row.InitialEarliest || base.ProtectedCommittedFloor != row.ProtectedCommittedFloor || row.FinalEarliest < row.InitialEarliest || row.FinalEarliest > row.ProtectedCommittedFloor || row.FinalCommitted != row.FinalExclusiveEnd || row.FinalCommitted < row.ProtectedCommittedFloor {
			return errors.New("Kafka原UUID/保留floor/固定末水位不符")
		}
		delete(expected, key)
	}
	var baseline []byte
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" {
			if baseline != nil {
				return errors.New("P0 Kafka目录原件重复")
			}
			baseline = raw
		}
	}
	if baseline == nil {
		return errors.New("P0缺broker/有效retention完整目录")
	}
	baselineDigest, err := kafkautil.StableDrainCatalogDigest(baseline)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual.HistoricalInventoryDigest, baselineDigest) {
		return errors.New("nonSQL历史目录digest不是原完整稳定目录")
	}
	var catalog kafkautil.DrainKafkaCatalog
	if json.Unmarshal(baseline, &catalog) != nil || len(catalog.BrokerConfigs) == 0 {
		return errors.New("P0 Kafka目录损坏")
	}
	if _, err := authorityKafkaBaseline(plan); err != nil {
		return err
	}
	names := []string{"__consumer_offsets"}
	for topic := range topics {
		names = append(names, topic)
	}
	if !releaseExactStrings(catalog.TopicNames, names) {
		return errors.New("P0目录没有全部历史业务topic")
	}
	groupNames := []string{}
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	if !releaseExactStrings(catalog.GroupNames, groupNames) {
		return errors.New("P0目录没有全部历史消费组")
	}
	for _, row := range plan.KafkaPartitions {
		if !bytes.Equal(row.ClusterId, catalog.ClusterID) || !bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) {
			return errors.New("P0实际目录与typed历史UUID不符")
		}
	}
	observed := 0
	seenPins := map[string]bool{}
	for _, pin := range actual.SourceEvidence {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_KAFKA_CATALOG_V2" {
			digest, digestErr := kafkautil.StableDrainCatalogDigest(raw)
			var captured kafkautil.DrainKafkaCatalog
			decodeErr := json.Unmarshal(raw, &captured)
			if seenPins[pin.AbsolutePath] || digestErr != nil || decodeErr != nil || !bytes.Equal(digest, baselineDigest) || captured.ObservedStartMS < actual.ObservedStartMs || captured.ObservedEndMS > actual.ObservedEndMs {
				return errors.New("Kafka首尾目录/broker/retention变化、重复原件或旧窗口")
			}
			seenPins[pin.AbsolutePath] = true
			observed++
		}
	}
	wantedCatalogs := 2
	if len(plan.ManualAssignments) > 0 {
		wantedCatalogs = 4
	}
	if observed != wantedCatalogs {
		return errors.New("Kafka缺本窗group/manual各自首尾真实目录")
	}
	return verifyManualClosure(plan, actual)
}

var authorityRedisPatterns = []string{"kafka:retry:queue:*", "kafka:dead:queue:*", "kafka:retry:processing:*", "kafka:retry:instances:*", "kafka:retry:instance_mode:*", "player:save_pending:*", "player:{*}:save_barrier"}

type authorityRedisScanPage struct {
	Pattern string   `json:"pattern"`
	Input   uint64   `json:"input_cursor"`
	Output  uint64   `json:"output_cursor"`
	Keys    []string `json:"keys"`
}
type authorityRedisScan struct {
	ObservedStartMS   uint64                   `json:"observed_start_ms"`
	ObservedEndMS     uint64                   `json:"observed_end_ms"`
	Schema            string                   `json:"schema"`
	Role              string                   `json:"role"`
	Address           string                   `json:"address"`
	DB                int32                    `json:"db"`
	RunID             []byte                   `json:"server_run_id"`
	NonceKey          string                   `json:"nonce_key"`
	NonceBefore       []byte                   `json:"nonce_before"`
	NonceAfter        []byte                   `json:"nonce_after"`
	NoncePTTLBeforeMS int64                    `json:"nonce_pttl_before_ms"`
	NoncePTTLAfterMS  int64                    `json:"nonce_pttl_after_ms"`
	Pages             []authorityRedisScanPage `json:"pages,omitempty"`
}

func verifyRedisNonceObservation(source *authorityRedisScan, target *databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure, runID []byte) error {
	key, err := drainproof.RedisMaintenanceNonceKey(runID)
	if err != nil {
		return err
	}
	if source == nil || target == nil || len(target.MaintenanceNonce) != 16 || source.NonceKey != key || source.NoncePTTLBeforeMS != -1 || source.NoncePTTLAfterMS != -1 || !bytes.Equal(source.NonceBefore, target.MaintenanceNonce) || !bytes.Equal(source.NonceAfter, target.MaintenanceNonce) {
		return errors.New("Redis维护nonce缺失、变化或有TTL，不能用输入副本代替实际读回")
	}
	return nil
}
func verifyAuthorityRedisBaseline(plan *databasepb.SqlWriterMaintenanceReceipt_Plan) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure{}
	for _, target := range plan.RedisTargets {
		expected[fmt.Sprintf("%s/%d", target.Address, target.Db)] = target
	}
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) != nil || marker.Schema != "SQL_DRAIN_REDIS_BASELINE_V1" {
			continue
		}
		var source authorityRedisScan
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&source) != nil || source.ObservedStartMS == 0 || source.ObservedEndMS < source.ObservedStartMS || source.ObservedEndMS-source.ObservedStartMS > 60000 || len(source.Pages) != 0 {
			return errors.New("Redis P0基线格式/窗口无效")
		}
		key := fmt.Sprintf("%s/%d", source.Address, source.DB)
		target := expected[key]
		if target == nil || source.Role != target.Role || !bytes.Equal(source.RunID, target.ServerRunId) {
			return errors.New("Redis P0原件重复或不属于固定物理目标")
		}
		if err := verifyRedisNonceObservation(&source, target, plan.RunId); err != nil {
			return err
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		return errors.New("Redis P0缺实际单次nonce读回基线")
	}
	return nil
}

func authorityScanKeys(target *databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure, startMS, endMS uint64, runID []byte) (map[string]bool, error) {
	if len(target.ScanSourceEvidence) != 1 {
		return nil, errors.New("Redis目标必须恰有完整SCAN原件")
	}
	raw, err := drainproof.ReadPin(target.ScanSourceEvidence[0], 16<<20)
	if err != nil {
		return nil, err
	}
	var source authorityRedisScan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&source) != nil || source.Schema != "SQL_DRAIN_REDIS_SCAN_V2" || source.ObservedStartMS < startMS || source.ObservedEndMS < source.ObservedStartMS || source.ObservedEndMS > endMS || source.ObservedEndMS-source.ObservedStartMS > 60000 || source.Role != target.Role || source.Address != target.Address || source.DB != target.Db || !bytes.Equal(source.RunID, target.ServerRunId) || len(source.Pages) == 0 || len(source.Pages) > 16384 {
		return nil, errors.New("Redis SCAN实际目标/epoch/格式不符")
	}
	if err := verifyRedisNonceObservation(&source, target, runID); err != nil {
		return nil, err
	}
	pages := map[string][]authorityRedisScanPage{}
	for _, page := range source.Pages {
		known := false
		for _, pattern := range authorityRedisPatterns {
			if page.Pattern == pattern {
				known = true
			}
		}
		if !known {
			return nil, errors.New("Redis SCAN未知pattern")
		}
		pages[page.Pattern] = append(pages[page.Pattern], page)
	}
	keys := map[string]bool{}
	for _, pattern := range authorityRedisPatterns {
		cursor := uint64(0)
		list := pages[pattern]
		if len(list) == 0 {
			return nil, errors.New("Redis SCAN缺pattern")
		}
		for i, page := range list {
			if page.Input != cursor || i < len(list)-1 && page.Output == 0 {
				return nil, errors.New("Redis SCAN游标链中断或提前结束")
			}
			for _, key := range page.Keys {
				matched, err := filepath.Match(pattern, key)
				if err != nil || !matched {
					return nil, errors.New("Redis SCAN页含不匹配原key")
				}
				keys[key] = true
				if len(keys) > 16384 {
					return nil, errors.New("Redis SCAN key超过16K")
				}
			}
			cursor = page.Output
		}
		if cursor != 0 {
			return nil, errors.New("Redis SCAN未读到cursor0")
		}
	}
	return keys, nil
}
func verifyAuthorityRedis(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, actual *databasepb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure{}
	for _, target := range plan.RedisTargets {
		expected[fmt.Sprintf("%s/%d", target.Address, target.Db)] = target
	}
	if len(actual.RedisTargets) != len(expected) {
		return errors.New("Redis目标全集不等")
	}
	topics := map[string]int{}
	for _, row := range plan.KafkaPartitions {
		n := int(row.Partition) + 1
		if n > topics[row.TopicName] {
			topics[row.TopicName] = n
		}
	}
	for _, row := range plan.ManualAssignments {
		if int(row.PartitionCount) > topics[row.TopicName] {
			topics[row.TopicName] = int(row.PartitionCount)
		}
	}
	for _, target := range actual.RedisTargets {
		if target == nil {
			return errors.New("Redis空目标")
		}
		key := fmt.Sprintf("%s/%d", target.Address, target.Db)
		base := expected[key]
		if base == nil || base.Role != target.Role || !bytes.Equal(base.ServerRunId, target.ServerRunId) || !bytes.Equal(base.MaintenanceNonce, target.MaintenanceNonce) || !releaseEqualZones(base.Zones, target.Zones) || len(target.Observations) > 16384 {
			return errors.New("Redis目标/原epoch/zone/nonce变化")
		}
		delete(expected, key)
		required, err := authorityScanKeys(target, actual.ObservedStartMs, actual.ObservedEndMs, plan.RunId)
		if err != nil {
			return err
		}
		for _, zone := range target.Zones {
			for bucket := 0; bucket < 64; bucket++ {
				required[fmt.Sprintf("player:save_pending:%d:%d", zone, bucket)] = true
			}
		}
		for topic, n := range topics {
			for _, prefix := range []string{"kafka:retry:queue:", "kafka:dead:queue:", "kafka:retry:processing:", "kafka:retry:instances:", "kafka:retry:instance_mode:"} {
				required[prefix+topic] = true
			}
			for partition := 0; partition < n; partition++ {
				required[fmt.Sprintf("kafka:retry:queue:%s:p%d", topic, partition)] = true
			}
		}
		if len(required) > 16384 || len(target.Observations) != len(required) {
			return errors.New("Redis固定必读与动态orphan目录非等集")
		}
		registries := map[string]map[string]bool{}
		modes := map[string]map[string]bool{}
		processing := map[string]map[string]bool{}
		totalMembers := 0
		for _, o := range target.Observations {
			if o != nil {
				totalMembers += len(o.Members)
			}
			if totalMembers > 16384 {
				return errors.New("Redis目标原成员总量超过16K预算")
			}
			if o == nil || !required[o.Key] {
				return errors.New("Redis观察重复/未知key")
			}
			delete(required, o.Key)
			members, values := []string{}, []string{}
			for _, member := range o.Members {
				members = append(members, string(member))
			}
			for _, value := range o.Values {
				values = append(values, string(value))
			}
			sha := sha256.Sum256([]byte(o.Key))
			old := maintenanceReleaseRedisKeyObservation{Key: o.Key, KeySHA: hex.EncodeToString(sha[:]), Kind: o.Kind, Topic: o.Topic, Type: o.RedisType, PTTL: o.PttlMs, Count: int64(o.Count), Members: members, Values: values, DumpSHA: hex.EncodeToString(o.DumpSha256)}
			if err := releaseRedisObservation(old, topics, target.Zones); err != nil {
				return err
			}
			if o.Kind == "instances" || o.Kind == "modes" {
				set := map[string]bool{}
				for _, member := range members {
					set[member] = true
				}
				if o.Kind == "instances" {
					registries[o.Topic] = set
				} else {
					modes[o.Topic] = set
				}
				if o.Count != 0 {
					return errors.New("Redis仍有已登记queue mutator")
				}
			}
			if o.Kind == "processing" && o.Key != "kafka:retry:processing:"+o.Topic {
				id := strings.TrimPrefix(o.Key, "kafka:retry:processing:"+o.Topic+":")
				if processing[o.Topic] == nil {
					processing[o.Topic] = map[string]bool{}
				}
				processing[o.Topic][id] = true
				if o.RedisType != "none" {
					return errors.New("Redis有未登记processing孤儿，即使空列表也HOLD")
				}
			}
		}
		if len(required) != 0 {
			return errors.New("Redis完整目录遗漏")
		}
		for topic := range topics {
			a, b := registries[topic], modes[topic]
			if len(a) != len(b) {
				return errors.New("Redis registry与mode非等集")
			}
			for id := range a {
				if !b[id] || !processing[topic][id] {
					return errors.New("Redis registry与processing双向不等")
				}
			}
			for id := range processing[topic] {
				if !a[id] {
					return errors.New("Redis processing orphan未归还")
				}
			}
		}
	}
	return nil
}

// VerifyAuthorityReleaseReadiness复用既有恢复owner的真实强读，不以任意文件或“恢复完成”布尔作审核。
// 支持正式fence-only promotion收据与实际合服审计；未知审计类型一律HOLD。
func (r Restorer) VerifyAuthorityReleaseReadiness(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact *databasepb.SqlWriterMaintenanceReceipt) error {
	evidence := fact.GetEvidence().GetReleaseReadiness()
	if evidence == nil || len(evidence.RecoveryAudit) == 0 || len(evidence.RecoveryAudit) > 128 {
		return errors.New("release缺正式恢复审计原件")
	}
	frozen, err := catalog.MaintenanceReceipt(ctx, evidence.OriginalFreezeReceiptId)
	if err != nil {
		return err
	}
	closed, err := catalog.MaintenanceReceipt(ctx, evidence.NonSqlClosedReceiptId)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(fact.PlanProto)
	if frozen.Kind != databasepb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != databasepb.SqlWriterMaintenanceReceipt_FROZEN || frozen.RunPhase != databasepb.SqlWriterDrainRun_SQL_FROZEN || !bytes.Equal(frozen.ReceiptDigest, evidence.OriginalFreezeReceiptDigest) || !bytes.Equal(closed.ReceiptDigest, evidence.NonSqlClosedReceiptDigest) || closed.RunPhase != databasepb.SqlWriterDrainRun_NON_SQL_CLOSED || frozen.AttemptRevision != fact.AttemptRevision || closed.AttemptRevision != fact.AttemptRevision || !bytes.Equal(frozen.RunId, plan.RunId) || !bytes.Equal(closed.RunId, plan.RunId) || frozen.Evidence == nil || closed.Evidence == nil || !bytes.Equal(frozen.Evidence.ManifestDigest, sum[:]) || !bytes.Equal(closed.Evidence.ManifestDigest, sum[:]) {
		return errors.New("release未绑定原正常freeze/nonSQL SQL收据")
	}
	if len(plan.MaintenanceIssuers) == 0 {
		if evidence.MaintenancePublication != nil {
			return errors.New("P0无维护工具，不能在P7增补")
		}
	} else {
		if err := verifyAuthorityPublicationOriginals(plan.MaintenanceIssuers, evidence.MaintenancePublication); err != nil {
			return err
		}
		if err := verifyAuthorityRoles(plan.MaintenanceIssuers, evidence.MaintenancePublication.LogicalRoles); err != nil {
			return err
		}
		for _, tool := range evidence.MaintenancePublication.LogicalRoles {
			if tool.AcceptingClosedMs < frozen.CommittedMs {
				return errors.New("维护工具闭合早于原正常冻结")
			}
		}
	}
	seen := map[string]bool{}
	for _, pin := range evidence.RecoveryAudit {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		routeAudit := &databasepb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, routeAudit) == nil && len(routeAudit.AuditInstanceId) != 0 {
			auditKey := "route/" + string(routeAudit.AuditInstanceId)
			if seen[auditKey] {
				return errors.New("路由审核原件重复")
			}
			seen[auditKey] = true
			if err := verifyRouteRecoveryAudit(plan, fact, frozen, routeAudit); err != nil {
				return err
			}
			if err := r.verifyRouteAuditCurrentRedis(ctx, routeAudit); err != nil {
				return err
			}
			if err := verifyRecoveryFrozenState(ctx, catalog, plan, fact, frozen); err != nil {
				return err
			}
			continue
		}
		mergeAudit := &databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, mergeAudit) == nil && len(mergeAudit.AuditInstanceId) != 0 {
			auditKey := "merge/" + string(mergeAudit.AuditInstanceId)
			if seen[auditKey] {
				return errors.New("合服审计原件重复")
			}
			seen[auditKey] = true
			if err := verifyMergeRecoveryAudit(plan, fact, frozen, mergeAudit); err != nil {
				return err
			}
			if err := verifyRecoveryFrozenState(ctx, catalog, plan, fact, frozen); err != nil {
				return err
			}
			continue
		}
		if r.DB == nil {
			return errors.New("恢复审计缺原物理SQL句柄")
		}
		receipt := &databasepb.SaveBarrierMaintenanceReceipt{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, receipt) != nil || receipt.Phase != databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED {
			return errors.New("恢复审计不是支持的正式已确认promotion原件")
		}
		promotion := &databasepb.SaveBarrierMaintenanceReceipt_PromotionPlan{}
		if proto.Unmarshal(receipt.PlanProto, promotion) != nil {
			return errors.New("恢复原计划损坏")
		}
		original, digest, err := PromotionDigest(promotion)
		if err != nil || !bytes.Equal(original, receipt.PlanProto) || !bytes.Equal(digest, receipt.RequestDigest) || !bytes.Equal(receipt.ReceiptId, promotion.ReceiptId) || receipt.PlayerId != promotion.PlayerId || promotion.RouteSchema != plan.AuthoritySchema || promotion.MaintenanceId != hex.EncodeToString(plan.RunId) || seen[string(receipt.ReceiptId)] {
			return errors.New("恢复审计换run/schema/原计划或重复")
		}
		seen[string(receipt.ReceiptId)] = true
		allowed := false
		for _, schema := range plan.ProtectedSchemas {
			if schema == promotion.RowSchema {
				allowed = true
			}
		}
		if !allowed {
			return errors.New("恢复审计越出保护schema")
		}
		phase, _, err := r.promotionSQL(ctx, promotion, original, digest, databasepb.SaveBarrierMaintenanceReceipt_PHASE_UNKNOWN)
		if err != nil {
			return err
		}
		if phase != databasepb.SaveBarrierMaintenanceReceipt_BARRIER_CONFIRMED {
			return errors.New("恢复SQL永久阶段未真实确认")
		}
	}
	return nil
}

func verifyRouteRecoveryAudit(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact, frozen *databasepb.SqlWriterMaintenanceReceipt, audit *databasepb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) error {
	ready := fact.GetEvidence().GetReleaseReadiness()
	if plan == nil || ready == nil || frozen == nil || audit == nil || len(audit.AuditInstanceId) != 16 || !bytes.Equal(audit.RunId, plan.RunId) || audit.Generation != fact.Generation || audit.AttemptRevision != fact.AttemptRevision || !bytes.Equal(audit.PlanDigest, fact.GetEvidence().GetManifestDigest()) || audit.AuthoritySchema != plan.AuthoritySchema || !bytes.Equal(audit.OriginalFreezeReceiptId, frozen.RequestId) || !bytes.Equal(audit.OriginalFreezeReceiptDigest, frozen.ReceiptDigest) || audit.ObservedStartMs < frozen.CommittedMs || audit.ObservedEndMs < audit.ObservedStartMs || audit.ObservedEndMs-audit.ObservedStartMs > 1800000 || audit.ObservedEndMs > uint64(time.Now().UnixMilli()) || !proto.Equal(audit.MaintenancePublication, ready.MaintenancePublication) {
		return errors.New("路由恢复审核没有绑定同run正常冻结及真实闭合")
	}
	if len(plan.MaintenanceIssuers) == 0 || audit.MaintenancePublication == nil {
		return errors.New("路由恢复审核缺P0维护工具")
	}
	for _, closed := range audit.MaintenancePublication.LogicalRoles {
		if closed == nil || closed.AcceptedSettledMs > audit.ObservedStartMs {
			return errors.New("路由审核先于恢复工具完成")
		}
	}
	stats := audit.Stats
	if stats == nil || !stats.Completed || !stats.FrozenRecovery || stats.RequireMapRev || stats.DbOnlyUnpublished > stats.DbRows || stats.MissingInRedis != 0 || stats.MissingInDb != 0 || stats.ValueMismatch != 0 || stats.RedisMalformed != 0 || stats.DbInvalid != 0 || stats.RedisRevAhead != 0 || stats.RedisRevBehind != 0 || stats.MergedIntoMissingInRedis != 0 || stats.MergedIntoMissingInDb != 0 || stats.MergedIntoMismatch != 0 || stats.RedisFencesActive != 0 || stats.TransientCleared != 0 {
		return errors.New("路由审核未完整完成或仍存在阻断差异/并发变化")
	}
	target := audit.MappingTarget
	var expected *databasepb.SqlWriterMaintenanceReceipt_RedisTargetClosure
	if target == nil {
		return errors.New("路由审核缺实际mapping目标")
	}
	for _, planned := range plan.RedisTargets {
		if planned != nil && planned.Address == target.Address && planned.Db == target.Db {
			if expected != nil {
				return errors.New("P0 mapping目标重复")
			}
			expected = planned
		}
	}
	if expected == nil || target.Role != expected.Role || !bytes.Equal(target.ServerRunId, expected.ServerRunId) || !bytes.Equal(target.MaintenanceNonce, expected.MaintenanceNonce) || !releaseEqualZones(target.Zones, expected.Zones) {
		return errors.New("路由审核换Redis物理实例或维护nonce")
	}
	if len(audit.InfoServerRaw) == 0 || len(audit.InfoServerRaw) > 65536 || len(audit.InfoReplicationRaw) == 0 || len(audit.InfoReplicationRaw) > 65536 || len(audit.ReadyMarkerRaw) > 16384 || len(audit.WatermarkRaw) > 16384 {
		return errors.New("路由身份或缓存标记原件超预算")
	}
	identity, err := placement.ParseRedisInstanceIdentity(string(audit.InfoServerRaw), string(audit.InfoReplicationRaw))
	if err != nil || identity.Role != "master" || identity.RunID != string(expected.ServerRunId) {
		return errors.New("路由审核INFO不是P0实际主实例")
	}
	verdict, marker := placement.VerifyRouteReady(placement.RouteReadyRead{MarkerPresent: len(audit.ReadyMarkerRaw) > 0, MarkerRaw: string(audit.ReadyMarkerRaw), Identity: identity})
	if verdict != placement.RouteReadyTrusted {
		return errors.New("路由缓存完整性原标记未实际验证，closure不能替代业务证据")
	}
	floor := marker.WatermarkMs
	if len(audit.WatermarkRaw) != 0 {
		watermark, err := placement.DecodeRouteWatermark(string(audit.WatermarkRaw))
		if err != nil {
			return err
		}
		if current := watermark.FloorFor(audit.Generation); current > floor {
			floor = current
		}
	}
	if audit.RouteWatermarkFloorMs != floor || ready.RouteWatermarkFloorMs != floor {
		return errors.New("路由释放水位不是实际原件推导值")
	}
	fixed := map[string][]byte{}
	for _, pin := range plan.SourcePins {
		if pin != nil {
			fixed[pin.AbsolutePath] = pin.Sha256
		}
	}
	if len(audit.AuditSourcePins) == 0 || len(audit.AuditSourcePins) > 128 {
		return errors.New("路由审核缺P0固定查询源码")
	}
	seen := map[string]bool{}
	for _, pin := range audit.AuditSourcePins {
		if pin == nil || len(pin.Sha256) != 32 || len(fixed[pin.AbsolutePath]) != 32 || !bytes.Equal(pin.Sha256, fixed[pin.AbsolutePath]) || seen[pin.AbsolutePath] {
			return errors.New("路由审核查询来源不属于P0")
		}
		seen[pin.AbsolutePath] = true
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	return nil
}

func (r Restorer) verifyRouteAuditCurrentRedis(ctx context.Context, audit *databasepb.SqlWriterMaintenanceReceipt_RouteRecoveryAudit) error {
	client := r.mapping()
	if client == nil || audit.MappingTarget == nil || client.Options().Addr != audit.MappingTarget.Address || int32(client.Options().DB) != audit.MappingTarget.Db {
		return errors.New("路由审核缺同一mapping物理连接")
	}
	key, err := drainproof.RedisMaintenanceNonceKey(audit.RunId)
	if err != nil {
		return err
	}
	reply, err := client.Eval(ctx, drainproof.RouteRecoveryReadScript, []string{key, placement.RouteReadyKey, placement.RouteWatermarkKey}).Result()
	if err != nil {
		return err
	}
	current, err := drainproof.ParseRouteRecoverySnapshot(reply, audit.RunId, audit.MappingTarget)
	if err != nil {
		return err
	}
	identity, err := placement.ParseRedisInstanceIdentity(string(current.InfoServerRaw), string(current.InfoReplicationRaw))
	if err != nil {
		return err
	}
	observed, err := placement.ParseRedisInstanceIdentity(string(audit.InfoServerRaw), string(audit.InfoReplicationRaw))
	if err != nil || identity != observed {
		return errors.New("路由审核后Redis主身份变化")
	}
	if !bytes.Equal(current.ReadyMarkerRaw, audit.ReadyMarkerRaw) || !bytes.Equal(current.WatermarkRaw, audit.WatermarkRaw) {
		return errors.New("路由审核后缓存完整性标记/水位变化")
	}
	return nil
}

func verifyRecoveryFrozenState(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact, frozen *databasepb.SqlWriterMaintenanceReceipt) error {
	run, err := catalog.DrainRun(ctx, plan.RunId)
	if err != nil {
		return err
	}
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		return err
	}
	if (run.Phase != databasepb.SqlWriterDrainRun_SQL_FROZEN && run.Phase != databasepb.SqlWriterDrainRun_RELEASE_READY) || !bytes.Equal(run.ManifestProto, fact.PlanProto) || !bytes.Equal(run.FrozenRequestId, frozen.RequestId) || run.Generation != fact.Generation || run.AttemptRevision != fact.AttemptRevision || meta.Writable != 0 || meta.WriterEpoch != frozen.AfterEpoch || meta.CatalogRevision != frozen.CatalogRevision || !bytes.Equal(meta.FreezeRunId, plan.RunId) {
		return errors.New("恢复审核后原SQL冻结资格变化")
	}
	return nil
}

// 合服业务查询仍由原runAuditMode执行；这里核完整typed结论、固定源码来源和不可变清单。
// RULE只能说明规则，不能顶替真实OBSERVATION；任一未知/缺失/warn/block均拒绝释放。
func verifyMergeRecoveryAudit(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact, frozen *databasepb.SqlWriterMaintenanceReceipt, audit *databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryAudit) error {
	ready := fact.GetEvidence().GetReleaseReadiness()
	if plan == nil || ready == nil || frozen == nil || audit == nil || len(audit.AuditInstanceId) != 16 || !bytes.Equal(audit.RunId, plan.RunId) || audit.Generation != fact.Generation || audit.AttemptRevision != fact.AttemptRevision || !bytes.Equal(audit.PlanDigest, fact.GetEvidence().GetManifestDigest()) || !bytes.Equal(audit.OriginalFreezeReceiptId, frozen.RequestId) || !bytes.Equal(audit.OriginalFreezeReceiptDigest, frozen.ReceiptDigest) || !audit.VerifyMerged || audit.SourceZone == 0 || audit.TargetZone == 0 || audit.SourceZone == audit.TargetZone || audit.MergeRunId == "" || len(audit.MergeRunId) > 256 || audit.ObservedStartMs < frozen.CommittedMs || audit.ObservedEndMs < audit.ObservedStartMs || audit.ObservedEndMs-audit.ObservedStartMs > 600000 || audit.ObservedEndMs > uint64(time.Now().UnixMilli()) || !proto.Equal(audit.MaintenancePublication, ready.MaintenancePublication) {
		return errors.New("合服审计没有绑定同run正常冻结及实际维护闭合")
	}
	if len(plan.MaintenanceIssuers) == 0 || audit.MaintenancePublication == nil {
		return errors.New("合服审计缺P0已固定维护工具")
	}
	for _, closed := range audit.MaintenancePublication.LogicalRoles {
		if closed == nil || closed.AcceptedSettledMs > audit.ObservedStartMs {
			return errors.New("合服审计早于维护工具真正完成")
		}
	}
	if audit.MergeManifest == nil {
		return errors.New("合服审计缺真实mergeManifest")
	}
	manifestRaw, err := drainproof.ReadPin(audit.MergeManifest, 16<<20)
	if err != nil {
		return err
	}
	manifest := map[string]json.RawMessage{}
	if json.Unmarshal(manifestRaw, &manifest) != nil {
		return errors.New("合服清单不是原JSON对象")
	}
	var runID, kind, routeSource string
	var src, dst uint32
	var dryRun bool
	if json.Unmarshal(manifest["run_id"], &runID) != nil || json.Unmarshal(manifest["source_zone"], &src) != nil || json.Unmarshal(manifest["target_zone"], &dst) != nil {
		return errors.New("合服清单身份缺失")
	}
	if raw, ok := manifest["kind"]; ok && json.Unmarshal(raw, &kind) != nil {
		return errors.New("合服清单kind无效")
	}
	if raw, ok := manifest["dry_run"]; ok && json.Unmarshal(raw, &dryRun) != nil {
		return errors.New("合服清单dry_run无效")
	}
	if raw, ok := manifest["route_source"]; ok && json.Unmarshal(raw, &routeSource) != nil {
		return errors.New("合服清单路由来源无效")
	}
	if routeSource == "" {
		routeSource = "redis"
	}
	if runID != audit.MergeRunId || src != audit.SourceZone || dst != audit.TargetZone || kind != "" || dryRun || (routeSource != "db" && routeSource != "redis") {
		return errors.New("合服审计换清单/run/zone或引用预览/搬库清单")
	}
	fixed := map[string][]byte{}
	for _, pin := range plan.SourcePins {
		if pin != nil {
			fixed[pin.AbsolutePath] = pin.Sha256
		}
	}
	sources := map[string][]byte{}
	if len(audit.AuditSourcePins) == 0 || len(audit.AuditSourcePins) > 128 {
		return errors.New("合服审计缺P0固定查询来源")
	}
	for _, pin := range audit.AuditSourcePins {
		if pin == nil || sources[pin.AbsolutePath] != nil || !bytes.Equal(fixed[pin.AbsolutePath], pin.Sha256) {
			return errors.New("合服审计更换或重复查询来源")
		}
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
		sources[pin.AbsolutePath] = pin.Sha256
	}
	required := map[string]databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_Kind{
		"player.name (global)":    databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_RULE,
		"friend":                  databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"friend_request":          databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"guild_member":            databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:mapping_src":      databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:manifest_mapping": databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:guild_zone":       databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:guild_rank":       databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:trade_listing":    databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:manifest_rows":    databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:save_barrier":     databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:source_hot_state": databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
		"verify:merge_fence":      databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION,
	}
	// P7实际审核固定从全局SQL读取，旧清单的redis来源只描述当时写入；仍须核当前SQL/缓存一致。
	required["verify:route_cache"] = databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_OBSERVATION
	if len(audit.Checks) != len(required) {
		return errors.New("合服审计不是完整实际检查目录")
	}
	for _, check := range audit.Checks {
		if check == nil || required[check.Name] == databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_KIND_UNSPECIFIED || check.Kind != required[check.Name] || check.Status != databasepb.SqlWriterMaintenanceReceipt_MergeRecoveryCheck_COMPLETE || check.Severity != "info" || check.SourceCount < 0 || check.TargetCount < 0 || check.ConflictCount != 0 || check.UniqueScope == "" || len(check.QuerySourcePins) == 0 || len(check.QuerySourcePins) > 128 {
			return errors.New("合服审计包含缺失、未完成、冲突或非信息级结论")
		}
		seen := map[string]bool{}
		for _, pin := range check.QuerySourcePins {
			if pin == nil || len(pin.Sha256) != 32 || len(sources[pin.AbsolutePath]) != 32 || seen[pin.AbsolutePath] || !bytes.Equal(sources[pin.AbsolutePath], pin.Sha256) {
				return errors.New("合服检查未绑定实际查询来源")
			}
			seen[pin.AbsolutePath] = true
		}
		delete(required, check.Name)
	}
	if len(required) != 0 {
		return errors.New("合服审计检查重复或遗漏")
	}
	return nil
}

// verifyConsumerReadiness 合并同组各个实际boot的分区；历史组是否重启由固定启动owner决定，
// 一旦声明要启动某组，其配置目录不能被分片报告缩小，所有实际分配的并集必须完整。
func verifyManualReadiness(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, boot *databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness, reports []*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, manifestDigest []byte) error {
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	// 当前命令流每个SOURCE在同topic只有一个手动owner；新boot nodeID由etcd重新分配。
	for _, row := range plan.ManualAssignments {
		if bytes.Equal(row.IssuerId, boot.IssuerId) {
			if expected[row.TopicName] != nil {
				return errors.New("manual SOURCE同topic配置重复")
			}
			expected[row.TopicName] = row
		}
	}
	seen := map[string]bool{}
	for _, report := range reports {
		if report == nil || !bytes.Equal(report.OperationId, plan.RunId) || !bytes.Equal(report.ManifestDigest, manifestDigest) || !report.AcceptingClosed || !report.CallbacksJoined || report.Accepted != 0 || report.Settled != 0 || report.UnresolvedPublications != 0 || len(report.Assignments) == 0 {
			return errors.New("manual新boot不是实际暂停且零接收准备")
		}
		for _, pin := range report.SourcePins {
			if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
		for _, row := range report.Assignments {
			if !validManualAssignment(row) {
				return errors.New("manual P8身份/位置无效")
			}
			prior := expected[row.TopicName]
			if prior == nil || seen[row.TopicName] || !bytes.Equal(row.IssuerId, boot.IssuerId) || !bytes.Equal(row.InstanceId, boot.InstanceId) || bytes.Equal(row.InstanceId, prior.InstanceId) || !bytes.Equal(row.ClusterId, prior.ClusterId) || !bytes.Equal(row.TopicId, prior.TopicId) || row.PartitionCount != prior.PartitionCount || row.Partition != row.TargetNodeId%row.PartitionCount || row.Position != row.StartOffset {
				return errors.New("manual新boot配置/原绝对起点/目标身份不匹配")
			}
			seen[row.TopicName] = true
		}
	}
	if len(seen) != len(expected) {
		return errors.New("manual新boot遗漏实际消费者")
	}
	return nil
}

func verifyConsumerReadiness(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, boots []*databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness, manifest []byte) error {
	type groupState struct {
		generation int32
		expected   map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure
		assigned   map[string]bool
		members    map[string]bool
	}
	groups := map[string]*groupState{}
	planned := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	key := func(row *databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure) string {
		return fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
	}
	for _, row := range plan.KafkaPartitions {
		planned[key(row)] = row
	}
	sum := sha256.Sum256(manifest)
	for _, boot := range boots {
		if boot == nil || len(boot.ConsumerReadyFrontierDigest) != 32 {
			return errors.New("boot缺消费者就绪摘要")
		}
		var selected *databasepb.SqlWriterMaintenanceReceipt_SourcePin
		for _, pin := range boot.SourcePins {
			if pin != nil && bytes.Equal(pin.Sha256, boot.ConsumerReadyFrontierDigest) {
				if selected != nil {
					return errors.New("consumer就绪原件摘要绑定不唯一")
				}
				selected = pin
			}
		}
		if selected == nil {
			return errors.New("boot遗漏正式consumer就绪原件，空组也必须提供")
		}
		raw, err := drainproof.ReadPin(selected, 16<<20)
		if err != nil {
			return err
		}
		report, err := drainproof.ParseConsumerReadiness(raw)
		if err != nil {
			return errors.New("consumer就绪原件不是正式ConsumerReadiness")
		}
		if err := verifyManualReadiness(plan, boot, report.ManualAssignments, sum[:]); err != nil {
			return err
		}
		{
			seenGroups := map[string]bool{}
			for _, current := range report.Groups {
				if current == nil || len(current.ProtoReflect().GetUnknown()) != 0 || !bytes.Equal(current.OperationId, plan.RunId) || !bytes.Equal(current.ManifestDigest, sum[:]) || !bytes.Equal(current.IssuerId, boot.IssuerId) || !bytes.Equal(current.InstanceId, boot.InstanceId) || current.Group == "" || current.GenerationId < 0 || current.MemberId == "" || len(current.MemberId) > 1024 || seenGroups[current.Group] || len(current.ExpectedPartitions) == 0 || len(current.ExpectedPartitions) > 16384 {
					return errors.New("consumer代次原件身份/配置目录无效")
				}
				seenGroups[current.Group] = true
				wanted := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
				for _, part := range current.ExpectedPartitions {
					if part == nil || part.Group != current.Group || wanted[key(part)] != nil || !proto.Equal(part, planned[key(part)]) {
						return errors.New("consumer配置目录不属于原P0全集")
					}
					wanted[key(part)] = part
				}
				state := groups[current.Group]
				if state == nil {
					state = &groupState{generation: current.GenerationId, expected: wanted, assigned: map[string]bool{}, members: map[string]bool{}}
					groups[current.Group] = state
				} else {
					if state.generation != current.GenerationId || len(state.expected) != len(wanted) {
						return errors.New("consumer组代次或配置范围不一致")
					}
					for id, part := range wanted {
						if !proto.Equal(part, state.expected[id]) {
							return errors.New("consumer副本配置分区不一致")
						}
					}
				}
				if state.members[current.MemberId] {
					return errors.New("consumer成员重复报告")
				}
				state.members[current.MemberId] = true
				for _, part := range current.AssignedPartitions {
					if part == nil {
						return errors.New("consumer assignment为空")
					}
					original := wanted[key(part)]
					if original == nil || state.assigned[key(part)] || !bytes.Equal(original.ClusterId, part.ClusterId) || !bytes.Equal(original.TopicId, part.TopicId) || original.ProtectedCommittedFloor != part.ProtectedCommittedFloor || original.InitialEarliest != part.InitialEarliest || part.FinalEarliest < 0 || part.FinalEarliest > part.ProtectedCommittedFloor || part.FinalCommitted < part.ProtectedCommittedFloor || part.FinalExclusiveEnd < part.FinalCommitted {
						return errors.New("consumer分配重复、物理身份变化、保留缺口或提交前沿未知")
					}
					state.assigned[key(part)] = true
				}
			}
		}
	}
	for _, state := range groups {
		if len(state.assigned) != len(state.expected) {
			return errors.New("consumer组的实际分配并集遗漏配置分区")
		}
		for id := range state.expected {
			if !state.assigned[id] {
				return errors.New("consumer分区未实际准备")
			}
		}
	}
	return nil
}

// VerifyAuthorityPublicationReadiness核全体全新boot、永久REGISTER原收据及每个实际open ACK。
func VerifyAuthorityPublicationReadiness(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact *databasepb.SqlWriterMaintenanceReceipt) error {
	ready := fact.GetEvidence().GetPublicationReadiness()
	if ready == nil || len(ready.PreparedBoots) != len(plan.RootIssuers) || len(ready.SourceOpenAcks) != len(plan.RootIssuers) {
		return errors.New("publication readiness不是原逻辑root全集")
	}
	released, err := catalog.MaintenanceReceipt(ctx, ready.ReleaseReceiptId)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(fact.PlanProto)
	if released.Kind != databasepb.SqlWriterMaintenanceReceipt_RELEASE || released.Phase != databasepb.SqlWriterMaintenanceReceipt_RELEASED || released.RunPhase != databasepb.SqlWriterDrainRun_SQL_RELEASED || released.Evidence == nil || !bytes.Equal(released.RunId, plan.RunId) || !bytes.Equal(released.ReceiptDigest, ready.ReleaseReceiptDigest) || !bytes.Equal(released.Evidence.ManifestDigest, sum[:]) || released.AttemptRevision != fact.AttemptRevision {
		return errors.New("prepared/open未绑定原SQL_RELEASED收据")
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	oldKeys := map[string]bool{}
	for _, role := range plan.RootIssuers {
		expected[string(role.IssuerId)] = role
	}
	for _, key := range plan.ActiveWriterKeys {
		oldKeys[string(key)] = true
	}
	boots := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerReadiness{}
	freshKeys := map[string]bool{}
	for _, boot := range ready.PreparedBoots {
		if boot == nil {
			return errors.New("prepared空原件")
		}
		prior := expected[string(boot.IssuerId)]
		if prior == nil || prior.LogicalRole != boot.LogicalRole || bytes.Equal(prior.InstanceId, boot.InstanceId) || len(boot.InstanceId) != 16 || boot.PreparedClosedMs < released.CommittedMs || boot.PreparedClosedMs > uint64(time.Now().UnixMilli()) || len(boot.ConsumerReadyFrontierDigest) != 32 || len(boot.SourcePins) == 0 || boots[string(boot.IssuerId)] != nil {
			return errors.New("prepared复用了旧boot/缺真实closed consumer前沿")
		}
		if len(prior.PhysicalWriterKey) != 0 {
			if len(boot.PhysicalWriterKey) != 16 || oldKeys[string(boot.PhysicalWriterKey)] || len(boot.RegistrationReceiptId) != 16 || len(boot.RegistrationReceiptDigest) != 32 {
				return errors.New("prepared复用旧物理writer或缺正式注册")
			}
			registration, err := catalog.LifecycleReceipt(ctx, boot.RegistrationReceiptId)
			if err != nil {
				return err
			}
			request := &databasepb.SqlWriterLifecycleReceipt_Request{}
			if registration.Kind != databasepb.SqlWriterLifecycleReceipt_REGISTER || !bytes.Equal(registration.RequestDigest, boot.RegistrationReceiptDigest) || proto.Unmarshal(registration.RequestProto, request) != nil || request.Writer == nil || !bytes.Equal(request.MaintenanceRunId, plan.RunId) || !bytes.Equal(request.Writer.WriterKey, boot.PhysicalWriterKey) || !bytes.Equal(request.Writer.InstanceId, boot.InstanceId) || request.Writer.State != databasepb.SqlWriterRegistry_ACTIVE || registration.ResultingEpoch != released.AfterEpoch {
				return errors.New("prepared注册不是本run发布后的全新同boot")
			}
			freshKeys[string(boot.PhysicalWriterKey)] = true
		} else if len(boot.PhysicalWriterKey) != 0 || len(boot.RegistrationReceiptId) != 0 || len(boot.RegistrationReceiptDigest) != 0 {
			return errors.New("无SQL逻辑root伪填物理注册")
		}
		for _, pin := range boot.SourcePins {
			if _, err = drainproof.ReadPin(pin, 16<<20); err != nil {
				return err
			}
		}
		boots[string(boot.IssuerId)] = boot
		delete(expected, string(boot.IssuerId))
	}
	if len(expected) != 0 {
		return errors.New("prepared root遗漏")
	}
	if err := verifyConsumerReadiness(plan, ready.PreparedBoots, fact.PlanProto); err != nil {
		return err
	}
	active, err := catalog.ActiveWriterKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range active {
		if oldKeys[string(key)] || !freshKeys[string(key)] {
			return errors.New("发布前旧writer未退役或出现未审定物理writer")
		}
		delete(freshKeys, string(key))
	}
	if len(freshKeys) != 0 {
		return errors.New("prepared物理writer已不在ACTIVE目录")
	}
	opened := map[string]bool{}
	for _, pin := range ready.SourceOpenAcks {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		ack := &databasepb.SqlWriterMaintenanceReceipt_SourceOpenAck{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, ack) != nil {
			return errors.New("实际open ACK非正式proto")
		}
		boot := boots[string(ack.IssuerId)]
		if boot == nil || opened[string(ack.IssuerId)] || !bytes.Equal(ack.InstanceId, boot.InstanceId) || !bytes.Equal(ack.PhysicalWriterKey, boot.PhysicalWriterKey) || ack.LogicalRole != boot.LogicalRole || !bytes.Equal(ack.RegistrationReceiptId, boot.RegistrationReceiptId) || !bytes.Equal(ack.RegistrationReceiptDigest, boot.RegistrationReceiptDigest) || !bytes.Equal(ack.OperationId, plan.RunId) || !bytes.Equal(ack.ManifestDigest, sum[:]) || ack.OpenedMs < boot.PreparedClosedMs || ack.OpenedMs > uint64(time.Now().UnixMilli()) || len(ack.OpenRequestId) != 16 || len(ack.AcceptedFrontierDigest) != 32 || len(ack.SourcePins) == 0 {
			return errors.New("实际open ACK换boot/run/注册或无真正提交点")
		}
		for _, source := range ack.SourcePins {
			if _, err = drainproof.ReadPin(source, 16<<20); err != nil {
				return err
			}
		}
		opened[string(ack.IssuerId)] = true
	}
	return nil
}

// VerifyAuthorityEmergency只为显式危险HOLD核原件；不生成NON_SQL_CLOSED或正常FROZEN结论。
func VerifyAuthorityEmergency(ctx context.Context, catalog *authoritysql.Catalog, plan *databasepb.SqlWriterMaintenanceReceipt_Plan, fact *databasepb.SqlWriterMaintenanceReceipt, requirePrior bool) error {
	danger := fact.GetEvidence().GetEmergency()
	if danger == nil || strings.TrimSpace(danger.Reason) == "" || len(danger.Reason) > 2048 || len(danger.SourceEvidence) == 0 || len(danger.SourceEvidence) > 128 {
		return errors.New("紧急HOLD缺危险原因和原始证据")
	}
	for _, pin := range danger.SourceEvidence {
		if _, err := drainproof.ReadPin(pin, 16<<20); err != nil {
			return err
		}
	}
	if !requirePrior {
		return nil
	}
	prior, err := catalog.MaintenanceReceipt(ctx, danger.DangerReceiptId)
	if err != nil {
		return err
	}
	if !bytes.Equal(prior.RunId, plan.RunId) || !bytes.Equal(prior.ReceiptDigest, danger.DangerReceiptDigest) || prior.AttemptRevision != fact.AttemptRevision || prior.Evidence == nil || prior.Evidence.GetEmergency() == nil || !bytes.Equal(prior.Evidence.ManifestDigest, fact.Evidence.ManifestDigest) {
		return errors.New("紧急HOLD危险原收据被替换")
	}
	return nil
}

// manualAssignmentKey按逻辑SOURCE、consumer及分区标识配置；boot变化另按阶段显式校验。
func manualAssignmentKey(row *databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) string {
	return fmt.Sprintf("%x/%q/%q/%d", row.IssuerId, row.ConsumerId, row.TopicName, row.Partition)
}
func validManualAssignment(row *databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) bool {
	return row != nil && len(row.ProtoReflect().GetUnknown()) == 0 && len(row.IssuerId) == 16 && len(row.InstanceId) == 16 && len(row.ConsumerId) > 0 && len(row.ConsumerId) <= 256 && !strings.ContainsAny(row.ConsumerId, "\x00\r\n") && len(row.ClusterId) > 0 && len(row.TopicId) == 16 && row.TopicName != "" && row.PartitionCount > 0 && row.PartitionCount <= 4096 && row.Partition < row.PartitionCount && row.TargetNodeId > 0 && row.Partition == row.TargetNodeId%row.PartitionCount && bytes.Equal(row.TargetInstanceId, row.InstanceId) && row.AssignmentRevision > 0 && row.StartOffset >= 0 && row.Position >= row.StartOffset && row.Earliest >= 0 && row.Earliest <= row.Position && row.ExclusiveEnd >= row.Position && row.ObservedMs > 0
}
func sameManualConfiguration(a, b *databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment) bool {
	return a != nil && b != nil && bytes.Equal(a.IssuerId, b.IssuerId) && a.ConsumerId == b.ConsumerId && bytes.Equal(a.ClusterId, b.ClusterId) && bytes.Equal(a.TopicId, b.TopicId) && a.TopicName == b.TopicName && a.PartitionCount == b.PartitionCount && a.Partition == b.Partition && a.TargetNodeId == b.TargetNodeId
}
func parseManualEvidence(raw []byte) (*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence, error) {
	if len(raw) == 0 || len(raw) > 16<<20 {
		return nil, errors.New("manual原件大小无效")
	}
	proof := &databasepb.SqlWriterMaintenanceReceipt_ManualConsumerEvidence{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, proof) != nil {
			return nil, errors.New("manual原件不是正式JSON")
		}
	} else if proto.Unmarshal(raw, proof) != nil {
		return nil, errors.New("manual原件不是正式二进制")
	}
	if len(proof.ProtoReflect().GetUnknown()) != 0 || len(proof.Assignments) == 0 || len(proof.Assignments) > 16384 || len(proof.SourcePins) > 128 {
		return nil, errors.New("manual原件为空或超预算")
	}
	for _, row := range proof.Assignments {
		if !validManualAssignment(row) {
			return nil, errors.New("manual原件身份/位置无效")
		}
	}
	for _, pin := range proof.SourcePins {
		if pin == nil || len(pin.ProtoReflect().GetUnknown()) != 0 {
			return nil, errors.New("manual证据pin无效")
		}
	}
	return proof, nil
}
func verifyManualBaseline(plan *databasepb.SqlWriterMaintenanceReceipt_Plan, catalog *kafkautil.DrainKafkaCatalog) error {
	if len(plan.ManualAssignments) > 16384 {
		return errors.New("manual P0目录超预算")
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
	sources := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, source := range plan.RootIssuers {
		sources[string(source.IssuerId)] = source
	}
	for _, row := range plan.ManualAssignments {
		if !validManualAssignment(row) {
			return errors.New("manual P0身份/位置无效")
		}
		source := sources[string(row.IssuerId)]
		if source == nil || !bytes.Equal(source.InstanceId, row.InstanceId) || !bytes.Equal(row.ClusterId, catalog.ClusterID) || !bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) || int(row.PartitionCount) != catalog.TopicPartitions[row.TopicName] {
			return errors.New("manual P0不属于实际SOURCE或物理topic目录")
		}
		key := manualAssignmentKey(row)
		if expected[key] != nil {
			return errors.New("manual P0重复配置")
		}
		expected[key] = row
	}
	seen := map[string]bool{}
	leaderBaselines := 0
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_MANUAL_KAFKA_BASELINE_V1" {
			leaderBaselines++
			if leaderBaselines != 1 || len(expected) == 0 {
				return errors.New("manual P0 leader基线重复或没有实际owner")
			}
			captured := &kafkautil.DrainManualKafkaBaseline{}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(captured) != nil || captured.CatalogSource == nil || captured.ObservedStartMS == 0 || captured.ObservedEndMS < captured.ObservedStartMS || captured.ObservedEndMS-captured.ObservedStartMS > 60000 || len(captured.Assignments) != len(expected) || !bytes.Equal(captured.CatalogSHA256, captured.CatalogSource.Sha256) {
				return errors.New("manual P0 leader基线格式/窗口无效")
			}
			actualCatalog, err := drainproof.ReadPin(captured.CatalogSource, 16<<20)
			if err != nil {
				return err
			}
			actualDigest, err := kafkautil.StableDrainCatalogDigest(actualCatalog)
			if err != nil {
				return err
			}
			mainRaw, err := json.Marshal(catalog)
			if err != nil {
				return err
			}
			mainDigest, err := kafkautil.StableDrainCatalogDigest(mainRaw)
			if err != nil || !bytes.Equal(actualDigest, mainDigest) {
				return errors.New("manual P0物理目录与group目录不同")
			}
			sampled := map[string]bool{}
			for _, encoded := range captured.Assignments {
				row := &databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment{}
				if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(encoded, row) != nil || !validManualAssignment(row) {
					return errors.New("manual P0 leader分区无效")
				}
				key := manualAssignmentKey(row)
				base := expected[key]
				if base == nil || sampled[key] || row.Earliest < base.Earliest || row.Earliest > base.Position || row.ExclusiveEnd < base.ExclusiveEnd || row.ObservedMs < captured.ObservedStartMS || row.ObservedMs > captured.ObservedEndMS || row.ObservedMs < base.ObservedMs {
					return errors.New("manual P0 leader保留范围变化或分区缺失")
				}
				stable := proto.Clone(row).(*databasepb.SqlWriterMaintenanceReceipt_ManualConsumerAssignment)
				stable.Earliest = base.Earliest
				stable.ExclusiveEnd = base.ExclusiveEnd
				stable.ObservedMs = base.ObservedMs
				if !proto.Equal(stable, base) {
					return errors.New("manual P0 leader采样替换了原生position/配置")
				}
				sampled[key] = true
			}
			continue
		}
		proof, err := parseManualEvidence(raw)
		if err != nil {
			continue
		}
		if !bytes.Equal(proof.OperationId, plan.RunId) || len(proof.ManifestDigest) != 0 {
			return errors.New("manual P0原件换run或形成manifest自引用")
		}
		source := sources[string(proof.Assignments[0].IssuerId)]
		if filepath.Base(pin.AbsolutePath) != "manual-baseline.report.json" || source == nil || !authorityPinSetEqual(source.SourceEvidence, proof.SourcePins) {
			return errors.New("manual P0原件未绑定固定SOURCE证据")
		}
		fixedDirectory := false
		for _, sourcePin := range source.SourceEvidence {
			if filepath.Clean(filepath.Dir(sourcePin.AbsolutePath)) == filepath.Clean(filepath.Dir(pin.AbsolutePath)) {
				fixedDirectory = true
			}
		}
		if !fixedDirectory {
			return errors.New("manual P0原件换私有目录")
		}
		for _, row := range proof.Assignments {
			if !bytes.Equal(row.IssuerId, source.IssuerId) || !bytes.Equal(row.InstanceId, source.InstanceId) {
				return errors.New("manual P0一个原件混入多个boot")
			}
		}
		for _, row := range proof.Assignments {
			key := manualAssignmentKey(row)
			if seen[key] || !proto.Equal(row, expected[key]) {
				return errors.New("manual P0配置不是实际owner原件或重复")
			}
			seen[key] = true
		}
		for _, source := range proof.SourcePins {
			if _, err := drainproof.ReadPin(source, 16<<20); err != nil {
				return err
			}
		}
	}
	if len(seen) != len(expected) || len(expected) > 0 && leaderBaselines != 1 {
		return errors.New("manual P0缺实际owner或leader基线原件")
	}
	return nil
}

func authorityKafkaBaseline(plan *databasepb.SqlWriterMaintenanceReceipt_Plan) (*kafkautil.DrainKafkaCatalog, error) {
	var catalog *kafkautil.DrainKafkaCatalog
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return nil, err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) != nil || marker.Schema != "SQL_DRAIN_KAFKA_CATALOG_V2" {
			continue
		}
		if catalog != nil {
			return nil, errors.New("P0 Kafka原目录重复")
		}
		catalog = &kafkautil.DrainKafkaCatalog{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(catalog) != nil {
			return nil, errors.New("P0 Kafka目录schema未知")
		}
	}
	if catalog == nil || catalog.ObservedStartMS == 0 || catalog.ObservedEndMS < catalog.ObservedStartMS || catalog.ObservedEndMS-catalog.ObservedStartMS > 60000 || len(catalog.ClusterID) == 0 || len(catalog.TopicNames) == 0 || len(catalog.TopicNames) > 129 || len(catalog.TopicIDs) != len(catalog.TopicNames) || len(catalog.TopicPartitions) != len(catalog.TopicNames) || len(catalog.TopicConfigs) != len(catalog.TopicNames) || len(catalog.BrokerConfigs) == 0 || len(catalog.BrokerConfigs) > 128 {
		return nil, errors.New("P0 Kafka目录/有效保留配置全集缺失")
	}
	topics := map[string]bool{}
	groups := map[string]bool{}
	subscriptions := map[string]map[uint32]bool{}
	for _, row := range plan.KafkaPartitions {
		if !bytes.Equal(row.ClusterId, catalog.ClusterID) || !bytes.Equal(row.TopicId, catalog.TopicIDs[row.TopicName]) {
			return nil, errors.New("P0 typed分区与实际UUID不符")
		}
		topics[row.TopicName] = true
		groups[row.Group] = true
		key := row.Group + "\x00" + row.TopicName
		if subscriptions[key] == nil {
			subscriptions[key] = map[uint32]bool{}
		}
		subscriptions[key][row.Partition] = true
	}
	for _, row := range plan.ManualAssignments {
		if row == nil {
			return nil, errors.New("manual P0空分区")
		}
		topics[row.TopicName] = true
	}
	names := []string{"__consumer_offsets"}
	for topic := range topics {
		names = append(names, topic)
	}
	if !releaseExactStrings(names, catalog.TopicNames) {
		return nil, errors.New("P0漏历史topic或未知业务topic")
	}
	groupNames := []string{}
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	if !releaseExactStrings(groupNames, catalog.GroupNames) {
		return nil, errors.New("P0漏历史group")
	}
	for key, partitions := range subscriptions {
		parts := strings.SplitN(key, "\x00", 2)
		n := catalog.TopicPartitions[parts[1]]
		if n < 1 || n > 4096 || len(partitions) != n {
			return nil, errors.New("P0订阅partition不是实际全量")
		}
		for p := 0; p < n; p++ {
			if !partitions[uint32(p)] {
				return nil, errors.New("P0分区不连续/缺失")
			}
		}
	}
	expectedTopicConfig := []string{"retention.ms", "retention.bytes", "cleanup.policy", "segment.ms", "segment.bytes", "delete.retention.ms", "min.insync.replicas", "unclean.leader.election.enable"}
	expectedBrokerConfig := []string{"log.retention.ms", "log.retention.bytes", "log.segment.bytes", "log.retention.check.interval.ms", "unclean.leader.election.enable", "address"}
	keys := func(m map[string]string) []string {
		out := []string{}
		for key, value := range m {
			if value == "" {
				return nil
			}
			out = append(out, key)
		}
		return out
	}
	for _, topic := range catalog.TopicNames {
		if len(catalog.TopicIDs[topic]) != 16 || catalog.TopicPartitions[topic] < 1 || !releaseExactStrings(keys(catalog.TopicConfigs[topic]), expectedTopicConfig) {
			return nil, errors.New("P0 topic UUID/有效retention配置缺项")
		}
		if topic != "__consumer_offsets" && catalog.TopicConfigs[topic]["cleanup.policy"] != "delete" {
			return nil, errors.New("P0业务topic可压缩删除，不能证明完整排空")
		}
	}
	for id, config := range catalog.BrokerConfigs {
		n, err := strconv.ParseInt(id, 10, 32)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != id || !releaseExactStrings(keys(config), expectedBrokerConfig) || !maintenanceAddress(config["address"]) {
			return nil, errors.New("P0 broker/有效retention配置缺项")
		}
	}
	var baseline *kafkautil.DrainKafkaBaseline
	for _, pin := range plan.SourcePins {
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return nil, err
		}
		var marker struct {
			Schema string `json:"schema"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.Schema == "SQL_DRAIN_KAFKA_BASELINE_V2" {
			if baseline != nil {
				return nil, errors.New("P0 C0/L0原件重复")
			}
			baseline = &kafkautil.DrainKafkaBaseline{}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(baseline) != nil {
				return nil, errors.New("P0 C0/L0原件损坏")
			}
		}
	}
	if baseline == nil || baseline.ObservedMS == 0 || len(baseline.Partitions) != len(plan.KafkaPartitions) {
		return nil, errors.New("P0缺实际固定C0/L0分区全原件")
	}
	catalogRaw, _ := json.Marshal(catalog)
	catalogSum := sha256.Sum256(catalogRaw)
	if !bytes.Equal(catalogSum[:], baseline.CatalogSHA256) {
		return nil, errors.New("P0 C0/L0未绑定同目录")
	}
	observed := map[string]*databasepb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{}
	for _, row := range baseline.Partitions {
		if row == nil {
			return nil, errors.New("P0空C0/L0原件")
		}
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		if observed[key] != nil {
			return nil, errors.New("P0 C0/L0分区重复")
		}
		observed[key] = row
	}
	for _, row := range plan.KafkaPartitions {
		key := fmt.Sprintf("%s/%s/%d", row.Group, row.TopicName, row.Partition)
		base := observed[key]
		if base == nil || !bytes.Equal(base.ClusterId, row.ClusterId) || !bytes.Equal(base.TopicId, row.TopicId) || base.InitialEarliest != row.InitialEarliest || base.ProtectedCommittedFloor != row.ProtectedCommittedFloor {
			return nil, errors.New("P0 typed C0/L0不是实际原件")
		}
		if base.FinalCommitted != base.ProtectedCommittedFloor && !(base.FinalCommitted == -1 && base.InitialEarliest == 0 && base.ProtectedCommittedFloor == 0 && base.FinalExclusiveEnd == 0) {
			return nil, errors.New("P0实际committed不构成保护floor")
		}
		delete(observed, key)
	}
	if err := verifyManualBaseline(plan, catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

func authorityPinSetEqual(expected, actual []*databasepb.SqlWriterMaintenanceReceipt_SourcePin) bool {
	if len(expected) != len(actual) {
		return false
	}
	set := map[string][]byte{}
	for _, pin := range expected {
		if pin == nil || set[pin.AbsolutePath] != nil {
			return false
		}
		set[pin.AbsolutePath] = pin.Sha256
	}
	for _, pin := range actual {
		if pin == nil || !bytes.Equal(set[pin.AbsolutePath], pin.Sha256) {
			return false
		}
		delete(set, pin.AbsolutePath)
	}
	return len(set) == 0
}
func verifyAuthorityPublicationOriginals(seeds []*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure, publication *databasepb.SqlWriterMaintenanceReceipt_PublicationEvidence) error {
	if publication == nil || len(publication.SourceEvidence) != len(seeds) {
		return errors.New("角色阶段缺实际app报告原件全集")
	}
	inventory := &databasepb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: seeds}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(inventory)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], publication.IssuerInventoryDigest) {
		return errors.New("角色阶段issuer inventory摘要偏离P0完整原序")
	}
	expected := map[string]*databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
	for _, role := range publication.LogicalRoles {
		if role == nil || expected[string(role.IssuerId)] != nil {
			return errors.New("角色阶段实际issuer重复")
		}
		expected[string(role.IssuerId)] = role
	}
	seenPins := map[string]bool{}
	for _, pin := range publication.SourceEvidence {
		if pin == nil || seenPins[pin.AbsolutePath] {
			return errors.New("app报告原件重复")
		}
		seenPins[pin.AbsolutePath] = true
		raw, err := drainproof.ReadPin(pin, 16<<20)
		if err != nil {
			return err
		}
		original := &databasepb.SqlWriterMaintenanceReceipt_IssuerClosure{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, original) != nil || !proto.Equal(original, expected[string(original.IssuerId)]) {
			return errors.New("阶段closure不是该app原报告完整正文")
		}
		delete(expected, string(original.IssuerId))
	}
	if len(expected) != 0 {
		return errors.New("app原报告遗漏")
	}
	return nil
}
