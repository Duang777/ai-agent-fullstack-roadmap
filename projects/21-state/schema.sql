-- 3.5 参考表结构（Postgres）。练习代码用内存实现，这个文件给练习 2 用。
-- 两层：events 是唯一的事实来源，只追加；其余表都是投影，可以删掉重建。

CREATE TABLE sessions (
    id          text PRIMARY KEY,
    user_id     text NOT NULL,
    title       text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- 事实来源：一个 run 一条流，(run_id, seq) 唯一，seq 连续。
CREATE TABLE events (
    pos         bigserial PRIMARY KEY,          -- 全局位置，投影按它追赶
    run_id      text NOT NULL,
    seq         int  NOT NULL,
    type        text NOT NULL,
    schema_ver  int  NOT NULL,
    data        jsonb NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)                         -- 并发追加时第二个写入者在这里失败
);

-- 以下都是投影。
CREATE TABLE runs (
    id            text PRIMARY KEY,
    session_id    text NOT NULL REFERENCES sessions(id),
    agent_version text NOT NULL,
    status        text NOT NULL,                 -- running / done / failed / interrupted
    input_tokens  int  NOT NULL DEFAULT 0,
    output_tokens int  NOT NULL DEFAULT 0,
    error         text,
    failed_step   int,
    last_seq      int  NOT NULL,                 -- 投影到哪一条了
    started_at    timestamptz NOT NULL,
    ended_at      timestamptz
);
CREATE INDEX runs_session ON runs (session_id, started_at);

CREATE TABLE steps (
    run_id        text NOT NULL REFERENCES runs(id),
    n             int  NOT NULL,
    request_hash  text NOT NULL,
    response      jsonb NOT NULL,
    input_tokens  int  NOT NULL,
    output_tokens int  NOT NULL,
    PRIMARY KEY (run_id, n)
);

CREATE TABLE tool_calls (
    run_id     text NOT NULL REFERENCES runs(id),
    call_id    text NOT NULL,
    step       int  NOT NULL,
    name       text NOT NULL,
    args       jsonb NOT NULL,
    idem_key   text NOT NULL UNIQUE,
    result     text,
    is_error   boolean,
    started_at timestamptz NOT NULL,
    ended_at   timestamptz,                      -- 为空 = 开始了没结束，崩溃后要核对
    PRIMARY KEY (run_id, call_id)
);
CREATE INDEX tool_calls_errors ON tool_calls (name) WHERE is_error;

-- 投影进度：每个投影记录自己读到的全局位置。
CREATE TABLE projection_positions (
    name text PRIMARY KEY,
    pos  bigint NOT NULL
);
