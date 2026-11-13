package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/redisslot"
)

// 托管归位(docs/design/player-connection-lost-hold.md §4.5)。
//
// 开了挂机月卡的玩家断线后,角色由 scene 节点离线托管(断线保留 + 月卡,同文 §4.1);托管期间本服务每 300 秒续一次断线租约
// (handleLeaseExpiry 的 afkPassActive 分支)。托管中的角色可能掉出世界、没有任何节点再持有他:托管着他的节点崩溃、排空改派
// 没被放行、托管落点的事件丢了或目标节点没建出来(同文 §4.3、§4.4 末的残余)。数据在盘上,但开月卡就是为了不登录也留在世界里。
//
// 做法是按期望状态对账:期望 = 会话断线中、月卡有效、托管归位信息就是这次登录;实际 = 在场心跳(此刻对他负责的 scene 节点
// 每 20 秒续一次,存活 60 秒,cpp hosted_presence.h)。续租时心跳不在,就替他向角色所在 zone 的 scene_manager 发一次托管归位:
// hosted_placement、不带 gate、关联号 0、不带去向(scene_manager 按 location 放回他最后在的场景,场景没了回默认大世界),
// 会话号与代次是断开的那次登录,归属修订取归位信息里托管节点记的那一份。后面的事全是 scene_manager 进场链既有的:
// 换手门、判死 / 被顶替接管、持有者释放(归位带 keep_held,持有节点有实体就不动)与 §17 撤销、铸代、托管落点、目标节点看护。
//
// 节拍就是续租的 300 秒,不另起扫描;租约本来就按 slot 分片,所以随副本数水平扩展。一次续租至多发一次,不在这里重试。

const (
	// hostedResumeMinPassRemaining:月卡剩下不到这么多就不归位了 —— 角色刚落地就会因月卡到期退出
	// (scene 侧提前 5 秒结束托管,connection_lost_hold::kHostingExitLead),白占一次落点。
	hostedResumeMinPassRemaining = 60 * time.Second
	// hostedResumeTimeout 是一次托管归位 EnterScene 的上限。zrpc 客户端自己的超时(SceneManagerRpc.Timeout)更短时以它为准。
	hostedResumeTimeout = 5 * time.Second
)

// hostedReturnRecord 是 scene 节点写的托管归位信息(cpp hosted_presence.h FormatReturnRecord)。
type hostedReturnRecord struct {
	zoneID          uint32
	ownerRevision   uint64
	sessionID       uint32
	loginGeneration uint64
}

const hostedReturnRecordVersion = "v1"

// parseHostedReturnRecord 解析 "v1:<zone>:<owner_revision>:<session_id>:<login_generation>"。四个字段都必须是非 0 的十进制数
// (与写入方 ReturnRecordWritable 同口径),版本前缀不认识也按格式错误处理。
func parseHostedReturnRecord(value string) (hostedReturnRecord, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 5 || parts[0] != hostedReturnRecordVersion {
		return hostedReturnRecord{}, fmt.Errorf("unexpected hosted return record %q", value)
	}
	var fields [4]uint64
	bits := [4]int{32, 64, 32, 64}
	for i := range fields {
		v, err := strconv.ParseUint(parts[i+1], 10, bits[i])
		if err != nil || v == 0 || parts[i+1][0] == '0' {
			return hostedReturnRecord{}, fmt.Errorf("unexpected hosted return record %q", value)
		}
		fields[i] = v
	}
	return hostedReturnRecord{zoneID: uint32(fields[0]), ownerRevision: fields[1], sessionID: uint32(fields[2]),
		loginGeneration: fields[3]}, nil
}

// hostedResumeVerdict 是续租时要不要替托管角色归位的判定。名字只进日志。底层类型 uint8 数的是结论种类数(现 7 种)。
type hostedResumeVerdict uint8

const (
	// hostedResumePresent:在场心跳还在 —— 有节点对他负责(托管着、改派途中、看护中)。最常见,不打日志。
	hostedResumePresent hostedResumeVerdict = iota
	// hostedResumePassEnding:月卡剩下不到 hostedResumeMinPassRemaining(或读不出)。
	hostedResumePassEnding
	// hostedResumeNoRecord:没有归位信息 —— 托管从没开始过(普通保留到期)、已过月卡到期,或归属修订被判失效后删掉了。
	hostedResumeNoRecord
	// hostedResumeRecordMalformed:归位信息坏了,组不出合法的请求。
	hostedResumeRecordMalformed
	// hostedResumeOtherLogin:归位信息不是这条会话那一次登录的(旧登录留下的),不替它归位。
	hostedResumeOtherLogin
	// hostedResumeNoZone:location 与归位信息都给不出 zone。
	hostedResumeNoZone
	// hostedResumeSend:发托管归位。
	hostedResumeSend

	hostedResumeVerdictCount
)

