
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



// travelpasstierSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type travelpasstierSnapshot struct {
    data   []*pb.TravelPassTierTable
    kvData map[uint32]*pb.TravelPassTierTable
    idxPassId map[uint32][]*pb.TravelPassTierTable
}

type TravelPassTierTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[travelpasstierSnapshot]
}

var TravelPassTierTableManagerInstance = NewTravelPassTierTableManager()

func NewTravelPassTierTableManager() *TravelPassTierTableManager {
    m := &TravelPassTierTableManager{}
    m.snap.Store(&travelpasstierSnapshot{
        kvData: make(map[uint32]*pb.TravelPassTierTable),
        idxPassId: make(map[uint32][]*pb.TravelPassTierTable),
    })
    return m
}

func (m *TravelPassTierTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *TravelPassTierTableManager) swapSnapshot(snap *travelpasstierSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *TravelPassTierTableManager) loadSnapshot(configDir string, useBinary bool) (*travelpasstierSnapshot, error) {
    var container pb.TravelPassTierTableData

    if useBinary {
        path := filepath.Join(configDir, "travelpasstier.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "travelpasstier.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &travelpasstierSnapshot{
        kvData: make(map[uint32]*pb.TravelPassTierTable, len(container.Data)),
        idxPassId: make(map[uint32][]*pb.TravelPassTierTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxPassId[row.PassId] = append(snap.idxPassId[row.PassId], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *TravelPassTierTableManager) FindAll() []*pb.TravelPassTierTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *TravelPassTierTableManager) FindById(id uint32) (*pb.TravelPassTierTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *TravelPassTierTableManager) GetByPassId(key uint32) []*pb.TravelPassTierTable {
    snap := m.snap.Load()
    return snap.idxPassId[key]
}



// ---- Exists ----

func (m *TravelPassTierTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *TravelPassTierTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *TravelPassTierTableManager) CountByPassIdIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxPassId[key])
}



// ---- FindByIds (IN) ----

func (m *TravelPassTierTableManager) FindByIds(ids []uint32) []*pb.TravelPassTierTable {
    snap := m.snap.Load()
    result := make([]*pb.TravelPassTierTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *TravelPassTierTableManager) RandOne() (*pb.TravelPassTierTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *TravelPassTierTableManager) Where(pred func(*pb.TravelPassTierTable) bool) []*pb.TravelPassTierTable {
    snap := m.snap.Load()
    var result []*pb.TravelPassTierTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *TravelPassTierTableManager) First(pred func(*pb.TravelPassTierTable) bool) (*pb.TravelPassTierTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: pass_id → TravelPassSeason.id


// ---- Composite Key ----

