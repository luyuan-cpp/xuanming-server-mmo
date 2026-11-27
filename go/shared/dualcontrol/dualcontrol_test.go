package dualcontrol

import (
	"errors"
	"testing"

	base "proto/common/base"
)

func approval() *base.AdminApproval {
	return &base.AdminApproval{ApprovalId: "9007199254740993", Proposer: "alice.w", Approver: "bob@corp.example", ApprovedAtMs: 1_800_000_000_000}
}

func TestRequireAcceptsAWellFormedApprovalFromAnotherPerson(t *testing.T) {
	if err := Require(approval(), "alice.w"); err != nil {
		t.Fatalf("Require() = %v, want nil", err)
	}
}

func TestRequireRejectsAMissingRecord(t *testing.T) {
	if err := Require(nil, "alice.w"); !errors.Is(err, ErrMissing) {
		t.Fatalf("Require(nil) = %v, want ErrMissing", err)
	}
}

func TestRequireRejectsEveryMalformedOrSelfApprovedRecord(t *testing.T) {
	cases := map[string]func(*base.AdminApproval){
		"空批准号":       func(a *base.AdminApproval) { a.ApprovalId = "" },
		"批准号带前导零":    func(a *base.AdminApproval) { a.ApprovalId = "0123" },
		"批准号超过 19 位": func(a *base.AdminApproval) { a.ApprovalId = "12345678901234567890" },
		"批准号不是数字":    func(a *base.AdminApproval) { a.ApprovalId = "12a" },
		"空发起人":       func(a *base.AdminApproval) { a.Proposer = "" },
		"发起人带空格":     func(a *base.AdminApproval) { a.Proposer = "alice w" },
		"空批准人":       func(a *base.AdminApproval) { a.Approver = "" },
		"批准人超长":      func(a *base.AdminApproval) { a.Approver = string(make([]byte, 65)) },
		"没有批准时刻":     func(a *base.AdminApproval) { a.ApprovedAtMs = 0 },
		"自己批自己":      func(a *base.AdminApproval) { a.Approver = a.Proposer },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := approval()
			mutate(a)
			if err := Require(a, a.GetProposer()); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Require() = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRequireRejectsAProposerWhoIsNotTheRequestOperator(t *testing.T) {
	for _, operator := range []string{"mallory", "", "Alice.w"} {
		if err := Require(approval(), operator); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Require(operator=%q) = %v, want ErrInvalid", operator, err)
		}
	}
}

func TestRequireComparesPeopleByteForByte(t *testing.T) {
	// 大小写不同就是两个标识;「一人一个标识」由后台登录保证,这里不替身份提供方做大小写折叠。
	a := approval()
	a.Approver = "ALICE.W"
	if err := Require(a, "alice.w"); err != nil {
		t.Fatalf("Require() = %v, want nil", err)
	}
}
