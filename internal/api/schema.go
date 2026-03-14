package api

// ChatRequest 前端发送给后端的对话请求。
type ChatRequest struct {
	SessionID string `json:"session_id,omitempty"`
	ChainID   string `json:"chain_id"`
	Message   string `json:"message"`
	Stream    bool   `json:"stream,omitempty"`
}

// ChatResponse 后端返回给前端的对话响应。
type ChatResponse struct {
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Content   string `json:"content"`
	Status    int    `json:"status"`
	Error     string `json:"error,omitempty"`
}

// HealthResponse 健康检查接口返回体。
type HealthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Checks  map[string]string `json:"checks,omitempty"`
}
