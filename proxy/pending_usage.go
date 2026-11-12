package proxy

import (
	"context"
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
	populateCompactUsageMetaFromRequest(c, input)
	populateUltraUsageMetaFromRequest(c, input)
	row := database.UsageLog{RequestID: requestID, Model: model, EffectiveModel: model, Stream: stream, ViaWebsocket: websocket, Endpoint: input.Endpoint, InboundEndpoint: input.InboundEndpoint, ClientIP: input.ClientIP, ClientUserAgent: input.ClientUserAgent, APIKeyID: input.APIKeyID, APIKeyName: input.APIKeyName, APIKeyMasked: input.APIKeyMasked, CreatedAt: time.Now(), ReasoningEffort: firstPendingString(gjson.ParseBytes(body), "reasoning.effort", "reasoning_effort"), ServiceTier: gjson.GetBytes(body, "service_tier").String()}
	row.Ultra = input.Ultra
	row.HasCompactionHistory = input.HasCompactionHistory
	row.Compact = strings.Contains(row.Endpoint, "/compact")
	row.RequestedServiceTier = row.ServiceTier
	h.db.BeginPendingUsage(row)
	progress := &pendingProgress{db: h.db, requestID: requestID, started: row.CreatedAt, input: *input, estimated: true}
	progress.input.InputTokens = estimatePendingInput(gjson.ParseBytes(body))
	progress.input.ServiceTier = row.ServiceTier
	progress.input.RequestedServiceTier = row.ServiceTier
	progress.input.ReasoningEffort = row.ReasoningEffort
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), pendingProgressKey{}, progress))
	progress.mu.Lock()
	progress.pushLocked(c.Request.Context())
	progress.mu.Unlock()
	return func() { progress.close(c.Request.Context()); h.db.FinishPendingUsage(requestID) }
}

// Freeze the same response-start timestamp used by the live projection before
// the existing WS-acquire adjustment is applied to the settled log.
func populatePendingFirstTokenFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil || input.IsRetryAttempt {
		return
	}
	if progress := progressFromContext(c.Request.Context()); progress != nil {
		progress.mu.Lock()
		if progress.input.FirstTokenMs > 0 {
			input.FirstTokenMs = progress.input.FirstTokenMs
		}
		progress.mu.Unlock()
	}
}
