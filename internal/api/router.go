package api

import (
	"encoding/json"
	"net/http"
)

const Version = "0.1.0"

// NewRouter 构建并返回顶层路由多路复用器。
func NewRouter() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("POST /api/chat", handleChatPlaceholder)

	// 将 web/ 目录下的前端静态资源挂载到根路径
	mux.Handle("/", http.FileServer(http.Dir("web")))

	return mux
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	resp := HealthResponse{
		Status:  "ok",
		Version: Version,
		Checks: map[string]string{
			"ollama": "unchecked",
			"qdrant": "unchecked",
			"sqlite": "unchecked",
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func handleChatPlaceholder(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, ChatResponse{
			Status: http.StatusBadRequest,
			Error:  "invalid request body: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusNotImplemented, ChatResponse{
		Status:  http.StatusNotImplemented,
		Content: "chat endpoint not yet implemented",
		Model:   "none",
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
