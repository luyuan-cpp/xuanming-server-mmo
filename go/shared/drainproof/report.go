// Package drainproof记录同一boot实际接收集合的关闭与收敛；取消和超时不会生成成功收据。
package drainproof

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	"shared/authoritysql"
)

var ErrClosed = errors.New("排空入口已永久关闭")
var ErrHold = errors.New("排空结局未知，保持维护")

// Session分别拥有一个逻辑角色。SOURCE关闭不关闭同boot的RELAY或物理SQL句柄。
// Enter/CloseAdmission/Finish由同一锁线性化；未知结局永久保留到人工处理，不能重新开闸。
type Session struct {
	mu        sync.Mutex
	closed    bool
	prepared  bool
	closedMS  uint64
	next      uint64
	settled   uint64
	pending   map[uint64]bool
	unknown   uint64
	changed   chan struct{}
	upstream  [][]byte
	finalized bool
	settledMS uint64
}

func New() *Session        { return &Session{pending: map[uint64]bool{}, changed: make(chan struct{})} }
func (s *Session) signal() { close(s.changed); s.changed = make(chan struct{}) }
func (s *Session) Enter() (func(), error) {
	s.mu.Lock()
	if s.closed || s.prepared || s.next == ^uint64(0) {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.next++
	id := s.next
	s.pending[id] = true
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); delete(s.pending, id); s.settled++; s.signal(); s.mu.Unlock() }) }, nil
}
func (s *Session) CloseAdmission() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.closedMS = uint64(time.Now().UnixMilli())
		s.signal()
	}
}
func (s *Session) Closed() bool    { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *Session) Pending() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return uint64(len(s.pending)) }

// Hold必须用于不能证明服务端结局的发布/回调，不会因ctx取消、TTL或进程消失被清零。
func (s *Session) Hold() { s.mu.Lock(); s.unknown++; s.signal(); s.mu.Unlock() }

