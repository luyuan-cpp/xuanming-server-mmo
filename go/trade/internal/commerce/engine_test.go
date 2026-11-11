package commerce

import (
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"shared/currencytype"
	"testing"
)

func TestSelectionBoundsAllStackedItemsBeforeEscrow(t *testing.T) {
	p := Policy{Items: []uint32{10, 11}}
	require.NoError(t, p.Selection(&tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2048, 11: 2048}}))
	require.Error(t, p.Selection(&tradepb.TradeAssetSelection{Items: map[uint32]uint32{10: 2048, 11: 2049}}))
}

// 绑定灵石不论 Currencies 怎么配都不可交易:配置校验(config.IsTradableCurrency)本就拒绝它进白名单,
// 这里绕过配置直接把它配进去,托管选择与报价仍按不可交易拒绝。用例只点名取值表常量,不写币种数字。
func TestBindDiamondIsNotTradableEvenWhenConfigured(t *testing.T) {
	p := Policy{Currencies: []uint32{currencytype.Gold, currencytype.Diamond, currencytype.BindDiamond}}
	require.NoError(t, p.Selection(&tradepb.TradeAssetSelection{CurrencyType: currencytype.Diamond, CurrencyAmount: 5}))
	require.ErrorIs(t, p.Selection(&tradepb.TradeAssetSelection{CurrencyType: currencytype.BindDiamond, CurrencyAmount: 5}), ErrNotTradable)
	require.True(t, validPrice(5, currencytype.Diamond, p))
	require.False(t, validPrice(5, currencytype.BindDiamond, p))
	require.False(t, validPrice(5, currencytype.Gold, Policy{Currencies: []uint32{currencytype.Diamond}}), "不在白名单里的币种照旧不能报价")
}

func TestTransferConvertsDebitSelectionToCreditWithoutChangingOriginalInstances(t *testing.T) {
	b := &assetpb.AssetBundle{ItemUuids: []uint64{77}, PetId: 88}
	s := &assetpb.AssetSnapshot{Items: []*assetpb.AssetItemInstance{{ItemUuid: 77, ConfigId: 10, StackSize: 1, InstanceProto: []byte{0x08, 0x01, 0xf8, 0x7f, 0x03}}}, Pet: &assetpb.AssetPetInstance{PetId: 88, InstanceProto: []byte{0x08, 0x02, 0xf8, 0x7f, 0x04}}}
	out, err := Transfer(b, s)
	require.NoError(t, err)
	require.Empty(t, out.ItemUuids, "credit不能携带debit选择字段")
	require.Zero(t, out.PetId)
	require.True(t, proto.Equal(s.Items[0], out.ItemInstances[0]))
	require.True(t, proto.Equal(s.Pet, out.PetInstance))
	require.Equal(t, []uint64{77}, b.ItemUuids, "不更改原托管请求")
	require.EqualValues(t, 88, b.PetId)
	s.Items[0].ItemUuid = 78
	_, err = Transfer(b, s)
	require.Error(t, err, "数量相等不能替代身份匹配")
	s.Items[0].ItemUuid = 77
	s.Pet.PetId = 89
	_, err = Transfer(b, s)
	require.Error(t, err)
}
