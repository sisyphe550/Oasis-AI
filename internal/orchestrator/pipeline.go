// Package orchestrator 实现 Oasis-AI 的多模型流水线调度引擎。
// 它是连接 LLM 推理层、向量记忆层和 SQL 持久化层的"大脑"，
// 负责按照预定义的数据流编排各模块的调用顺序。
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yourname/oasis-ai/internal/llm"
	"github.com/yourname/oasis-ai/internal/storage"
)

// ─── 配置 ─────────────────────────────────────────────────────────────────────

// PipelineConfig 定义流水线引擎使用的模型和存储参数。
type PipelineConfig struct {
	// RouterModel 是轻量级预处理模型（模型 A），用于意图提取 / 上下文压缩。
	RouterModel string

	// GeneratorModel 是主要的对话生成模型（模型 B），生成最终回复。
	GeneratorModel string

	// EmbedModel 用于生成语义嵌入向量，已安装 nomic-embed-text（768 维）。
	EmbedModel string

	// EmbedDimension 与 EmbedModel 的输出维度一致，nomic-embed-text = 768。
	EmbedDimension uint64

	// Collection 是 Qdrant 中存储对话记忆的集合名称。
	Collection string

	// TopK 是语义检索时返回的最相关历史记忆条数。
	TopK int

	// DefaultSystemPrompt 当会话未绑定任何规则书时使用的兜底提示词。
	DefaultSystemPrompt string
}

// DefaultConfig 返回面向 nomic-embed-text 的开箱即用默认配置。
func DefaultConfig() PipelineConfig {
	return PipelineConfig{
		RouterModel:    "qwen2.5:1.5b",
		GeneratorModel: "qwen2.5:7b",
		EmbedModel:     "nomic-embed-text",
		EmbedDimension: 768,
		Collection:     "oasis_memories",
		TopK:           3,
		DefaultSystemPrompt: "你是 Oasis-AI，一个智能、友好的 AI 助手，并且拥有完美的记忆力。" +
			"你将收到一份从数据库中检索出的【已知背景事实】以及用户的原始问题。" +
			"请严格基于【已知背景事实】来回答用户问题。如果背景事实中明确提示\"无相关历史记忆\"，你再基于常识自由回答。",
	}
}

// ─── 引擎 ─────────────────────────────────────────────────────────────────────

// PipelineEngine 是核心编排引擎，持有对各下游模块的接口引用。
type PipelineEngine struct {
	lm     llm.LLMClient
	vector storage.VectorStore
	db     storage.MessageStore
	cfg    PipelineConfig
}

// New 创建一个新的 PipelineEngine。
func New(lm llm.LLMClient, vector storage.VectorStore, db storage.MessageStore, cfg PipelineConfig) *PipelineEngine {
	return &PipelineEngine{lm: lm, vector: vector, db: db, cfg: cfg}
}

// ─── 编排层专用请求/响应类型 ──────────────────────────────────────────────────

// PipelineRequest 是编排引擎接收的输入。
// SystemPrompt 为该会话的规则书，传空则沿用数据库中已保存的历史值。
type PipelineRequest struct {
	SessionID    string
	SystemPrompt string // 会话绑定型规则书，由前端传入
	Message      string
}

// PipelineResponse 是编排引擎返回的输出。
type PipelineResponse struct {
	SessionID string
	Model     string
	Content   string
}

// ─── 核心数据流 ───────────────────────────────────────────────────────────────

