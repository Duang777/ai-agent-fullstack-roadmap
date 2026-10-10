-- 0002：记录每次运行用的模型。加列带常量默认值，Postgres 11+ 只改元数据，不重写整表
ALTER TABLE runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
