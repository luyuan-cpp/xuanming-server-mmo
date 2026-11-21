-- 由V3目录管理库升级；先执行迁移，应用只读验证，不自动DDL。
-- 本表只证明本地mock提供方事实，不能作为真实渠道或外部资金凭证。
CREATE TABLE commerce_mock_transaction (
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
CREATE INDEX idx_commerce_refund_pending ON commerce_refund_intent(status,order_sn,channel,transaction_id);
