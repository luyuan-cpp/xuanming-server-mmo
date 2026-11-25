
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



// skillscrollcraftSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type skillscrollcraftSnapshot struct {
    data   []*pb.SkillScrollCraftTable
    kvData map[uint32]*pb.SkillScrollCraftTable
    idxFragmentItemId map[uint32][]*pb.SkillScrollCraftTable
}

type SkillScrollCraftTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[skillscrollcraftSnapshot]
}

var SkillScrollCraftTableManagerInstance = NewSkillScrollCraftTableManager()

func NewSkillScrollCraftTableManager() *SkillScrollCraftTableManager {
    m := &SkillScrollCraftTableManager{}
    m.snap.Store(&skillscrollcraftSnapshot{
        kvData: make(map[uint32]*pb.SkillScrollCraftTable),
        idxFragmentItemId: make(map[uint32][]*pb.SkillScrollCraftTable),
    })
    return m
}

func (m *SkillScrollCraftTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *SkillScrollCraftTableManager) swapSnapshot(snap *skillscrollcraftSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *SkillScrollCraftTableManager) loadSnapshot(configDir string, useBinary bool) (*skillscrollcraftSnapshot, error) {
    var container pb.SkillScrollCraftTableData

    if useBinary {
        path := filepath.Join(configDir, "skillscrollcraft.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "skillscrollcraft.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &skillscrollcraftSnapshot{
        kvData: make(map[uint32]*pb.SkillScrollCraftTable, len(container.Data)),
        idxFragmentItemId: make(map[uint32][]*pb.SkillScrollCraftTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxFragmentItemId[row.FragmentItemId] = append(snap.idxFragmentItemId[row.FragmentItemId], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *SkillScrollCraftTableManager) FindAll() []*pb.SkillScrollCraftTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *SkillScrollCraftTableManager) FindById(id uint32) (*pb.SkillScrollCraftTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *SkillScrollCraftTableManager) GetByFragmentItemId(key uint32) []*pb.SkillScrollCraftTable {
    snap := m.snap.Load()
    return snap.idxFragmentItemId[key]
}



// ---- Exists ----

func (m *SkillScrollCraftTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *SkillScrollCraftTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *SkillScrollCraftTableManager) CountByFragmentItemIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxFragmentItemId[key])
}



// ---- FindByIds (IN) ----

func (m *SkillScrollCraftTableManager) FindByIds(ids []uint32) []*pb.SkillScrollCraftTable {
    snap := m.snap.Load()
    result := make([]*pb.SkillScrollCraftTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *SkillScrollCraftTableManager) RandOne() (*pb.SkillScrollCraftTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *SkillScrollCraftTableManager) Where(pred func(*pb.SkillScrollCraftTable) bool) []*pb.SkillScrollCraftTable {
    snap := m.snap.Load()
    var result []*pb.SkillScrollCraftTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *SkillScrollCraftTableManager) First(pred func(*pb.SkillScrollCraftTable) bool) (*pb.SkillScrollCraftTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: fragment_item_id → Item.id


// ---- Composite Key ----

