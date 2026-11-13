package noderegistry

// Spec.LeaseBoundMarkers 的纯单测:不连 etcd。标记随注册 Txn 写、挂同一个租约,见包注释「租约绑定标记」。

import (
	"errors"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// 前缀不得为空、不得重复,也不得落在注册前缀的子树下(那棵子树按 NodeInfo 解析、扫已占 node_id 时整棵读)。
func TestSpecValidateRejectsBadLeaseBoundMarkers(t *testing.T) {
	cases := []struct {
		name    string
		markers []string
		wantErr bool
	}{
		{name: "没有标记", markers: nil, wantErr: false},
		{name: "两个不同前缀", markers: []string{"cap/a/", "cap/b/"}, wantErr: false},
		{name: "空前缀", markers: []string{""}, wantErr: true},
		{name: "重复前缀", markers: []string{"cap/a/", "cap/a/"}, wantErr: true},
		{name: "落在注册前缀子树下", markers: []string{unitPrefix + "/caps/"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := unitSpec()
			spec.LeaseBoundMarkers = tc.markers
			err := spec.validate()
			if tc.wantErr {
				if !errors.Is(err, errInvalidSpec) {
					t.Fatalf("validate() = %v, want errInvalidSpec", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}

// 三条注册路径(首次注册、失租后重夺原号、换号)都只从 registrationPuts 取 Put:双 key 在前,标记键按前缀顺序在后,
// 全部挂同一个租约。少一个标记,租约一换就会留下「有 NodeInfo、没有标记」的实例。
func TestRegistrationPutsCarryEveryMarkerOnTheSameLease(t *testing.T) {
	spec := unitSpec()
	spec.LeaseBoundMarkers = []string{"cap/a/", "cap/b/"}
	r := newRegistration(nil, spec, unitUUID, "")
	defer r.cancel()
	const (
		id    uint32 = 7
		lease        = clientv3.LeaseID(0x42)
	)

	puts := r.registrationPuts(id, "value", lease)

	wantKeys := []string{
		AllocationKey(unitPrefix, unitNodeType, id),
		RpcPath(unitPrefix, unitZone, unitNodeType, id),
		"cap/a/" + unitUUID,
		"cap/b/" + unitUUID,
	}
	wantValues := []string{unitUUID, "value", markerValue, markerValue}
	if len(puts) != len(wantKeys) {
		t.Fatalf("registrationPuts returned %d ops, want %d", len(puts), len(wantKeys))
	}
	for i, op := range puts {
		if !op.IsPut() {
			t.Fatalf("op %d is not a Put", i)
		}
		if got := string(op.KeyBytes()); got != wantKeys[i] {
			t.Errorf("op %d key = %q, want %q", i, got, wantKeys[i])
		}
		if got := string(op.ValueBytes()); got != wantValues[i] {
			t.Errorf("op %d value = %q, want %q", i, got, wantValues[i])
		}
	}
}

// 没有标记时只写双 key,与加这个选项之前一样。
func TestRegistrationPutsWithoutMarkersWritesOnlyTheTwoKeys(t *testing.T) {
	r := newRegistration(nil, unitSpec(), unitUUID, "")
	defer r.cancel()

	if puts := r.registrationPuts(7, "value", clientv3.LeaseID(0x42)); len(puts) != 2 {
		t.Fatalf("registrationPuts returned %d ops, want 2", len(puts))
	}
	if keys := markerKeysFor(nil, unitUUID); keys != nil {
		t.Fatalf("markerKeysFor(nil) = %v, want nil", keys)
	}
}
