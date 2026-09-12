
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
    // 主键可重复:一个 id 对应多行
    kvData map[uint32][]*pb.TestMultiKeyTable
    kvStringKeyData map[string]*pb.TestMultiKeyTable
    kvUint32KeyData map[uint32]*pb.TestMultiKeyTable
    kvInt32KeyData map[int32]*pb.TestMultiKeyTable
    kvMStringKeyData map[string][]*pb.TestMultiKeyTable
    kvMUint32KeyData map[uint32][]*pb.TestMultiKeyTable
    kvMInt32KeyData map[int32][]*pb.TestMultiKeyTable
    idxEffect map[uint32][]*pb.TestMultiKeyTable
    idxTestRefs map[uint32][]*pb.TestMultiKeyTable
    idxLevel map[uint32][]*pb.TestMultiKeyTable
    idxTestRef map[uint32][]*pb.TestMultiKeyTable
}

type TestMultiKeyTableManager struct {
    // snap 指向不可变快照:Load 先整批建好新 snapshot,再原子换指针;读侧无锁 Load()。
    //
    // 热更契约:返回出去的 *pb 行属于**当时**那个快照。Go 有 GC,存着不会崩,
    // 但会永远拿到热更前的旧值。调用方**只存 id**,用的时候现查。
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
        kvData: make(map[uint32][]*pb.TestMultiKeyTable),
        kvStringKeyData: make(map[string]*pb.TestMultiKeyTable),
        kvUint32KeyData: make(map[uint32]*pb.TestMultiKeyTable),
        kvInt32KeyData: make(map[int32]*pb.TestMultiKeyTable),
        kvMStringKeyData: make(map[string][]*pb.TestMultiKeyTable),
        kvMUint32KeyData: make(map[uint32][]*pb.TestMultiKeyTable),
        kvMInt32KeyData: make(map[int32][]*pb.TestMultiKeyTable),
        idxEffect: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTestRefs: make(map[uint32][]*pb.TestMultiKeyTable),
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
        kvData: make(map[uint32][]*pb.TestMultiKeyTable, len(container.Data)),
        kvStringKeyData: make(map[string]*pb.TestMultiKeyTable, len(container.Data)),
        kvUint32KeyData: make(map[uint32]*pb.TestMultiKeyTable, len(container.Data)),
        kvInt32KeyData: make(map[int32]*pb.TestMultiKeyTable, len(container.Data)),
        kvMStringKeyData: make(map[string][]*pb.TestMultiKeyTable),
        kvMUint32KeyData: make(map[uint32][]*pb.TestMultiKeyTable),
        kvMInt32KeyData: make(map[int32][]*pb.TestMultiKeyTable),
        idxEffect: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTestRefs: make(map[uint32][]*pb.TestMultiKeyTable),
        idxLevel: make(map[uint32][]*pb.TestMultiKeyTable),
        idxTestRef: make(map[uint32][]*pb.TestMultiKeyTable),
    }

    for _, row := range container.Data {
        snap.kvData[row.Id] = append(snap.kvData[row.Id], row)
        snap.kvStringKeyData[row.StringKey] = row
        snap.kvUint32KeyData[row.Uint32Key] = row
        snap.kvInt32KeyData[row.Int32Key] = row
        snap.kvMStringKeyData[row.MStringKey] = append(snap.kvMStringKeyData[row.MStringKey], row)
        snap.kvMUint32KeyData[row.MUint32Key] = append(snap.kvMUint32KeyData[row.MUint32Key], row)
        snap.kvMInt32KeyData[row.MInt32Key] = append(snap.kvMInt32KeyData[row.MInt32Key], row)
        for _, elem := range row.Effect {
            snap.idxEffect[elem] = append(snap.idxEffect[elem], row)
        }
        for _, elem := range row.TestRefs {
            snap.idxTestRefs[elem] = append(snap.idxTestRefs[elem], row)
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

// FindAllById 返回该 id 命中的**全部**行(表内顺序)。
// 这张表的主键声明了 (cfg_multi),所以不生成 FindById —— 把可重复主键当单值用
// 会在编译期断，而不是悄悄只看第一行。
func (m *TestMultiKeyTableManager) FindAllById(id uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvData[id]
}

func (m *TestMultiKeyTableManager) FindByStringKey(key string) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvStringKeyData[key]
    return row, ok
}

func (m *TestMultiKeyTableManager) FindByUint32Key(key uint32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvUint32KeyData[key]
    return row, ok
}

func (m *TestMultiKeyTableManager) FindByInt32Key(key int32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    row, ok := snap.kvInt32KeyData[key]
    return row, ok
}


func (m *TestMultiKeyTableManager) FindByMStringKey(key string) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvMStringKeyData[key]
}


func (m *TestMultiKeyTableManager) FindByMUint32Key(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvMUint32KeyData[key]
}


func (m *TestMultiKeyTableManager) FindByMInt32Key(key int32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.kvMInt32KeyData[key]
}


func (m *TestMultiKeyTableManager) FindByEffectIndex(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.idxEffect[key]
}


func (m *TestMultiKeyTableManager) FindByTestRefsIndex(key uint32) []*pb.TestMultiKeyTable {
    snap := m.snap.Load()
    return snap.idxTestRefs[key]
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
    return len(snap.kvData[id]) > 0
}

func (m *TestMultiKeyTableManager) ExistsByStringKey(key string) bool {
    snap := m.snap.Load()
    _, ok := snap.kvStringKeyData[key]
    return ok
}

func (m *TestMultiKeyTableManager) ExistsByUint32Key(key uint32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvUint32KeyData[key]
    return ok
}

func (m *TestMultiKeyTableManager) ExistsByInt32Key(key int32) bool {
    snap := m.snap.Load()
    _, ok := snap.kvInt32KeyData[key]
    return ok
}



// ---- Count ----

func (m *TestMultiKeyTableManager) Count() int {
    snap := m.snap.Load()
    return len(snap.data)
}

func (m *TestMultiKeyTableManager) CountByMStringKey(key string) int {
    snap := m.snap.Load()
    return len(snap.kvMStringKeyData[key])
}

func (m *TestMultiKeyTableManager) CountByMUint32Key(key uint32) int {
    snap := m.snap.Load()
    return len(snap.kvMUint32KeyData[key])
}

func (m *TestMultiKeyTableManager) CountByMInt32Key(key int32) int {
    snap := m.snap.Load()
    return len(snap.kvMInt32KeyData[key])
}


func (m *TestMultiKeyTableManager) CountByEffectIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxEffect[key])
}


func (m *TestMultiKeyTableManager) CountByTestRefsIndex(key uint32) int {
    snap := m.snap.Load()
    return len(snap.idxTestRefs[key])
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
        result = append(result, snap.kvData[id]...)
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

func (m *TestMultiKeyTableManager) RandOneByMStringKey(key string) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    rows := snap.kvMStringKeyData[key]
    if len(rows) == 0 {
        return nil, false
    }
    return rows[rand.IntN(len(rows))], true
}

func (m *TestMultiKeyTableManager) RandOneByMUint32Key(key uint32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    rows := snap.kvMUint32KeyData[key]
    if len(rows) == 0 {
        return nil, false
    }
    return rows[rand.IntN(len(rows))], true
}

func (m *TestMultiKeyTableManager) RandOneByMInt32Key(key int32) (*pb.TestMultiKeyTable, bool) {
    snap := m.snap.Load()
    rows := snap.kvMInt32KeyData[key]
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

