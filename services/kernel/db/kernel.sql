-- 内核表：每个子系统数据库中由 kernel 迁移创建，Go 与 Python 的 kernel 使用同一份定义（测试校验一致）。
-- 约定见 docs/03-architecture/03-integration-contracts.md 第 3.4 节。

-- 事务性发件箱：与业务写入同事务追加，由 outbox-relay 按 seq 顺序发布到 Kafka。
CREATE TABLE IF NOT EXISTS kernel_outbox (
    seq           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id      text        NOT NULL UNIQUE,         -- CloudEvents id
    topic         text        NOT NULL,
    partition_key text        NOT NULL,
    envelope      jsonb       NOT NULL,                -- 完整的 CloudEvents 信封
    created_at    timestamptz NOT NULL DEFAULT now(),
    published_at  timestamptz
);
CREATE INDEX IF NOT EXISTS kernel_outbox_pending ON kernel_outbox (seq) WHERE published_at IS NULL;

-- 消费去重：消费者在处理事件的同一事务中登记事件 id，重复投递时跳过。
CREATE TABLE IF NOT EXISTS kernel_inbox (
    consumer     text        NOT NULL,
    event_id     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

-- 发件箱中继只能读取并标记发件箱。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'outbox_relay') THEN
        GRANT USAGE ON SCHEMA public TO outbox_relay;
        GRANT SELECT, UPDATE (published_at) ON kernel_outbox TO outbox_relay;
    END IF;
END $$;
