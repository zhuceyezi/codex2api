package proxy

import (
	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) beginPendingHTTPUsage(c *gin.Context) func() {
	if c.Request.Method != http.MethodPost {
		return func() {}
	}
	path := strings.TrimPrefix(c.Request.URL.Path, "/v1")
	path = strings.TrimPrefix(path, "/backend-api/codex")
	supported := path == "/responses" || path == "/responses/compact" || path == "/chat/completions" || path == "/messages" || path == "/images/generations" || path == "/images/edits" || strings.HasPrefix(path, "/videos/") || strings.HasPrefix(c.Request.URL.Path, "/v1beta/models/")
	if !supported {
		return func() {}
	}
	var body []byte
	if !strings.Contains(c.GetHeader("Content-Type"), "multipart/") {
		body = api.GetRawBody(c)
	}
	return h.beginPendingUsage(c, body, false)
}
func (h *Handler) beginPendingUsage(c *gin.Context, body []byte, websocket bool) func() {
	if h == nil || h.db == nil || c == nil || c.Request == nil {
		return func() {}
	}
	audit := upstreamTraceFromContext(c.Request.Context())
	if audit == nil {
		return func() {}
	}
	audit.mu.Lock()
	requestID := audit.requestID
	audit.pendingDB = h.db
	audit.mu.Unlock()
	model := gjson.GetBytes(body, "model").String()
	stream := websocket || gjson.GetBytes(body, "stream").Bool()
	input := &database.UsageLogInput{RequestID: requestID, Model: model, Stream: stream, ViaWebsocket: websocket, Endpoint: c.Request.URL.Path, InboundEndpoint: c.Request.URL.Path}
	populateAPIKeyMetaFromContext(c, input)
	populateClientIPFromRequest(c, input)
	populateUserAgentMetaFromRequest(c, input)
	row := database.UsageLog{RequestID: requestID, Model: model, EffectiveModel: model, Stream: stream, ViaWebsocket: websocket, Endpoint: input.Endpoint, InboundEndpoint: input.InboundEndpoint, ClientIP: input.ClientIP, ClientUserAgent: input.ClientUserAgent, APIKeyID: input.APIKeyID, APIKeyName: input.APIKeyName, APIKeyMasked: input.APIKeyMasked, CreatedAt: time.Now(), ReasoningEffort: gjson.GetBytes(body, "reasoning.effort").String(), ServiceTier: gjson.GetBytes(body, "service_tier").String()}
	h.db.BeginPendingUsage(row)
	return func() { h.db.FinishPendingUsage(requestID) }
}
