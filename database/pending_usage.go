package database

import (
	"context"
	"github.com/codex2api/internal/liveupdates"
	"sort"
	"strings"
	"time"
)

type pendingUsage struct {
	log       UsageLog
	sequence  uint64
	committed uint64
	finished  bool
}

// Pending rows are display-only snapshots. Billing and aggregate statistics
// continue to use committed usage_logs, exactly once per recorded attempt.
func (db *DB) BeginPendingUsage(log UsageLog) {
	if db == nil || log.RequestID == "" {
		return
	}
	log.ID = -db.pendingUsageID.Add(1)
	log.RequestState = "running"
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	db.pendingUsageMu.Lock()
	if db.pendingUsage == nil {
		db.pendingUsage = make(map[string]*pendingUsage)
	}
	db.pendingUsage[log.RequestID] = &pendingUsage{log: log}
	db.pendingUsageMu.Unlock()
	liveupdates.Publish(liveupdates.Requests, log.APIKeyID)
}
func (db *DB) UpdatePendingUsageAccount(requestID string, accountID int64, channel string) {
	if db == nil {
		return
	}
	db.pendingUsageMu.Lock()
	p := db.pendingUsage[requestID]
	if p == nil {
		db.pendingUsageMu.Unlock()
		return
	}
	same := p.log.AccountID == accountID && (p.log.AccountName != "" || p.log.AccountEmail != "")
	p.log.AccountID = accountID
	p.log.Channel = channel
	keyID := p.log.APIKeyID
	db.pendingUsageMu.Unlock()
	if !same {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var name string
		var raw interface{}
		err := db.conn.QueryRowContext(ctx, `SELECT name, credentials FROM accounts WHERE id = $1`, accountID).Scan(&name, &raw)
		cancel()
		if err == nil {
			db.pendingUsageMu.Lock()
			if current := db.pendingUsage[requestID]; current != nil && current.log.AccountID == accountID {
				current.log.AccountName = name
				current.log.AccountEmail = accountEmailFromRawCredentials(raw)
			}
			db.pendingUsageMu.Unlock()
		}
	}
	liveupdates.Publish(liveupdates.Requests, keyID)
}
func (db *DB) FinishPendingUsage(requestID string) {
	if db == nil {
		return
	}
	db.pendingUsageMu.Lock()
	p := db.pendingUsage[requestID]
	var keyID int64
	if p != nil {
		keyID = p.log.APIKeyID
		p.finished = true
		p.log.RequestState = "completed"
		if p.sequence == 0 || p.committed == p.sequence {
			delete(db.pendingUsage, requestID)
		}
	}
	db.pendingUsageMu.Unlock()
	if p != nil {
		liveupdates.Publish(liveupdates.Requests, keyID)
	}
}
func (db *DB) capturePendingUsage(input *UsageLogInput) uint64 {
	if input.RequestID == "" || input.IsRetryAttempt {
		return 0
	}
	db.pendingUsageMu.Lock()
	defer db.pendingUsageMu.Unlock()
	p := db.pendingUsage[input.RequestID]
	if p == nil {
		return 0
	}
	p.log.ProgressAvailable = true
	p.log.UsageEstimated = false
	p.log.ProgressUpdatedAt = time.Now()
	p.log.AccountID = input.AccountID
	p.log.Channel = input.Channel
	p.log.Model = input.Model
	p.log.EffectiveModel = input.EffectiveModel
	p.log.StatusCode = input.StatusCode
	p.log.DurationMs = input.DurationMs
	p.log.InputTokens = input.InputTokens
	p.log.OutputTokens = input.OutputTokens
	p.log.TotalTokens = input.TotalTokens
	p.log.FirstTokenMs = input.FirstTokenMs
	p.log.ReasoningTokens = input.ReasoningTokens
	p.log.CachedTokens = input.CachedTokens
	p.log.ReasoningEffort = input.ReasoningEffort
	p.log.EffectiveReasoningEffort = input.EffectiveReasoningEffort
	p.log.APIKeyID = input.APIKeyID
	p.log.APIKeyName = input.APIKeyName
	p.log.APIKeyMasked = input.APIKeyMasked
	p.log.ErrorMessage = input.ErrorMessage
	p.log.UpstreamErrorKind = input.UpstreamErrorKind
	p.log.Stream = input.Stream
	p.log.ViaWebsocket = input.ViaWebsocket
	p.log.Compact = input.Compact
	p.log.AccountBilled = UsageLogBilledCost(input)
	p.log.UserBilled = UsageLogUserBilledCost(input)
	p.log.UserBilling = input.UserBillingDetails()
	p.log.ServiceTier = input.ServiceTier
	p.log.RequestedServiceTier = input.RequestedServiceTier
	p.log.ActualServiceTier = input.ActualServiceTier
	p.log.BillingServiceTier = input.BillingServiceTier
	p.log.CacheWrite5mTokens = input.CacheWrite5mTokens
	p.log.CacheWrite1hTokens = input.CacheWrite1hTokens
	p.log.ImageInputTokens = input.ImageInputTokens
	p.log.ImageOutputTokens = input.ImageOutputTokens
	p.log.CachedImageInputTokens = input.CachedImageInputTokens
	p.log.ImageCount = input.ImageCount
	p.log.ImageSize = input.ImageSize
	p.log.ImageWidth = input.ImageWidth
	p.log.ImageHeight = input.ImageHeight
	p.log.ImageBytes = input.ImageBytes
	p.log.ImageFormat = input.ImageFormat
	p.log.VideoSeconds = input.VideoSeconds
	p.log.HasCompactionHistory = input.HasCompactionHistory
	p.log.Ultra = input.Ultra
	p.log.DaybreakProgram = input.DaybreakProgram
	p.log.WsAcquireMs = input.WsAcquireMs
	p.log.PromptTokens = input.PromptTokens
	p.log.CompletionTokens = input.CompletionTokens
	p.log.UpstreamRequestID = input.UpstreamRequestID
	p.log.UpstreamProxyID = input.UpstreamProxyID
	p.log.UpstreamProxyName = input.UpstreamProxyName
	p.log.populateBillingBreakdown()
	p.log.AttemptIndex = input.AttemptIndex
	p.log.InternalReason = input.InternalReason
	p.log.Endpoint = input.Endpoint
	p.log.InboundEndpoint = input.InboundEndpoint
	p.log.UpstreamEndpoint = input.UpstreamEndpoint
	p.sequence++
	return p.sequence
}
func (db *DB) commitPendingUsage(batch []usageLogEntry) {
	db.pendingUsageMu.Lock()
	defer db.pendingUsageMu.Unlock()
	for _, entry := range batch {
		p := db.pendingUsage[entry.RequestID]
		if p == nil || entry.pendingSequence == 0 {
			continue
		}
		if entry.pendingSequence > p.committed {
			p.committed = entry.pendingSequence
		}
		if p.finished && p.committed == p.sequence {
			delete(db.pendingUsage, entry.RequestID)
		}
	}
}