// WaitIdle不关闭入口，只用于在上游永久关闭后让已经接收的回调链继续派生收尾工作。
func (s *Session) WaitIdle(ctx context.Context) error {
	for {
		s.mu.Lock()
		pending, ch := len(s.pending), s.changed
		s.mu.Unlock()
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}
func (s *Session) Wait(ctx context.Context) error {
	for {
		s.mu.Lock()
		closed, pending, unknown, ch := s.closed, len(s.pending), s.unknown, s.changed
		s.mu.Unlock()
		if !closed {
			return ErrClosed
		}
		if pending == 0 {
			if unknown != 0 {
				return ErrHold
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// Closure只填正式proto；调用者提供已固定的发行者身份与永久禁止复活原件。
func (s *Session) Closure(ctx context.Context, identity *pb.SqlWriterMaintenanceReceipt_IssuerClosure) (*pb.SqlWriterMaintenanceReceipt_IssuerClosure, error) {
	if err := s.Wait(ctx); err != nil {
		return nil, err
	}
	if identity == nil || len(identity.IssuerId) != 16 || len(identity.InstanceId) != 16 || len(identity.PermanentRevivalFenceId) != 16 || identity.LogicalRole == "" || len(identity.SourceEvidence) == 0 {
		return nil, ErrHold
	}
	out := proto.Clone(identity).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed || len(s.pending) != 0 || s.unknown != 0 {
		return nil, ErrHold
	}
	s.finalized = true
	if s.settledMS == 0 {
		s.settledMS = uint64(time.Now().UnixMilli())
	}
	out.AcceptingClosedMs = s.closedMS
	out.AcceptedSettledMs = s.settledMS
	out.UnresolvedPublications = s.unknown
	var frontier [24]byte
	binary.BigEndian.PutUint64(frontier[0:8], s.next)
	binary.BigEndian.PutUint64(frontier[8:16], s.settled)
	binary.BigEndian.PutUint64(frontier[16:24], s.unknown)
	h := sha256.New()
	h.Write(out.IssuerId)
	h.Write(out.InstanceId)
	h.Write([]byte(out.LogicalRole))
	h.Write(frontier[:])
	for _, digest := range s.upstream {
		h.Write(digest)
	}
	out.AcceptedFrontierDigest = h.Sum(nil)
	return out, nil
}

// VerifyClosure复核正式原件及全部被pin字节；上限内也不接受未知字段或空摘要。
func VerifyClosure(c *pb.SqlWriterMaintenanceReceipt_IssuerClosure) error {
	if c == nil || len(c.ProtoReflect().GetUnknown()) != 0 || len(c.IssuerId) != 16 || len(c.InstanceId) != 16 || len(c.PermanentRevivalFenceId) != 16 || c.LogicalRole == "" || c.AcceptingClosedMs == 0 || c.AcceptedSettledMs < c.AcceptingClosedMs || len(c.AcceptedFrontierDigest) != 32 || c.UnresolvedPublications != 0 || len(c.SourceEvidence) == 0 || len(c.SourceEvidence) > 128 {
		return ErrHold
	}
	seen := map[string]bool{}
	for _, pin := range c.SourceEvidence {
		if pin == nil || !filepath.IsAbs(pin.AbsolutePath) || len(pin.Sha256) != 32 || seen[pin.AbsolutePath] {
			return ErrHold
		}
		seen[pin.AbsolutePath] = true
		stat, err := os.Lstat(pin.AbsolutePath)
		if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > 16<<20 {
			return ErrHold
		}
		file, err := os.Open(pin.AbsolutePath)
		if err != nil {
			return ErrHold
		}
		raw, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(raw) > 16<<20 {
			return ErrHold
		}
		sum := sha256.Sum256(raw)
		if !bytes.Equal(sum[:], pin.Sha256) {
			return ErrHold
		}
	}
	return nil
}
func WriteClosure(path string, c *pb.SqlWriterMaintenanceReceipt_IssuerClosure) error {
	if !filepath.IsAbs(path) {
		return ErrHold
	}
	if err := VerifyClosure(c); err != nil {
		return err
	}
	raw, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(c)
	if err != nil {
		return err
	}
	return authoritysql.WritePrivateControlFile(path, raw)
}

// Runtime只保存正式typed manifest和本boot角色seed；stop文件本身没有授权意义。
type Runtime struct {
	Request   *pb.SqlWriterMaintenanceReceipt
	Plan      *pb.SqlWriterMaintenanceReceipt_Plan
	Directory string
	bound     bool
	boundMu   sync.Mutex
}

func LoadRuntime(planPath, directory, expectedSHA string) (*Runtime, error) {
	if !filepath.IsAbs(planPath) || !filepath.IsAbs(directory) {
		return nil, ErrHold
	}
	if err := authoritysql.CheckPrivateControlDirectory(directory); err != nil {
		return nil, ErrHold
	}
	digest, err := hex.DecodeString(expectedSHA)
	if err != nil || len(digest) != 32 {
		return nil, ErrHold
	}
	raw, err := authoritysql.ReadPrivateControlFile(planPath, 16<<20)
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(raw)
	if !bytes.Equal(actual[:], digest) {
		return nil, ErrHold
	}
	request := &pb.SqlWriterMaintenanceReceipt{}
	if err = (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, request); err != nil {
		return nil, err
	}
	plan := &pb.SqlWriterMaintenanceReceipt_Plan{}
	if proto.Unmarshal(request.PlanProto, plan) != nil || len(plan.ProtoReflect().GetUnknown()) != 0 {
		return nil, ErrHold
	}
	sum := sha256.Sum256(request.PlanProto)
	if request.Evidence == nil || len(request.Evidence.OperationId) != 16 || !bytes.Equal(request.RunId, request.Evidence.OperationId) || !bytes.Equal(plan.RunId, request.RunId) || !bytes.Equal(request.Evidence.ManifestDigest, sum[:]) || request.Evidence.GetPublication() == nil || len(plan.RootIssuers) == 0 || len(plan.QueueMutators) == 0 || len(plan.KafkaPartitions) == 0 || len(plan.RedisTargets) == 0 || len(plan.SourcePins) == 0 || len(plan.Launchers) == 0 {
		return nil, ErrHold
	}
	runtime := &Runtime{Request: request, Plan: plan, Directory: directory}
	seeds, err := runtime.roleSeeds()
	if err != nil {
		return nil, err
	}
	for _, seed := range seeds {
		if seed.AcceptingClosedMs != 0 || seed.AcceptedSettledMs != 0 || seed.UnresolvedPublications != 0 || len(seed.AcceptedFrontierDigest) != 0 {
			return nil, ErrHold
		}
	}
	for _, pin := range plan.SourcePins {
		if _, err = ReadPin(pin, 16<<20); err != nil {
			return nil, err
		}
	}
	return runtime, nil
}

// LoadMaintenanceRuntime只接受P0预先列明的单次维护工具boot；真正开写还必须BindMaintenance强读当前SQL。
func LoadMaintenanceRuntime(planPath, directory, expectedSHA string) (*Runtime, error) {
	r, err := LoadRuntime(planPath, directory, expectedSHA)
	if err != nil {
		return nil, err
	}
	if r.Request.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || r.Seed("MAINTENANCE") == nil || len(r.Request.Evidence.GetPublication().LogicalRoles) != 1 {
		return nil, ErrHold
	}
	if filepath.Clean(filepath.Dir(planPath)) != filepath.Clean(directory) {
		return nil, ErrHold
	}
	fixedDirectory := false
	for _, pin := range r.Seed("MAINTENANCE").SourceEvidence {
		if _, err := ReadPin(pin, 16<<20); err != nil {
			return nil, err
		}
		if filepath.Clean(filepath.Dir(pin.AbsolutePath)) == filepath.Clean(directory) {
			fixedDirectory = true
		}
	}
	if !fixedDirectory {
		return nil, ErrHold
	}
	return r, nil
}

// CheckMaintenance核同一NORMAL已消费票据和当前冻结原件；它不是跨SQL/Redis事务。
// ReleaseReadiness必须包含本工具实际关闭原件，因此在本工具门禁关闭前不能释放；SQL每事务仍要锁meta复核。
func (r *Runtime) CheckMaintenance(ctx context.Context, catalog *authoritysql.Catalog, ticketID []byte) (*pb.SqlWriterMaintenanceReceipt, error) {
	if r == nil || catalog == nil || len(ticketID) != 16 || r.Request == nil || r.Request.Evidence == nil || r.Request.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || r.Seed("MAINTENANCE") == nil {
		return nil, ErrHold
	}
	ticket, err := catalog.FreezeTicket(ctx, ticketID)
	if err != nil {
		return nil, err
	}
	if ticket.State != pb.SqlWriterFreezeTicket_CONSUMED || ticket.AuthorizationKind != pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE || !bytes.Equal(ticket.OperationId, r.Plan.RunId) || ticket.Generation != r.Request.Generation || ticket.AttemptRevision != r.Request.AttemptRevision || !bytes.Equal(ticket.ManifestDigest, r.Request.Evidence.ManifestDigest) {
		return nil, ErrHold
	}
	frozen, err := catalog.MaintenanceReceipt(ctx, ticket.FreezeRequestId)
	if err != nil {
		return nil, err
	}
	if frozen.Kind != pb.SqlWriterMaintenanceReceipt_FREEZE || frozen.Phase != pb.SqlWriterMaintenanceReceipt_FROZEN || frozen.RunPhase != pb.SqlWriterDrainRun_SQL_FROZEN || !bytes.Equal(frozen.ReceiptDigest, ticket.ConsumedReceiptDigest) || !bytes.Equal(frozen.PlanProto, r.Request.PlanProto) || !bytes.Equal(frozen.RunId, r.Plan.RunId) || frozen.Generation != r.Request.Generation || frozen.AttemptRevision != ticket.AttemptRevision || frozen.AfterEpoch != r.Request.BeforeEpoch || frozen.CatalogRevision != r.Request.CatalogRevision {
		return nil, ErrHold
	}
	run, err := catalog.DrainRun(ctx, r.Plan.RunId)
	if err != nil {
		return nil, err
	}
	if run.Phase != pb.SqlWriterDrainRun_SQL_FROZEN || !bytes.Equal(run.ManifestProto, r.Request.PlanProto) || !bytes.Equal(run.FrozenRequestId, frozen.RequestId) || !bytes.Equal(run.LatestReceiptId, frozen.RequestId) || !bytes.Equal(run.LatestReceiptDigest, frozen.ReceiptDigest) {
		return nil, ErrHold
	}
	meta, err := catalog.CurrentMeta(ctx)
	if err != nil {
		return nil, err
	}
	if meta.Writable != 0 || meta.Generation != frozen.Generation || meta.WriterEpoch != frozen.AfterEpoch || meta.CatalogRevision != frozen.CatalogRevision || !bytes.Equal(meta.FreezeRunId, r.Plan.RunId) {
		return nil, ErrHold
	}
	return frozen, nil
}
func (r *Runtime) BindMaintenance(ctx context.Context, catalog *authoritysql.Catalog, ticketID []byte) (*Session, error) {
	if _, err := r.CheckMaintenance(ctx, catalog, ticketID); err != nil {
		return nil, err
	}
	seed := r.Seed("MAINTENANCE")
	if err := r.BindBoot(seed.InstanceId, nil); err != nil {
		return nil, err
	}
	return New(), nil
}

func ReadPin(pin *pb.SqlWriterMaintenanceReceipt_SourcePin, limit int64) ([]byte, error) {
	if pin == nil || !filepath.IsAbs(pin.AbsolutePath) || len(pin.Sha256) != 32 || limit < 1 || limit > 16<<20 {
		return nil, ErrHold
	}
	stat, err := os.Lstat(pin.AbsolutePath)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() < 1 || stat.Size() > limit {
		return nil, ErrHold
	}
	file, err := os.Open(pin.AbsolutePath)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || int64(len(raw)) > limit {
		return nil, ErrHold
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], pin.Sha256) {
		return nil, ErrHold
	}
	return raw, nil
}
func (r *Runtime) Seed(role string) *pb.SqlWriterMaintenanceReceipt_IssuerClosure {
	if r == nil || r.Request == nil || r.Request.Evidence == nil || r.Request.Evidence.GetPublication() == nil || r.Plan == nil {
		return nil
	}
	expected := r.Plan.RootIssuers
	if role == "RELAY" {
		expected = r.Plan.QueueMutators
	} else if role == "MAINTENANCE" && r.Request.RunPhase == pb.SqlWriterDrainRun_SQL_FROZEN {
		expected = r.Plan.MaintenanceIssuers
	} else if role != "SOURCE" {
		return nil
	}
	var selected *pb.SqlWriterMaintenanceReceipt_IssuerClosure
	for _, out := range r.Request.Evidence.GetPublication().LogicalRoles {
		if out == nil {
			continue
		}
		for _, seed := range expected {
			if seed == nil || !bytes.Equal(seed.IssuerId, out.IssuerId) || seed.LogicalRole != out.LogicalRole {
				continue
			}
			matched := false
			if r.Request.RunPhase == pb.SqlWriterDrainRun_SQL_RELEASED {
				matched = !bytes.Equal(seed.InstanceId, out.InstanceId) && len(out.InstanceId) == 16 && (len(seed.PhysicalWriterKey) == 0 && len(out.PhysicalWriterKey) == 0 || len(out.PhysicalWriterKey) == 16 && !bytes.Equal(seed.PhysicalWriterKey, out.PhysicalWriterKey))
			} else {
				matched = bytes.Equal(seed.InstanceId, out.InstanceId) && bytes.Equal(seed.PhysicalWriterKey, out.PhysicalWriterKey) && bytes.Equal(seed.PermanentRevivalFenceId, out.PermanentRevivalFenceId)
			}
			if role == "MAINTENANCE" {
				matched = proto.Equal(seed, out) && len(out.PhysicalWriterKey) == 0
			}
			if !matched || selected != nil {
				return nil
			}
			selected = out
		}
	}
	if selected == nil || len(selected.IssuerId) != 16 || len(selected.InstanceId) != 16 || len(selected.PermanentRevivalFenceId) != 16 || selected.LogicalRole == "" || len(selected.SourceEvidence) == 0 {
		return nil
	}
	return proto.Clone(selected).(*pb.SqlWriterMaintenanceReceipt_IssuerClosure)
}

// 本boot只绑定真实承担的角色；不为只有SOURCE或RELAY的服务伪造另一角色。
func (r *Runtime) roleSeeds() ([]*pb.SqlWriterMaintenanceReceipt_IssuerClosure, error) {
	if r == nil || r.Request == nil || r.Request.Evidence == nil || r.Request.Evidence.GetPublication() == nil {
		return nil, ErrHold
	}
	var seeds []*pb.SqlWriterMaintenanceReceipt_IssuerClosure
	roles := []string{"SOURCE", "RELAY"}
	if r.Request.RunPhase == pb.SqlWriterDrainRun_SQL_FROZEN {
		roles = []string{"MAINTENANCE"}
	}
	for _, role := range roles {
		if seed := r.Seed(role); seed != nil {
			seeds = append(seeds, seed)
		}
	}
	if len(seeds) == 0 || len(seeds) != len(r.Request.Evidence.GetPublication().LogicalRoles) {
		return nil, ErrHold
	}
	return seeds, nil
}
func (r *Runtime) Requested(role string) (bool, error) {
	name := "close-source.request"
	if role == "RELAY" {
		name = "close-relay.request"
	} else if role != "SOURCE" {
		return false, ErrHold
	}
	full := filepath.Join(r.Directory, name)
	raw, err := authoritysql.ReadPrivateControlFile(full, 64)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(raw)) != "CLOSE_"+role {
		return false, ErrHold
	}
	return true, nil
}
func (r *Runtime) Write(role string, gate *Session) error {
	r.boundMu.Lock()
	bound := r.bound
	r.boundMu.Unlock()
	if !bound {
		return ErrHold
	}
	seed := r.Seed(role)
	if seed == nil {
		return ErrHold
	}
	closure, err := gate.Closure(context.Background(), seed)
	if err != nil {
		return err
	}
	name := "source.issuer.json"
	if role == "RELAY" {
		name = "relay.issuer.json"
	} else if role == "MAINTENANCE" {
		name = "maintenance.issuer.json"
	}
	return WriteClosure(filepath.Join(r.Directory, name), closure)
}

// MergeSettled把真实下游owner的前沿并入本逻辑角色；不接收pending=0布尔替代原digest。
func (s *Session) MergeSettled(digest []byte) error {
	if len(digest) != 32 {
		return ErrHold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.upstream = append(s.upstream, bytes.Clone(digest))
	return nil
}

// MergeClosedSettled只供真实下游已join后的最终汇总；发布任何前沿后不能再改变凭据。
func (s *Session) MergeClosedSettled(digest []byte) error {
	if len(digest) != 32 {
		return ErrHold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed || len(s.pending) != 0 || s.unknown != 0 || s.finalized {
		return ErrHold
	}
	s.upstream = append(s.upstream, bytes.Clone(digest))
	return nil
}
func (s *Session) SettledDigest(ctx context.Context) ([]byte, error) {
	if err := s.Wait(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed || len(s.pending) != 0 || s.unknown != 0 {
		return nil, ErrHold
	}
	s.finalized = true
	var raw [24]byte
	binary.BigEndian.PutUint64(raw[:8], s.next)
	binary.BigEndian.PutUint64(raw[8:16], s.settled)
	binary.BigEndian.PutUint64(raw[16:], s.unknown)
	h := sha256.New()
	h.Write(raw[:])
	for _, digest := range s.upstream {
		h.Write(digest)
	}
	return h.Sum(nil), nil
}

func NewPrepared() *Session { s := New(); s.prepared = true; return s }
func (s *Session) OpenPrepared() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !s.prepared || s.next != 0 {
		return ErrClosed
	}
	s.prepared = false
	s.signal()
	return nil
}

// BindBoot由真实装配owner传当前已消费grant身份；无SQL的发行者也以同一私有目录consume阻止重启冒旧boot。
func (r *Runtime) BindBoot(instance, writer []byte) error {
	r.boundMu.Lock()
	defer r.boundMu.Unlock()
	if r.bound {
		return ErrHold
	}
	seeds, err := r.roleSeeds()
	if err != nil {
		return err
	}
	for _, seed := range seeds {
		if !bytes.Equal(seed.InstanceId, instance) || !bytes.Equal(seed.PhysicalWriterKey, writer) {
			return ErrHold
		}
	}
	marker := filepath.Join(r.Directory, "boot-"+hex.EncodeToString(instance)+".consumed")
	if err := authoritysql.WritePrivateControlFile(marker, r.Request.Evidence.ManifestDigest); err != nil {
		return ErrHold
	}
	r.bound = true
	return nil
}
func (r *Runtime) Readiness(role string, digest []byte) (*pb.SqlWriterMaintenanceReceipt_IssuerReadiness, error) {
	r.boundMu.Lock()
	bound := r.bound
	r.boundMu.Unlock()
	seed := r.Seed(role)
	if !bound || seed == nil || len(digest) != 32 || r.Request.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED {
		return nil, ErrHold
	}
	ready := &pb.SqlWriterMaintenanceReceipt_IssuerReadiness{IssuerId: bytes.Clone(seed.IssuerId), InstanceId: bytes.Clone(seed.InstanceId), PhysicalWriterKey: bytes.Clone(seed.PhysicalWriterKey), LogicalRole: seed.LogicalRole, PreparedClosedMs: uint64(time.Now().UnixMilli()), ConsumerReadyFrontierDigest: bytes.Clone(digest), SourcePins: seed.SourceEvidence}
	if len(seed.PhysicalWriterKey) == 16 {
		found := 0
		for _, pin := range seed.SourceEvidence {
			raw, err := ReadPin(pin, 16<<20)
			if err != nil {
				return nil, err
			}
			receipt := &pb.SqlWriterLifecycleReceipt{}
			if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, receipt) != nil {
				continue
			}
			request := &pb.SqlWriterLifecycleReceipt_Request{}
			if receipt.Kind != pb.SqlWriterLifecycleReceipt_REGISTER || proto.Unmarshal(receipt.RequestProto, request) != nil || request.Writer == nil || !bytes.Equal(request.Writer.WriterKey, seed.PhysicalWriterKey) || !bytes.Equal(request.Writer.InstanceId, seed.InstanceId) || !bytes.Equal(request.MaintenanceRunId, r.Plan.RunId) {
				continue
			}
			ready.RegistrationReceiptId = bytes.Clone(receipt.RequestId)
			ready.RegistrationReceiptDigest = bytes.Clone(receipt.RequestDigest)
			found++
		}
		if found != 1 {
			return nil, ErrHold
		}
	}
	return ready, nil
}

// ParseConsumerReadiness 只读取正式消费者原件；无消费者也必须显式给出空 groups。
// JSON 与 protobuf 二进制可跨语言互读；固定摘要绑定原字节，不重编码代替原件。
func ParseConsumerReadiness(raw []byte) (*pb.SqlWriterMaintenanceReceipt_ConsumerReadiness, error) {
	if len(raw) == 0 || len(raw) > 16<<20 {
		return nil, ErrHold
	}
	report := &pb.SqlWriterMaintenanceReceipt_ConsumerReadiness{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(raw, &envelope) != nil || envelope["groups"] == nil || bytes.Equal(bytes.TrimSpace(envelope["groups"]), []byte("null")) {
			return nil, ErrHold
		}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, report) != nil {
			return nil, ErrHold
		}
	} else if proto.Unmarshal(raw, report) != nil {
		return nil, ErrHold
	}
	if len(report.ProtoReflect().GetUnknown()) != 0 || len(report.Groups) > 128 || len(report.ManualAssignments) > 128 {
		return nil, ErrHold
	}
	for _, group := range report.Groups {
		if group == nil || len(group.ProtoReflect().GetUnknown()) != 0 || len(group.ExpectedPartitions) > 16384 || len(group.AssignedPartitions) > 16384 {
			return nil, ErrHold
		}
		for _, rows := range [][]*pb.SqlWriterMaintenanceReceipt_KafkaPartitionClosure{group.ExpectedPartitions, group.AssignedPartitions} {
			for _, row := range rows {
				if row == nil || len(row.ProtoReflect().GetUnknown()) != 0 {
					return nil, ErrHold
				}
			}
		}
	}
	for _, manual := range report.ManualAssignments {
		if manual == nil || len(manual.ProtoReflect().GetUnknown()) != 0 || len(manual.Assignments) == 0 || len(manual.Assignments) > 16384 || len(manual.SourcePins) > 128 {
			return nil, ErrHold
		}
		for _, row := range manual.Assignments {
			if row == nil || len(row.ProtoReflect().GetUnknown()) != 0 {
				return nil, ErrHold
			}
		}
		for _, pin := range manual.SourcePins {
			if pin == nil || len(pin.ProtoReflect().GetUnknown()) != 0 {
				return nil, ErrHold
			}
		}
	}
	return report, nil
}

// ReadinessWithEvidence 将实际消费者代次/分区原件固化后附到本次 P8；不改 P0 或旧 boot 身份。
// digest 必须就是 raw 的 SHA256。私有目录 CreateNew 保证同 boot 不能用后一次观察覆盖前沿。
func (r *Runtime) ReadinessWithEvidence(role string, digest, raw []byte) (*pb.SqlWriterMaintenanceReceipt_IssuerReadiness, error) {
	if len(raw) == 0 || len(raw) > 16<<20 {
		return nil, ErrHold
	}
	sum := sha256.Sum256(raw)
	if !bytes.Equal(sum[:], digest) {
		return nil, ErrHold
	}
	report, err := ParseConsumerReadiness(raw)
	if err != nil {
		return nil, err
	}
	ready, err := r.Readiness(role, digest)
	if err != nil {
		return nil, err
	}
	for _, group := range report.Groups {
		if !bytes.Equal(group.OperationId, r.Plan.RunId) || !bytes.Equal(group.ManifestDigest, r.Request.Evidence.ManifestDigest) || !bytes.Equal(group.IssuerId, ready.IssuerId) || !bytes.Equal(group.InstanceId, ready.InstanceId) {
			return nil, ErrHold
		}
	}
	for _, manual := range report.ManualAssignments {
		if !bytes.Equal(manual.OperationId, r.Plan.RunId) || !bytes.Equal(manual.ManifestDigest, r.Request.Evidence.ManifestDigest) {
			return nil, ErrHold
		}
		for _, row := range manual.Assignments {
			if !bytes.Equal(row.IssuerId, ready.IssuerId) || !bytes.Equal(row.InstanceId, ready.InstanceId) || !bytes.Equal(row.TargetInstanceId, ready.InstanceId) {
				return nil, ErrHold
			}
		}
	}
	for _, pin := range ready.SourcePins {
		if pin != nil && bytes.Equal(pin.Sha256, digest) {
			return nil, ErrHold
		}
	}
	path := filepath.Join(r.Directory, "consumer-ready."+hex.EncodeToString(ready.IssuerId)+"."+hex.EncodeToString(ready.InstanceId)+".json")
	if err = writeNewRuntime(path, raw); err != nil {
		return nil, err
	}
	saved, err := authoritysql.ReadPrivateControlFile(path, 16<<20)
	if err != nil || !bytes.Equal(saved, raw) {
		return nil, ErrHold
	}
	ready.SourcePins = append(append([]*pb.SqlWriterMaintenanceReceipt_SourcePin{}, ready.SourcePins...), &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: bytes.Clone(sum[:])})
	return ready, nil
}

// AwaitOpen只为SQL_RELEASED后全新boot服务。prepared与实际open分别保存正式原件，旧SOURCE Session从不重开。
func (r *Runtime) AwaitOpen(ctx context.Context, ready *pb.SqlWriterMaintenanceReceipt_IssuerReadiness, open func(context.Context) ([]byte, error)) error {
	if ready == nil || open == nil || r.Request.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED {
		return ErrHold
	}
	prepared, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(ready)
	if err != nil {
		return err
	}
	path := filepath.Join(r.Directory, "prepared.source.json")
	if err = writeNewRuntime(path, prepared); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		requestPath := filepath.Join(r.Directory, "open-source.request")
		raw, err := authoritysql.ReadPrivateControlFile(requestPath, 1<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		request := &pb.SqlWriterMaintenanceReceipt_SourceOpenAck{}
		if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, request) != nil || !bytes.Equal(request.IssuerId, ready.IssuerId) || !bytes.Equal(request.InstanceId, ready.InstanceId) || !bytes.Equal(request.PhysicalWriterKey, ready.PhysicalWriterKey) || request.LogicalRole != ready.LogicalRole || !bytes.Equal(request.RegistrationReceiptId, ready.RegistrationReceiptId) || !bytes.Equal(request.RegistrationReceiptDigest, ready.RegistrationReceiptDigest) || !bytes.Equal(request.OperationId, r.Plan.RunId) || !bytes.Equal(request.ManifestDigest, r.Request.Evidence.ManifestDigest) || len(request.OpenRequestId) != 16 || request.OpenedMs != 0 || len(request.AcceptedFrontierDigest) != 0 {
			return ErrHold
		}
		if len(request.SourcePins) != len(ready.SourcePins) {
			return ErrHold
		}
		for i, pin := range request.SourcePins {
			if !proto.Equal(pin, ready.SourcePins[i]) {
				return ErrHold
			}
		}
		frontier, err := open(ctx)
		if err != nil || len(frontier) != 32 {
			if err != nil {
				return err
			}
			return ErrHold
		}
		pinSum := sha256.Sum256(raw)
		request.OpenedMs = uint64(time.Now().UnixMilli())
		request.AcceptedFrontierDigest = bytes.Clone(frontier)
		request.SourcePins = append(append([]*pb.SqlWriterMaintenanceReceipt_SourcePin{}, ready.SourcePins...), &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: requestPath, Sha256: pinSum[:]})
		raw, err = (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(request)
		if err != nil {
			return err
		}
		return writeNewRuntime(filepath.Join(r.Directory, "source.open.ack.json"), raw)
	}
}
func writeNewRuntime(path string, raw []byte) error {
	return authoritysql.WritePrivateControlFile(path, raw)
}

// SnapshotDigest在实际open提交点读取同一admission锁下的accepted前沿，不拿prepared摘要代替已开事实。
func (s *Session) SnapshotDigest() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := sha256.New()
	var value [8]byte
	for _, n := range []uint64{s.next, s.settled, s.unknown, uint64(len(s.pending))} {
		binary.BigEndian.PutUint64(value[:], n)
		h.Write(value[:])
	}
	ids := []uint64{}
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		binary.BigEndian.PutUint64(value[:], id)
		h.Write(value[:])
	}
	for _, digest := range s.upstream {
		h.Write(digest)
	}
	return h.Sum(nil)
}

