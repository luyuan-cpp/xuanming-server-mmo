// Package dualcontrol 核对高风险管理请求带的双人复核批准记录(docs/design/admin-identity-and-dual-control.md §5.5)。
//
// 「谁能做什么」只在后台(java/gm_admin)判一次,规则也只写在后台流水里;这里是同一条规则在资源服务一侧的只读复查,
// 防的是后台的缺陷与误配置(把没批准的命令发了下来、把发起人当成了批准人),不产生任何状态。
// 防不了后台进程本身被攻破:那样的后台能伪造两个人,升级路径见设计 §4 的做法 C,届时只改这一个函数和密钥配置。
//
// 批准记录在请求体里,请求自带的 ServiceCallAuth 签名覆盖整个请求摘要,所以调用方必须先验调用证明,再调 Require。
package dualcontrol

import (
	"errors"
	"fmt"
	"regexp"

	base "proto/common/base"
)

var (
	// ErrMissing 表示请求没带批准记录。
	ErrMissing = errors.New("dual-control approval record is missing")
	// ErrInvalid 表示批准记录不合格:缺项、格式不对、两人相同,或者发起人不是请求里的操作人。
	ErrInvalid = errors.New("dual-control approval record is invalid")
)

var (
	// subjectPattern 与后台登录时对身份提供方主体的限制相同(GmOidcProvider.SUBJECT)。
	subjectPattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)
	// approvalIDPattern 是后台流水号:63 位以内的十进制正整数文本。
	approvalIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
)

// Require 核对一条高风险请求带了合格的批准记录:批准号、发起人、批准人、批准时刻四项都在且合规,
// 两人不同(按字节比较,不做大小写折叠),发起人就是请求里的操作人。合格返回 nil;否则返回包着 ErrMissing 或 ErrInvalid 的错误,
// 错误文本只点出哪一项不合格,不回显请求内容。
func Require(approval *base.AdminApproval, operator string) error {
	if approval == nil {
		return ErrMissing
	}
	switch {
	case !approvalIDPattern.MatchString(approval.GetApprovalId()):
		return fmt.Errorf("%w: approval_id must be a positive decimal number", ErrInvalid)
	case !subjectPattern.MatchString(approval.GetProposer()):
		return fmt.Errorf("%w: proposer is not a valid operator identifier", ErrInvalid)
	case !subjectPattern.MatchString(approval.GetApprover()):
		return fmt.Errorf("%w: approver is not a valid operator identifier", ErrInvalid)
	case approval.GetApprovedAtMs() == 0:
		return fmt.Errorf("%w: approved_at_ms is missing", ErrInvalid)
	case approval.GetProposer() == approval.GetApprover():
		return fmt.Errorf("%w: proposer and approver are the same person", ErrInvalid)
	case approval.GetProposer() != operator:
		return fmt.Errorf("%w: proposer differs from the request operator", ErrInvalid)
	}
	return nil
}
