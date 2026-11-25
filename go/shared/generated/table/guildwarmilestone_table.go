
package table

import (
    "fmt"
    "math/rand/v2"
    "os"
    "path/filepath"
    "sync/atomic"

    "google.golang.org/protobuf/encoding/protojson"
    "google.golang.org/protobuf/proto"
    pb "shared/generated/pb/table"
)



// guildwarmilestoneSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type guildwarmilestoneSnapshot struct {
    data   []*pb.GuildWarMilestoneTable
    kvData map[uint32]*pb.GuildWarMilestoneTable
    idxRewardItem1Id map[uint32][]*pb.GuildWarMilestoneTable
    idxRewardItem2Id map[uint32][]*pb.GuildWarMilestoneTable
}

type GuildWarMilestoneTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[guildwarmilestoneSnapshot]
}

var GuildWarMilestoneTableManagerInstance = NewGuildWarMilestoneTableManager()

func NewGuildWarMilestoneTableManager() *GuildWarMilestoneTableManager {
    m := &GuildWarMilestoneTableManager{}
    m.snap.Store(&guildwarmilestoneSnapshot{
        kvData: make(map[uint32]*pb.GuildWarMilestoneTable),
        idxRewardItem1Id: make(map[uint32][]*pb.GuildWarMilestoneTable),
        idxRewardItem2Id: make(map[uint32][]*pb.GuildWarMilestoneTable),
    })
    return m
}

func (m *GuildWarMilestoneTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *GuildWarMilestoneTableManager) swapSnapshot(snap *guildwarmilestoneSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *GuildWarMilestoneTableManager) loadSnapshot(configDir string, useBinary bool) (*guildwarmilestoneSnapshot, error) {
    var container pb.GuildWarMilestoneTableData

    if useBinary {
        path := filepath.Join(configDir, "guildwarmilestone.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "guildwarmilestone.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &guildwarmilestoneSnapshot{
        kvData: make(map[uint32]*pb.GuildWarMilestoneTable, len(container.Data)),
        idxRewardItem1Id: make(map[uint32][]*pb.GuildWarMilestoneTable),
        idxRewardItem2Id: make(map[uint32][]*pb.GuildWarMilestoneTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxRewardItem1Id[row.RewardItem1Id] = append(snap.idxRewardItem1Id[row.RewardItem1Id], row)
        snap.idxRewardItem2Id[row.RewardItem2Id] = append(snap.idxRewardItem2Id[row.RewardItem2Id], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *GuildWarMilestoneTableManager) FindAll() []*pb.GuildWarMilestoneTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *GuildWarMilestoneTableManager) FindById(id uint32) (*pb.GuildWarMilestoneTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *GuildWarMilestoneTableManager) GetByRewardItem1Id(key uint32) []*pb.GuildWarMilestoneTable {
    snap := m.snap.Load()
    return snap.idxRewardItem1Id[key]
}


func (m *GuildWarMilestoneTableManager) GetByRewardItem2Id(key uint32) []*pb.GuildWarMilestoneTable {
    snap := m.snap.Load()
    return snap.idxRewardItem2Id[key]
}



// ---- Exists ----

func (m *GuildWarMilestoneTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *GuildWarMilestoneTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *GuildWarMilestoneTableManager) CountByRewardItem1IdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxRewardItem1Id[key])
}


func (m *GuildWarMilestoneTableManager) CountByRewardItem2IdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxRewardItem2Id[key])
}



// ---- FindByIds (IN) ----

func (m *GuildWarMilestoneTableManager) FindByIds(ids []uint32) []*pb.GuildWarMilestoneTable {
    snap := m.snap.Load()
    result := make([]*pb.GuildWarMilestoneTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *GuildWarMilestoneTableManager) RandOne() (*pb.GuildWarMilestoneTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *GuildWarMilestoneTableManager) Where(pred func(*pb.GuildWarMilestoneTable) bool) []*pb.GuildWarMilestoneTable {
    snap := m.snap.Load()
    var result []*pb.GuildWarMilestoneTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *GuildWarMilestoneTableManager) First(pred func(*pb.GuildWarMilestoneTable) bool) (*pb.GuildWarMilestoneTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: reward_item1_id → Item.id

// FK: reward_item2_id → Item.id


// ---- Composite Key ----