// ExecuteChain 执行完整的六步多模型流水线，返回最终的 AI 回复。
//
// 数据流总览：
//
//	用户输入
//	  └─ [A] EnsureConversation → 获取 sessionID + 本次生效的 systemPrompt
//	  └─ [B] 生成用户输入的 Embedding 向量
//	  └─ [C] 按 sessionID 隔离检索 Qdrant 历史记忆 (Top-K)
//	  └─ [D] 组装 Prompt → 调用模型 A (意图提取 / 上下文压缩)
//	  └─ [E] 注入 systemPrompt → 调用模型 B (最终回复生成)
//	  └─ [F] 回复落盘 SQLite + Qdrant → 返回给调用方
func (e *PipelineEngine) ExecuteChain(ctx context.Context, req PipelineRequest) (PipelineResponse, error) {
	// ── 步骤 A：持久化 / 恢复会话，获取本次生效的 System Prompt ──────────────
	// EnsureConversation 实现"规则书继承"语义：
	//   - 前端传了新规则书 → 更新数据库，本次用新值
	//   - 前端未传（空）   → 从数据库恢复上次的值
	sessionID, systemPrompt, err := e.db.EnsureConversation(ctx, req.SessionID, req.SystemPrompt)
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step A EnsureConversation: %w", err)
	}

	// Fallback：整个链路都没有规则书时，使用配置中的默认提示词
	if systemPrompt == "" {
		systemPrompt = e.cfg.DefaultSystemPrompt
	}

	if err := e.db.SaveMessage(ctx, sessionID, "user", req.Message); err != nil {
		return PipelineResponse{}, fmt.Errorf("step A SaveMessage: %w", err)
	}

	// ── 步骤 B：为用户输入生成语义 Embedding 向量 ────────────────────────────
	embedResp, err := e.lm.GenerateEmbedding(ctx, llm.EmbeddingRequest{
		Model: e.cfg.EmbedModel,
		Input: req.Message,
	})
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step B GenerateEmbedding: %w", err)
	}
	if len(embedResp.Embeddings) == 0 || len(embedResp.Embeddings[0]) == 0 {
		return PipelineResponse{}, fmt.Errorf("step B: embedding model returned empty vector")
	}
	queryVector := embedResp.Embeddings[0]

	// ── 步骤 C：按 sessionID 隔离，检索 Qdrant 最相关的历史记忆 ──────────────
	memories, err := e.vector.SearchContext(ctx, e.cfg.Collection, queryVector, e.cfg.TopK, sessionID)
	if err != nil {
		// 向量检索失败降级为无记忆模式，不中断流程
		memories = nil
	}

	log.Printf("[Pipeline] step C: retrieved %d memories for session %s", len(memories), sessionID)
	for i, m := range memories {
		log.Printf("[Pipeline]   memory[%d]: %s", i+1, m.Text)
	}

	// ── 步骤 D：组装 Prompt，调用模型 A 提取核心意图 ─────────────────────────
	// 将世界书追加在意图提取指令之后，让模型 A 理解自定义的专有名词和世界观设定，
	// 避免把"落日森林"、"新霓虹"之类的世界书词汇误判为"无相关历史记忆"。
	intentResp, err := e.lm.GenerateCompletion(ctx, llm.CompletionRequest{
		Model: e.cfg.RouterModel,
		Messages: []llm.Message{
			{Role: "system", Content: buildRouterSystemPrompt(systemPrompt)},
			{Role: "user", Content: buildIntentPrompt(req.Message, memories)},
		},
	})
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step D model A (%s): %w", e.cfg.RouterModel, err)
	}
	compressedContext := intentResp.Message.Content

	log.Printf("[Pipeline] step D: model A summary:\n%s", compressedContext)

	// ── 步骤 E：注入会话规则书，调用模型 B 生成最终回复 ──────────────────────
	// systemPrompt 已在步骤 A 从数据库中确定（新设 / 继承 / 默认），直接注入。
	finalResp, err := e.lm.GenerateCompletion(ctx, llm.CompletionRequest{
		Model: e.cfg.GeneratorModel,
		Messages: []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: buildGeneratorPrompt(req.Message, compressedContext)},
		},
	})
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step E model B (%s): %w", e.cfg.GeneratorModel, err)
	}
	reply := finalResp.Message.Content

	// ── 步骤 F：落盘回复到 SQLite 和 Qdrant ──────────────────────────────────
	if err := e.db.SaveMessage(ctx, sessionID, "assistant", reply); err != nil {
		_ = err // 落盘失败不阻断响应
	}

	memText := fmt.Sprintf("Q: %s\nA: %s", req.Message, reply)
	memEmbedResp, err := e.lm.GenerateEmbedding(ctx, llm.EmbeddingRequest{
		Model: e.cfg.EmbedModel,
		Input: memText,
	})
	if err == nil && len(memEmbedResp.Embeddings) > 0 {
		_ = e.vector.UpsertMemory(ctx, e.cfg.Collection, storage.Memory{
			Vector:         memEmbedResp.Embeddings[0],
			Text:           memText,
			ConversationID: sessionID,
		})
	}

	return PipelineResponse{
		SessionID: sessionID,
		Model:     e.cfg.GeneratorModel,
		Content:   reply,
	}, nil
}

// InitVectorCollection 在服务启动时确保 Qdrant 集合已创建。
func (e *PipelineEngine) InitVectorCollection(ctx context.Context) error {
	return e.vector.InitCollection(ctx, e.cfg.Collection, e.cfg.EmbedDimension)
}

// ─── Prompt 构建辅助函数 ──────────────────────────────────────────────────────

// routerBasePrompt 是模型 A（意图提取）的基础指令，固定不变。
const routerBasePrompt = "你是一个记忆提取与上下文整理助手。\n" +
	"你的任务是：仔细阅读提供的【相关历史记忆】，从中提取出能够回答用户当前问题的所有具体事实（如名称、地点、设定、物品等），并将其整理成清晰的背景知识摘要。\n" +
	"请直接输出提取到的事实。如果历史记忆中没有与用户问题相关的信息，请直接回复\"无相关历史记忆\"。"

// buildRouterSystemPrompt 在基础意图提取指令之后追加会话世界书，
// 让模型 A 能够理解自定义的专有名词与世界观设定，避免误判"无相关历史记忆"。
// 若 sessionSystemPrompt 为空，则退化为纯基础指令，行为与修改前完全一致。
func buildRouterSystemPrompt(sessionSystemPrompt string) string {
	if sessionSystemPrompt == "" {
		return routerBasePrompt
	}
	return routerBasePrompt + "\n\n【当前会话世界观背景（仅供理解专有名词，不影响你的提取任务）】\n" + sessionSystemPrompt
}

// buildIntentPrompt 将检索到的历史记忆和当前用户输入拼合成给模型 A 的 Prompt。
func buildIntentPrompt(userInput string, memories []storage.Memory) string {
	var sb strings.Builder
	if len(memories) > 0 {
		sb.WriteString("【相关历史记忆】\n")
		for i, m := range memories {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, m.Text)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("【用户当前输入】\n")
	sb.WriteString(userInput)
	return sb.String()
}

// buildGeneratorPrompt 将模型 A 的意图摘要和用户原始输入拼合成给模型 B 的 Prompt。
func buildGeneratorPrompt(userInput, compressedContext string) string {
	return fmt.Sprintf("【已知背景事实】\n%s\n\n【用户问题】\n%s", compressedContext, userInput)
}
