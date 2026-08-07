
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



// testmultikeySnapshot holds all parsed data and indices.
// Load() builds a new snapshot and swaps it in, replacing the old one.
type testmultikeySnapshot struct {
    data   []*pb.TestMultiKeyTable
    kvData map[uint32]*pb.TestMultiKeyTable
    kvString_keyData map[string]*pb.TestMultiKeyTable
    kvUint32_keyData map[uint32]*pb.TestMultiKeyTable
    kvInt32_keyData map[int32]*pb.TestMultiKeyTable
    kvM_string_keyData map[string][]*pb.TestMultiKeyTable
    kvM_uint32_keyData map[uint32][]*pb.TestMultiKeyTable
    kvM_int32_keyData map[int32][]*pb.TestMultiKeyTable
    idxEffect map[uint32][]*pb.TestMultiKeyTable
    idxTest_refs map[uint32][]*pb.TestMultiKeyTable
    idxLevel map[uint32][]*pb.TestMultiKeyTable
    idxTestRef map[uint32][]*pb.TestMultiKeyTable
}

type TestMultiKeyTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    // 不能退回裸字段 —— 热更的本质就是「服务跑着的时候再 Load 一次」,那一刻裸赋值与
    // 并发读就是数据竞争(go test -race 会报)。
    // 访问方法一律**在开头取一次**本地快照再用:同一次调用里多次 Load 可能拿到不同快照,
    // 表缩小时 data[rand.IntN(len(data))] 会越界。
    snap atomic.Pointer[testmultikeySnapshot]
}

var TestMultiKeyTableManagerInstance = NewTestMultiKeyTableManager()

func NewTestMultiKeyTableManager() *TestMultiKeyTableManager {
    m := &TestMultiKeyTableManager{}
    m.snap.Store(&testmultikeySnapshot{
        kvData: make(map[uint32]*pb.TestMultiKeyTable),
        kvString_keyData: make(map[string]*pb.TestMultiKeyTable),
        kvUint32_keyData: make(map[uint32]*pb.TestMultiKeyTable),
        kvInt32_keyData: make(map[int32]*pb.TestMultiKeyTable),
        kvM_string_keyData: make(map[string][]*pb.TestMultiKeyTable),
        kvM_uint32_keyData: make(map[uint32][]*pb.TestMultiKeyTable),
        kvM_int32_keyData: make(map[int32][]*pb.TestMultiKeyTable),
        idxEffect: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTest_refs: make(map[uint32][]*pb.TestMultiKeyTable),
        idxLevel: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTestRef: make(map[uint32][]*pb.TestMultiKeyTable),
    })
    return m
}

func (m *TestMultiKeyTableManager) Load(configDir string, useBinary bool) error {
    var container pb.TestMultiKeyTableData

    if useBinary {
        path := filepath.Join(configDir, "testmultikey.pb")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := proto.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse binary: %w", err)
        }
    } else {
        path := filepath.Join(configDir, "testmultikey.json")
        raw, err := os.ReadFile(path)
        if err != nil {
            return fmt.Errorf("failed to read file: %w", err)
        }
        if err := protojson.Unmarshal(raw, &container); err != nil {
            return fmt.Errorf("failed to parse json: %w", err)
        }
    }

    snap := &testmultikeySnapshot{
        kvData: make(map[uint32]*pb.TestMultiKeyTable, len(container.Data)),
        kvString_keyData: make(map[string]*pb.TestMultiKeyTable, len(container.Data)),
        kvUint32_keyData: make(map[uint32]*pb.TestMultiKeyTable, len(container.Data)),
        kvInt32_keyData: make(map[int32]*pb.TestMultiKeyTable, len(container.Data)),
        kvM_string_keyData: make(map[string][]*pb.TestMultiKeyTable),
        kvM_uint32_keyData: make(map[uint32][]*pb.TestMultiKeyTable),
        kvM_int32_keyData: make(map[int32][]*pb.TestMultiKeyTable),
        idxEffect: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTest_refs: make(map[uint32][]*pb.TestMultiKeyTable),
        idxLevel: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTestRef: make(map[uint32][]*pb.TestMultiKeyTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = row
        snap.kvString_keyData[row.StringKey] = row
        snap.kvUint32_keyData[row.Uint32Key] = row
        snap.kvInt32_keyData[row.Int32Key] = row
        snap.kvM_string_keyData[row.MStringKey] = append(snap.kvM_string_keyData[row.MStringKey], row)
        snap.kvM_uint32_keyData[row.MUint32Key] = append(snap.kvM_uint32_keyData[row.MUint32Key], row)
        snap.kvM_int32_keyData[row.MInt32Key] = append(snap.kvM_int32_keyData[row.MInt32Key], row)
        for _, elem := range row.Effect {
            snap.idxEffect[elem] = append(snap.idxEffect[elem], row)
        }
        for _, elem := range row.TestRefs {
            snap.idxTest_refs[elem] = append(snap.idxTest_refs[elem], row)
        }
        snap.idxLevel[row.Level] = append(snap.idxLevel[row.Level], row)
        snap.idxTestRef[row.TestRef] = append(snap.idxTestRef[row.TestRef], row)
    }

    snap.data = container.Data
    m.snap.Store(snap)
    return nil
}

