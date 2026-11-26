
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



// jianghuruleSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type jianghuruleSnapshot struct {
    data   []*pb.JianghuRuleTable
    kvData map[uint32]*pb.JianghuRuleTable
    idxGoldWritItemId map[uint32][]*pb.JianghuRuleTable
    idxClearItemId map[uint32][]*pb.JianghuRuleTable
}

type JianghuRuleTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[jianghuruleSnapshot]
}

var JianghuRuleTableManagerInstance = NewJianghuRuleTableManager()

func NewJianghuRuleTableManager() *JianghuRuleTableManager {
    m := &JianghuRuleTableManager{}
    m.snap.Store(&jianghuruleSnapshot{
        kvData: make(map[uint32]*pb.JianghuRuleTable),
        idxGoldWritItemId: make(map[uint32][]*pb.JianghuRuleTable),
        idxClearItemId: make(map[uint32][]*pb.JianghuRuleTable),
    })
    return m
}

func (m *JianghuRuleTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *JianghuRuleTableManager) swapSnapshot(snap *jianghuruleSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *JianghuRuleTableManager) loadSnapshot(configDir string, useBinary bool) (*jianghuruleSnapshot, error) {
    var container pb.JianghuRuleTableData

    if useBinary {
        path := filepath.Join(configDir, "jianghurule.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "jianghurule.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &jianghuruleSnapshot{
        kvData: make(map[uint32]*pb.JianghuRuleTable, len(container.Data)),
        idxGoldWritItemId: make(map[uint32][]*pb.JianghuRuleTable),
        idxClearItemId: make(map[uint32][]*pb.JianghuRuleTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxGoldWritItemId[row.GoldWritItemId] = append(snap.idxGoldWritItemId[row.GoldWritItemId], row)
        snap.idxClearItemId[row.ClearItemId] = append(snap.idxClearItemId[row.ClearItemId], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *JianghuRuleTableManager) FindAll() []*pb.JianghuRuleTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *JianghuRuleTableManager) FindById(id uint32) (*pb.JianghuRuleTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *JianghuRuleTableManager) GetByGoldWritItemId(key uint32) []*pb.JianghuRuleTable {
    snap := m.snap.Load()
    return snap.idxGoldWritItemId[key]
}


func (m *JianghuRuleTableManager) GetByClearItemId(key uint32) []*pb.JianghuRuleTable {
    snap := m.snap.Load()
    return snap.idxClearItemId[key]
}



// ---- Exists ----

func (m *JianghuRuleTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *JianghuRuleTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *JianghuRuleTableManager) CountByGoldWritItemIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxGoldWritItemId[key])
}


func (m *JianghuRuleTableManager) CountByClearItemIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxClearItemId[key])
}



// ---- FindByIds (IN) ----

func (m *JianghuRuleTableManager) FindByIds(ids []uint32) []*pb.JianghuRuleTable {
    snap := m.snap.Load()
    result := make([]*pb.JianghuRuleTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *JianghuRuleTableManager) RandOne() (*pb.JianghuRuleTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *JianghuRuleTableManager) Where(pred func(*pb.JianghuRuleTable) bool) []*pb.JianghuRuleTable {
    snap := m.snap.Load()
    var result []*pb.JianghuRuleTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *JianghuRuleTableManager) First(pred func(*pb.JianghuRuleTable) bool) (*pb.JianghuRuleTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: gold_writ_item_id → Item.id

// FK: clear_item_id → Item.id


// ---- Composite Key ----

