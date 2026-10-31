// Package playerbundle 是线上玩家导出包的格式和判定:debug_fetch 写,debug_import 读
// (docs/ops/online-debug-data-fetch.md)。除了读一行路由,这里不碰数据库,取数和写入在两个命令里。
package playerbundle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dbpb "proto/common/database"
	storepb "proto/player_store"
	"shared/placement"
)

// Format 是导出包的格式号。字段只增不改名;不兼容的改动换一个格式号,导入端按它拒绝。
const Format = "player-store-bundle-v1"

const ownerKind = int32(storepb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)

// Bundle 是一个玩家的导出包。ID、代次和序号写成字符串:64 位整数在常见 JSON 工具里会丢精度。
type Bundle struct {
	Format         string   `json:"format"`
	PlayerID       uint64   `json:"player_id,string"`
	ExportedAtMs   int64    `json:"exported_at_ms"`
	Reason         string   `json:"reason"`
	ServerRevision string   `json:"server_revision,omitempty"`
	Analysis       Analysis `json:"analysis"`
	Route          Route    `json:"route"`
	Rows           []Row    `json:"rows"`
	Global         Global   `json:"global"`
}

// Analysis 是读包时最先看的结论。
type Analysis struct {
	// Online:写权行有持有者且没有释放。玩家在线,或者上次没有正常释放;这时导出的不是最终态。
	Online    bool     `json:"online"`
	Frozen    bool     `json:"frozen"`
	SaveSeq   uint64   `json:"save_seq,string"`
	SchemaGen uint32   `json:"schema_gen"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Route 是全局库 player_route 的一行。
type Route struct {
	HomeZone    uint32 `json:"home_zone"`
	StorageID   uint32 `json:"storage_id"`
	FrozenRunID string `json:"frozen_run_id,omitempty"`
	Rev         uint64 `json:"rev"`
	StoreSchema string `json:"store_schema"`
}

// Row 是 player_store 的一行。Payload 是原字节,导入只用它;Decoded 只给人看,解码用的是导出时本地检出的 proto。
type Row struct {
	Kind           int32           `json:"kind"`
	KindName       string          `json:"kind_name"`
	OwnerEpoch     uint64          `json:"owner_epoch,string"`
	SaveSeq        uint64          `json:"save_seq,string"`
	SchemaGen      uint32          `json:"schema_gen"`
	HolderInstance string          `json:"holder_instance,omitempty"`
	Released       uint32          `json:"released"`
	Payload        []byte          `json:"payload"`
	Decoded        json.RawMessage `json:"decoded,omitempty"`
	DecodeNote     string          `json:"decode_note,omitempty"`
}

// Global 是全局库里和这个玩家有关的记录,用来对比和追查。读不到只记 Errors,不影响存档部分。
type Global struct {
	Account        []map[string]any `json:"account,omitempty"`
	Snapshots      []map[string]any `json:"snapshots,omitempty"`
	TransactionLog []map[string]any `json:"transaction_log,omitempty"`
	Errors         []string         `json:"errors,omitempty"`
}

// dataMessage 按 kind 给出 payload 的消息类型。前四种都是 player_database 按分组投影的片段。
func dataMessage(kind int32) proto.Message {
	switch storepb.PlayerStoreKind(kind) {
	case storepb.PlayerStoreKind_PLAYER_STORE_KIND_HOT, storepb.PlayerStoreKind_PLAYER_STORE_KIND_ASSET,
		storepb.PlayerStoreKind_PLAYER_STORE_KIND_GROWTH, storepb.PlayerStoreKind_PLAYER_STORE_KIND_PROFILE:
		return &dbpb.PlayerDatabase{}
	case storepb.PlayerStoreKind_PLAYER_STORE_KIND_LEGACY1:
		return &dbpb.PlayerDatabase_1{}
	default:
		return nil
	}
}

// Decode 补上给人看的 KindName、Decoded。解不开或者带未知字段只记 DecodeNote:原字节还在,包仍然可用。
func (r *Row) Decode() {
	r.KindName = storepb.PlayerStoreKind(r.Kind).String()
	msg := dataMessage(r.Kind)
	if msg == nil {
		if r.Kind != ownerKind {
			r.DecodeNote = "unknown kind: the local checkout is older than the server"
		}
		return
	}
	if err := proto.Unmarshal(r.Payload, msg); err != nil {
		r.DecodeNote = "cannot decode payload: " + err.Error()
		return
	}
	decoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		r.DecodeNote = "cannot render payload: " + err.Error()
		return
	}
	r.Decoded = decoded
	if len(msg.ProtoReflect().GetUnknown()) > 0 {
		r.DecodeNote = "payload has unknown fields: the local checkout is older than the server"
	}
}

// Analyze 从路由和存档行算出 Analysis。
func Analyze(route Route, rows []Row) Analysis {
	a := Analysis{Frozen: route.FrozenRunID != ""}
	if a.Frozen {
		a.Warnings = append(a.Warnings, "route is frozen by run "+route.FrozenRunID)
	}
	owner := findOwner(rows)
	if owner == nil {
		a.Warnings = append(a.Warnings, "no owner row: the role does not exist or was never finished")
		return a
	}
	a.SaveSeq, a.SchemaGen = owner.SaveSeq, owner.SchemaGen
	a.Online = owner.Released == 0 && owner.HolderInstance != ""
	if a.Online {
		a.Warnings = append(a.Warnings, "owner row is held by "+owner.HolderInstance+": this is not the final state, export again after the player leaves")
	}
	for i := range rows {
		if rows[i].DecodeNote != "" {
			a.Warnings = append(a.Warnings, fmt.Sprintf("%s: %s", rows[i].KindName, rows[i].DecodeNote))
		}
	}
	return a
}

func findOwner(rows []Row) *Row {
	for i := range rows {
		if rows[i].Kind == ownerKind {
			return &rows[i]
		}
	}
	return nil
}

// Parse 读导出包并核对格式号。
func Parse(raw []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("parse bundle: %w", err)
	}
	if b.Format != Format {
		return nil, fmt.Errorf("bundle format %q is not %q", b.Format, Format)
	}
	return &b, nil
}

// LocalOwner 是本地目标角色写权行的现状。
type LocalOwner struct {
	SaveSeq   uint64
	SchemaGen uint32
	Holder    []byte
	Released  uint32
}

// ImportPlan 是一次导入要写的内容。
type ImportPlan struct {
	SaveSeq   uint64
	SchemaGen uint32
	Rows      []Row
}

// PlanImport 判定能不能把包导进本地角色,并给出要写的内容。写权行只改序号和世代:代次和持有者留给本地,
// 本地发号器发的代次和线上没有关系,抄过来会让本地再也认领不了。序号只进不退,世代取大,它们是存盘判定的依据。
// binaryGen 是本地检出的世代:包比它新时本地 scene 认领不了,在这里就拒绝。
func PlanImport(b *Bundle, local LocalOwner, binaryGen uint32) (ImportPlan, error) {
	owner := findOwner(b.Rows)
	if owner == nil {
		return ImportPlan{}, errors.New("bundle has no owner row")
	}
	if local.Released == 0 && len(local.Holder) > 0 {
		return ImportPlan{}, fmt.Errorf("local player is held by %q: log it out first", local.Holder)
	}
	if owner.SchemaGen > binaryGen {
		return ImportPlan{}, fmt.Errorf("bundle schema generation %d is newer than this checkout's %d: check out the server revision of the bundle (%s)",
			owner.SchemaGen, binaryGen, b.ServerRevision)
	}
	plan := ImportPlan{SaveSeq: max(local.SaveSeq, owner.SaveSeq), SchemaGen: max(local.SchemaGen, owner.SchemaGen)}
	seen := map[int32]bool{}
	for _, row := range b.Rows {
		if row.Kind == ownerKind {
			continue
		}
		if dataMessage(row.Kind) == nil {
			return ImportPlan{}, fmt.Errorf("bundle row kind %d is unknown to this checkout", row.Kind)
		}
		if seen[row.Kind] {
			return ImportPlan{}, fmt.Errorf("bundle has two rows of kind %d", row.Kind)
		}
		seen[row.Kind] = true
		plan.Rows = append(plan.Rows, row)
	}
	return plan, nil
}

// ReadRoute 读一个玩家的路由并算出存档所在的库。found=false 是没有路由行或者路由没有落点。
func ReadRoute(ctx context.Context, global *sql.DB, playerID uint64) (route Route, found bool, err error) {
	var home, storage sql.NullInt64
	var frozen sql.NullString
	err = global.QueryRowContext(ctx, "SELECT home_zone,storage_id,frozen_run_id,rev FROM player_route WHERE route_shard=? AND player_id=?",
		playerID%64, playerID).Scan(&home, &storage, &frozen, &route.Rev)
	if errors.Is(err, sql.ErrNoRows) {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, fmt.Errorf("read player_route: %w", err)
	}
	route.HomeZone, route.StorageID, route.FrozenRunID = uint32(home.Int64), uint32(storage.Int64), frozen.String
	// 建角恒写落点,没有落点就是路由不完整,和 go/db 一样不猜库。
	schema, ok := placement.StoreDBName(route.StorageID)
	if !ok {
		return Route{}, false, nil
	}
	route.StoreSchema = schema
	return route, true, nil
}
