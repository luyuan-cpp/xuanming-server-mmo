// Package mailgrant 只转换邮件展示附件和既有资产包，不拥有玩家资产。
package mailgrant

import (
	"errors"
	"math"

	"shared/currencytype"

	assetpb "proto/common/asset"
	mailpb "proto/mail"
)

var ErrInvalid = errors.New("邮件附件无效")

// Bundle 是发信与榜奖共同的唯一校验/转换；exists必须接当前权威Item表。
func Bundle(attachments *mailpb.MailAttachments, exists func(uint32) bool) (*assetpb.AssetBundle, error) {
	result := &assetpb.AssetBundle{}
	if attachments == nil {
		return result, nil
	}
	if len(attachments.ProtoReflect().GetUnknown()) != 0 || len(attachments.Currencies) > 4 || len(attachments.Items) > 16 {
		return nil, ErrInvalid
	}
	currencies := map[uint32]bool{}
	for _, currency := range attachments.Currencies {
		// 币种按 proto 同源取值表校验(shared/currencytype):写错的值在这里拒绝,scene 才能把不认识的币种判成版本错位。
		if currency == nil || len(currency.ProtoReflect().GetUnknown()) != 0 || !currencytype.IsKnown(currency.CurrencyType) || currency.Amount == 0 || currency.Amount > math.MaxInt64 || currencies[currency.CurrencyType] {
			return nil, ErrInvalid
		}
		currencies[currency.CurrencyType] = true
		result.Currencies = append(result.Currencies, &assetpb.CurrencyAmount{CurrencyType: currency.CurrencyType, Amount: currency.Amount})
	}
	items := map[uint32]bool{}
	var totalItems uint64
	for _, item := range attachments.Items {
		if item == nil || len(item.ProtoReflect().GetUnknown()) != 0 || item.ConfigId == 0 || item.Count == 0 || items[item.ConfigId] || exists == nil || !exists(item.ConfigId) {
			return nil, ErrInvalid
		}
		totalItems += uint64(item.Count)
		if totalItems > 4096 { // 与scene资产通道的整包授予上限一致，不能生成永远无法到账的意图。
			return nil, ErrInvalid
		}
		items[item.ConfigId] = true
		result.Items = append(result.Items, &assetpb.ItemGrant{ConfigId: item.ConfigId, Count: item.Count})
	}
	return result, nil
}

// View对当前邮件可发形状完全转换；未知新形状必须拒绝，不显示成一封空奖邮件。
func View(bundle *assetpb.AssetBundle) (*mailpb.MailAttachments, error) {
	result := &mailpb.MailAttachments{}
	if bundle == nil {
		return result, nil
	}
	if len(bundle.ProtoReflect().GetUnknown()) != 0 || len(bundle.ItemUuids) > 0 || bundle.PetId != 0 || len(bundle.ItemInstances) > 0 || bundle.PetInstance != nil {
		return nil, ErrInvalid
	}
	for _, currency := range bundle.Currencies {
		if currency == nil {
			return nil, ErrInvalid
		}
		result.Currencies = append(result.Currencies, &mailpb.MailCurrency{CurrencyType: currency.CurrencyType, Amount: currency.Amount})
	}
	for _, item := range bundle.Items {
		if item == nil {
			return nil, ErrInvalid
		}
		result.Items = append(result.Items, &mailpb.MailItem{ConfigId: item.ConfigId, Count: item.Count})
	}
	return result, nil
}
