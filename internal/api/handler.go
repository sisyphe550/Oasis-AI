package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/yourname/oasis-ai/internal/orchestrator"
)

// Handler 持有所有 HTTP 处理函数需要的依赖。
type Handler struct {
	pipeline *orchestrator.PipelineEngine
}

// NewHandler 创建一个绑定了编排引擎的 Handler。
func NewHandler(pipeline *orchestrator.PipelineEngine) *Handler {
	return &Handler{pipeline: pipeline}
}

// HandleChat 处理 POST /api/chat 请求。
// 将前端的 ChatRequest 映射为编排层的 PipelineRequest，
// 执行完整流水线后将 PipelineResponse 映射回 ChatResponse 返回给前端。
func (h *Handler) HandleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ChatResponse{
			Status: http.StatusBadRequest,
			Error:  "invalid request body: " + err.Error(),
		})
		return
	}

	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, ChatResponse{
			Status: http.StatusBadRequest,
			Error:  "message must not be empty",
		})
		return
	}

	// 将 HTTP 层类型映射为编排层类型（薄转换，不含任何业务逻辑）
	pipeReq := orchestrator.PipelineRequest{
		SessionID:      req.SessionID,
		SystemPrompt:   req.SystemPrompt,
		Message:        req.Message,
		RouterModel:    req.RouterModel,
		GeneratorModel: req.GeneratorModel,
	}

	pipeResp, err := h.pipeline.ExecuteChain(r.Context(), pipeReq)
	if err != nil {
		log.Printf("ExecuteChain error: %v", err)

		status := http.StatusInternalServerError
		msg := "pipeline execution failed"
		if errors.Is(err, errInvalidInput) {
			status = http.StatusBadRequest
			msg = err.Error()
		}

		writeJSON(w, status, ChatResponse{
			Status: status,
			Error:  msg,
		})
		return
	}

	writeJSON(w, http.StatusOK, ChatResponse{
		SessionID: pipeResp.SessionID,
		Model:     pipeResp.Model,
		Content:   pipeResp.Content,
		Status:    http.StatusOK,
	})
}

// errInvalidInput 用于区分业务层输入校验错误与系统级错误。
var errInvalidInput = errors.New("invalid input")
