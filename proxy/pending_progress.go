package proxy

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
)

type pendingProgressKey struct{}
type pendingProgress struct {
	reasoningKnown     bool
	reasoningActual    bool
	geminiUsage        geminiNativeUsage
	mu                 sync.Mutex
	db                 *database.DB
	requestID          string
	started            time.Time
	input              database.UsageLogInput
	estimated          bool
	outputTextChars    int
	reasoningTextChars int
	outputBase         int
	reasoningBase      int
	inputActual        bool
	outputActual       bool
	lastPush           time.Time
	lastOutput         int
	rate               float64
	timer              *time.Timer
	closed             bool
}

func progressFromContext(ctx context.Context) *pendingProgress {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(pendingProgressKey{}).(*pendingProgress)
	return p
}
func estimateVisibleTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return int(math.Ceil(float64(ascii)/4)) + other
}
func estimatePendingInput(root gjson.Result) int {
	count := 0
	var walk func(gjson.Result)
	walk = func(v gjson.Result) {
		if v.Type == gjson.String {
			count += estimateVisibleTokens(v.String())
			return
		}
		if v.Type != gjson.JSON {
			return
		}
		v.ForEach(func(k, v gjson.Result) bool {
			switch k.String() {
			case "image_url", "url", "data", "encrypted_content", "signature":
				return true
			}
			walk(v)
			return true
		})
	}
	for _, key := range []string{"input", "messages", "system", "instructions", "tools", "contents"} {
		v := root.Get(key)
		if v.Exists() {
			walk(v)
		}
	}
	if count > 0 {
		return count + 8
	}
	return 0
}
func (p *pendingProgress) pushLocked(ctx context.Context) {
	if p.closed {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	now := time.Now()
	if seconds := now.Sub(p.lastPush).Seconds(); !p.lastPush.IsZero() && seconds >= 0.1 {
		p.rate = float64(max(0, p.input.OutputTokens-p.lastOutput)) / seconds
	}
	p.lastPush = now
	p.lastOutput = p.input.OutputTokens
	p.input.WsAcquireMs = wsAcquireAuditMs(ctx)
	if ua, known := upstreamUserAgentAudit(ctx); known {
		p.input.UpstreamUserAgent = ua
		p.input.UserAgentOverridden = ua != p.input.ClientUserAgent
	}
	trace := snapshotUpstreamTrace(ctx)
	p.input.UpstreamRequestID = trace.UpstreamRequestID
	p.input.UpstreamProxyID = trace.Proxy.ID
	p.input.UpstreamProxyName = trace.Proxy.Name
	p.input.UpstreamTurnState = trace.UpstreamTurnState
	p.input.EffectiveReasoningEffort = trace.EffectiveReasoningEffort
	snapshot := p.input
	if CurrentRuntimeSettings().FirstTokenExcludesWsAcquire && snapshot.FirstTokenMs > 0 && snapshot.WsAcquireMs > 0 {
		snapshot.FirstTokenMs = max(1, snapshot.FirstTokenMs-snapshot.WsAcquireMs)
	}
	p.db.UpdatePendingUsageProgress(p.requestID, snapshot, p.estimated, p.rate, p.reasoningKnown)
}
func (p *pendingProgress) scheduleLocked(ctx context.Context) {
	if p.closed {
		return
	}
	if time.Since(p.lastPush) >= 250*time.Millisecond {
		p.pushLocked(ctx)
		return
	}
	if p.timer == nil {
		p.timer = time.AfterFunc(250*time.Millisecond, func() { p.mu.Lock(); defer p.mu.Unlock(); p.pushLocked(ctx) })
	}
}
func (p *pendingProgress) close(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushLocked(ctx)
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
}
func (p *pendingProgress) observe(ctx context.Context, raw []byte) {
	if !gjson.ValidBytes(raw) {
		return
	}
	root := gjson.ParseBytes(raw)
	if response := root.Get("response"); response.Get("usageMetadata").Exists() || response.Get("candidates").IsArray() {
		root = response
		raw = []byte(response.Raw)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	firstTokenWasUnknown := p.input.FirstTokenMs == 0
	kind := root.Get("type").String()
	output, reasoning := "", ""
	switch kind {
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		output = root.Get("delta").String()
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		reasoning = root.Get("delta").String()
	case "content_block_delta":
		delta := root.Get("delta")
		switch delta.Get("type").String() {
		case "text_delta":
			output = delta.Get("text").String()
		case "thinking_delta":
			reasoning = delta.Get("thinking").String()
		case "input_json_delta":
			output = delta.Get("partial_json").String()
		}
	}
	for _, choice := range root.Get("choices").Array() {
		delta := choice.Get("delta")
		output += delta.Get("content").String()
		r := delta.Get("reasoning_content").String()
		if r == "" {
			r = delta.Get("reasoning").String()
		}
		reasoning += r
		for _, tool := range delta.Get("tool_calls").Array() {
			output += tool.Get("function.arguments").String()
		}
	}
	for _, candidate := range root.Get("candidates").Array() {
		for _, part := range candidate.Get("content.parts").Array() {
			if part.Get("thought").Bool() {
				reasoning += part.Get("text").String()
			} else {
				output += part.Get("text").String()
			}
		}
	}
	if token := root.Get("result.response.token").String(); token != "" {
		if root.Get("result.response.isThinking").Bool() {
			reasoning += token
		} else {
			output += token
		}
	}
	if output != "" || reasoning != "" {
		// Characters are counted cumulatively before rounding, so token estimates
		// do not depend on the provider's arbitrary chunk boundaries.
		p.outputTextChars += pendingTextUnits(output)
		p.reasoningTextChars += pendingTextUnits(reasoning)
		p.input.OutputTokens = p.outputBase + (p.outputTextChars+p.reasoningTextChars+3)/4
		p.input.ReasoningTokens = p.reasoningBase + (p.reasoningTextChars+3)/4
		p.estimated = true
		p.outputActual = false
		if reasoning != "" {
			p.reasoningKnown = true
			p.reasoningActual = false
		}
	}
	// Share the settled statistic: the first upstream response frame counts,
	// even when text or reasoning has not begun.
	if firstTokenWasUnknown && isUpstreamResponseStartResult(root) {
		p.input.FirstTokenMs = max(1, int(time.Since(p.started).Milliseconds()))
	}
	usage := root.Get("response.usage")
	if !usage.Exists() {
		usage = root.Get("usage")
	}
	if !usage.Exists() {
		usage = root.Get("message.usage")
	}
	if usage.IsObject() {
		input := usage.Get("input_tokens")
		if !input.Exists() {
			input = usage.Get("prompt_tokens")
		}
		out := usage.Get("output_tokens")
		if !out.Exists() {
			out = usage.Get("completion_tokens")
		}
		cache := usage.Get("input_tokens_details.cached_tokens")
		if !cache.Exists() {
			cache = usage.Get("prompt_tokens_details.cached_tokens")
		}
		if v := usage.Get("cache_read_input_tokens"); v.Exists() {
			cache = v
		}
		if cache.Exists() {
			p.input.CachedTokens = max(0, int(cache.Int()))
		}
		if v := usage.Get("cache_creation_input_tokens"); v.Exists() {
			p.input.CacheWrite5mTokens = max(0, int(v.Int()))
		}
		if v := usage.Get("cache_creation.ephemeral_5m_input_tokens"); v.Exists() {
			p.input.CacheWrite5mTokens = max(0, int(v.Int()))
		}
		if v := usage.Get("cache_creation.ephemeral_1h_input_tokens"); v.Exists() {
			p.input.CacheWrite1hTokens = max(0, int(v.Int()))
			if !usage.Get("cache_creation.ephemeral_5m_input_tokens").Exists() {
				p.input.CacheWrite5mTokens = max(0, int(usage.Get("cache_creation_input_tokens").Int())-p.input.CacheWrite1hTokens)
			}
		}
		if input.Exists() {
			p.input.InputTokens = max(0, int(input.Int()))
			if kind == "message_start" || kind == "message_delta" {
				p.input.InputTokens += p.input.CachedTokens + p.input.CacheWrite5mTokens + p.input.CacheWrite1hTokens
			}
			p.inputActual = true
		}
		if out.Exists() {
			p.input.OutputTokens = max(0, int(out.Int()))
			p.outputBase = p.input.OutputTokens
			p.outputTextChars = 0
			p.reasoningTextChars = 0
			p.outputActual = true
		}
		r := usage.Get("output_tokens_details.reasoning_tokens")
		if !r.Exists() {
			r = usage.Get("completion_tokens_details.reasoning_tokens")
		}
		if r.Exists() {
			p.reasoningKnown = true
			p.reasoningActual = true
			p.input.ReasoningTokens = max(0, int(r.Int()))
			p.reasoningBase = p.input.ReasoningTokens
		}
		for _, field := range []struct {
			path string
			dest *int
		}{{"input_tokens_details.image_tokens", &p.input.ImageInputTokens}, {"output_tokens_details.image_tokens", &p.input.ImageOutputTokens}, {"input_tokens_details.cached_tokens_details.image_tokens", &p.input.CachedImageInputTokens}} {
			if v := usage.Get(field.path); v.Exists() {
				*field.dest = max(0, int(v.Int()))
			}
		}
		if p.inputActual && p.outputActual {
			p.estimated = p.reasoningKnown && !p.reasoningActual
		}
	}
	if root.Get("usageMetadata").Exists() {
		p.geminiUsage.observe(raw)
		u := p.geminiUsage.usageInfo()
		p.input.InputTokens = u.InputTokens
		p.input.OutputTokens = u.OutputTokens
		p.input.ReasoningTokens = u.ReasoningTokens
		p.input.CachedTokens = u.CachedTokens
		p.estimated = false
		p.reasoningKnown = p.reasoningKnown || root.Get("usageMetadata.thoughtsTokenCount").Exists()
		p.outputBase = u.OutputTokens
		p.reasoningBase = u.ReasoningTokens
		p.outputTextChars = 0
		p.reasoningTextChars = 0
	}
	model := root.Get("response.model").String()
	if model == "" {
		model = root.Get("model").String()
	}
	if model != "" {
		p.input.UpstreamResponseModel = model
		if p.input.EffectiveModel == "" {
			p.input.EffectiveModel = model
		}
		mismatch := p.input.EffectiveModel != model
		p.input.UpstreamModelMismatch = &mismatch
	}
	tier := root.Get("response.service_tier").String()
	if tier == "" {
		tier = root.Get("service_tier").String()
	}
	if tier != "" {
		p.input.ActualServiceTier = tier
		p.input.BillingServiceTier = tier
	}
	if errorMessage := firstPendingString(root, "error.message", "response.error.message"); errorMessage != "" {
		p.input.ErrorMessage = security.MaskSensitiveData(security.SafeTruncate(errorMessage, 600))
		p.input.UpstreamErrorKind = "upstream_error"
	}
	if firstTokenWasUnknown && p.input.FirstTokenMs > 0 {
		p.pushLocked(ctx)
	} else {
		p.scheduleLocked(ctx)
	}
}

type pendingProgressBody struct {
	io.ReadCloser
	ctx      context.Context
	p        *pendingProgress
	line     []byte
	data     []byte
	dropping bool
}

func (b *pendingProgressBody) Read(dst []byte) (int, error) {
	n, err := b.ReadCloser.Read(dst)
	if n > 0 {
		b.feed(dst[:n])
	}
	if err == io.EOF {
		b.flush()
	}
	return n, err
}
func (b *pendingProgressBody) Close() error { b.flush(); return b.ReadCloser.Close() }
func (b *pendingProgressBody) feed(data []byte) {
	for _, c := range data {
		if c == '\n' {
			if !b.dropping {
				b.consumeLine(bytes.TrimSuffix(b.line, []byte{'\r'}))
			}
			b.line = b.line[:0]
			b.dropping = false
			continue
		}
		if b.dropping {
			continue
		}
		if len(b.line) >= 1<<20 {
			b.line = b.line[:0]
			b.dropping = true
			continue
		}
		b.line = append(b.line, c)
	}
}
func (b *pendingProgressBody) consumeLine(line []byte) {
	if len(line) == 0 {
		if len(b.data) > 0 {
			b.p.observe(b.ctx, b.data)
			b.data = b.data[:0]
		}
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		v := bytes.TrimSpace(line[5:])
		if len(b.data)+len(v) < 1<<20 {
			if len(b.data) > 0 {
				b.data = append(b.data, '\n')
			}
			b.data = append(b.data, v...)
		}
		return
	}
	if bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) {
		b.p.observe(b.ctx, line)
	}
}
func (b *pendingProgressBody) flush() {
	if len(b.line) > 0 && !b.dropping {
		b.consumeLine(b.line)
		b.line = nil
	}
	if len(b.data) > 0 {
		b.p.observe(b.ctx, b.data)
		b.data = nil
	}
}

func observePendingResponse(ctx context.Context, respBody io.ReadCloser) io.ReadCloser {
	p := progressFromContext(ctx)
	if p == nil || respBody == nil {
		return respBody
	}
	return &pendingProgressBody{ReadCloser: respBody, ctx: ctx, p: p}
}

func firstPendingString(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if s := root.Get(path).String(); s != "" {
			return s
		}
	}
	return ""
}
func notifyPendingProgress(ctx context.Context) {
	if p := progressFromContext(ctx); p != nil {
		p.mu.Lock()
		p.scheduleLocked(ctx)
		p.mu.Unlock()
	}
}
func updatePendingUpstreamRequest(ctx context.Context, request *http.Request) {
	p := progressFromContext(ctx)
	if p == nil || request == nil {
		return
	}
	var raw []byte
	if request.GetBody != nil {
		if body, err := request.GetBody(); err == nil {
			raw, _ = io.ReadAll(io.LimitReader(body, 1<<20))
			body.Close()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.input.UpstreamEndpoint = request.URL.Path
	if model := gjson.GetBytes(raw, "model").String(); model != "" {
		p.input.EffectiveModel = model
	}
	p.scheduleLocked(ctx)
}

func updatePendingPayload(ctx context.Context, raw []byte) {
	p := progressFromContext(ctx)
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	root := gjson.ParseBytes(raw)
	if model := root.Get("model").String(); model != "" {
		p.input.EffectiveModel = model
	}
	if tier := root.Get("service_tier").String(); tier != "" {
		p.input.ServiceTier = tier
		p.input.RequestedServiceTier = tier
	}
	p.scheduleLocked(ctx)
}

func pendingTextUnits(text string) int {
	n := 0
	for _, r := range text {
		if r < 128 {
			n++
		} else {
			n += 4
		}
	}
	return n
}
