package accountdirectory

import (
	"context"
	"regexp"
	"shared/savebarrier"
	"strings"
	"unicode/utf8"

	base "proto/common/base"
	dspb "proto/data_service"
)

const SceneSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE"
const MaxTradeHolders = 256

var instanceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Admissions 的成功写应答表示 SQL 已提交。Register 未知结果只重试原世代，不能先激活。
// Close/Ack 只接受 scene 在本地封住 <= through 的实体与全部迟到回调后发出的证明；
// Confirm 仍独立强读真实玩家 SQL，scene 的声明本身不能使交易进入 FROZEN。
type Admissions interface {
	Register(context.Context, *dspb.RegisterRoleAdmissionRequest) (*dspb.RegisterRoleAdmissionResponse, error)
	CloseAdmissions(context.Context, *dspb.CloseRoleAdmissionsRequest) (*dspb.CloseRoleAdmissionsResponse, error)
	AckDrain(context.Context, *dspb.AckRoleTradeDrainRequest) error
}

func ValidSceneInstance(id string) bool { return instanceIDPattern.MatchString(id) }
func validSceneNode(id string) bool {
	return id != "" && len(id) <= 96 && utf8.ValidString(id) && !strings.ContainsAny(id, "\x00\r\n\t ")
}
func ValidAdmission(i *base.RoleAdmissionIdentity) bool {
	return i != nil && i.PlayerId > 0 && ValidSceneInstance(i.SceneInstanceId) && i.Generation > 0 && i.OwnerRevision > 0 && i.OwnerEpoch > 0 && validSceneNode(i.SceneNodeId) && i.SceneId > 0 && i.ZoneId > 0 && i.SessionId > 0 && i.LoginGeneration > 0
}
func ValidTradeSeal(s *base.RoleTradeSeal) bool {
	return s != nil && s.ListingId > 0 && s.PlayerId > 0 && s.OwnerRevision > 0 && ValidSceneInstance(s.SceneInstanceId) && s.ThroughGeneration > 0 && validSceneNode(s.SceneNodeId) && s.ZoneId > 0
}
func ValidSaveFloor(epoch, seq uint64) bool { return (epoch == 0) == (seq == 0) }

func ValidFreezeEvidence(e *dspb.AccountRoleFreezeEvidence) bool {
	if e == nil || e.PlayerId == 0 || e.ListingId == 0 || e.OwnerRevision == 0 || len(e.PersistedDigest) != 32 {
		return false
	}
	switch e.Kind {
	case dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_NO_ADMISSION:
		return e.OwnerEpoch == 0 && e.SaveSeq == 0
	case dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_SCENE:
		return e.OwnerEpoch > 0 && e.SaveSeq > 0 &&
			!(e.OwnerEpoch == savebarrier.InitialOwnerEpoch && e.SaveSeq == savebarrier.InitialSaveSeq)
	case dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT:
		return e.OwnerEpoch == savebarrier.InitialOwnerEpoch && e.SaveSeq == savebarrier.InitialSaveSeq
	default:
		return false
	}
}

// NoActorFreezeEvidence 的两类原件都不能携带原生 holder；初始 SQL 提交不冒充 scene 保存。
func NoActorFreezeEvidence(e *dspb.AccountRoleFreezeEvidence) bool {
	return e != nil && (e.Kind == dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_NO_ADMISSION ||
		e.Kind == dspb.AccountRoleFreezeEvidenceKind_ACCOUNT_ROLE_FREEZE_EVIDENCE_KIND_INITIAL_COMMIT)
}
