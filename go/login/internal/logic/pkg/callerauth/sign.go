package callerauth

import (
	"context"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	login_proto "proto/common/base"
)

// SignedMetadata 造出一条**全新**的、带签名的出站 metadata。
//
// # 为什么是「全新」而不是在入站 metadata 上追加
//
// confused deputy:一个服务替上游转发自己没验过的凭据,就变成了攻击者的
// 代理人。所以签名侧的纪律是 —— 出站 metadata 从空开始拼,入站的
// authorization / cookie / 别人的 x-caller-sig 一个字节都不带过去。
// 本函数不接收任何 context,天然做不到「顺手继承」,这是刻意的接口形状。
//
// subject 传 SessionDetails 的**原始序列化字节**;函数内部负责 base64。
func SignedMetadata(s Signer, caller, fullMethod string, subject []byte) (metadata.MD, error) {
	nonce, err := NewNonce()
	if err != nil {
		return nil, err
	}
	ts := NowMillis()
	sig, err := SignCanonical(s, caller, fullMethod, subject, ts, nonce)
	if err != nil {
		return nil, err
	}
	return metadata.Pairs(
		MetaSessionDetail, encodeSubject(subject),
		MetaCaller, caller,
		MetaTimestamp, formatMillis(ts),
		MetaNonce, nonce,
		MetaSignature, sig,
	), nil
}

// SignedOutgoingContext 把 SignedMetadata 装进出站 context。
//
// metadata.NewOutgoingContext 是**整体替换**语义,不会把 ctx 上已有的出站
// metadata 合并进来 —— 这正是我们要的:任何未经本次签名的字段都不许出去。
func SignedOutgoingContext(ctx context.Context, s Signer, caller, fullMethod string, subject []byte) (context.Context, error) {
	md, err := SignedMetadata(s, caller, fullMethod, subject)
	if err != nil {
		return ctx, err
	}
	return metadata.NewOutgoingContext(ctx, md), nil
}

// SignedMetadataForDetails 是给「手上是 SessionDetails 对象」的调用方用的薄封装。
func SignedMetadataForDetails(s Signer, caller, fullMethod string, detail *login_proto.SessionDetails) (metadata.MD, error) {
	bin, err := proto.Marshal(detail)
	if err != nil {
		return nil, err
	}
	return SignedMetadata(s, caller, fullMethod, bin)
}
