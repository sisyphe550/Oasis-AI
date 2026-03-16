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
	// SaveMessage 将一条消息落盘到指定对话。
	// role 应为 "user" 或 "assistant"。
	SaveMessage(ctx context.Context, conversationID, role, content string) error

	// GetRecentMessages 按时间倒序获取指定对话的最近 N 条消息，
	// 结果按时间正序返回，方便直接拼入 Prompt。
	GetRecentMessages(ctx context.Context, conversationID string, limit int) ([]MessageRecord, error)

	// EnsureConversation 确保对话记录存在，不存在则创建并返回其 ID。
	EnsureConversation(ctx context.Context, conversationID, chainID string) (string, error)

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

const ddl = `
CREATE TABLE IF NOT EXISTS conversations (
    id         TEXT PRIMARY KEY,
    chain_id   TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT (datetime('now'))
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
// dataDir 通常为项目根目录下的 "data/" 文件夹。
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

// EnsureConversation 确保对话存在，不存在则以给定 ID 创建。
// 若 conversationID 为空，则自动生成一个新 ID 并返回。
func (s *SQLiteDB) EnsureConversation(ctx context.Context, conversationID, chainID string) (string, error) {
	if conversationID == "" {
		conversationID = newUUID()
	}

	// INSERT OR IGNORE 保持幂等：相同 ID 已存在时不报错
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO conversations(id, chain_id) VALUES (?, ?)`,
		conversationID, chainID,
	)
	if err != nil {
		return "", fmt.Errorf("EnsureConversation: %w", err)
	}
	return conversationID, nil
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