var hostedResumeVerdictNames = [...]string{
	hostedResumePresent:         "present",
	hostedResumePassEnding:      "pass_ending",
	hostedResumeNoRecord:        "no_record",
	hostedResumeRecordMalformed: "record_malformed",
	hostedResumeOtherLogin:      "other_login",
	hostedResumeNoZone:          "no_zone",
	hostedResumeSend:            "send",
}

// 名表长度与 hostedResumeVerdictCount 不一致时编译失败(漏加 / 多加一行)。
var _ [hostedResumeVerdictCount]struct{} = [len(hostedResumeVerdictNames)]struct{}{}

func (v hostedResumeVerdict) String() string {
	if v < hostedResumeVerdictCount {
		return hostedResumeVerdictNames[v]
	}
	return "unknown"
}

// hostedResumeFacts 是 decideHostedResume 的输入,全部由调用方一次读好。零值取「不发」的一侧。
type hostedResumeFacts struct {
	presencePresent bool
	passRemaining   time.Duration // 月卡剩余;没有 / 读不出 = 0
	recordFound     bool
	recordErr       error
	record          hostedReturnRecord
	sessionID       uint32 // 这条断线会话的登录身份
	loginGeneration uint64
	locationZone    uint32 // location 记的 zone;没有 location 或无 zone = 0
}

// recordBelongsToSession:归位信息解析得出,且就是这条断线会话那一次登录(会话号与代次逐一相等、代次非 0)。
func recordBelongsToSession(f hostedResumeFacts) bool {
	return f.recordFound && f.recordErr == nil && f.loginGeneration != 0 &&
		f.record.sessionID == f.sessionID && f.record.loginGeneration == f.loginGeneration
}

// decideHostedResume 按顺序判,零副作用。zone 取 location 的(权威),location 没有 zone 时取归位信息里托管节点的。
func decideHostedResume(f hostedResumeFacts) (hostedResumeVerdict, uint32) {
	switch {
	case f.presencePresent:
		return hostedResumePresent, 0
	case f.passRemaining < hostedResumeMinPassRemaining:
		return hostedResumePassEnding, 0
	case !f.recordFound:
		return hostedResumeNoRecord, 0
	case f.recordErr != nil:
		return hostedResumeRecordMalformed, 0
	case !recordBelongsToSession(f):
		return hostedResumeOtherLogin, 0
	}
	zone := f.locationZone
	if zone == 0 {
		zone = f.record.zoneID
	}
	if zone == 0 {
		return hostedResumeNoZone, 0
	}
	return hostedResumeSend, zone
}

// returnRecordTTLAlignment:归位信息该活多久 —— 到月卡到期为止,与写入方(cpp hosted_presence::ReturnRecordTtlSec)同一口径。
// 只对这次登录的那一份给出;ok = false 时不动它。托管着他的节点每 20 秒按自己读到的月卡重写一次;不在任何节点上托管时
// (改派、看护途中,或已掉出世界)没人重写,月卡续费后它会按旧的到期时刻过期,归位就断了 —— 续租时由这里按当下的月卡对齐。
func returnRecordTTLAlignment(f hostedResumeFacts) (seconds int64, ok bool) {
	if !recordBelongsToSession(f) || f.passRemaining < time.Second {
		return 0, false
	}
	return int64(f.passRemaining / time.Second), true
}

// luaDeleteHostedReturnIfValue:归位信息仍是读到的那一份才删(托管节点可能刚写了新的)。
var luaDeleteHostedReturnIfValue = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// luaExpireHostedReturnIfValue:归位信息仍是读到的那一份才把存活改成 ARGV[2] 秒。
var luaExpireHostedReturnIfValue = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// readHostedResumeFacts 一次 pipeline 读齐判定要的四个键(都在玩家自己的 slot 上)。任一读失败都返回错误:拿不准就不发。
func readHostedResumeFacts(ctx context.Context, svcCtx *svc.ServiceContext, session *pb.PlayerSession, now time.Time) (hostedResumeFacts, string, error) {
	playerID := session.PlayerId
	var presence, record, pass *redis.StringCmd
	var location *redis.StringCmd
	_, _ = svcCtx.PlayerRedis.Pipelined(ctx, func(p redis.Pipeliner) error {
		presence = p.Get(ctx, redisslot.HostedPresenceKey(playerID))
		record = p.Get(ctx, redisslot.HostedReturnKey(playerID))
		pass = p.Get(ctx, afkPassKey(playerID))
		location = p.Get(ctx, sceneManagerLocationKey(playerID))
		return nil
	})
	facts := hostedResumeFacts{sessionID: session.SessionId, loginGeneration: session.LoginGeneration}
	for _, cmd := range []*redis.StringCmd{presence, record, pass, location} {
		if err := cmd.Err(); err != nil && !errors.Is(err, redis.Nil) {
			return facts, "", fmt.Errorf("read hosted resume facts for player %d: %w", playerID, err)
		}
	}
	facts.presencePresent = presence.Err() == nil
	if expiry, err := strconv.ParseInt(pass.Val(), 10, 64); pass.Err() == nil && err == nil {
		facts.passRemaining = time.Unix(expiry, 0).Sub(now)
	}
	rawRecord := ""
	if record.Err() == nil {
		facts.recordFound = true
		rawRecord = record.Val()
		facts.record, facts.recordErr = parseHostedReturnRecord(rawRecord)
	}
	if location.Err() == nil {
		loc := &smpb.PlayerLocation{}
		if err := proto.Unmarshal([]byte(location.Val()), loc); err != nil {
			return facts, "", fmt.Errorf("decode SceneManager location for player %d: %w", playerID, err)
		}
		facts.locationZone = loc.GetZoneId()
	}
	return facts, rawRecord, nil
}

