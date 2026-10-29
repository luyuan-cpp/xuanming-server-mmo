package logic

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dspb "proto/data_service"
	smpb "proto/scene_manager"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var errLocationProjectionCorrupt = errors.New("场景位置投影损坏")

// 原PB毒数据沿既有清理契约处理；新sidecar损坏不能被当成清理成功，调用方必须保留原任务。
var errLocationRevisionProjectionCorrupt = errors.New("场景位置原修订标签损坏")

func locationRevisionKey(player uint64) string {
	return fmt.Sprintf("player:%d:location_revision", player)
}
func locationRevisionFloorKey(player uint64) string {
	return fmt.Sprintf("player:%d:location_revision_floor", player)
}
func revisionText(revision uint64) string { return fmt.Sprintf("%020d", revision) }
func optionalRevisionText(revision uint64) string {
	if revision == 0 {
		return ""
	}
	return revisionText(revision)
}
func locationRevisionTag(revision uint64, raw string) string {
	if revision == 0 || raw == "" {
		return ""
	}
	return fmt.Sprintf("%s:%x", revisionText(revision), sha256.Sum256([]byte(raw)))
}

func taggedLocationRevision(tag, raw string) (uint64, error) {
	if tag == "" {
		return 0, nil
	}
	if len(tag) != 85 || tag[20] != ':' {
		return 0, fmt.Errorf("位置原修订标签损坏")
	}
	revision, err := strconv.ParseUint(tag[:20], 10, 64)
	if err != nil || revision == 0 || tag != locationRevisionTag(revision, raw) {
		return 0, fmt.Errorf("位置原修订与原文不匹配")
	}
	return revision, nil
}

// floor只表示已观察的SQL修订下界，不是角色授权。20位十进制串比较不经Lua double。
const luaObserveLocationRevision = `
local floor = redis.call('GET', KEYS[1])
if floor ~= false and (string.len(floor) ~= 20 or string.find(floor, '%D')) then return -1 end
if floor ~= false and floor > ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
return 1
`

func lookupCurrentRole(ctx context.Context, sc *svc.ServiceContext, player uint64) (*dspb.AccountRoleOwnership, error) {
	if sc.RoleDirectory == nil {
		return nil, status.Error(codes.Unavailable, "角色目录只读依赖未就绪")
	}
	// enter_scene_stage_seconds{stage="role_lookup"} 只量这一次 RPC(不论结局);口径(zone_id 恒为 0、含 LeaveScene)见 EnterSceneStageRoleLookup。
	lookupStart := time.Now()
	role, err := sc.RoleDirectory.Lookup(ctx, player)
	metrics.ObserveEnterSceneStage(0, metrics.EnterSceneStageRoleLookup, time.Since(lookupStart))
	if err != nil {
		return nil, status.Error(codes.Unavailable, "角色目录强读失败")
	}
	if role == nil || role.Role == nil || role.Role.PlayerId != player || role.Account == "" || role.OwnerRevision == 0 || role.TradeListingId != 0 {
		return nil, status.Error(codes.FailedPrecondition, "角色当前归属不可用于场景调度")
	}
	return role, nil
}

func observeCurrentRevision(sc *svc.ServiceContext, player, revision uint64) error {
	result, err := sc.Redis.Eval(luaObserveLocationRevision, []string{locationRevisionFloorKey(player)}, revisionText(revision))
	if err != nil {
		return err
	}
	if fmt.Sprint(result) != "1" {
		return status.Error(codes.FailedPrecondition, "场景投影已观察更新的归属修订或floor损坏")
	}
	return nil
}

const luaReadLocationSnapshot = `return {redis.call('GET',KEYS[1]) or '',redis.call('GET',KEYS[2]) or ''}`

func getPlayerLocationSnapshot(sc *svc.ServiceContext, player uint64) (*smpb.PlayerLocation, string, string, error) {
	out, err := sc.Redis.Eval(luaReadLocationSnapshot, []string{getPlayerLocationKey(player), locationRevisionKey(player)})
	if err != nil {
		return nil, "", "", err
	}
	items, ok := out.([]interface{})
	if !ok || len(items) != 2 {
		return nil, "", "", fmt.Errorf("位置快照结果非法")
	}
	raw, tag := fmt.Sprint(items[0]), fmt.Sprint(items[1])
	if raw == "" {
		return nil, "", tag, nil
	}
	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal([]byte(raw), loc); err != nil {
		return nil, "", "", fmt.Errorf("%w: %v", errLocationProjectionCorrupt, err)
	}
	if _, err := taggedLocationRevision(tag, raw); err != nil {
		return nil, "", "", fmt.Errorf("%w: %v", errLocationRevisionProjectionCorrupt, err)
	}
	return loc, raw, tag, nil
}

// 已知旧修订的调度缓存可以修复；无标签不能从当前目录补权，继续走既有holder正证协议。
func locationForRevision(sc *svc.ServiceContext, player, revision uint64) (*smpb.PlayerLocation, string, string, error) {
	loc, raw, tag, err := getPlayerLocationSnapshot(sc, player)
	if err != nil || loc == nil {
		return loc, raw, tag, err
	}
	previous, err := taggedLocationRevision(tag, raw)
	if err != nil {
		return nil, "", "", err
	}
	if previous == 0 || previous == revision {
		return loc, raw, tag, nil
	}
	if previous > revision {
		return nil, "", "", errOwnerEpochConflict
	}
	outcome, err := deletePlayerLocationGuardedWithTag(sc, player, raw, loc.ZoneId, loc.SceneId, leaveDeleteModeAny, 0, tag, revision)
	if err != nil {
		return nil, "", "", err
	}
	if outcome != leaveOutcomeDeleted && outcome != leaveOutcomeAlreadyGone {
		return nil, "", "", errOwnerEpochConflict
	}
	return nil, "", "", nil
}

// 可选参数仅兼容直接Lua/无目录测试夹具；全部生产落点显式传非零原R与观察tag。
const luaPlacementRevisionCheck = `
local requested_revision = ARGV[6]
if requested_revision ~= nil and requested_revision ~= '' then
    if redis.call('GET',KEYS[5]) ~= requested_revision then return 0 end
    local tag = redis.call('GET',KEYS[4]) or ''
    if redis.call('GET',KEYS[2]) == ARGV[2] and tag == ARGV[7] then
    elseif tag ~= ARGV[8] then return 0 end
end
`
const luaWritePlacementRevision = `
if requested_revision ~= nil and requested_revision ~= '' and ARGV[7] ~= '' then
    redis.call('SET',KEYS[4],ARGV[7])
elseif KEYS[4] ~= nil then
    redis.call('DEL',KEYS[4])
end
`

func restoreRevisionTag(placed placedLocation, raw string) string {
	if raw == "" || placed.previousTag == "" {
		return ""
	}
	text, _, ok := strings.Cut(placed.previousTag, ":")
	if !ok {
		return ""
	}
	r, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return ""
	}
	return locationRevisionTag(r, raw)
}
