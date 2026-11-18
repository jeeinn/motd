-- 0001_init：认证域 + 论坛核心域初始结构
-- 依据：评审稿 v0.2 §5.1（认证相关表）、§5.2（论坛核心表）
-- 说明：
--   1. FTS5 全文索引属 P1（§5.3），本迁移不含；
--   2. 邮箱/密码相关表（local_credentials、email_tokens）在 P1 才启用，
--      但结构与认证域强耦合，一次性建好，避免 P1 再做破坏性迁移；
--   3. 时间字段统一存 Unix 秒（INTEGER），不用文本日期。

-- ---------------------------------------------------------------------------
-- 认证域
-- ---------------------------------------------------------------------------

CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL COLLATE NOCASE UNIQUE
                  CHECK (length(username) BETWEEN 3 AND 20
                         AND username NOT GLOB '*[^a-z0-9_-]*'),
  display_name  TEXT NOT NULL DEFAULT '',
  primary_email TEXT,
  karma         INTEGER NOT NULL DEFAULT 0,
  status        TEXT NOT NULL CHECK (status IN ('active','pending','deleting','deleted')),
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

-- 一个用户可挂多个身份源；(provider, provider_uid) 全局唯一。
-- provider_uid 对 GitHub 取数字 id，不用 login / email。
CREATE TABLE identities (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id        INTEGER NOT NULL REFERENCES users(id),
  provider       TEXT NOT NULL CHECK (provider IN ('github','email')),
  provider_uid   TEXT NOT NULL,
  email          TEXT,
  email_verified INTEGER NOT NULL DEFAULT 0,
  profile_json   TEXT NOT NULL DEFAULT '{}',
  created_at     INTEGER NOT NULL,
  UNIQUE (provider, provider_uid)
);
CREATE INDEX idx_identities_user ON identities(user_id);

-- 本地密码凭证与身份源分离：GitHub 身份、邮箱身份、本地密码互不耦合。
CREATE TABLE local_credentials (
  user_id       INTEGER PRIMARY KEY REFERENCES users(id),
  password_hash TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

-- SSH 公钥是凭证不是身份；一钥只属一号，解绑不影响其他钥。
CREATE TABLE ssh_keys (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES users(id),
  fingerprint   TEXT NOT NULL UNIQUE,
  public_key    TEXT NOT NULL,
  label         TEXT NOT NULL DEFAULT '',
  last_used_at  INTEGER,
  revoked_at    INTEGER,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_ssh_keys_user ON ssh_keys(user_id, revoked_at);

-- 邮箱验证码：purpose 三类互不通用，消费时校验用途与请求一致。
CREATE TABLE email_tokens (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  email       TEXT NOT NULL,
  code_hash   TEXT NOT NULL,
  purpose     TEXT NOT NULL CHECK (purpose IN ('register','verify','recover')),
  expires_at  INTEGER NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  consumed_at INTEGER,
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_email_tokens_cleanup ON email_tokens(expires_at, consumed_at);

-- Web OAuth 中间态：state 只存散列，PKCE verifier 加密存储且用完即清。
CREATE TABLE oauth_states (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  provider          TEXT NOT NULL,
  state_hash        TEXT NOT NULL UNIQUE,
  code_verifier_enc TEXT,
  redirect_uri      TEXT,
  pending_user_id   INTEGER,
  expires_at        INTEGER NOT NULL,
  consumed_at       INTEGER,
  created_at        INTEGER NOT NULL
);

-- Device Flow 中间态：需跨请求存活，必须持久化，不复用 oauth_states。
CREATE TABLE device_auth_states (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  provider         TEXT NOT NULL,
  device_code      TEXT NOT NULL UNIQUE,
  user_code        TEXT NOT NULL,
  polling_interval INTEGER NOT NULL DEFAULT 5,
  expires_at       INTEGER NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('pending','authorized','denied','expired')),
  session_ref      TEXT,
  created_at       INTEGER NOT NULL
);
CREATE INDEX idx_device_auth_states_expiry ON device_auth_states(expires_at, status);

-- 长期身份会话（web / cli / device）。SSH 连接不入此表，由内存连接注册表管理。
CREATE TABLE sessions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id      INTEGER NOT NULL REFERENCES users(id),
  token_hash   TEXT NOT NULL UNIQUE,
  kind         TEXT NOT NULL CHECK (kind IN ('web','cli','device')),
  expires_at   INTEGER NOT NULL,
  revoked_at   INTEGER,
  last_seen_at INTEGER,
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id, expires_at, revoked_at);

-- 安全审计：登录失败、验证码错误、state 不匹配等。
CREATE TABLE auth_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER,
  event       TEXT NOT NULL,
  provider    TEXT,
  ip_hash     TEXT,
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_auth_events_time ON auth_events(created_at);

-- ---------------------------------------------------------------------------
-- 论坛核心域：Node → Thread → Post，Vote 多态指向 Thread / Post
-- ---------------------------------------------------------------------------

CREATE TABLE nodes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE threads (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id       INTEGER NOT NULL REFERENCES nodes(id),
  user_id       INTEGER NOT NULL REFERENCES users(id),
  title         TEXT NOT NULL,
  score         INTEGER NOT NULL DEFAULT 0,
  last_reply_at INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_threads_node ON threads(node_id, last_reply_at);
CREATE INDEX idx_threads_score ON threads(score DESC);

-- 内容删除只改 status，不物理删除；parent_post_id 预留不启用（扁平回复）。
CREATE TABLE posts (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  thread_id      INTEGER NOT NULL REFERENCES threads(id),
  user_id        INTEGER NOT NULL REFERENCES users(id),
  body_md        TEXT NOT NULL,
  status         TEXT NOT NULL CHECK (status IN ('normal','deleted','hidden')),
  parent_post_id INTEGER REFERENCES posts(id),
  created_at     INTEGER NOT NULL
);
CREATE INDEX idx_posts_thread ON posts(thread_id, created_at);

-- 多态关联无法用外键约束目标存在性，引用有效性由 Application 层在事务内校验。
CREATE TABLE votes (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  target_type TEXT NOT NULL CHECK (target_type IN ('thread','post')),
  target_id   INTEGER NOT NULL,
  value       INTEGER NOT NULL CHECK (value IN (-1, 1)),
  created_at  INTEGER NOT NULL,
  UNIQUE (user_id, target_type, target_id)
);

-- 成就是唯一允许插件「申请解锁」的结果表，插件不得直接写。
CREATE TABLE achievements (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users(id),
  code        TEXT NOT NULL,
  unlocked_at INTEGER NOT NULL,
  UNIQUE (user_id, code)
);
