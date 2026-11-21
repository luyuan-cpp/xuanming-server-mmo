"use strict";
// 只保留运维令牌的当前输入；不写浏览器存储，不接触商城HMAC密钥。
const keyInput = document.getElementById("admin-key");
const result = document.getElementById("result");
const status = document.getElementById("status");
const endpoint = new URL("admin/mall/", window.location.href);
let pending = false;
document.getElementById("forget-key").addEventListener("click", () => { keyInput.value = ""; });
window.addEventListener("pagehide", () => { keyInput.value = ""; });
for (const form of document.querySelectorAll("form")) {
  const request = form.elements.namedItem("requestId");
  if (request) { request.value = crypto.randomUUID(); }
  const button = form.querySelector(".new-request");
  if (button) { button.addEventListener("click", () => { request.value = crypto.randomUUID(); }); }
}
function integer(form, name, min, max) {
  const raw = form.elements.namedItem(name).value;
  if (!/^-?(0|[1-9][0-9]*)$/.test(raw)) { throw new Error("整数格式非法：" + name); }
  const value = BigInt(raw);
  if (value < min || value > max) { throw new Error("整数超出范围：" + name); }
  // 以十进制字符串发送，服务端按long/int解析；不经JS浮点数丢失SKU或修订号。
  return raw;
}
function value(form, name) { return form.elements.namedItem(name).value; }
async function command(path, input) {
  if (pending) { return; }
  if (!keyInput.value) { status.textContent = "请先填写运维令牌"; return; }
  pending = true;
  const buttons = document.querySelectorAll("form button");
  buttons.forEach(button => { button.disabled = true; });
  status.textContent = "请求中…";
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 10000);
  try {
    const response = await fetch(new URL(path, endpoint), {
      method: "POST", headers: {"Content-Type": "application/json", "X-Admin-Key": keyInput.value},
      body: JSON.stringify(input), signal: controller.signal, cache: "no-store", redirect: "error", credentials: "omit"
    });
    // 不用JSON.parse重编码64位整数；原始响应安全写入文本节点。
    result.textContent = await response.text();
    status.textContent = response.ok ? "成功 · HTTP " + response.status : "请求被拒绝 · HTTP " + response.status;
  } catch (error) {
    status.textContent = "结果未确认";
    result.textContent = "请求中断或响应不可用。请保留原请求号和参数，查询结果后以原请求号重放。";
  } finally {
    clearTimeout(timeout); pending = false;
    buttons.forEach(button => { button.disabled = false; });
  }
}
function submit(id, build) {
  const form = document.getElementById(id);
  form.addEventListener("submit", event => {
    event.preventDefault();
    try { const [path, body] = build(form); void command(path, body); }
    catch (error) { status.textContent = "未提交"; result.textContent = error.message; }
  });
}
const maxLong = 9223372036854775807n;
submit("list-form", form => ["skus/list", {afterSkuId: integer(form, "afterSkuId", 0n, maxLong), limit: integer(form, "limit", 1n, 100n)}]);
submit("order-form", form => ["order-view", {orderId: value(form, "orderId")}]);
submit("terms-form", form => {
  const isCurrency = value(form, "grantKind") === "currency";
  const terms = {
    productId: integer(form, "productId", 1n, maxLong), skuCode: value(form, "skuCode"),
    title: value(form, "title"), priceMinor: integer(form, "priceMinor", 1n, 9000000000000000n), currency: value(form, "currency"),
    itemConfigId: isCurrency ? "0" : integer(form, "itemConfigId", 1n, 2147483647n),
    countPerUnit: isCurrency ? "0" : integer(form, "countPerUnit", 1n, 4096n),
    currencyType: isCurrency ? integer(form, "currencyType", 0n, 2n) : "0",
    currencyAmount: isCurrency ? integer(form, "currencyAmount", 1n, 9000000000000000n) : "0",
    published: value(form, "published") === "true"
  };
  const body = {requestId: value(form, "requestId"), skuId: integer(form, "skuId", 1n, maxLong), terms};
  if (value(form, "mode") === "create") { body.stock = integer(form, "stock", 0n, 2147483647n); }
  else { body.expectedRevision = integer(form, "expectedRevision", 0n, maxLong - 1n); }
  return ["skus/" + value(form, "mode"), body];
});
submit("stock-form", form => ["skus/stock", {
  requestId: value(form, "requestId"), skuId: integer(form, "skuId", 1n, maxLong),
  expectedRevision: integer(form, "expectedRevision", 0n, maxLong - 1n), delta: integer(form, "delta", -2147483648n, 2147483647n)
}]);
submit("manual-list-form", form => ["manual/list", {
  afterOrderId: value(form, "afterOrderId").trim(),
  afterListingId: value(form, "afterListingId").trim(),
  afterRefundCursor: value(form, "afterRefundCursor").trim(),
  limit: integer(form, "limit", 1n, 100n)
}]);
submit("manual-order-form", form => ["manual/orders/resolve", {
  requestId: value(form, "requestId"), orderId: value(form, "orderId").trim(),
  action: value(form, "action"), reason: value(form, "reason").trim()
}]);
submit("manual-refund-form", form => ["manual/refunds/resolve", {
  requestId: value(form, "requestId"), orderId: value(form, "orderId").trim(),
  channel: value(form, "channel").trim(), paymentTransactionId: value(form, "paymentTransactionId").trim(),
  action: value(form, "action"), externalRefundId: value(form, "externalRefundId").trim(),
  reason: value(form, "reason").trim()
}]);
submit("manual-consignment-form", form => ["manual/consignments/resolve", {
  requestId: value(form, "requestId"), listingId: value(form, "listingId").trim(),
  action: value(form, "action"), reason: value(form, "reason").trim()
}]);
