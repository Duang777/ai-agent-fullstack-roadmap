-- 0001：会话、消息、运行记录三张核心表
CREATE TABLE conversations (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id     TEXT        NOT NULL,
    title       TEXT        NOT NULL DEFAULT '',
    last_seq    INT         NOT NULL DEFAULT 0,          -- 已分配的最大消息序号
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- “我的会话列表，最近活跃的在前”：等值列在前，排序列在后，id 兜底保证顺序唯一
CREATE INDEX conversations_user_recent_idx
    ON conversations (user_id, updated_at DESC, id DESC);

CREATE TABLE messages (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    conversation_id  BIGINT      NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    seq              INT         NOT NULL,
    role             TEXT        NOT NULL CHECK (role IN ('system', 'user', 'assistant', 'tool')),
    content          JSONB       NOT NULL,                -- 文本、工具调用、工具结果结构不同，用 JSONB
    tokens           INT         NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)                          -- 唯一约束自带索引，同时服务“按会话顺序读”
);

CREATE TABLE runs (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    conversation_id  BIGINT      NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    idempotency_key  TEXT        NOT NULL UNIQUE,          -- 客户端重试同一个请求，只建一条
    status           TEXT        NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    worker           TEXT,
    input_tokens     BIGINT      NOT NULL DEFAULT 0,
    output_tokens    BIGINT      NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ
);
-- 部分索引：只索引排队中的行，队列再长，这个索引也只有“待处理”那么大
CREATE INDEX runs_queued_idx ON runs (created_at, id) WHERE status = 'queued';
CREATE INDEX runs_conversation_idx ON runs (conversation_id);