// resumeHostedIfLost 在 AFK 续租成功之后调用(handleLeaseExpiry)。不返回错误:结果只进日志,下一次续租再判。
func resumeHostedIfLost(ctx context.Context, svcCtx *svc.ServiceContext, session *pb.PlayerSession, now time.Time) {
	facts, rawRecord, err := readHostedResumeFacts(ctx, svcCtx, session, now)
	if err != nil {
		logx.Errorf("LeaseMonitor: hosted resume check skipped: %v", err)
		return
	}
	// 先把归位信息的存活对齐到当下的月卡(不论有没有节点负责):改派 / 看护途中续费,不对齐的话旧的到期时刻一过它就没了。
	// 对齐失败不挡归位,下一次续租再对齐。
	if ttl, ok := returnRecordTTLAlignment(facts); ok {
		if err := luaExpireHostedReturnIfValue.Run(ctx, svcCtx.PlayerRedis, []string{redisslot.HostedReturnKey(session.PlayerId)},
			rawRecord, ttl).Err(); err != nil {
			logx.Errorf("LeaseMonitor: align hosted return record ttl for player %d failed: %v", session.PlayerId, err)
		}
	}
	verdict, zone := decideHostedResume(facts)
	switch verdict {
	case hostedResumePresent, hostedResumeNoRecord, hostedResumePassEnding:
		return
	case hostedResumeSend:
	default:
		logx.Errorf("LeaseMonitor: hosted character of player %d has no responsible scene node but cannot be resumed: verdict=%s session=%d login_generation=%d record_err=%v",
			session.PlayerId, verdict, session.SessionId, session.LoginGeneration, facts.recordErr)
		return
	}

	client, err := svcCtx.SceneManagerForZone(zone)
	if err != nil {
		logx.Errorf("LeaseMonitor: hosted resume of player %d not sent: %v", session.PlayerId, err)
		return
	}
	logx.Infof("LeaseMonitor: hosted character of player %d has no responsible scene node; requesting hosted resume in zone %d (session=%d login_generation=%d owner_revision=%d)",
		session.PlayerId, zone, session.SessionId, session.LoginGeneration, facts.record.ownerRevision)
	callCtx, cancel := context.WithTimeout(ctx, hostedResumeTimeout)
	defer cancel()
	resp, err := client.EnterScene(callCtx, &smpb.EnterSceneRequest{
		PlayerId:        session.PlayerId,
		SessionId:       session.SessionId,
		LoginGeneration: session.LoginGeneration,
		OwnerRevision:   facts.record.ownerRevision,
		GateZoneId:      zone,
		HostedPlacement: true,
	})
	switch {
	case status.Code(err) == codes.FailedPrecondition:
		// 归属修订已失效(角色被交易 / 封存、换了账号)或角色此刻不能调度:这份归位信息授权不了任何请求,删掉,不再替他归位。
		deleted, delErr := luaDeleteHostedReturnIfValue.Run(ctx, svcCtx.PlayerRedis,
			[]string{redisslot.HostedReturnKey(session.PlayerId)}, rawRecord).Int()
		logx.Infof("LeaseMonitor: hosted resume of player %d refused by scene_manager (ownership revision no longer valid); return record dropped=%v del_err=%v: %v",
			session.PlayerId, deleted == 1, delErr, err)
	case err != nil:
		logx.Errorf("LeaseMonitor: hosted resume of player %d failed (next lease renewal retries): %v", session.PlayerId, err)
	case resp.GetErrorCode() != 0:
		// 18 = location 的节点还没交出(已请它补凭证,拿不出资格时由 §17 撤销)、17 = 再入屏障未到、1 = 暂时没有去处……
		// 都是 scene_manager 进场链的既有结论,下一次续租再来。
		logx.Infof("LeaseMonitor: hosted resume of player %d not placed yet: error_code=%d msg=%q (next lease renewal retries)",
			session.PlayerId, resp.GetErrorCode(), resp.GetErrorMessage())
	default:
		// 落点已提交、事件已投给目标节点;目标节点建不出来时由它的看护有界重试(§4.4),都不成心跳仍不在,下一次续租再来。
		logx.Infof("LeaseMonitor: hosted resume of player %d placed in zone %d", session.PlayerId, zone)
	}
}
