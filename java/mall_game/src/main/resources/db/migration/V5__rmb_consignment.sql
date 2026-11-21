-- V4后执行；寄售关联同一OMS订单，不建立第二套订单或玩家游戏货币余额。
-- 旧单缺权威原身份时不推断/回填；交付保持拒绝，待受控原件核实。
CREATE TABLE commerce_official_owner (
  order_sn VARCHAR(36) PRIMARY KEY,
  buyer_account VARBINARY(191) NOT NULL,
  buyer_owner_revision VARBINARY(20) NOT NULL,
  INDEX idx_official_owner(buyer_account,buyer_owner_revision,order_sn)
);
CREATE TABLE commerce_consignment (
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
CREATE TABLE commerce_consignment_order (
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
CREATE TABLE commerce_seller_guard(account VARBINARY(191) PRIMARY KEY);
CREATE TABLE commerce_seller_income (
  order_sn VARCHAR(36) PRIMARY KEY,
  account VARBINARY(191) NOT NULL,
  gross_fen BIGINT NOT NULL,
  fee_fen BIGINT NOT NULL,
  net_fen BIGINT NOT NULL,
  reserved_fen BIGINT NOT NULL DEFAULT 0,
  paid_fen BIGINT NOT NULL DEFAULT 0,
  forfeited_fen BIGINT NOT NULL DEFAULT 0,
  available_ms BIGINT NOT NULL,
  INDEX idx_seller_income(account,order_sn),
  CHECK(gross_fen>0), CHECK(fee_fen>=0 AND net_fen>=0), CHECK(fee_fen+net_fen=gross_fen),
  CHECK(reserved_fen>=0 AND paid_fen>=0 AND forfeited_fen>=0 AND reserved_fen+paid_fen+forfeited_fen<=net_fen)
);
CREATE TABLE commerce_withdrawal (
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
CREATE TABLE commerce_withdrawal_allocation (
  withdrawal_id VARCHAR(36) NOT NULL,
  order_sn VARCHAR(36) NOT NULL,
  amount_fen BIGINT NOT NULL,
  PRIMARY KEY(withdrawal_id,order_sn),
  CHECK(amount_fen>0)
);
CREATE TABLE commerce_withdrawal_receipt (
  withdrawal_id VARCHAR(36) PRIMARY KEY,
  channel VARBINARY(32) NOT NULL,
  transaction_id VARBINARY(128) NOT NULL,
  amount_fen BIGINT NOT NULL,
  successful INT NOT NULL,
  UNIQUE(channel,transaction_id)
);
CREATE TABLE commerce_consignment_return (
  listing_id VARCHAR(20) PRIMARY KEY,
  return_proof VARCHAR(64) NOT NULL
);
-- 本地mock出款原件与结算回执分事务，支持两个提交窗口的同单恢复。
CREATE TABLE commerce_mock_payout (
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
