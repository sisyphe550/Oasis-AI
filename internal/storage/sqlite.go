package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 实现的 SQLite 驱动，无需 CGO
)

// ─── 接口定义 ────────────────────────────────────────────────────────────────

// MessageStore 是结构化对话存储的统一抽象接口。
type MessageStore interface {
	// EnsureConversation 确保会话记录存在，并管理该会话的规则书（System Prompt）。
	// 返回 (最终的 sessionID, 最终生效的 systemPrompt, error)。
	//
	// 行为规则：
	//   - sessionID 为空          → 新建会话，写入 systemPrompt，返回新 ID
	//   - sessionID 存在且 systemPrompt 非空 → 用新值 UPDATE 数据库，返回新值
	//   - sessionID 存在且 systemPrompt 为空 → 从数据库读取历史值并返回
	EnsureConversation(ctx context.Context, sessionID, systemPrompt string) (string, string, error)

	// SaveMessage 将一条消息落盘到指定对话。
	// role 应为 "user" 或 "assistant"。
	SaveMessage(ctx context.Context, conversationID, role, content string) error

	// GetRecentMessages 按时间倒序获取指定对话的最近 N 条消息，
	// 结果按时间正序返回，方便直接拼入 Prompt。
	GetRecentMessages(ctx context.Context, conversationID string, limit int) ([]MessageRecord, error)

	// Close 关闭数据库连接，应在服务关闭时调用。
	Close() error
}

// MessageRecord 代表从数据库取出的单条消息记录。
type MessageRecord struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	CreatedAt      time.Time
}

// ─── DDL ─────────────────────────────────────────────────────────────────────
// 注意：修改表结构后，需删除旧的 data/oasis.db 让 DDL 重新执行。

const ddl = `
CREATE TABLE IF NOT EXISTS conversations (
    id            TEXT PRIMARY KEY,
    system_prompt TEXT NOT NULL DEFAULT '',
    created_at    DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id),
    role            TEXT NOT NULL CHECK(role IN ('user','assistant','system')),
    content         TEXT NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_messages_conversation_id ON messages(conversation_id, created_at);
`

// ─── 实现 ─────────────────────────────────────────────────────────────────────

// SQLiteDB 是 MessageStore 接口的 SQLite 实现。
type SQLiteDB struct {
	db *sql.DB
}

// InitDB 在 dataDir 目录下打开（或创建）oasis.db，并自动执行建表 DDL。
func InitDB(dataDir string) (*SQLiteDB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("InitDB mkdir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "oasis.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("InitDB open: %w", err)
	}

	// SQLite 并发写入建议限制连接数为 1
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("InitDB ping: %w", err)
	}

	// 开启 WAL 模式，提升并发读性能
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("InitDB WAL: %w", err)
	}

	if _, err := db.Exec(ddl); err != nil {
		return nil, fmt.Errorf("InitDB DDL: %w", err)
	}

	return &SQLiteDB{db: db}, nil
}

// EnsureConversation 实现会话绑定型规则书的核心逻辑：
//
//  1. sessionID 为空 → 新建会话，生成 UUID，写入 systemPrompt，返回之
//  2. sessionID 存在，但数据库中无此记录 → 以此 ID 创建新会话，写入 systemPrompt，返回之
//  3. sessionID 存在，systemPrompt 非空 → UPDATE 该会话的规则书，返回新值
//  4. sessionID 存在，systemPrompt 为空 → SELECT 历史规则书并返回（维持上次设定）
func (s *SQLiteDB) EnsureConversation(ctx context.Context, sessionID, systemPrompt string) (string, string, error) {
	// 场景 1：全新会话
	if sessionID == "" {
		sessionID = newUUID()
		if err := s.insertConversation(ctx, sessionID, systemPrompt); err != nil {
			return "", "", err
		}
		return sessionID, systemPrompt, nil
	}

	// 查询会话是否已存在
	var saved string
	err := s.db.QueryRowContext(ctx,
		`SELECT system_prompt FROM conversations WHERE id = ?`, sessionID,
	).Scan(&saved)

	if err == sql.ErrNoRows {
		// 场景 2：ID 已由前端指定，但数据库中尚无记录
		if err := s.insertConversation(ctx, sessionID, systemPrompt); err != nil {
			return "", "", err
		}
		return sessionID, systemPrompt, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("EnsureConversation query: %w", err)
	}

	// 会话已存在
	if systemPrompt != "" {
		// 场景 3：前端传入了新规则书，UPDATE 并覆盖
		_, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET system_prompt = ? WHERE id = ?`,
			systemPrompt, sessionID,
		)
		if err != nil {
			return "", "", fmt.Errorf("EnsureConversation update: %w", err)
		}
		return sessionID, systemPrompt, nil
	}

	// 场景 4：前端未传规则书，返回数据库中保存的历史值
	return sessionID, saved, nil
}

// insertConversation 是内部辅助方法，执行 conversations 表的插入。
func (s *SQLiteDB) insertConversation(ctx context.Context, id, systemPrompt string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations(id, system_prompt) VALUES (?, ?)`,
		id, systemPrompt,
	)
	if err != nil {
		return fmt.Errorf("insertConversation: %w", err)
	}
	return nil
}

// SaveMessage 将一条消息写入数据库。
func (s *SQLiteDB) SaveMessage(ctx context.Context, conversationID, role, content string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(id, conversation_id, role, content) VALUES (?, ?, ?, ?)`,
		newUUID(), conversationID, role, content,
	)
	if err != nil {
		return fmt.Errorf("SaveMessage: %w", err)
	}
	return nil
}

// GetRecentMessages 按时间倒序取最近 limit 条消息，再反转后以正序返回。
func (s *SQLiteDB) GetRecentMessages(ctx context.Context, conversationID string, limit int) ([]MessageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_id, role, content, created_at
		FROM messages
		WHERE conversation_id = ?
		ORDER BY created_at DESC
		LIMIT ?
	`, conversationID, limit)
	if err != nil {
		return nil, fmt.Errorf("GetRecentMessages query: %w", err)
	}
	defer rows.Close()

	var records []MessageRecord
	for rows.Next() {
		var r MessageRecord
		var createdAt string
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.Role, &r.Content, &createdAt); err != nil {
			return nil, fmt.Errorf("GetRecentMessages scan: %w", err)
		}
		r.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetRecentMessages rows: %w", err)
	}

	// 反转为时间正序
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records, nil
}

// Close 关闭底层数据库连接。
func (s *SQLiteDB) Close() error {
	return s.db.Close()
}
