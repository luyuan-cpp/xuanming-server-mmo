
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



// tournamentrewardSnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type tournamentrewardSnapshot struct {
    data   []*pb.TournamentRewardTable
    kvData map[uint32]*pb.TournamentRewardTable
    idxItemIds map[uint32][]*pb.TournamentRewardTable
    idxItemCounts map[uint32][]*pb.TournamentRewardTable
    idxTierKind map[uint32][]*pb.TournamentRewardTable
}

type TournamentRewardTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[tournamentrewardSnapshot]
}

var TournamentRewardTableManagerInstance = NewTournamentRewardTableManager()

func NewTournamentRewardTableManager() *TournamentRewardTableManager {
    m := &TournamentRewardTableManager{}
    m.snap.Store(&tournamentrewardSnapshot{
        kvData: make(map[uint32]*pb.TournamentRewardTable),
        idxItemIds: make(map[uint32][]*pb.TournamentRewardTable),
        idxItemCounts: make(map[uint32][]*pb.TournamentRewardTable),
        idxTierKind: make(map[uint32][]*pb.TournamentRewardTable),
    })
    return m
}

func (m *TournamentRewardTableManager) Load(configDir string, useBinary bool) error {
    snap, err := m.loadSnapshot(configDir, useBinary)
    if err != nil {
        return err
    }
    m.snap.Store(snap)
    return nil
}

func (m *TournamentRewardTableManager) swapSnapshot(snap *tournamentrewardSnapshot) {
    if snap != nil {
        m.snap.Store(snap)
    }
}

func (m *TournamentRewardTableManager) loadSnapshot(configDir string, useBinary bool) (*tournamentrewardSnapshot, error) {
    var container pb.TournamentRewardTableData

    if useBinary {
        path := filepath.Join(configDir, "tournamentreward.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "tournamentreward.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return nil, fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return nil, fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &tournamentrewardSnapshot{
        kvData: make(map[uint32]*pb.TournamentRewardTable, len(container.Data)),
        idxItemIds: make(map[uint32][]*pb.TournamentRewardTable),
        idxItemCounts: make(map[uint32][]*pb.TournamentRewardTable),
        idxTierKind: make(map[uint32][]*pb.TournamentRewardTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        for _, elem := range row.ItemIds {
            snap.idxItemIds[elem] = append(snap.idxItemIds[elem], row)
        }
        for _, elem := range row.ItemCounts {
            snap.idxItemCounts[elem] = append(snap.idxItemCounts[elem], row)
        }
        snap.idxTierKind[row.TierKind] = append(snap.idxTierKind[row.TierKind], row)
    }

    snap.data = container.Data
    return snap, nil
}

func (m *TournamentRewardTableManager) FindAll() []*pb.TournamentRewardTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *TournamentRewardTableManager) FindById(id uint32) (*pb.TournamentRewardTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}


func (m *TournamentRewardTableManager) FindByItemIdsIndex(key uint32) []*pb.TournamentRewardTable {
    snap := m.snap.Load()
    return snap.idxItemIds[key]
}


func (m *TournamentRewardTableManager) FindByItemCountsIndex(key uint32) []*pb.TournamentRewardTable {
    snap := m.snap.Load()
    return snap.idxItemCounts[key]
}


func (m *TournamentRewardTableManager) GetByTierKind(key uint32) []*pb.TournamentRewardTable {
    snap := m.snap.Load()
    return snap.idxTierKind[key]
}



// ---- Exists ----

func (m *TournamentRewardTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}



// ---- Count ----

func (m *TournamentRewardTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}


func (m *TournamentRewardTableManager) CountByItemIdsIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxItemIds[key])
}


func (m *TournamentRewardTableManager) CountByItemCountsIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxItemCounts[key])
}


func (m *TournamentRewardTableManager) CountByTierKindIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxTierKind[key])
}



// ---- FindByIds (IN) ----

func (m *TournamentRewardTableManager) FindByIds(ids []uint32) []*pb.TournamentRewardTable {
    snap := m.snap.Load()
    result := make([]*pb.TournamentRewardTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *TournamentRewardTableManager) RandOne() (*pb.TournamentRewardTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}



// ---- Where / First ----

func (m *TournamentRewardTableManager) Where(pred func(*pb.TournamentRewardTable) bool) []*pb.TournamentRewardTable {
    snap := m.snap.Load()
    var result []*pb.TournamentRewardTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *TournamentRewardTableManager) First(pred func(*pb.TournamentRewardTable) bool) (*pb.TournamentRewardTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: item_ids → Item.id


// ---- Composite Key ----

