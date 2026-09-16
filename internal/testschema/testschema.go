// Package testschema holds historical store schema fixtures shared by tests
// across packages (the internal/testsshd precedent: test-only infrastructure
// in its own package, imported by tests, never by production code). When a
// future migration adds a new old shape, land its DDL here so every package's
// migration tests share one source of truth — a bare copy in a second test
// file is exactly how the next migration gets missed.
package testschema

// OldShapeCacheTokens is the pre-Plan-39 cache_tokens schema (no profile_id):
// the shape a legacy fleet DB presents to store.Open's migration, which adds
// the column and leaves existing rows NULL (= unbound, pulls refused with 403
// until `cache-tokens bind` repairs them).
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
