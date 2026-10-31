// Package testschema holds historical store schema fixtures shared by tests
// across packages (the internal/testsshd precedent: test-only infrastructure
// in its own package, imported by tests, never by production code). When a
// future migration adds a new old shape, land its DDL here so every package's
// migration tests share one source of truth — a bare copy in a second test
// file is exactly how the next migration gets missed.
package testschema

// OldShapeCacheTokens 是没有 profile_id 与 client_version 的历史设备码表。
// 旧 vault 打开时,现有迁移会补上 profile_id 并保持旧行为空值(未绑定,
// 拉取会被拒绝,直到 `cache-tokens bind` 修复),再补上空 client_version,
// 显示为旧版客户端。
const OldShapeCacheTokens = `CREATE TABLE cache_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  last_pull_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`