func (m *TestMultiKeyTableManager) FindAll() []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.data
}

func (m *TestMultiKeyTableManager) FindById(id uint32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvData[id]
    return row, ok
}

func (m *TestMultiKeyTableManager) FindByString_key(key string) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvString_keyData[key]
    return row, ok
}

func (m *TestMultiKeyTableManager) FindByUint32_key(key uint32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvUint32_keyData[key]
    return row, ok
}

func (m *TestMultiKeyTableManager) FindByInt32_key(key int32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvInt32_keyData[key]
    return row, ok
}


func (m *TestMultiKeyTableManager) FindByM_string_key(key string) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvM_string_keyData[key]
}


func (m *TestMultiKeyTableManager) FindByM_uint32_key(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvM_uint32_keyData[key]
}


func (m *TestMultiKeyTableManager) FindByM_int32_key(key int32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvM_int32_keyData[key]
}


func (m *TestMultiKeyTableManager) FindByEffectIndex(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.idxEffect[key]
}


func (m *TestMultiKeyTableManager) FindByTest_refsIndex(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.idxTest_refs[key]
}


func (m *TestMultiKeyTableManager) GetByLevel(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.idxLevel[key]
}


func (m *TestMultiKeyTableManager) GetByTestRef(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.idxTestRef[key]
}



// ---- Exists ----

func (m *TestMultiKeyTableManager) Exists(id uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvData[id]
    return ok
}

func (m *TestMultiKeyTableManager) ExistsByString_key(key string) bool {
    snap := m.snap.Load()
    _, ok := snap.kvString_keyData[key]
    return ok
}

func (m *TestMultiKeyTableManager) ExistsByUint32_key(key uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvUint32_keyData[key]
    return ok
}

func (m *TestMultiKeyTableManager) ExistsByInt32_key(key int32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvInt32_keyData[key]
    return ok
}



// ---- Count ----

func (m *TestMultiKeyTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}

func (m *TestMultiKeyTableManager) CountByM_string_key(key string) int {
    snap := m.snap.Load()
    return len(snap.kvM_string_keyData[key])
}

func (m *TestMultiKeyTableManager) CountByM_uint32_key(key uint32) int {
    snap := m.snap.Load()
    return len(snap.kvM_uint32_keyData[key])
}

func (m *TestMultiKeyTableManager) CountByM_int32_key(key int32) int {
    snap := m.snap.Load()
    return len(snap.kvM_int32_keyData[key])
}


func (m *TestMultiKeyTableManager) CountByEffectIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxEffect[key])
}


func (m *TestMultiKeyTableManager) CountByTest_refsIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxTest_refs[key])
}


func (m *TestMultiKeyTableManager) CountByLevelIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxLevel[key])
}


func (m *TestMultiKeyTableManager) CountByTestRefIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxTestRef[key])
}



// ---- FindByIds (IN) ----

func (m *TestMultiKeyTableManager) FindByIds(ids []uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    result := make([]*pb.TestMultiKeyTable, 0, len(ids))
    for _, id := range ids {
        if row, ok := snap.kvData[id]; ok {
            result = append(result, row)
        }
    }
    return result
}

// ---- RandOne ----

func (m *TestMultiKeyTableManager) RandOne() (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    if len(snap.data) == 0 {
        return nil, false
    }
    return snap.data[rand.IntN(len(snap.data))], true
}

func (m *TestMultiKeyTableManager) RandOneByM_string_key(key string) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    rows := snap.kvM_string_keyData[key]
    if len(rows) == 0 {
        return nil, false
    }
    return rows[rand.IntN(len(rows))], true
}

func (m *TestMultiKeyTableManager) RandOneByM_uint32_key(key uint32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    rows := snap.kvM_uint32_keyData[key]
    if len(rows) == 0 {
        return nil, false
    }
    return rows[rand.IntN(len(rows))], true
}

func (m *TestMultiKeyTableManager) RandOneByM_int32_key(key int32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    rows := snap.kvM_int32_keyData[key]
    if len(rows) == 0 {
        return nil, false
    }
    return rows[rand.IntN(len(rows))], true
}



// ---- Where / First ----

func (m *TestMultiKeyTableManager) Where(pred func(*pb.TestMultiKeyTable) bool) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    var result []*pb.TestMultiKeyTable
    for _, row := range snap.data {
        if pred(row) {
            result = append(result, row)
        }
    }
    return result
}

func (m *TestMultiKeyTableManager) First(pred func(*pb.TestMultiKeyTable) bool) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    for _, row := range snap.data {
        if pred(row) {
            return row, true
        }
    }
    return nil, false
}
// FK: test_ref → Test.id


// ---- Composite Key ----

