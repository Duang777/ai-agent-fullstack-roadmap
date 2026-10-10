package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID 是一个任意但固定的数字，所有实例用它抢同一把咨询锁。
const migrationLockID = 727_000_001

// Migrate 按文件名顺序执行还没执行过的迁移，返回本次执行的版本号。
// 多个实例同时启动时，只有拿到咨询锁的那个在执行，其余排队，轮到时发现已执行便跳过。
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // 已 Commit 时这是空操作

	// 事务级咨询锁：事务结束自动释放，进程崩了也不会留下死锁
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}

	done := map[string]bool{}
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		done[v] = true
	}

	var applied []string
	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		if done[version] {
			continue
		}
		sql, err := migrationFS.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return nil, fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			return nil, err
		}
		applied = append(applied, version)
	}
	return applied, tx.Commit(ctx)
}
