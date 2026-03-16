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
// 所有字段均可在运行时通过环境变量覆盖（由 config 层负责注入）。
type PipelineConfig struct {
	// RouterModel 是轻量级预处理模型（模型 A），
	// 用于意图提取 / 上下文压缩，建议使用 ≤3B 的小模型保证速度。
	RouterModel string

	// GeneratorModel 是主要的对话生成模型（模型 B），
	// 接收模型 A 的摘要输出，生成最终回复。
	GeneratorModel string

	// EmbedModel 用于生成语义嵌入向量的 Embedding 模型。
	// 已安装 nomic-embed-text（768 维）。
	EmbedModel string

	// EmbedDimension 与 EmbedModel 的输出维度保持一致。
	// nomic-embed-text = 768。
	EmbedDimension uint64

	// Collection 是 Qdrant 中存储对话记忆的集合名称。
	Collection string

	// TopK 是语义检索时返回的最相关历史记忆条数。
	TopK int
}

// DefaultConfig 返回面向 nomic-embed-text 的开箱即用默认配置。
// RouterModel 和 GeneratorModel 应由用户根据本机已安装的模型进行调整。
func DefaultConfig() PipelineConfig {
	return PipelineConfig{
		RouterModel:    "qwen2.5:1.5b", // 轻量压缩模型，可替换为本机已有的小模型
		GeneratorModel: "qwen2.5:7b",   // 主力对话模型，可替换
		EmbedModel:     "nomic-embed-text",
		EmbedDimension: 768,
		Collection:     "oasis_memories",
		TopK:           3,
	}
}

// ─── 引擎 ─────────────────────────────────────────────────────────────────────

// PipelineEngine 是核心编排引擎，持有对各下游模块的接口引用。
// 依赖接口而非具体实现，方便单元测试时注入 mock。
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
// 这里刻意不复用 api 包的类型，以避免循环依赖。
// api 层（handler）负责在两套类型之间做薄薄的映射。

// PipelineRequest 是编排引擎接收的输入。
type PipelineRequest struct {
	SessionID string
	ChainID   string
	Message   string
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
//	  └─ [A] 落盘 SQLite
//	  └─ [B] 生成 Embedding 向量
//	  └─ [C] 检索 Qdrant 历史记忆 (Top-K)
//	  └─ [D] 组装 Prompt → 调用模型 A (意图提取 / 上下文压缩)
//	  └─ [E] 调用模型 B (最终回复生成)
//	  └─ [F] 回复落盘 SQLite + Qdrant → 返回给调用方
func (e *PipelineEngine) ExecuteChain(ctx context.Context, req PipelineRequest) (PipelineResponse, error) {
	// ── 步骤 A：持久化用户输入到 SQLite ──────────────────────────────────────
	sessionID, err := e.db.EnsureConversation(ctx, req.SessionID, req.ChainID)
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step A EnsureConversation: %w", err)
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

	// ── 步骤 C：从 Qdrant 检索最相关的历史记忆 ───────────────────────────────
	memories, err := e.vector.SearchContext(ctx, e.cfg.Collection, queryVector, e.cfg.TopK)
	if err != nil {
		// 向量检索失败不应中断流程，降级为无记忆模式继续
		memories = nil
	}

	// === 🚀 新增监控日志 ===
	log.Printf("\n[流水线监控] 从 Qdrant 检索到 %d 条相关记忆:", len(memories))
	for i, m := range memories {
		log.Printf("  -> 记忆 %d: %s", i+1, m.Text)
	}
	// ========================

	// ── 步骤 D：组装 Prompt，调用模型 A 提取核心意图 ─────────────────────────
	intentPrompt := buildIntentPrompt(req.Message, memories)
	intentResp, err := e.lm.GenerateCompletion(ctx, llm.CompletionRequest{
		Model: e.cfg.RouterModel,
		Messages: []llm.Message{
			{Role: "system", Content: systemPromptRouter},
			{Role: "user", Content: intentPrompt},
		},
	})
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step D model A (%s): %w", e.cfg.RouterModel, err)
	}
	compressedContext := intentResp.Message.Content

	// === 🚀 新增监控日志 ===
	log.Printf("\n[流水线监控] 模型 A 提取的事实摘要如下:\n%s\n", compressedContext)
	// ========================

	// ── 步骤 E：调用模型 B，基于压缩上下文生成最终回复 ──────────────────────
	finalResp, err := e.lm.GenerateCompletion(ctx, llm.CompletionRequest{
		Model: e.cfg.GeneratorModel,
		Messages: []llm.Message{
			{Role: "system", Content: systemPromptGenerator},
			{Role: "user", Content: buildGeneratorPrompt(req.Message, compressedContext)},
		},
	})
	if err != nil {
		return PipelineResponse{}, fmt.Errorf("step E model B (%s): %w", e.cfg.GeneratorModel, err)
	}
	reply := finalResp.Message.Content

	// ── 步骤 F：落盘回复到 SQLite 和 Qdrant ──────────────────────────────────
	if err := e.db.SaveMessage(ctx, sessionID, "assistant", reply); err != nil {
		// 落盘失败不阻断响应，后续可加重试队列
		_ = err
	}

	// 将本轮对话的 Q+A 拼合后向量化存入 Qdrant，作为未来的检索记忆
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
// 应在 main.go 的初始化阶段调用一次。
func (e *PipelineEngine) InitVectorCollection(ctx context.Context) error {
	return e.vector.InitCollection(ctx, e.cfg.Collection, e.cfg.EmbedDimension)
}

// ─── Prompt 构建辅助函数 ──────────────────────────────────────────────────────

// const systemPromptRouter = `你是一个上下文分析助手。
// 你的任务是：根据提供的历史记忆和用户当前输入，提炼出用户的核心意图，并以简洁的一段话输出。
// 不要直接回答用户的问题，只需要输出意图摘要。`
const systemPromptRouter = `你是一个记忆提取与上下文整理助手。
你的任务是：仔细阅读提供的【相关历史记忆】，从中提取出能够回答用户当前问题的所有具体事实（如名称、地点、设定、物品等），并将其整理成清晰的背景知识摘要。
请直接输出提取到的事实。如果历史记忆中没有与用户问题相关的信息，请直接回复“无相关历史记忆”。`

// const systemPromptGenerator = `你是 Oasis-AI，一个智能、友好的 AI 助手。
// 你将收到一段背景摘要和用户的原始问题，请基于背景摘要给出详细、自然的回复。`
const systemPromptGenerator = `你是 Oasis-AI，一个智能、友好的 AI 助手，并且拥有完美的记忆力。
你将收到一份从数据库中检索出的【已知背景事实】以及用户的原始问题。
请严格基于【已知背景事实】来回答用户问题。如果背景事实中明确提示“无相关历史记忆”，你再基于你的常识自由回答。`

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
//
//	func buildGeneratorPrompt(userInput, compressedContext string) string {
//		return fmt.Sprintf("【背景摘要】\n%s\n\n【用户原始输入】\n%s", compressedContext, userInput)
//	}
func buildGeneratorPrompt(userInput, compressedContext string) string {
	return fmt.Sprintf("【已知背景事实】\n%s\n\n【用户问题】\n%s", compressedContext, userInput)
}
