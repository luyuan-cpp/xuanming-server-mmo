
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



// festivalbeastactivitySnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type festivalbeastactivitySnapshot struct {
    data   []*pb.FestivalBeastActivityTable
    kvData map[uint32]*pb.FestivalBeastActivityTable
    idxBattleConfigId map[uint32][]*pb.FestivalBeastActivityTable
    idxRewardPetTableId map[uint32][]*pb.FestivalBeastActivityTable
}

type FestivalBeastActivityTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[festivalbeastactivitySnapshot]
}

var FestivalBeastActivityTableManagerInstance = NewFestivalBeastActivityTableManager()

func NewFestivalBeastActivityTableManager() *FestivalBeastActivityTableManager {
    m := &FestivalBeastActivityTableManager{}
    m.snap.Store(&festivalbeastactivitySnapshot{
        kvData: make(map[uint32]*pb.FestivalBeastActivityTable),
        idxBattleConfigId: make(map[uint32][]*pb.FestivalBeastActivityTable),
        idxRewardPetTableId: make(map[uint32][]*pb.FestivalBeastActivityTable),
    })
    return m
}

func (m *FestivalBeastActivityTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *FestivalBeastActivityTableManager) swapSnapshot(snap *festivalbeastactivitySnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *FestivalBeastActivityTableManager) loadSnapshot(configDir string, useBinary bool) (*festivalbeastactivitySnapshot, error) {
    var container pb.FestivalBeastActivityTableData

    if useBinary {
        path := filepath.Join(configDir, "festivalbeastactivity.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "festivalbeastactivity.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &festivalbeastactivitySnapshot{
        kvData: make(map[uint32]*pb.FestivalBeastActivityTable, len(container.Data)),
        idxBattleConfigId: make(map[uint32][]*pb.FestivalBeastActivityTable),
        idxRewardPetTableId: make(map[uint32][]*pb.FestivalBeastActivityTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxBattleConfigId[row.BattleConfigId] = append(snap.idxBattleConfigId[row.BattleConfigId], row)
        snap.idxRewardPetTableId[row.RewardPetTableId] = append(snap.idxRewardPetTableId[row.RewardPetTableId], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *FestivalBeastActivityTableManager) FindAll() []*pb.FestivalBeastActivityTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *FestivalBeastActivityTableManager) FindById(id uint32) (*pb.FestivalBeastActivityTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *FestivalBeastActivityTableManager) GetByBattleConfigId(key uint32) []*pb.FestivalBeastActivityTable {
    snap := m.snap.Load()
    return snap.idxBattleConfigId[key]
}


func (m *FestivalBeastActivityTableManager) GetByRewardPetTableId(key uint32) []*pb.FestivalBeastActivityTable {
    snap := m.snap.Load()
    return snap.idxRewardPetTableId[key]
}



// ---- Exists ----

func (m *FestivalBeastActivityTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *FestivalBeastActivityTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *FestivalBeastActivityTableManager) CountByBattleConfigIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxBattleConfigId[key])
}


func (m *FestivalBeastActivityTableManager) CountByRewardPetTableIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxRewardPetTableId[key])
}



// ---- FindByIds (IN) ----

func (m *FestivalBeastActivityTableManager) FindByIds(ids []uint32) []*pb.FestivalBeastActivityTable {
    snap := m.snap.Load()
    result := make([]*pb.FestivalBeastActivityTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *FestivalBeastActivityTableManager) RandOne() (*pb.FestivalBeastActivityTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *FestivalBeastActivityTableManager) Where(pred func(*pb.FestivalBeastActivityTable) bool) []*pb.FestivalBeastActivityTable {
    snap := m.snap.Load()
    var result []*pb.FestivalBeastActivityTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *FestivalBeastActivityTableManager) First(pred func(*pb.FestivalBeastActivityTable) bool) (*pb.FestivalBeastActivityTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: battle_config_id → Dungeon.id

// FK: reward_pet_table_id → Pet.id


// ---- Composite Key ----

