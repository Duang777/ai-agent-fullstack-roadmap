// Package store 是 Agent 平台的持久层：Postgres 存会话、消息、运行记录，Redis 做缓存和限流。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("state conflict") // 状态不允许这个操作，比如对已结束的 run 再次 Finish
)

type Store struct {
	pool *pgxpool.Pool
}

// Open 建连接池。连接池大小、超时都可以写在 URL 里，例如 ?pool_max_conns=10。
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

type Conversation struct {
	ID        int64     `json:"id"`
	UserID    string    `json:"user_id"`
	Title     string    `json:"title"`
	LastSeq   int       `json:"last_seq"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Message struct {
	ID             int64           `json:"id"`
	ConversationID int64           `json:"conversation_id"`
	Seq            int             `json:"seq"`
	Role           string          `json:"role"`
	Content        json.RawMessage `json:"content"`
	Tokens         int             `json:"tokens"`
	CreatedAt      time.Time       `json:"created_at"`
}

type Run struct {
	ID             int64
	ConversationID int64
	IdempotencyKey string
	Status         string
	Worker         *string // 可为 NULL 的列用指针接
	InputTokens    int64
	OutputTokens   int64
	Model          string
}

// ---------- 会话 ----------

func (s *Store) CreateConversation(ctx context.Context, userID, title string) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO conversations (user_id, title) VALUES ($1, $2) RETURNING id`,
		userID, title).Scan(&id)
	return id, err
}

func (s *Store) GetConversation(ctx context.Context, id int64) (Conversation, error) {
	var c Conversation
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, title, last_seq, updated_at FROM conversations WHERE id = $1`, id).
		Scan(&c.ID, &c.UserID, &c.Title, &c.LastSeq, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func (s *Store) RenameConversation(ctx context.Context, id int64, title string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations SET title = $2, updated_at = now() WHERE id = $1`, id, title)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Cursor 是键集分页的游标：上一页最后一行的排序键。
type Cursor struct {
	UpdatedAt time.Time `json:"u"`
	ID        int64     `json:"i"`
}

// ListConversations 按最近活跃倒序分页。cursor 为 nil 表示第一页；返回的 next 为 nil 表示没有下一页。
// 用 (updated_at, id) < (游标) 而不是 OFFSET：翻到第 1000 页也只扫 limit 行。
func (s *Store) ListConversations(ctx context.Context, userID string, cursor *Cursor, limit int) ([]Conversation, *Cursor, error) {
	var rows pgx.Rows
	var err error
	const cols = `SELECT id, user_id, title, last_seq, updated_at FROM conversations`
	if cursor == nil {
		rows, err = s.pool.Query(ctx, cols+`
			WHERE user_id = $1
			ORDER BY updated_at DESC, id DESC LIMIT $2`, userID, limit+1)
	} else {
		rows, err = s.pool.Query(ctx, cols+`
			WHERE user_id = $1 AND (updated_at, id) < ($2, $3)
			ORDER BY updated_at DESC, id DESC LIMIT $4`, userID, cursor.UpdatedAt, cursor.ID, limit+1)
	}
	if err != nil {
		return nil, nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Conversation, error) {
		var c Conversation
		err := r.Scan(&c.ID, &c.UserID, &c.Title, &c.LastSeq, &c.UpdatedAt)
		return c, err
	})
	if err != nil {
		return nil, nil, err
	}
	// 多查一行用来判断“还有没有下一页”，省掉一次 COUNT(*)
	if len(list) <= limit {
		return list, nil, nil
	}
	list = list[:limit]
	last := list[limit-1]
	return list, &Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}, nil
}

// ---------- 消息 ----------

// AppendMessage 给会话追加一条消息，seq 从 1 开始连续递增，并发追加也不会重号、跳号。
func (s *Store) AppendMessage(ctx context.Context, convID int64, role string, content json.RawMessage, tokens int) (Message, error) {
	var m Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// UPDATE 会给这一行会话加行锁；同一会话的并发追加在这里排队，不同会话互不影响
		var seq int
		err := tx.QueryRow(ctx,
			`UPDATE conversations SET last_seq = last_seq + 1, updated_at = now()
			 WHERE id = $1 RETURNING last_seq`, convID).Scan(&seq)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO messages (conversation_id, seq, role, content, tokens)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, conversation_id, seq, role, content, tokens, created_at`,
			convID, seq, role, content, tokens).
			Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.Content, &m.Tokens, &m.CreatedAt)
	})
	return m, err
}

// ListMessages 读 seq > afterSeq 的消息，最多 limit 条。断线重连时传最后收到的 seq 即可续上。
func (s *Store) ListMessages(ctx context.Context, convID int64, afterSeq, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, conversation_id, seq, role, content, tokens, created_at
		 FROM messages WHERE conversation_id = $1 AND seq > $2
		 ORDER BY seq LIMIT $3`, convID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Message, error) {
		var m Message
		err := r.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.Content, &m.Tokens, &m.CreatedAt)
		return m, err
	})
}

// ---------- 运行记录：一个用 Postgres 实现的任务队列 ----------

const runCols = `id, conversation_id, idempotency_key, status, worker, input_tokens, output_tokens, model`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.ConversationID, &r.IdempotencyKey, &r.Status, &r.Worker,
		&r.InputTokens, &r.OutputTokens, &r.Model)
	return r, err
}

// EnqueueRun 用幂等键创建一次运行。同一个 key 重复调用（客户端超时重试）返回同一条记录，created=false。
func (s *Store) EnqueueRun(ctx context.Context, convID int64, key, model string) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`INSERT INTO runs (conversation_id, idempotency_key, model) VALUES ($1, $2, $3)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING `+runCols, convID, key, model))
	if err == nil {
		return r, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, err
	}
	// 冲突了：说明已经有人用这个 key 建过，查出来返回
	r, err = scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runCols+` FROM runs WHERE idempotency_key = $1`, key))
	return r, false, err
}

// ClaimRun 让一个 worker 领走最早的一条排队任务。没有任务时返回 ErrNotFound。
// FOR UPDATE SKIP LOCKED：别的 worker 正在领的行直接跳过，多个 worker 并发领也不会领到同一条。
func (s *Store) ClaimRun(ctx context.Context, worker string) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`UPDATE runs SET status = 'running', worker = $1, started_at = now()
		 WHERE id = (
		     SELECT id FROM runs WHERE status = 'queued'
		     ORDER BY created_at, id
		     FOR UPDATE SKIP LOCKED
		     LIMIT 1)
		 RETURNING `+runCols, worker))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// FinishRun 把 running 的任务标记为结束。WHERE 里带上旧状态，任务已结束或不在 running 时返回 ErrConflict。
func (s *Store) FinishRun(ctx context.Context, id int64, ok bool) error {
	status := "failed"
	if ok {
		status = "succeeded"
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET status = $2, finished_at = now()
		 WHERE id = $1 AND status = 'running'`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// AddUsage 累加 token 用量。在 SQL 里做加法是原子的，并发调用不会丢更新。
func (s *Store) AddUsage(ctx context.Context, runID, in, out int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET input_tokens = input_tokens + $2, output_tokens = output_tokens + $3
		 WHERE id = $1`, runID, in, out)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
