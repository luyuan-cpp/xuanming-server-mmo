package main

// -mode capability-check -db-capability-zones <list>:只读,逐个 zone 核对 go/db 能力标记
// db:capability:zone:{z} == placement-routing-v1(docs/design/player-storage-placement.md §4.3)。
//
// 用途:合服 T-0 的 C 阶段查不了 src / dst(两者已 zone-down,90s 心跳标记已过期),dst 的能力改在它 zone-up 之后、
// 开服之前用本模式核对(runbook §8 Step 6)。也可以在任何动手之前单独核对一组 zone。
//
// 退出码与 -mode audit 同一口径:
//
//	0 — 列出的 zone 全部 present
//	1 — 至少一个 missing(没有标记,或值不是 placement-routing-v1):该 zone 此刻没有新版 go/db 在续写,不许开服 / 动手
//	2 — 至少一个 unreadable(读失败):结论不可信,不是「通过」。2 优先于 1。
//	    参数用法错误(-db-capability-zones 没给 / 写 none / 写错)同样是 2:一个标记都没查,属于「没查成」,
//	    不能与 missing 的 1 同码 —— 否则运维会照 runbook §8 Step 6 的 exit 1 分支去换 go/db,方向错了

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// capabilityProbe 是一个 zone 的能力标记读取结果。
type capabilityProbe struct {
	Zone    uint32
	Value   string // 键存在时的原值
	Present bool   // 键存在
	Err     error  // 读失败(Present / Value 无意义)
}

// capabilityGetter 读一把键:(值, 键存在, 错误)。生产实现见 redisCapabilityGetter,测试用内存替身。
type capabilityGetter func(ctx context.Context, key string) (string, bool, error)

// redisCapabilityGetter 把 GET 包成 capabilityGetter:redis.Nil = 键不存在,不是错误。
func redisCapabilityGetter(rdb *redis.Client) capabilityGetter {
	return func(ctx context.Context, key string) (string, bool, error) {
		v, err := rdb.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	}
}

// probeCapabilityMarkers 逐个 zone 读标记。逐个 GET 而不是一次 MGET:一个 zone 读失败只让那个 zone 算 unreadable。
func probeCapabilityMarkers(ctx context.Context, get capabilityGetter, zones []uint32) []capabilityProbe {
	out := make([]capabilityProbe, 0, len(zones))
	for _, z := range zones {
		v, present, err := get(ctx, capabilityKey(z))
		out = append(out, capabilityProbe{Zone: z, Value: v, Present: present, Err: err})
	}
	return out
}

// modeCapabilityCheck 是 -mode 的取值。
const modeCapabilityCheck = "capability-check"

// 退出码(与 audit 同一口径)。
const (
	capabilityCheckExitOK         = 0
	capabilityCheckExitMissing    = 1
	capabilityCheckExitUnreadable = 2
)

// capabilityCheckZones 是 -mode capability-check 的参数校验:解析 -db-capability-zones 原文并按 capabilityForCheck 要求必填、
// 不接受 none。成功返回要查的 zone 与 capabilityCheckExitOK;参数写错 / 没给 / 写 none 时返回 capabilityCheckExitUnreadable
// 与原因(一个标记都没查 = 没查成,见文件头退出码说明)。纯函数。
func capabilityCheckZones(raw string) ([]uint32, int, error) {
	spec, err := parseCapabilityZoneSpec(raw)
	if err == nil {
		err = requireCapabilityZones(spec, capabilityForCheck)
	}
	if err != nil {
		return nil, capabilityCheckExitUnreadable, err
	}
	return spec.zones, capabilityCheckExitOK, nil
}

// evaluateCapabilityCheck 是纯判定:逐 zone 给出 present / missing / unreadable 一行报告,外加一行汇总;
// 返回退出码。值不等于 placement-routing-v1 算 missing 并打印实际值;读失败算 unreadable。
// 没有任何 probe 时按 unreadable 处理(什么都没查 ≠ 通过)。
func evaluateCapabilityCheck(probes []capabilityProbe) (int, []string) {
	var report []string
	present, missing, unreadable := 0, 0, 0
	for _, p := range probes {
		key := capabilityKey(p.Zone)
		switch {
		case p.Err != nil:
			unreadable++
			report = append(report, fmt.Sprintf("zone %d: unreadable (%s: %v)", p.Zone, key, p.Err))
		case !p.Present:
			missing++
			report = append(report, fmt.Sprintf("zone %d: missing (%s does not exist — no placement-aware go/db has refreshed it "+
				"within the last 90s)", p.Zone, key))
		case p.Value != capabilityRoutingV1:
			missing++
			report = append(report, fmt.Sprintf("zone %d: missing (%s = %q, want %q)", p.Zone, key, p.Value, capabilityRoutingV1))
		default:
			present++
			report = append(report, fmt.Sprintf("zone %d: present (%s = %q)", p.Zone, key, p.Value))
		}
	}
	code := capabilityCheckExitOK
	verdict := "all listed zones run a placement-aware go/db"
	switch {
	case unreadable > 0 || len(probes) == 0:
		code = capabilityCheckExitUnreadable
		verdict = "could NOT complete — the result is not a clean bill of health"
	case missing > 0:
		code = capabilityCheckExitMissing
		verdict = "NOT confirmed — do not open the zone or proceed until its go/db is the placement-aware version"
	}
	report = append(report, fmt.Sprintf("capability-check: %d present, %d missing, %d unreadable — %s",
		present, missing, unreadable, verdict))
	return code, report
}

// capabilityCheckEntryParams 是 -mode capability-check 的全部输入。
type capabilityCheckEntryParams struct {
	zones       []uint32
	mappingAddr string
	mappingPwd  string
	mappingDB   int
	timeout     time.Duration
}

// runCapabilityCheckEntry 执行 -mode capability-check 并以判定的退出码结束进程。只读,不需要 -dry-run / -apply。
func runCapabilityCheckEntry(p capabilityCheckEntryParams) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	log.Printf("=== Capability check for zones %v (mapping=%s db=%d) — read-only ===", p.zones, p.mappingAddr, p.mappingDB)
	rdb := redis.NewClient(&redis.Options{Addr: p.mappingAddr, Password: p.mappingPwd, DB: p.mappingDB})
	probes := probeCapabilityMarkers(ctx, redisCapabilityGetter(rdb), p.zones)
	_ = rdb.Close()
	code, report := evaluateCapabilityCheck(probes)
	for _, line := range report {
		log.Print(line)
	}
	os.Exit(code)
}
