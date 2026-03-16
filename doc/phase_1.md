# Phase 1

## 任务 1：初始化项目与依赖

- 生成 go.mod 文件（模块名: github.com/yourname/oasis-ai）。
- 引入必要的库，建议使用原生的 net/http 或轻量级的 gin 框架，以及 google/uuid 等基础库。

## 任务 2：编写 Docker Compose 基础设施配置

- 在 deploy/docker-compose.yml 中，只配置 qdrant/qdrant 服务（暴露 6333 端口），映射一个本地 volume 以持久化向量数据。

## 任务 3：设计 API 契约 (API Schema)

- 在 internal/api/schema.go 中，定义前后端交互的基础 JSON 结构体。
- 至少包含：ChatRequest (包含用户输入、使用的链条ID) 和 ChatResponse (包含生成的文本、状态码、使用的模型)。

## 任务 4：搭建基础 HTTP Server

- 在 cmd/server/main.go 中，编写基础的 HTTP 服务器代码，监听 :8080 端口。
- 包含优雅停机 (Graceful Shutdown) 逻辑。
- 提供一个简单的 /api/health 健康检查接口。