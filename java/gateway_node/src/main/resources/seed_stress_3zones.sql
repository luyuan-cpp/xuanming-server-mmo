-- Seed 3 zones for the 3-zone × 15000 robot stress test.
--
-- Capacity is set to 20000 (vs 15000 robot_count per zone) on purpose:
--
--   - The queue path only kicks in when capacity is exceeded, so we want
--     headroom to differentiate "open-server burst absorbed cleanly" from
--     "everyone went through the queue and that's why the timing looks
--     smooth." If you specifically want to exercise the queue, drop
--     capacity to 12000 for one zone before the run.
--   - Per ARCH §7, capacity here is the *zone admission* cap, not the
--     scene tick budget. Scene承载 is governed elsewhere.
--
-- Idempotent: re-runnable. Existing zone-1 row (seeded by schema.sql) is
-- bumped to capacity=20000 + recommended=1 (default landing zone for
-- robots with zone_id=0). Zones 2 and 3 are non-recommended so the
-- robot-driven distribution comes purely from per-process yaml config,
-- not from /api/server-list auto-routing.
--
-- 改已有行必须让 version 加一:网关的整行 PUT 按 ETag / If-Match 比版本,不加一的话
-- 按改之前 GET 的 ETag 发的 PUT 照样通过,把这里刚改的值写回去(2026-11-19 评审补)。
--
-- Run against the mmorpg database:
--   mysql -u root -p mmorpg < seed_stress_3zones.sql

INSERT INTO zone_config (zone_id, name, manual_status, capacity, recommended, sort_order)
VALUES
    (1, 'zone-1', 0, 20000, 1, 1),
    (2, 'zone-2', 0, 20000, 0, 2),
    (3, 'zone-3', 0, 20000, 0, 3)
ON DUPLICATE KEY UPDATE
    name          = VALUES(name),
    manual_status = VALUES(manual_status),
    capacity      = VALUES(capacity),
    recommended   = VALUES(recommended),
    sort_order    = VALUES(sort_order),
    version       = version + 1;

-- Sanity print so the operator sees what they just applied.
SELECT zone_id, name, capacity, recommended, manual_status FROM zone_config ORDER BY sort_order;
