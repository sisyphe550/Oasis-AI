# Phase2 核心任务

## 任务 1：`internal/llm/ollama.go`

**架构设计要点：**

```
LLMClient (interface)
    ├── GenerateCompletion(ctx, req) → CompletionResponse
    └── GenerateEmbedding(ctx, req) → EmbeddingResponse

OllamaClient (struct，实现上述接口)
    └── doPost() → 统一 HTTP 请求 + JSON 编解码
```

关键决策：
- `LLMClient` 面向接口，编排层只依赖接口，方便注入测试 mock
- 超时控制由调用方的 `context.Context` 决定，`http.Client` 的 `Timeout` 仅作为兜底
- `baseURL` 从 `OLLAMA_BASE_URL` 环境变量读取，Ollama 运行在宿主机上，Docker 容器内可设为 `http://host.docker.internal:11434`
- `GenerateCompletion` 强制 `stream: false`，流式场景后续单独扩展

---

## 任务 2：`internal/storage/qdrant.go`

**架构设计要点：**

```
VectorStore (interface)
    ├── InitCollection(ctx, collection, dimension)
    ├── UpsertMemory(ctx, collection, Memory)
    └── SearchContext(ctx, collection, vector, topK) → []Memory

QdrantClient (struct，实现上述接口)
    ├── doPut()   → 创建集合 / 写入向量点
    └── doPost()  → 向量检索
```

关键决策：
- 直接封装 Qdrant REST API，不引入重量级 SDK，零外部依赖
- `InitCollection` 先 `GET` 探测集合是否存在，幂等设计，服务每次启动都可以安全调用
- UUID 生成用标准库 `crypto/rand` 实现，完全符合 RFC 4122 v4 格式
- 向量相似度度量统一使用 `Cosine`（余弦相似度），适配所有主流 Embedding 模型
- `baseURL` 从 `VECTOR_DB_URL` 环境变量读取，默认 `http://localhost:6333`

---

**关于向量维度选择** —— 你需要根据实际使用的 Embedding 模型来定：

| Embedding 模型 | 维度 |
|---|---|
| `nomic-embed-text` | 768 |
| `mxbai-embed-large` | 1024 |
| `bge-m3` | 1024 |
| `qwen2.5:7b`（需验证） | 4096 |

调用 `InitCollection` 时传入对应的维度值即可。

### 任务 3：`internal/storage/sqlite.go`

**架构决策：**

选用 `modernc.org/sqlite`（纯 Go，无需 CGO），而非 `go-sqlite3`，原因如下：
- `go-sqlite3` 需要 GCC 工具链，在 Mac 上编译 CI 环境中容易出问题
- 纯 Go 实现真正实现"编译一次，到处运行"

**接口设计：**

```
MessageStore (interface)
  ├── EnsureConversation(ctx, conversationID, chainID) → string
  ├── SaveMessage(ctx, conversationID, role, content)
  ├── GetRecentMessages(ctx, conversationID, limit) → []MessageRecord
  └── Close()
```

**数据库表结构：**

```sql
conversations(id, chain_id, created_at)
messages(id, conversation_id, role, content, created_at)
           └── INDEX on (conversation_id, created_at)
```

关键设计细节：
- 开启 `WAL` 模式，提升并发读写性能
- `EnsureConversation` 使用 `INSERT OR IGNORE`，设计为幂等，服务重启安全
- `GetRecentMessages` 先倒序取最近 N 条，再反转为正序，方便直接拼入 Prompt

---

### 任务 4：`internal/orchestrator/pipeline.go`

**关键修复 —— 循环依赖消除：**

原始设计中 `pipeline.go` 引用了 `api.ChatRequest`，这会在任务 5 中产生：

```
api → orchestrator → api   ✗ 循环依赖，Go 编译器直接报错
```

修复后依赖图（单向，无环）：

```
cmd/server/main.go
       ↓
   internal/api          ← 只持有 HTTP 层职责
       ↓ (import)
internal/orchestrator    ← 定义自己的 PipelineRequest/PipelineResponse
       ↓ (import)
internal/llm    internal/storage    ← 最底层，不向上引用任何包
```

