// Package storage 封装所有持久化与语义检索操作。
// qdrant.go 通过 Qdrant REST API 管理向量记忆的存取与检索。
package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ─── 接口定义 ────────────────────────────────────────────────────────────────

// VectorStore 是向量存储层的统一抽象接口。
type VectorStore interface {
	// InitCollection 确保指定集合存在，不存在则自动创建。
	// dimension 为向量维度，应与 Embedding 模型输出维度一致。
	InitCollection(ctx context.Context, collection string, dimension uint64) error

	// UpsertMemory 将一条记忆文本及其向量写入或更新到集合中。
	UpsertMemory(ctx context.Context, collection string, mem Memory) error

	// SearchContext 根据查询向量，在指定会话范围内返回 Top-K 最相关的历史记忆。
	// conversationID 用于 Payload Filtering，确保不同会话的记忆严格隔离。
	SearchContext(ctx context.Context, collection string, vector []float64, topK int, conversationID string) ([]Memory, error)
}

// Memory 代表一条可语义检索的记忆单元。
type Memory struct {
	// ID 唯一标识一条记忆，留空时自动生成 UUID。
	ID string
	// Vector 由 Embedding 模型生成的语义向量。
	Vector []float64
	// Text 原始文本内容，存入 payload 方便检索后直接使用。
	Text string
	// ConversationID 关联的对话 ID，用于按会话过滤。
	ConversationID string
}

// ─── Qdrant REST API 数据结构 ─────────────────────────────────────────────────

type qdrantVectorParams struct {
	Size     uint64 `json:"size"`
	Distance string `json:"distance"`
}

type qdrantCreateCollection struct {
	Vectors qdrantVectorParams `json:"vectors"`
}

type qdrantPoint struct {
	ID      string         `json:"id"`
	Vector  []float64      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

type qdrantUpsertRequest struct {
	Points []qdrantPoint `json:"points"`
}

// qdrantMatch 对应 Qdrant Payload Filtering 的精确匹配条件。
type qdrantMatch struct {
	Value string `json:"value"`
}

// qdrantFieldCondition 代表一条 payload 字段过滤规则。
type qdrantFieldCondition struct {
	Key   string      `json:"key"`
	Match qdrantMatch `json:"match"`
}

// qdrantFilter 对应 Qdrant 的 filter 对象，must 中的条件全部满足才会命中。
type qdrantFilter struct {
	Must []qdrantFieldCondition `json:"must"`
}

type qdrantSearchRequest struct {
	Vector      []float64     `json:"vector"`
	Limit       int           `json:"limit"`
	WithPayload bool          `json:"with_payload"`
	Filter      *qdrantFilter `json:"filter,omitempty"` // nil 时不序列化，保持向后兼容
}

type qdrantSearchResult struct {
	Result []struct {
		ID      string         `json:"id"`
		Score   float64        `json:"score"`
		Payload map[string]any `json:"payload"`
	} `json:"result"`
}

// ─── 实现 ─────────────────────────────────────────────────────────────────────

// QdrantClient 是 VectorStore 接口的 Qdrant HTTP 实现。
type QdrantClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewQdrantClient 创建一个新的 QdrantClient。
// baseURL 从环境变量 VECTOR_DB_URL 读取，缺省为 http://localhost:6333。
func NewQdrantClient() *QdrantClient {
	baseURL := os.Getenv("VECTOR_DB_URL")
	if baseURL == "" {
		baseURL = "http://localhost:6333"
	}
	return &QdrantClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// InitCollection 检查集合是否存在，不存在则以余弦相似度创建。
// 对于 Qwen / nomic-embed 等模型，常见维度为 768、1536 或 4096。
func (q *QdrantClient) InitCollection(ctx context.Context, collection string, dimension uint64) error {
	// 先探测集合是否已存在
	url := fmt.Sprintf("%s/collections/%s", q.baseURL, collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("InitCollection build GET: %w", err)
	}
	resp, err := q.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("InitCollection GET: %w", err)
	}
	resp.Body.Close()

	// 集合已存在，无需创建
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	// 集合不存在（404），调用 PUT 创建
	body := qdrantCreateCollection{
		Vectors: qdrantVectorParams{
			Size:     dimension,
			Distance: "Cosine",
		},
	}
	if err := q.doPut(ctx, "/collections/"+collection, body, nil); err != nil {
		return fmt.Errorf("InitCollection create: %w", err)
	}
	return nil
}

// UpsertMemory 将一条 Memory 向量点写入 Qdrant 集合。
// 若 Memory.ID 为空，则自动生成一个 UUID v4。
func (q *QdrantClient) UpsertMemory(ctx context.Context, collection string, mem Memory) error {
	if mem.ID == "" {
		mem.ID = newUUID()
	}

	point := qdrantPoint{
		ID:     mem.ID,
		Vector: mem.Vector,
		Payload: map[string]any{
			"text":            mem.Text,
			"conversation_id": mem.ConversationID,
		},
	}
	body := qdrantUpsertRequest{Points: []qdrantPoint{point}}

	path := fmt.Sprintf("/collections/%s/points", collection)
	if err := q.doPut(ctx, path, body, nil); err != nil {
		return fmt.Errorf("UpsertMemory: %w", err)
	}
	return nil
}

// SearchContext 根据查询向量检索 Top-K 相关记忆，结果按相似度由高到低排列。
// conversationID 非空时注入 Payload Filter，严格隔离不同会话的记忆，防止串戏。
func (q *QdrantClient) SearchContext(ctx context.Context, collection string, vector []float64, topK int, conversationID string) ([]Memory, error) {
	body := qdrantSearchRequest{
		Vector:      vector,
		Limit:       topK,
		WithPayload: true,
	}

	// 仅当 conversationID 非空时才注入过滤条件，保持对无会话场景的向后兼容
	if conversationID != "" {
		body.Filter = &qdrantFilter{
			Must: []qdrantFieldCondition{
				{
					Key:   "conversation_id",
					Match: qdrantMatch{Value: conversationID},
				},
			},
		}
	}

	path := fmt.Sprintf("/collections/%s/points/search", collection)
	var result qdrantSearchResult
	if err := q.doPost(ctx, path, body, &result); err != nil {
		return nil, fmt.Errorf("SearchContext: %w", err)
	}

	memories := make([]Memory, 0, len(result.Result))
	for _, r := range result.Result {
		mem := Memory{ID: r.ID}
		if t, ok := r.Payload["text"].(string); ok {
			mem.Text = t
		}
		if cid, ok := r.Payload["conversation_id"].(string); ok {
			mem.ConversationID = cid
		}
		memories = append(memories, mem)
	}
	return memories, nil
}

// ─── HTTP 辅助方法 ─────────────────────────────────────────────────────────────

func (q *QdrantClient) doPost(ctx context.Context, path string, payload, out any) error {
	return q.doRequest(ctx, http.MethodPost, path, payload, out)
}

func (q *QdrantClient) doPut(ctx context.Context, path string, payload, out any) error {
	return q.doRequest(ctx, http.MethodPut, path, payload, out)
}

// newUUID 用标准库 crypto/rand 生成一个符合 RFC 4122 v4 格式的 UUID 字符串。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:]),
	)
}

func (q *QdrantClient) doRequest(ctx context.Context, method, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := q.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant returned HTTP %d for %s %s", resp.StatusCode, method, path)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
