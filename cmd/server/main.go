package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yourname/oasis-ai/internal/api"
	"github.com/yourname/oasis-ai/internal/llm"
	"github.com/yourname/oasis-ai/internal/orchestrator"
	"github.com/yourname/oasis-ai/internal/storage"
)

func main() {
	// ── 1. 依赖组装（手动 DI，从底层向上逐层初始化）────────────────────────

	// 推理层：Ollama 客户端（OLLAMA_BASE_URL 环境变量，默认 127.0.0.1:11434）
	ollamaClient := llm.NewOllamaClient()

	// 持久化层：SQLite（数据库文件写入 data/ 目录）
	dataDir := envOr("OASIS_DATA_DIR", filepath.Join(".", "data"))
	sqliteDB, err := storage.InitDB(dataDir)
	if err != nil {
		log.Fatalf("failed to init SQLite: %v", err)
	}
	defer func() {
		if err := sqliteDB.Close(); err != nil {
			log.Printf("sqlite close error: %v", err)
		}
	}()

	// 持久化层：Qdrant 客户端（VECTOR_DB_URL 环境变量，默认 localhost:6333）
	qdrantClient := storage.NewQdrantClient()

	// 编排层：流水线引擎
	cfg := orchestrator.DefaultConfig()
	engine := orchestrator.New(ollamaClient, qdrantClient, sqliteDB, cfg)

	// 确保 Qdrant 集合存在（服务启动时执行一次）
	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer initCancel()
	if err := engine.InitVectorCollection(initCtx); err != nil {
		// Qdrant 不可达时打印警告，不阻断启动（允许纯离线模式运行）
		log.Printf("WARNING: could not init Qdrant collection: %v", err)
	}

	// ── 2. 构建 HTTP 服务器 ──────────────────────────────────────────────────

	addr := envOr("OASIS_ADDR", ":8080")
	handler := api.NewHandler(engine)

	srv := &http.Server{
		Addr:         addr,
		Handler:      api.NewRouter(handler),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // 留出模型推理时间
		IdleTimeout:  120 * time.Second,
	}

	// ── 3. 启动服务，监听系统信号，执行优雅停机 ─────────────────────────────

	go func() {
		log.Printf("oasis-ai %s  listening on %s", api.Version, addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	log.Printf("received signal %v, shutting down gracefully…", sig)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("server stopped cleanly")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
