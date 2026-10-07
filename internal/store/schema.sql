-- B0 冻结基线：本文件与 docs/planning/iterations/v0.2/tech-design.md §3.2~§3.4 逐字一致，B1~B7 不得修改。

-- 1. 项目（FR1）
CREATE TABLE projects (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  code                  TEXT    NOT NULL,                    -- 项目标识（如 proj-a），全局唯一
  name                  TEXT    NOT NULL DEFAULT '',
  status                TEXT    NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active','archived')),
  heartbeat_timeout_sec INTEGER NOT NULL DEFAULT 900,        -- T3：项目级失联阈值覆盖（默认=服务配置同值）
  created_at            TEXT    NOT NULL,
  updated_at            TEXT    NOT NULL,
  UNIQUE (code)
);

-- 2. 栏目（FR2）
CREATE TABLE columns (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  code       TEXT    NOT NULL,                               -- 栏目标识（如 "05"），项目内唯一
  name       TEXT    NOT NULL DEFAULT '',
  status     TEXT    NOT NULL DEFAULT 'active'
             CHECK (status IN ('active','archived')),
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  UNIQUE (project_id, code)
);

-- 3. 会话（T1 隐式注册；FR12 心跳挂此表）
--    实体四元组=(项目,栏目,会话名,角色)；换机重开同名=同实体（upsert 幂等）
CREATE TABLE sessions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id   INTEGER NOT NULL REFERENCES projects(id),
  column_id    INTEGER NOT NULL REFERENCES columns(id),      -- 会话必属某栏目（总控多栏目=多实体行）
  name         TEXT    NOT NULL,                             -- --session 显式名（controller / executor-A…）
  role         TEXT    NOT NULL,                             -- controller / executor / …（不枚举硬校验，登记自由）
  last_seen_at TEXT    NOT NULL,                             -- 心跳时间戳（服务端时间）
  created_at   TEXT    NOT NULL,
  UNIQUE (project_id, column_id, name)
);

-- 4. 哨兵（FR10/FR12：活性独立信号，不冒充会话心跳——决策 A9）
CREATE TABLE sentinels (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   INTEGER NOT NULL REFERENCES sessions(id),
  project_id   INTEGER NOT NULL REFERENCES projects(id),
  column_id    INTEGER NOT NULL REFERENCES columns(id),
  role         TEXT    NOT NULL,                             -- 监控的信箱角色
  started_at   TEXT    NOT NULL,
  last_ping_at TEXT    NOT NULL,                             -- 每轮 watch 循环刷新
  UNIQUE (session_id, column_id, role)                       -- 同会话同信箱单哨兵
);

-- 5. 消息（FR5/FR6/FR7/FR15；append-only，§3.3 触发器禁改删）
--    kind 四形态：direct(定向) / bus(总线) / chat(看板对话) / receipt(系统回告)
CREATE TABLE messages (
  seq                INTEGER PRIMARY KEY AUTOINCREMENT,      -- 全局唯一序号（FR5，服务端生成）
  project_id         INTEGER NOT NULL REFERENCES projects(id),
  column_id          INTEGER NOT NULL REFERENCES columns(id),
                       -- direct=目标栏目；bus=发送方所在栏目(审计锚点)；chat=目标会话所属栏目；receipt=原发送方会话栏目（四 kind 均为真实栏目行，FK 成立）
  kind               TEXT    NOT NULL CHECK (kind IN ('direct','bus','chat','receipt')),
  target_role        TEXT    NOT NULL DEFAULT '',            -- direct：目标角色；其余''
  target_session_id  INTEGER NOT NULL DEFAULT 0,             -- chat/receipt：目标会话；其余 0（弱关联：0=无对应行，不设 FK，防 foreign_keys=ON 插入失败）
  sender_session_id  INTEGER NOT NULL DEFAULT 0,             -- 发送方会话（0=系统/看板，弱关联不设 FK）
  sender_label       TEXT    NOT NULL DEFAULT '',            -- 冗余显示：'controller-A@05' / 'board-user:张三' / 'system'
  level              TEXT    NOT NULL CHECK (level IN ('normal','important','block')),
  body               TEXT    NOT NULL,                       -- ≤256KB
  created_at         TEXT    NOT NULL                        -- 服务端时间（AC5.4）
);

-- 6. 阻断回执（FR7/T4；一消息一回执）
CREATE TABLE message_receipts (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  message_seq        INTEGER NOT NULL UNIQUE REFERENCES messages(seq),
  receipt_session_id INTEGER NOT NULL REFERENCES sessions(id), -- 回执方会话身份（AC7.3）
  created_at         TEXT    NOT NULL                          -- 服务端时间（AC7.3）
);

-- 7. 消费位点（FR8/FR9/T4；决策 A7：consumer 统一两种维度）
--    consumer='controller'        → 信箱位点（栏目+角色记账，PRD 冻结术语）
--    consumer='chat:session:<id>' → 会话对话位点（AC15.4 会话隔离所需）
CREATE TABLE ack_positions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  column_id  INTEGER NOT NULL REFERENCES columns(id),
  consumer   TEXT    NOT NULL,
  position   INTEGER NOT NULL DEFAULT 0,                     -- 已确认的最大消息序号高位
  updated_at TEXT    NOT NULL,
  UNIQUE (column_id, consumer)
);

