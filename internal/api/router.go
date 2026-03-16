package api

import (
	"encoding/json"
	"net/http"
)

const Version = "0.1.0"

// NewRouter 构建并返回顶层路由多路复用器。
// handler 持有编排引擎引用，由 main.go 完成依赖组装后注入。
func NewRouter(handler *Handler) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", handleHealth)
	mux.HandleFunc("POST /api/chat", handler.HandleChat)

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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