// WaitRuntime让正常serve的同一boot接收唯一受控排空，不通过重启替换旧实例。
// 配置正文与SHA分开CreateNew交付；读取失败不解释为“没有任务”。
func WaitRuntime(ctx context.Context, directory string) (*Runtime, error) {
	if err := authoritysql.CheckPrivateControlDirectory(directory); err != nil {
		return nil, ErrHold
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		planPath := filepath.Join(directory, "active.runtime.json")
		hashPath := filepath.Join(directory, "active.runtime.sha256")
		_, planErr := os.Lstat(planPath)
		_, hashErr := os.Lstat(hashPath)
		if errors.Is(planErr, os.ErrNotExist) || errors.Is(hashErr, os.ErrNotExist) {
			continue
		}
		if planErr != nil || hashErr != nil {
			return nil, ErrHold
		}
		hash, err := authoritysql.ReadPrivateControlFile(hashPath, 66)
		if err != nil {
			return nil, err
		}
		if len(hash) < 64 {
			return nil, ErrHold
		}
		return LoadRuntime(planPath, directory, strings.TrimSpace(string(hash)))
	}
}

// RedisMaintenanceNonceKey 是P0单次建立、P4只读的同run唯一nonce键。
func RedisMaintenanceNonceKey(runID []byte) (string, error) {
	if len(runID) != 16 || bytes.Equal(runID, make([]byte, 16)) {
		return "", ErrHold
	}
	return "sql:writer:maintenance:" + hex.EncodeToString(runID) + ":nonce", nil
}