-- 8. 共享资源（FR4）
CREATE TABLE resources (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id    INTEGER NOT NULL REFERENCES projects(id),
  column_id     INTEGER NOT NULL REFERENCES columns(id),
  rtype         TEXT    NOT NULL CHECK (rtype IN ('port','account_range','data_range')),
  value         TEXT    NOT NULL,                            -- 原始规范串（'8080' / 'acct:1000-1999'）
  range_prefix  TEXT    NOT NULL DEFAULT '',                 -- 解析冗余列（冲突判定走索引）：port=''
  range_start   INTEGER NOT NULL,                            -- port：=端口值；range：=起始值
  range_end     INTEGER NOT NULL,                            -- port：=端口值；range：=结束值
  note          TEXT    NOT NULL DEFAULT '',                 -- 用途说明（AC4.1）
  status        TEXT    NOT NULL DEFAULT 'in_use' CHECK (status IN ('in_use','released')),
  created_by    INTEGER NOT NULL REFERENCES sessions(id),
  created_at    TEXT    NOT NULL,
  released_at   TEXT
);

-- 9. 阶段时间窗（FR3；column_id=0 表示项目级默认窗，栏目级覆盖优先）
CREATE TABLE stage_windows (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL REFERENCES projects(id),
  column_id  INTEGER NOT NULL DEFAULT 0,                     -- 0=项目级默认（弱关联：0 时无对应栏目行，不设 FK）
  stage      TEXT    NOT NULL,                               -- 'S0'..'S7'（枚举弱校验，未列阶段查询亦兼容）
  start_time TEXT    NOT NULL,                               -- 'HH:MM'；start>end=跨午夜（AC3.3）
  end_time   TEXT    NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  created_at TEXT    NOT NULL,
  updated_at TEXT    NOT NULL,
  UNIQUE (project_id, column_id, stage)
);

-- 10. 审计留痕（AC1.4/AC2.4；登记类动作全留痕）
CREATE TABLE audit_log (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id INTEGER NOT NULL DEFAULT 0,                     -- 三列均为弱关联（0=系统/跨域动作），不设 FK
  column_id  INTEGER NOT NULL DEFAULT 0,
  session_id INTEGER NOT NULL DEFAULT 0,                     -- 操作会话身份（0=系统）
  action     TEXT    NOT NULL,                               -- 枚举见下
  detail     TEXT    NOT NULL DEFAULT '{}',                  -- JSON 快照（操作前后）
  created_at TEXT    NOT NULL
);

CREATE TRIGGER messages_no_update BEFORE UPDATE ON messages
BEGIN
  SELECT RAISE(ABORT, 'messages is append-only: UPDATE forbidden');
END;

CREATE TRIGGER messages_no_delete BEFORE DELETE ON messages
BEGIN
  SELECT RAISE(ABORT, 'messages is append-only: DELETE forbidden');
END;

-- poll 可见性谓词三分支（§3.6 查询 SQL 的支撑）
CREATE INDEX idx_messages_direct ON messages (column_id, target_role, seq);
CREATE INDEX idx_messages_bus    ON messages (project_id, seq) WHERE kind = 'bus';   -- 部分索引
CREATE INDEX idx_messages_chat   ON messages (target_session_id, seq) WHERE kind IN ('chat','receipt');
-- 发送方视角查询（AC7.2 未回执清单、history --sent-by）
CREATE INDEX idx_messages_sender ON messages (sender_session_id, level, seq);
-- 看板总线流倒序（AC14.3）
CREATE INDEX idx_messages_bus_recent ON messages (created_at DESC) WHERE kind = 'bus';
-- 表内 UNIQUE 已构成：projects(code) / columns(project_id,code) / sessions(project_id,column_id,name)
--                  / sentinels(session_id,column_id,role) / message_receipts(message_seq)
--                  / ack_positions(column_id,consumer) / stage_windows(project_id,column_id,stage)
CREATE INDEX idx_sessions_last_seen ON sessions (last_seen_at);        -- 失联扫描（AC12.2）
CREATE INDEX idx_sentinels_ping     ON sentinels (last_ping_at);       -- 哨兵活性扫描（AC10.3）
CREATE INDEX idx_resources_conflict ON resources (rtype, range_prefix, range_start)
  WHERE status = 'in_use';                                            -- 冲突判定（AC4.2）
CREATE INDEX idx_audit_project ON audit_log (project_id, id DESC);

-- 11. progress_reports 进度上报（FR23，2026-10-03 总控全局变更窗增补——观测域独立表，不进消息流）
CREATE TABLE IF NOT EXISTS progress_reports (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   INTEGER NOT NULL REFERENCES sessions(id),
  batch        TEXT,
  task         TEXT,
  commit_hash  TEXT,
  branch       TEXT,
  test_status  TEXT CHECK (test_status IN ('pass','fail','unknown')),
  summary      TEXT,
  created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_progress_session_time ON progress_reports(session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_progress_created ON progress_reports(created_at DESC);
