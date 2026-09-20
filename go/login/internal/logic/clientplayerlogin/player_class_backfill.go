package clientplayerloginlogic

import (
    "context"
    "fmt"
    "login/internal/logic/pkg/sessionmanager"
    comppb "proto/common/component"
    dbpb "proto/common/database"

    "github.com/redis/go-redis/v9"
    "google.golang.org/protobuf/proto"
)

// backfillPlayerIdentity 只在首次入场前补齐存档里缺失的角色身份:旧存档漏存的职业,
// 以及角色名副本 PlayerProfileComp.name。PlayerAllData 在建角时还不存在,首次 EnterGame
// 才由预加载建出,所以名字只能和职业走同一条路、同一次整字节 CAS 写进去。
// 热缓存、子表缓存、DB 加载都汇入 applyLoadedPlayerSession，所以不会漏掉缓存命中。
// 已有会话的角色仍由 scene 持有权威数据；旧账号职业为零时也不猜测职业。
// 名字只填空缺、不覆盖已有值:v1 无改名,存档里已有的名字与注册表一致,覆盖只会制造竞态。
// 要补的名字由入场前的 resolveEnterName 定好放在 state.playerName;本函数不发 RPC。
func (l *EnterGameLogic) backfillPlayerIdentity(ctx context.Context, existing *sessionmanager.PlayerSession, state enterGameSessionState) error {
    if existing != nil || (state.classID == 0 && state.playerName == "") {
        return nil
    }
    if err := ctx.Err(); err != nil {
        return err
    }
    if state.playerLockKey == "" || state.playerLockToken == "" {
        return fmt.Errorf("身份补齐缺少玩家登录锁: player=%d", state.playerID)
    }
    parent := &dbpb.PlayerAllData{}
    key := fmt.Sprintf("%s:%d", parent.ProtoReflect().Descriptor().FullName(), state.playerID)
    previous, err := l.svcCtx.RedisClient.Get(ctx, key).Bytes()
    if err != nil {
        return fmt.Errorf("读取身份补齐存档: %w", err)
    }
    if err := proto.Unmarshal(previous, parent); err != nil {
        return fmt.Errorf("解析身份补齐存档: %w", err)
    }
    player := parent.GetPlayerDatabaseData()
    if player == nil || player.GetPlayerId() != state.playerID {
        return fmt.Errorf("身份补齐存档与玩家不匹配: player=%d", state.playerID)
    }
    needClass := player.GetUint32PbComponent().GetClass() == 0 && state.classID != 0
    needName := player.GetProfileComponent().GetName() == "" && state.playerName != ""
    if !needClass && !needName {
        return nil
    }
    if needClass {
        if player.Uint32PbComponent == nil {
            player.Uint32PbComponent = &comppb.PlayerUint32Comp{}
        }
        player.Uint32PbComponent.Class = state.classID
    }
    if needName {
        if player.ProfileComponent == nil {
            player.ProfileComponent = &comppb.PlayerProfileComp{}
        }
        player.ProfileComponent.Name = state.playerName
    }
    updated, err := proto.Marshal(parent)
    if err != nil {
        return fmt.Errorf("序列化身份补齐存档: %w", err)
    }
    // 只读检查 player_locator 会话与 SceneManager 权威位置键：正常离场先删会话，
    // 后清位置，不能把两步之间仍在场的角色当成离线档。完整字节 CAS 防止旧快照
    // 覆盖 scene 并发存盘；令牌校验阻止失锁的旧登录链继续写入。保持原有缓存 TTL。
    result, err := backfillPlayerClassScript.Run(ctx, l.svcCtx.RedisClient,
        []string{key, state.playerLockKey,
            fmt.Sprintf("player:session:%d", state.playerID),
            fmt.Sprintf("player:%d:location", state.playerID)},
        state.playerLockToken, previous, updated).Int()
    if err != nil {
        return fmt.Errorf("保存身份补齐存档: %w", err)
    }
    switch result {
    case 1, 2: // 成功，或新的会话/场景仍持有权威数据而跳过。
        return nil
    case -1:
        return fmt.Errorf("身份补齐时玩家登录锁已失效: player=%d", state.playerID)
    default:
        return fmt.Errorf("身份补齐时存档已变化，请重试入场: player=%d", state.playerID)
    }
}

// backfillPlayerClassScript 沿用原名:脚本本身与返回码语义没有变,只是写入的字节里多了名字;
// go/shared/scenenode/locator.go 等处的注释按这个名字引用 KEYS[4]。

var backfillPlayerClassScript = redis.NewScript(`
if redis.call("GET", KEYS[2]) ~= ARGV[1] then
    return -1
end
if redis.call("EXISTS", KEYS[3], KEYS[4]) > 0 then
    return 2
end
if redis.call("GET", KEYS[1]) ~= ARGV[2] then
    return 0
end
redis.call("SET", KEYS[1], ARGV[3], "KEEPTTL")
return 1
`)