**六步数据流实现：**

| 步骤 | 操作 | 降级策略 |
|---|---|---|
| A | 用户输入落盘 SQLite | 失败则终止（必须保证持久化） |
| B | nomic-embed-text 生成 768 维向量 | 失败则终止 |
| C | Qdrant 检索 Top-3 历史记忆 | **失败降级**：无记忆模式继续，不中断 |
| D | 模型 A（轻量 Router）提取意图摘要 | 失败则终止 |
| E | 模型 B（Generator）生成最终回复 | 失败则终止 |
| F | 回复落盘 SQLite + Qdrant | **失败不阻断**：回复已生成，异步落盘 |

---

`go vet` 零警告，linter 零报错，全项目编译通过。

---

### 任务 5 完成总结

#### 改动的三个文件

**`internal/api/handler.go`（新建）**

`Handler` 结构体持有 `*orchestrator.PipelineEngine`，是 HTTP 层与编排层之间唯一的连接点。`HandleChat` 做了三件事：

1. **输入校验** —— 解析 JSON，拒绝空 `message`，提前返回 400
2. **类型映射** —— `ChatRequest` → `PipelineRequest`（薄转换，不含业务逻辑）
3. **调用引擎 + 错误分发** —— 区分业务错误（400）和系统错误（500），避免把内部错误栈直接暴露给前端

**`internal/api/router.go`（改造）**

去掉了 Phase 1 的 `handleChatPlaceholder`，`NewRouter` 改为接受 `*Handler` 参数注入，路由绑定由此变为：

```
GET  /api/health  →  handleHealth（内联，无依赖）
POST /api/chat    →  handler.HandleChat（持有 pipeline 引用）
GET  /            →  FileServer("web/")（前端静态资源）
```

**`cmd/server/main.go`（重写）**

实现了完整的手动依赖注入（DI），组装顺序严格遵循"底层先初始化"原则：

```
Ollama Client    (llm 层，无状态，直接 New)
     ↓
SQLite DB        (storage 层，InitDB 创建文件 + 建表)
     ↓
Qdrant Client    (storage 层，无状态，直接 New)
     ↓
PipelineEngine   (orchestrator 层，注入以上三个依赖)
     ↓
api.Handler      (api 层，注入 engine)
     ↓
http.Server      (顶层，注入 router)
```

两处关键细节：
- **Qdrant 启动失败不阻断服务** —— `InitVectorCollection` 失败只打 WARNING，允许在没有 Docker 的纯离线环境下启动
- **WriteTimeout 调大到 60s** —— 给模型推理留出足够时间，避免大模型还在生成时连接被强制断开

---

### Phase 2 整体依赖关系图（最终状态）

```
cmd/server/main.go
        │
        ├── internal/api
        │       ├── handler.go  → orchestrator
        │       ├── router.go
        │       └── schema.go
        │
        ├── internal/orchestrator
        │       └── pipeline.go → llm
        │                       → storage
        │
        ├── internal/llm
        │       └── ollama.go   (仅标准库)
        │
        └── internal/storage
                ├── qdrant.go   (仅标准库)
                └── sqlite.go   → modernc.org/sqlite
```

无环，依赖方向全部自上而下。

---

### 启动方式（你现在就可以跑）

```bash
# 1. 启动 Qdrant（需要 Docker）
cd deploy && docker compose up -d

# 2. 启动 Go 服务（Ollama 需已在宿主机运行）
cd .. && go run ./cmd/server/

# 3. 测试健康检查
curl http://localhost:8080/api/health

# 4. 测试对话接口
curl -X POST http://localhost:8080/api/chat \
  -H "Content-Type: application/json" \
  -d '{"chain_id":"default","message":"你好，介绍一下你自己"}'
```

> 记得在 Ollama 中有 `qwen2.5:1.5b`（模型 A）和 `qwen2.5:7b`（模型 B），或者修改 `orchestrator.DefaultConfig()` 中的模型名为你本机已有的模型。