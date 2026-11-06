package com.game.mall;
import static org.junit.jupiter.api.Assertions.*;
import java.util.*;
import org.junit.jupiter.api.Test;
class DeliveryStoreTest {
 private List<String> paidOrders(CommerceFixture f, int count) {
  List<String> ids = new ArrayList<>();
  for (int i = 0; i < count; i++) {
   var order = f.store.create("1", new CommerceApi.CreateOrder("poll-order-" + i, 1, 1, "测试账号", "1"));
   f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(), "test", "poll-payment-" + i, order.amountMinor(), order.currency()));
   ids.add(order.orderId());
  }
  ids.sort(String::compareTo);
  return ids;
 }

 @Test void 二十个未终局意图不挡后五项_固定同毫秒上界_完成与新增不破坏翻页() {
  try (var f = new CommerceFixture()) {
   f.sku(1, 100, 100);
   var ids = paidOrders(f, 25);
   var store = new DeliveryStore(f.jdbc, f.manager, f.props);
   var first = store.poll(new DeliveryApi.Poll(20, ""));
   assertEquals(ids.subList(0, 20), first.entries().stream().map(e -> e.intent().orderId()).toList());
   String cursor = first.entries().getLast().cursor();
   assertFalse(cursor.isEmpty());
   assertEquals(first, new DeliveryStore(f.jdbc, f.manager, f.props).poll(new DeliveryApi.Poll(20, "")), "多worker可重复扫描，不能破坏性领取");
   store.receipt(new DeliveryApi.Receipt(ids.getFirst(), "123", "DELIVERED", "a".repeat(64)));
   var later = f.store.create("1", new CommerceApi.CreateOrder("poll-later-order", 1, 1, "测试账号", "1"));
   f.store.recordTrustedPayment(new CommerceApi.Payment(later.orderId(), "test", "poll-later-payment", later.amountMinor(), later.currency()));
   f.jdbc.update("UPDATE commerce_delivery_intent SET created_ms=created_ms+10000 WHERE order_sn=?", later.orderId());
   var second = store.poll(new DeliveryApi.Poll(20, cursor));
   assertEquals(ids.subList(20, 25), second.entries().stream().map(e -> e.intent().orderId()).toList());
   assertEquals("", second.entries().getLast().cursor(), "本轮末项处理后绕回，新单不延长本轮上界");
   var nextRound = store.poll(new DeliveryApi.Poll(20, ""));
   var nextTail = store.poll(new DeliveryApi.Poll(20, nextRound.entries().getLast().cursor()));
   assertTrue(nextTail.entries().stream().anyMatch(e -> e.intent().orderId().equals(later.orderId())));
   assertEquals(25, f.jdbc.queryForObject("SELECT COUNT(*) FROM commerce_delivery_intent WHERE status='PENDING'", Integer.class));
  }
 }

 @Test void 上界意图在分页之间完成_空尾页允许绕回_原Intent不混入游标() throws Exception {
  try (var f = new CommerceFixture()) {
   f.sku(1, 10, 100);
   var ids = paidOrders(f, 2);
   var store = new DeliveryStore(f.jdbc, f.manager, f.props);
   var first = store.poll(new DeliveryApi.Poll(1, ""));
   var intent = first.entries().getFirst().intent();
   String original = new com.fasterxml.jackson.databind.ObjectMapper().writeValueAsString(intent);
   assertFalse(original.contains("cursor"));
   assertFalse(original.contains("createdMs"));
   store.receipt(new DeliveryApi.Receipt(ids.getLast(), "123", "DELIVERED", "b".repeat(64)));
   assertTrue(store.poll(new DeliveryApi.Poll(1, first.entries().getFirst().cursor())).entries().isEmpty());
   var restarted = store.poll(new DeliveryApi.Poll(1, ""));
   assertEquals(intent, restarted.entries().getFirst().intent());
   assertEquals(original, new com.fasterxml.jackson.databind.ObjectMapper().writeValueAsString(restarted.entries().getFirst().intent()));
  }
 }

 @Test void 畸形或越界游标整体拒绝_不能当第一页_空队列返回空数组() {
  try (var f = new CommerceFixture()) {
   var store = new DeliveryStore(f.jdbc, f.manager, f.props);
   String low = "00112233-4455-6677-8899-aabbccddeeff", high = "11112233-4455-6677-8899-aabbccddeeff";
   for (String invalid : List.of(" ", "v2:1:" + low + ":2:" + high, "v1:01:" + low + ":2:" + high,
       "v1:-1:" + low + ":2:" + high, "v1:+1:" + low + ":2:" + high,
       "v1:9223372036854775808:" + low + ":9223372036854775808:" + high,
       "v1:1:" + low.toUpperCase(Locale.ROOT) + ":2:" + high,
       "v1:3:" + low + ":2:" + high, "v1:2:" + high + ":2:" + low, "x".repeat(257))) {
    assertEquals(400, assertThrows(CommerceException.class, () -> store.poll(new DeliveryApi.Poll(20, invalid))).status(), invalid);
   }
   assertEquals(400, assertThrows(CommerceException.class, () -> store.poll(new DeliveryApi.Poll(0, ""))).status());
   assertEquals(400, assertThrows(CommerceException.class, () -> store.poll(new DeliveryApi.Poll(101, ""))).status());
   assertNotNull(store.poll(new DeliveryApi.Poll(20, "")).entries());
   assertTrue(store.poll(new DeliveryApi.Poll(20, "")).entries().isEmpty());
  }
 }
 @Test void 无游戏映射或交付数量越界下单回滚库存与订单(){try(var f=new CommerceFixture()){f.sku(1,10,100);f.jdbc.update("DELETE FROM commerce_sku_grant WHERE sku_id=1");assertThrows(CommerceException.class,()->f.store.create("1",new CommerceApi.CreateOrder("unknown-sku-001",1,1, "测试账号", "1")));assertEquals(0,f.count("oms_order"));assertEquals(0,f.value("pms_sku_stock","lock_stock"));f.jdbc.update("INSERT INTO commerce_sku_grant(sku_id,item_config_id,count_per_unit,title) VALUES(1,10,4096,'玄铁')");assertThrows(CommerceException.class,()->f.store.create("1",new CommerceApi.CreateOrder("over-sku-001",1,2, "测试账号", "1")));assertEquals(0,f.count("oms_order"));assertEquals(0,f.value("pms_sku_stock","lock_stock"));}}
 @Test void 冻结商品映射_支付后可恢复领取_持久回执幂等且拒改合同(){try(var f=new CommerceFixture()){
  f.sku(1,5,100);f.jdbc.update("UPDATE commerce_sku_grant SET count_per_unit=2 WHERE sku_id=1");
  var order=f.store.create("1",new CommerceApi.CreateOrder("delivery-001",1,2, "测试账号", "1"));
  f.jdbc.update("UPDATE commerce_sku_grant SET item_config_id=99,count_per_unit=9 WHERE sku_id=1");
  f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(),"test","payment-delivery",order.amountMinor(),order.currency()));
  var store=new DeliveryStore(f.jdbc,f.manager,f.props);assertEquals(1,store.poll(20).size());assertEquals(10,store.poll(20).getFirst().itemConfigId());assertEquals(4,store.poll(20).getFirst().itemCount());assertEquals(f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")),f.store.list("1",new DeliveryApi.OrderPage("",10, "测试账号", "1")).getFirst());assertTrue(f.store.list("1",new DeliveryApi.OrderPage(order.orderId(),10, "测试账号", "1")).isEmpty());assertThrows(CommerceException.class,()->f.store.list("2",new DeliveryApi.OrderPage(order.orderId(),10, "测试账号", "1")));
  assertEquals("DELIVERY_PENDING",f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());
  var receipt=new DeliveryApi.Receipt(order.orderId(),"123","DELIVERED","a".repeat(64));store.receipt(receipt);store.receipt(receipt);
  assertEquals("DELIVERED",f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());assertTrue(store.poll(20).isEmpty());
  assertThrows(CommerceException.class,()->store.receipt(new DeliveryApi.Receipt(order.orderId(),"124","DELIVERED","a".repeat(64))));
 }}
 @Test void 拒绝到账才退款_可信渠道退款回执复核金额币种并幂等(){try(var f=new CommerceFixture()){
  f.sku(1,5,100);var order=f.store.create("1",new CommerceApi.CreateOrder("refund-001",1,1, "测试账号", "1"));f.store.recordTrustedPayment(new CommerceApi.Payment(order.orderId(),"test","payment-refund",100,"CNY"));
  var store=new DeliveryStore(f.jdbc,f.manager,f.props);store.receipt(new DeliveryApi.Receipt(order.orderId(),"123","REFUND_PENDING","b".repeat(64)));
  // 交付被拒:退款待办挂在订单绑定的那笔付款上(按交易键),来源记 DELIVERY_REJECTED。
  assertEquals(List.of(new CommerceApi.Payment(order.orderId(),"test","payment-refund",100,"CNY")),store.refunds());
  assertEquals("DELIVERY_REJECTED",f.jdbc.queryForObject("SELECT source FROM commerce_refund_intent WHERE order_sn=?",String.class,order.orderId()));
  assertThrows(CommerceException.class,()->store.refunded(new CommerceApi.Refund(order.orderId(),"test","payment-refund","refund-01",99,"CNY")));
  assertEquals(400,assertThrows(CommerceException.class,()->store.refunded(new CommerceApi.Refund(order.orderId(),"test",null,"refund-01",100,"CNY"))).status(),"回执必须指明退的是哪一笔付款");
  var refund=new CommerceApi.Refund(order.orderId(),"test","payment-refund","refund-01",100,"CNY");store.refunded(refund);store.refunded(refund);assertEquals("REFUNDED",f.store.get("1", new CommerceApi.OwnedOrder(order.orderId(), "测试账号", "1")).state());assertTrue(store.refunds().isEmpty());
 }}
}