func (db *DB) PendingUsageLogs(f UsageLogFilter) []*UsageLog {
	result := []*UsageLog{}
	if db == nil || f.Page > 1 {
		return result
	}
	now := time.Now()
	db.pendingUsageMu.Lock()
	defer db.pendingUsageMu.Unlock()
	for _, entry := range db.pendingUsage {
		row := entry.log
		if !pendingUsageMatches(row, f) {
			continue
		}
		if !entry.finished {
			row.DurationMs = int(now.Sub(row.CreatedAt).Milliseconds())
			row.StatusCode = 0
		}
		result = append(result, &row)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	return result
}
func pendingUsageMatches(row UsageLog, f UsageLogFilter) bool {
	if !f.Start.IsZero() && row.CreatedAt.Before(f.Start) {
		return false
	}
	// Moving ranges are resolved just before the request; allow no future rows.
	if !f.End.IsZero() && row.CreatedAt.After(f.End) {
		return false
	}
	if f.RequestID != "" && row.RequestID != f.RequestID {
		return false
	}
	if f.UpstreamRequestID != "" && row.UpstreamRequestID != f.UpstreamRequestID {
		return false
	}
	if f.Model != "" && row.Model != f.Model {
		return false
	}
	if f.Endpoint != "" && row.InboundEndpoint != f.Endpoint && row.Endpoint != f.Endpoint {
		return false
	}
	if f.Channel != "" && row.Channel != f.Channel {
		return false
	}
	if f.APIKeyID != nil && row.APIKeyID != *f.APIKeyID {
		return false
	}
	if f.AccountID != nil && row.AccountID != *f.AccountID {
		return false
	}
	if f.ErrorOnly || f.StatusCode != 0 || f.StatusFamily != "" || f.ErrorKind != "" {
		return false
	}
	if f.FastOnly != nil && (row.ServiceTier == "priority") != *f.FastOnly {
		return false
	}
	if f.StreamOnly != nil && row.Stream != *f.StreamOnly {
		return false
	}
	if f.CompactOnly != nil && row.Compact != *f.CompactOnly {
		return false
	}
	if f.CompactionHistoryOnly != nil && row.HasCompactionHistory != *f.CompactionHistoryOnly {
		return false
	}
	if f.RetryOnly != nil && row.IsRetryAttempt != *f.RetryOnly {
		return false
	}
	if f.ViaWebsocketOnly != nil && row.ViaWebsocket != *f.ViaWebsocketOnly {
		return false
	}
	if f.UltraOnly != nil && row.Ultra != *f.UltraOnly {
		return false
	}
	if f.UpstreamModelMismatchOnly != nil {
		return false
	}
	if f.Email != "" && !strings.Contains(strings.ToLower(row.AccountEmail), strings.ToLower(f.Email)) {
		return false
	}
	if f.Query != "" {
		text := strings.Join([]string{row.RequestID, row.Model, row.EffectiveModel, row.Endpoint, row.ClientIP, row.ClientUserAgent, row.APIKeyName, row.APIKeyMasked, row.AccountEmail, row.AccountName}, " ")
		if !strings.Contains(strings.ToLower(text), strings.ToLower(f.Query)) {
			return false
		}
	}
	return true
}

// UpdatePendingUsageProgress changes only the display projection. It never
// touches the log buffer, quota counters or the authoritative billing ledger.
func (db *DB) UpdatePendingUsageProgress(requestID string, input UsageLogInput, estimated bool, rate float64, reasoningKnown bool) {
	if db == nil {
		return
	}
	db.pendingUsageMu.Lock()
	p := db.pendingUsage[requestID]
	if p == nil || p.finished || p.sequence > 0 {
		db.pendingUsageMu.Unlock()
		return
	}
	firstTokenArrived := p.log.FirstTokenMs == 0 && input.FirstTokenMs > 0
	p.log.ProgressAvailable = true
	p.log.UsageEstimated = estimated
	p.log.ProgressUpdatedAt = time.Now()
	p.log.TokensPerSecond = rate
	p.log.ReasoningTokensKnown = reasoningKnown
	p.log.InputTokens = input.InputTokens
	p.log.PromptTokens = input.InputTokens
	p.log.OutputTokens = input.OutputTokens
	p.log.CompletionTokens = input.OutputTokens
	p.log.TotalTokens = input.InputTokens + input.OutputTokens
	p.log.ReasoningTokens = input.ReasoningTokens
	p.log.CachedTokens = input.CachedTokens
	p.log.CacheWrite5mTokens = input.CacheWrite5mTokens
	p.log.CacheWrite1hTokens = input.CacheWrite1hTokens
	p.log.ImageInputTokens = input.ImageInputTokens
	p.log.ImageOutputTokens = input.ImageOutputTokens
	p.log.CachedImageInputTokens = input.CachedImageInputTokens
	p.log.FirstTokenMs = input.FirstTokenMs
	p.log.WsAcquireMs = input.WsAcquireMs
	if input.EffectiveModel != "" {
		p.log.EffectiveModel = input.EffectiveModel
	}
	if input.UpstreamResponseModel != "" {
		p.log.UpstreamResponseModel = input.UpstreamResponseModel
		p.log.UpstreamModelMismatch = input.UpstreamModelMismatch
	}
	if input.EffectiveReasoningEffort != "" {
		p.log.EffectiveReasoningEffort = input.EffectiveReasoningEffort
	}
	p.log.UpstreamUserAgent = input.UpstreamUserAgent
	p.log.UserAgentOverridden = input.UserAgentOverridden
	p.log.UpstreamRequestID = input.UpstreamRequestID
	p.log.UpstreamProxyID = input.UpstreamProxyID
	p.log.UpstreamProxyName = input.UpstreamProxyName
	if input.UpstreamEndpoint != "" {
		p.log.UpstreamEndpoint = input.UpstreamEndpoint
	}
	p.log.InjectedTurnState = input.InjectedTurnState
	p.log.UpstreamTurnState = input.UpstreamTurnState
	if input.ActualServiceTier != "" {
		p.log.ActualServiceTier = input.ActualServiceTier
	}
	if input.ServiceTier != "" {
		p.log.ServiceTier = input.ServiceTier
	}
	if input.BillingServiceTier != "" {
		p.log.BillingServiceTier = input.BillingServiceTier
	}
	p.log.Ultra = input.Ultra
	p.log.HasCompactionHistory = input.HasCompactionHistory
	if input.ErrorMessage != "" {
		p.log.ErrorMessage = input.ErrorMessage
		p.log.UpstreamErrorKind = input.UpstreamErrorKind
	}
	costInput := input
	costInput.Model = p.log.Model
	costInput.EffectiveModel = p.log.EffectiveModel
	costInput.DaybreakProgram = p.log.DaybreakProgram
	costInput.ServiceTier = p.log.ServiceTier
	p.log.AccountBilled = UsageLogBilledCost(&costInput)
	p.log.UserBilled = UsageLogUserBilledCost(&costInput)
	p.log.populateBillingBreakdown()
	keyID := p.log.APIKeyID
	db.pendingUsageMu.Unlock()
	if firstTokenArrived {
		liveupdates.PublishImmediate(liveupdates.Requests, keyID)
	} else {
		liveupdates.Publish(liveupdates.Requests, keyID)
	}
}
