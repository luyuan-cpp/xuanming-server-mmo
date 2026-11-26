
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



// spiritshopgoodsSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type spiritshopgoodsSnapshot struct {
    data   []*pb.SpiritShopGoodsTable
    kvData map[uint32]*pb.SpiritShopGoodsTable
    idxItemId_1 map[uint32][]*pb.SpiritShopGoodsTable
}

type SpiritShopGoodsTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[spiritshopgoodsSnapshot]
}

var SpiritShopGoodsTableManagerInstance = NewSpiritShopGoodsTableManager()

func NewSpiritShopGoodsTableManager() *SpiritShopGoodsTableManager {
    m := &SpiritShopGoodsTableManager{}
    m.snap.Store(&spiritshopgoodsSnapshot{
        kvData: make(map[uint32]*pb.SpiritShopGoodsTable),
        idxItemId_1: make(map[uint32][]*pb.SpiritShopGoodsTable),
    })
    return m
}

func (m *SpiritShopGoodsTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *SpiritShopGoodsTableManager) swapSnapshot(snap *spiritshopgoodsSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *SpiritShopGoodsTableManager) loadSnapshot(configDir string, useBinary bool) (*spiritshopgoodsSnapshot, error) {
    var container pb.SpiritShopGoodsTableData

    if useBinary {
        path := filepath.Join(configDir, "spiritshopgoods.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "spiritshopgoods.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &spiritshopgoodsSnapshot{
        kvData: make(map[uint32]*pb.SpiritShopGoodsTable, len(container.Data)),
        idxItemId_1: make(map[uint32][]*pb.SpiritShopGoodsTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.idxItemId_1[row.ItemId_1] = append(snap.idxItemId_1[row.ItemId_1], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *SpiritShopGoodsTableManager) FindAll() []*pb.SpiritShopGoodsTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *SpiritShopGoodsTableManager) FindById(id uint32) (*pb.SpiritShopGoodsTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *SpiritShopGoodsTableManager) GetByItemId_1(key uint32) []*pb.SpiritShopGoodsTable {
    snap := m.snap.Load()
    return snap.idxItemId_1[key]
}



// ---- Exists ----

func (m *SpiritShopGoodsTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *SpiritShopGoodsTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *SpiritShopGoodsTableManager) CountByItemId_1Index(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxItemId_1[key])
}



// ---- FindByIds (IN) ----

func (m *SpiritShopGoodsTableManager) FindByIds(ids []uint32) []*pb.SpiritShopGoodsTable {
    snap := m.snap.Load()
    result := make([]*pb.SpiritShopGoodsTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *SpiritShopGoodsTableManager) RandOne() (*pb.SpiritShopGoodsTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *SpiritShopGoodsTableManager) Where(pred func(*pb.SpiritShopGoodsTable) bool) []*pb.SpiritShopGoodsTable {
    snap := m.snap.Load()
    var result []*pb.SpiritShopGoodsTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *SpiritShopGoodsTableManager) First(pred func(*pb.SpiritShopGoodsTable) bool) (*pb.SpiritShopGoodsTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: item_id_1 → Item.id


// ---- Composite Key ----

