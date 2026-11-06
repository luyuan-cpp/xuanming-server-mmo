-- 从 macrozheng/mall dcaa93b3 的 PMS/OMS 提取并加固；只新增，不 DROP、不写样例账户。
-- 第三方电商表例外：本模块 SQL 是迁移事实源，不属于游戏 proto 表生成树。
-- price_minor/pay_amount 全部为最小货币单位整数；player_id 使用规范 uint64 十进制字符串。
-- 必须使用独立库，不能连接上游 mall 实物电商库；此事实源只负责全新 v1 安装。
CREATE TABLE IF NOT EXISTS commerce_schema_version (
  version INT PRIMARY KEY,
  storage_contract VARCHAR(80) NOT NULL
);
CREATE TABLE IF NOT EXISTS commerce_buyer_guard (buyer_id VARCHAR(20) PRIMARY KEY);
CREATE TABLE IF NOT EXISTS pms_sku_stock (
  id BIGINT PRIMARY KEY,
  product_id BIGINT NOT NULL,
  sku_code VARCHAR(64) NOT NULL,
  price_minor BIGINT NOT NULL,
  currency VARCHAR(16) NOT NULL,
  stock INT NOT NULL,
  lock_stock INT NOT NULL DEFAULT 0,
  published INT NOT NULL DEFAULT 0,
  catalog_revision BIGINT NOT NULL DEFAULT 0,
  CHECK (price_minor > 0), CHECK (stock >= 0), CHECK (lock_stock >= 0 AND lock_stock <= stock)
);
CREATE TABLE IF NOT EXISTS oms_order (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  order_sn VARCHAR(36) NOT NULL,
  buyer_id VARCHAR(20) NOT NULL,
  request_id VARBINARY(128) NOT NULL,
  sku_id BIGINT NOT NULL,
  quantity INT NOT NULL,
  pay_amount BIGINT NOT NULL,
  currency VARCHAR(16) NOT NULL,
  status INT NOT NULL,
  create_time_ms BIGINT NOT NULL,
  payment_deadline_ms BIGINT NOT NULL,
  payment_time_ms BIGINT NULL,
  UNIQUE (order_sn), UNIQUE (buyer_id, request_id), INDEX idx_commerce_buyer_status (buyer_id,status),
  CHECK (quantity > 0), CHECK (pay_amount > 0)
);
CREATE TABLE IF NOT EXISTS oms_order_item (
  order_sn VARCHAR(36) PRIMARY KEY,
  product_id BIGINT NOT NULL,
  product_sku_id BIGINT NOT NULL,
  product_sku_code VARCHAR(64) NOT NULL,
  product_quantity INT NOT NULL,
  product_price_minor BIGINT NOT NULL
);
-- 原账号与目录修订在下单同事务冻结，角色后续过户不能继承旧付款的交付资格。
CREATE TABLE IF NOT EXISTS commerce_official_owner (
  order_sn VARCHAR(36) PRIMARY KEY,
  buyer_account VARBINARY(191) NOT NULL,
  buyer_owner_revision VARBINARY(20) NOT NULL,
  INDEX idx_official_owner(buyer_account,buyer_owner_revision,order_sn)
);
-- 商城收到的每一笔付款都在这里占一行，(channel, transaction_id) 全局唯一。order_sn 是这笔付款绑定的订单，一单至多绑定一笔；
-- 为 NULL 表示已付订单又收到的另一笔，没有绑定任何订单，归属与退款见 commerce_duplicate_payment / commerce_refund_intent。
CREATE TABLE IF NOT EXISTS commerce_payment_receipt (
  channel VARBINARY(32) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  order_sn VARCHAR(36) NULL,
  amount_minor BIGINT NOT NULL,
  currency VARCHAR(16) NOT NULL,
  PRIMARY KEY (channel, transaction_id), UNIQUE (order_sn)
);
-- 已付订单的重复付款(2026-11-05，第一轮数据审计 §4 第 19 条)：只记它冲的是哪张订单，金额币种在上表那一行；
-- 退款走 commerce_refund_intent(source=DUPLICATE_PAYMENT)，订单、库存与交付都不动。
CREATE TABLE IF NOT EXISTS commerce_duplicate_payment (
  channel VARBINARY(32) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  order_sn VARCHAR(36) NOT NULL,
  created_ms BIGINT NOT NULL,
  PRIMARY KEY (channel, transaction_id),
  INDEX idx_commerce_duplicate_order (order_sn),
  CHECK (created_ms >= 0)
);
CREATE TABLE IF NOT EXISTS commerce_delivery_intent (
  order_sn VARCHAR(36) PRIMARY KEY,
  buyer_id VARCHAR(20) NOT NULL,
  sku_id BIGINT NOT NULL,
  quantity INT NOT NULL,
  status VARCHAR(24) NOT NULL,
  created_ms BIGINT NOT NULL,
  INDEX idx_commerce_delivery_pending(status,created_ms,order_sn)
);
CREATE TABLE IF NOT EXISTS commerce_request_nonce (
  caller_role VARCHAR(16) NOT NULL,
  nonce VARBINARY(128) NOT NULL,
  expires_ms BIGINT NOT NULL,
  PRIMARY KEY (caller_role, nonce),
  INDEX idx_commerce_nonce_expiry (expires_ms)
);
-- 安装成功才盖章；应用只读验证，不自行重跑 DDL。此安装脚本仅用于全新独立库。
-- 游戏交付扩展：仅创建新的可靠合同表，绝不修改已经支付订单的事实。
CREATE TABLE IF NOT EXISTS commerce_sku_grant (sku_id BIGINT PRIMARY KEY,item_config_id INT NOT NULL,count_per_unit INT NOT NULL,title VARCHAR(128) NOT NULL,CHECK (item_config_id>0),CHECK (count_per_unit>0 AND count_per_unit<=4096));
CREATE TABLE IF NOT EXISTS commerce_order_grant (order_sn VARCHAR(36) PRIMARY KEY,item_config_id INT NOT NULL,count_per_unit INT NOT NULL,title VARCHAR(128) NOT NULL);
CREATE TABLE IF NOT EXISTS commerce_delivery_receipt (order_sn VARCHAR(36) PRIMARY KEY,trade_order_id VARCHAR(20) NOT NULL,state VARCHAR(24) NOT NULL,proof VARCHAR(64) NOT NULL);
-- 退款待办与退款回执按被退的那笔付款 (channel, transaction_id) 识别(2026-11-05 起；之前按 order_sn，一单只能退一笔)：
-- 一张订单可以同时有订单本身的退款和重复付款的退款。source 决定退款回执是否推进订单：CANCELLED_LATE(取消后迟到的付款)
-- 与 DELIVERY_REJECTED(交付被拒)退的是订单绑定的那笔，回执把订单 5→6；DUPLICATE_PAYMENT 只终结自己的待办，不碰订单。
CREATE TABLE IF NOT EXISTS commerce_refund_intent (
  channel VARBINARY(32) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  order_sn VARCHAR(36) NOT NULL,
  source VARCHAR(24) NOT NULL,
  status VARCHAR(24) NOT NULL,
  PRIMARY KEY (channel, transaction_id),
  INDEX idx_commerce_refund_pending (status, order_sn),
  CHECK (source IN ('CANCELLED_LATE','DELIVERY_REJECTED','DUPLICATE_PAYMENT')), CHECK (status IN ('PENDING','REFUNDED'))
);
-- payment_transaction_id 是被退的那笔付款，transaction_id 是渠道的退款流水；一笔付款只认一条退款流水。
CREATE TABLE IF NOT EXISTS commerce_refund_receipt (
  channel VARBINARY(32) NOT NULL,
  payment_transaction_id VARBINARY(128) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  order_sn VARCHAR(36) NOT NULL,
  amount_minor BIGINT NOT NULL,
  currency VARCHAR(16) NOT NULL,
  PRIMARY KEY (channel, payment_transaction_id), UNIQUE (channel, transaction_id)
);
-- 管理幂等日志与SKU修改同事务，pending标记不应出现在已提交的成功命令中。
CREATE TABLE IF NOT EXISTS commerce_admin_command (
  actor VARBINARY(64) NOT NULL,
  request_id VARBINARY(128) NOT NULL,
  command_type VARCHAR(16) NOT NULL,
  request_digest VARCHAR(64) NOT NULL,
  result_json MEDIUMTEXT NOT NULL,
  created_ms BIGINT NOT NULL,
  PRIMARY KEY(actor,request_id)
);
-- 本地mock提供方的不可变交易原件；applied仅为可恢复投递标记，不代表真实外部资金。
CREATE TABLE IF NOT EXISTS commerce_mock_transaction (
  kind VARCHAR(8) NOT NULL,
  order_sn VARCHAR(36) NOT NULL,
  request_id VARBINARY(128) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  payment_transaction_id VARBINARY(128) NOT NULL,
  amount_minor BIGINT NOT NULL,
  currency VARCHAR(16) NOT NULL,
  created_ms BIGINT NOT NULL,
  applied INT NOT NULL DEFAULT 0,
  PRIMARY KEY(kind,order_sn), UNIQUE(kind,request_id), UNIQUE(transaction_id),
  INDEX idx_commerce_mock_pending(kind,applied,order_sn),
  CHECK (kind IN ('PAYMENT','REFUND')), CHECK (amount_minor>0), CHECK (created_ms>=0), CHECK (applied IN (0,1))
);
-- V4后执行；寄售关联同一OMS订单，不建立第二套订单或玩家游戏货币余额。
CREATE TABLE IF NOT EXISTS commerce_consignment (
  listing_id VARCHAR(20) PRIMARY KEY,
  seller_player_id VARCHAR(20) NOT NULL,
  seller_account VARBINARY(191) NOT NULL,
  market_zone BIGINT NOT NULL,
  asset_kind VARCHAR(16) NOT NULL,
  asset_currency_type INT NULL,
  escrow_reference VARCHAR(20) NOT NULL,
  escrow_proof VARCHAR(64) NOT NULL,
  request_digest VARCHAR(64) NOT NULL,
  title VARCHAR(128) NOT NULL,
  price_fen BIGINT NOT NULL,
  fee_bps INT NOT NULL,
  cooling_ms BIGINT NOT NULL,
  notice_end_ms BIGINT NOT NULL,
  sale_end_ms BIGINT NOT NULL,
  state VARCHAR(24) NOT NULL,
  current_order_sn VARCHAR(36) NULL,
  created_ms BIGINT NOT NULL,
  UNIQUE(escrow_reference),
  INDEX idx_consignment_state(state,listing_id),
  CHECK(price_fen>0), CHECK(fee_bps>=0 AND fee_bps<=10000), CHECK(cooling_ms>=0)
);
CREATE TABLE IF NOT EXISTS commerce_consignment_order (
  order_sn VARCHAR(36) PRIMARY KEY,
  listing_id VARCHAR(20) NOT NULL,
  buyer_account VARBINARY(191) NOT NULL,
  buyer_owner_revision VARBINARY(20) NOT NULL,
  request_id VARBINARY(128) NOT NULL,
  seller_account VARBINARY(191) NOT NULL,
  escrow_reference VARCHAR(20) NOT NULL,
  escrow_proof VARCHAR(64) NOT NULL,
  fee_bps INT NOT NULL,
  cooling_ms BIGINT NOT NULL,
  INDEX idx_consignment_order_listing(listing_id),
  UNIQUE(buyer_account,request_id)
);
CREATE TABLE IF NOT EXISTS commerce_seller_guard(account VARBINARY(191) PRIMARY KEY);
CREATE TABLE IF NOT EXISTS commerce_seller_income (
  order_sn VARCHAR(36) PRIMARY KEY,
  account VARBINARY(191) NOT NULL,
  gross_fen BIGINT NOT NULL,
  fee_fen BIGINT NOT NULL,
  net_fen BIGINT NOT NULL,
  reserved_fen BIGINT NOT NULL DEFAULT 0,
  paid_fen BIGINT NOT NULL DEFAULT 0,
  available_ms BIGINT NOT NULL,
  INDEX idx_seller_income(account,order_sn),
  CHECK(gross_fen>0), CHECK(fee_fen>=0 AND net_fen>=0), CHECK(fee_fen+net_fen=gross_fen),
  CHECK(reserved_fen>=0 AND paid_fen>=0 AND reserved_fen+paid_fen<=net_fen)
);
CREATE TABLE IF NOT EXISTS commerce_withdrawal (
  withdrawal_id VARCHAR(36) PRIMARY KEY,
  account VARBINARY(191) NOT NULL,
  request_id VARBINARY(128) NOT NULL,
  amount_fen BIGINT NOT NULL,
  state VARCHAR(24) NOT NULL,
  channel VARBINARY(32) NULL,
  transaction_id VARBINARY(128) NULL,
  created_ms BIGINT NOT NULL,
  UNIQUE(account,request_id),
  INDEX idx_withdrawal_pending(state,withdrawal_id),
  CHECK(amount_fen>0)
);
CREATE TABLE IF NOT EXISTS commerce_withdrawal_allocation (
  withdrawal_id VARCHAR(36) NOT NULL,
  order_sn VARCHAR(36) NOT NULL,
  amount_fen BIGINT NOT NULL,
  PRIMARY KEY(withdrawal_id,order_sn),
  CHECK(amount_fen>0)
);
CREATE TABLE IF NOT EXISTS commerce_withdrawal_receipt (
  withdrawal_id VARCHAR(36) PRIMARY KEY,
  channel VARBINARY(32) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  amount_fen BIGINT NOT NULL,
  successful INT NOT NULL,
  UNIQUE(channel,transaction_id)
);
CREATE TABLE IF NOT EXISTS commerce_consignment_return (
  listing_id VARCHAR(20) PRIMARY KEY,
  return_proof VARCHAR(64) NOT NULL
);
-- 本地mock出款原件与结算回执分事务，支持两个提交窗口的同单恢复。
CREATE TABLE IF NOT EXISTS commerce_mock_payout (
  withdrawal_id VARCHAR(36) PRIMARY KEY,
  request_id VARBINARY(128) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  amount_fen BIGINT NOT NULL,
  successful INT NOT NULL,
  created_ms BIGINT NOT NULL,
  applied INT NOT NULL DEFAULT 0,
  UNIQUE(request_id), UNIQUE(transaction_id),
  INDEX idx_mock_payout_pending(applied,withdrawal_id),
  CHECK(amount_fen>0), CHECK(successful IN (0,1)), CHECK(applied IN (0,1))
);

INSERT INTO commerce_schema_version(version,storage_contract) VALUES (1,'mall-game-official-sku-v1-minor-units');
