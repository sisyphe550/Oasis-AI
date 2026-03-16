// Package llm 封装与 Ollama 推理层的所有 HTTP 交互。
// Ollama 运行在宿主机原生环境中（Mac Metal / Linux CUDA），
// Go 后端通过 OLLAMA_BASE_URL 环境变量与其通信。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ─── 接口定义 ────────────────────────────────────────────────────────────────

// LLMClient 是与大语言模型交互的统一抽象接口。
// 通过接口解耦，方便上层编排引擎注入 mock 进行单元测试。
type LLMClient interface {
	// GenerateCompletion 发送一次非流式文本生成请求，返回模型输出文本。
	GenerateCompletion(ctx context.Context, req CompletionRequest) (CompletionResponse, error)

	// GenerateEmbedding 对给定文本生成语义嵌入向量。
	GenerateEmbedding(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
}

// ─── 请求 / 响应结构体 ────────────────────────────────────────────────────────

// Message 表示对话历史中的单条消息（role: system / user / assistant）。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CompletionRequest 对应 Ollama /api/chat 接口的请求体。
type CompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Options  *Options  `json:"options,omitempty"`
}

// Options 可选的采样参数，全部字段均为可选。
type Options struct {
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
}

// CompletionResponse 对应 Ollama /api/chat 接口的响应体（非流式）。
type CompletionResponse struct {
	Model     string  `json:"model"`
	Message   Message `json:"message"`
	DoneReason string `json:"done_reason,omitempty"`
	Done      bool    `json:"done"`
}

// EmbeddingRequest 对应 Ollama /api/embed 接口的请求体。
type EmbeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbeddingResponse 对应 Ollama /api/embed 接口的响应体。
type EmbeddingResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float64 `json:"embeddings"`
}

// ─── 实现 ─────────────────────────────────────────────────────────────────────

// OllamaClient 是 LLMClient 接口的 Ollama HTTP 实现。
type OllamaClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewOllamaClient 创建一个新的 OllamaClient。
// baseURL 从环境变量 OLLAMA_BASE_URL 读取，缺省为 http://127.0.0.1:11434。
func NewOllamaClient() *OllamaClient {
	baseURL := os.Getenv("OLLAMA_BASE_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	return &OllamaClient{
		baseURL: baseURL,
		// 外层 Transport 超时兜底；实际每次请求超时由 context 控制。
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// GenerateCompletion 调用 Ollama /api/chat 接口，以非流式方式获取模型回复。
func (c *OllamaClient) GenerateCompletion(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	req.Stream = false // 强制非流式，流式场景另行扩展

	var resp CompletionResponse
	if err := c.doPost(ctx, "/api/chat", req, &resp); err != nil {
		return CompletionResponse{}, fmt.Errorf("GenerateCompletion: %w", err)
	}
	return resp, nil
}

// GenerateEmbedding 调用 Ollama /api/embed 接口，获取文本的语义嵌入向量。
func (c *OllamaClient) GenerateEmbedding(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error) {
	var resp EmbeddingResponse
	if err := c.doPost(ctx, "/api/embed", req, &resp); err != nil {
		return EmbeddingResponse{}, fmt.Errorf("GenerateEmbedding: %w", err)
	}
	return resp, nil
}

// doPost 是通用的 JSON POST 请求辅助方法。
// ctx 用于超时与取消控制，payload 为请求体，out 为响应体解码目标。
func (c *OllamaClient) doPost(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama returned HTTP %d for %s", resp.StatusCode, path)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
