# 主世界场景分线系统 (Main World Scene Lines)

## 概念

每个主世界地图 (config_id) 可以有 N 条线 (channel)。玩家进入时自动分配到人数最少的线。线在启动时预创建，生命周期与区服一致。

## 配置

`scene_manager_service.yaml`:
```yaml
WorldChannelCount: 3   # 每个主世界场景的线数，默认 1（向后兼容）
# 地图列表来自 generated World 表；自动伸缩启用后，Redis desired 值是运行期权威。
```

## Redis 数据结构

| Key | Type | 说明 |
|-----|------|------|
| `world_channels:zone:{zoneId}:{confId}` | SET | 该地图所有线的 sceneId |
| `instance:{sceneId}:player_count` | STRING | 每条线的玩家数（与副本共用） |
| `scene:{sceneId}:node` | STRING | sceneId → nodeId 映射 |
| `scene:{sceneId}:zone` | STRING | sceneId → zoneId 映射 |
| `node:zone:{zoneId}:{nodeId}:player_count` | STRING | 节点玩家聚合（nodeId 仅 zone 内唯一） |

## 核心流程

### 启动创建线
```
initWorldScenesForZone(zoneId, confIds)
  for each confId:
    existing = SMEMBERS(world_channels:zone:{zoneId}:{confId})
    for i = len(existing) to WorldChannelCount-1:
      sceneId = SceneIDGen.Generate()  # etcd 租约保护的 Snowflake；失租后 fail-closed
      SET scene:{sceneId}:node = assignNodeByHash(confId*1000+i, nodes)
      SADD world_channels:zone:{zoneId}:{confId} sceneId
      SET instance:{sceneId}:player_count = 0
    for each sceneId in all lines:
      RPC → C++ CreateScene(config_id, scene_id)   // 幂等
```

### 玩家选线
```
GetBestWorldChannel(confId, zoneId)
  members = SMEMBERS(world_channels:zone:{zoneId}:{confId})
  for each sceneId in members:
    count = GET(instance:{sceneId}:player_count)
  return sceneId with lowest count
```

### 玩家进出
- `EnterScene` → `IncrInstancePlayerCount(zoneId, sceneId)`；自动选线走原子预留
- `LeaveScene` → `DecrInstancePlayerCount(zoneId, sceneId)`

有在线玩家的频道不会在活节点之间热迁移；当前没有跨节点玩家状态传输协议。
节点确认死亡后的 urgent 路径只重建场景映射，玩家仍需走重连恢复。
同一 `(zoneId,nodeId)` 出现重复 etcd 注册也不是“节点死亡”；选线、懒重分配和
rebalance 都会跳过该歧义身份并保持原 ownership，直到注册唯一性恢复。

## Proto 变更

`proto/scene/scene.proto`:
```protobuf
message CreateSceneRequest {
  uint32 config_id = 1;
  uint64 scene_id = 2;  // Go 分配的唯一 ID，C++ 按此去重
}
```

## C++ 去重逻辑

`scene_handler.cpp` CreateScene:
- `scene_id > 0`: 按 `guid == scene_id` 去重（支持同 config_id 多实体 = 多线）
- `scene_id == 0`: 按 `config_id` 去重（向后兼容）
- 新实体的 `guid` 使用 Go 传入的 `scene_id`

## 节点分配

`assignNodeByHash(confId*1000 + lineIdx, sortedNodes)` — 不同线可分配到不同节点，哈希确定性分配。

## 向后兼容

- `WorldChannelCount` 默认 1 → 行为与旧版完全一致（每个 confId 一个场景）
- `scene_id = 0` 时 C++ 仍按 `config_id` 去重

## 测试覆盖

| 测试用例 | 验证内容 |
|---------|---------|
| `TestInitWorldScenes_MultipleLines` | 3 条线各有独立 sceneId |
| `TestInitWorldScenes_MultipleLines_Idempotent` | 重复初始化不产生多余线 |
| `TestGetBestWorldChannel_SelectsLowestPlayerCount` | 选择人数最少的线 |
| `TestGetBestWorldChannel_AllEmpty` | 所有线为空时仍能返回 |
| `TestCreateScene_MainWorld_MultipleLines_ReturnsLeastLoaded` | 高负载线→自动切换 |
| `TestCreateScene_MainWorld_ChannelCountDefault1_BackwardCompat` | LineCount=1 等同旧行为 |
