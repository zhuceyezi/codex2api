package database

import (
	"context"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	quotaCalibrationDays = 35
	quotaMinimumSamples  = 3
	quotaMaxSampleSpan   = 45 * time.Minute
	quotaMaxIdleGap      = 5 * time.Minute
)

type UsageQuotaPlan struct {
	Plan          string           `json:"plan"`
	Samples       int              `json:"samples"`
	USDPerPercent float64          `json:"usd_per_percent,omitempty"`
	Models        []UsageModelStat `json:"models"`
}

type quotaCalibrationCache struct {
	mu    sync.Mutex
	until time.Time
	plans map[string]UsageQuotaPlan
}

type quotaObservation struct {
	account, generation, at, reset int64
	plan                           string
	used                           float64
}

type quotaSample struct {
	dollarsPerPoint float64
	points          float64
}

func (db *DB) ensureCodexQuotaSchema(ctx context.Context) error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS codex_weekly_quota_observations (
			account_id BIGINT NOT NULL, generation BIGINT NOT NULL, observed_at BIGINT NOT NULL,
			reset_at BIGINT NOT NULL, plan TEXT NOT NULL, used_percent DOUBLE PRECISION NOT NULL,
			PRIMARY KEY (account_id, observed_at)
		)`,
		`CREATE INDEX IF NOT EXISTS codex_weekly_quota_observed ON codex_weekly_quota_observations(observed_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) pruneCodexQuotaObservations(ctx context.Context) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `DELETE FROM codex_weekly_quota_observations WHERE observed_at < $1`,
			time.Now().AddDate(0, 0, -quotaCalibrationDays).Unix())
		return err
	})
}

// EnqueueCodexWeeklyQuota keeps writes off the response path.
func (db *DB) EnqueueCodexWeeklyQuota(accountID, generation int64, plan string, used float64, reset, at time.Time) {
	plan = strings.ToLower(strings.TrimSpace(plan))
	if plan == "" || generation <= 0 || !reset.After(at) || used < 0 || used > 100 || math.IsNaN(used) {
		return
	}
	// Mark windows with incomplete usage logging so calibration can't bridge
	// them if full logging is restored within the same quota cycle.
	if db.GetUsageLogMode() != UsageLogModeFull {
		plan = ""
	}
	item := quotaObservation{
		account: accountID, generation: generation, at: at.Unix(),
		reset: reset.Unix(), plan: plan, used: used,
	}
	select {
	case db.quotaObservationsQueue <- item:
	default:
	}
}

func (db *DB) startCodexQuotaRecorder() {
	db.quotaObservationsQueue = make(chan quotaObservation, 256)
	db.logWg.Add(1)
	go func() {
		defer db.logWg.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		prune := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.pruneCodexQuotaObservations(ctx); err != nil {
				log.Printf("清理周额度观测失败: %v", err)
			}
		}
		prune()
		for {
			select {
			case <-db.logStop:
				return
			case <-ticker.C:
				prune()
			case item := <-db.quotaObservationsQueue:
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				err := db.recordCodexWeeklyQuota(ctx, item)
				cancel()
				if err != nil {
					log.Printf("保存周额度观测失败: %v", err)
				}
			}
		}
	}()
}

// Keep one unchanged reading a minute as well as every percentage change.
func (db *DB) recordCodexWeeklyQuota(ctx context.Context, item quotaObservation) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO codex_weekly_quota_observations (account_id, generation, observed_at, reset_at, plan, used_percent)
			SELECT CAST($1 AS BIGINT), CAST($2 AS BIGINT), CAST($3 AS BIGINT), CAST($4 AS BIGINT), CAST($5 AS TEXT), CAST($6 AS DOUBLE PRECISION)
			WHERE NOT EXISTS (
				SELECT 1 FROM codex_weekly_quota_observations
				WHERE account_id = $1 AND generation = $2 AND observed_at >= $7 AND observed_at <= $3
				AND plan = $5 AND used_percent = $6 AND reset_at BETWEEN $4 - 2 AND $4 + 2
			)
			ON CONFLICT (account_id, observed_at) DO NOTHING`,
			item.account, item.generation, item.at, item.reset, item.plan, item.used, item.at-item.at%60)
		return err
	})
}

func (db *DB) quotaObservations(ctx context.Context, start, end time.Time) ([]quotaObservation, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT account_id, generation, observed_at, reset_at, plan, used_percent
		FROM codex_weekly_quota_observations WHERE observed_at >= $1 AND observed_at < $2
		ORDER BY account_id, observed_at`, start.Unix(), end.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []quotaObservation
	for rows.Next() {
		var item quotaObservation
		if err := rows.Scan(&item.account, &item.generation, &item.at, &item.reset, &item.plan, &item.used); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func sameQuotaCycle(a, b quotaObservation) bool {
	return a.account == b.account && a.generation == b.generation && a.plan != "" && a.plan == b.plan &&
		b.at < a.reset && a.reset >= b.reset-2 && a.reset <= b.reset+2 && b.used >= a.used
}

func (db *DB) quotaSampleCost(ctx context.Context, a, b quotaObservation, checkpoints []quotaObservation) (float64, error) {
	start, end := time.Unix(a.at, 0), time.Unix(b.at, 0)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, COALESCE(duration_ms, 0), COALESCE(account_billed, 0),
		       COALESCE(quota_plan, ''), COALESCE(total_tokens, 0), COALESCE(status_code, 0)
		FROM usage_logs
		WHERE account_id = $1 AND credential_generation = $2 AND created_at >= $3 AND created_at < $4
		  AND channel = 'codex'
		ORDER BY created_at`, a.account, a.generation, db.timeArg(start), db.timeArg(end))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var cost float64
	last := start
	for rows.Next() {
		var raw any
		var duration, tokens, status int64
		var dollars float64
		var plan string
		if err := rows.Scan(&raw, &duration, &dollars, &plan, &tokens, &status); err != nil {
			return 0, err
		}
		at, err := parseDBTimeValue(raw)
		if err != nil {
			return 0, err
		}
		if plan != a.plan || (tokens > 0 && dollars <= 0) {
			return 0, nil
		}
		if dollars <= 0 {
			continue
		}
		// A large drop during idle time is poor evidence about gateway cost.
		// Avoid fitting on incomplete requests or estimated failed-stream usage.
		if status < 200 || status >= 300 {
			return 0, nil
		}
		requestStart := at.Add(-time.Duration(duration) * time.Millisecond)
		if requestStart.Sub(last) > quotaMaxIdleGap &&
			!quotaIdleGapConfirmed(checkpoints, a.used, last, requestStart, at) {
			return 0, nil
		}
		cost += dollars
		last = at
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if end.Sub(last) > quotaMaxIdleGap {
		return 0, nil
	}
	return cost, nil
}

// An idle gap does not invalidate earlier gateway usage if a reading near
// the next request still shows the starting percentage. A reading during
// that request can also bracket the gap, before the request is logged.
func quotaIdleGapConfirmed(checkpoints []quotaObservation, used float64, last, requestStart, completed time.Time) bool {
	for _, item := range checkpoints {
		if item.at >= completed.Unix() {
			break
		}
		if item.used == used && item.at > last.Unix() &&
			requestStart.Sub(time.Unix(item.at, 0)) <= quotaMaxIdleGap {
			return true
		}
	}
	return false
}

func fitQuotaPlan(plan string, samples []quotaSample) UsageQuotaPlan {
	result := UsageQuotaPlan{Plan: plan, Samples: len(samples), Models: []UsageModelStat{}}
	if len(samples) < quotaMinimumSamples {
		return result
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].dollarsPerPoint < samples[j].dollarsPerPoint })
	median := samples[len(samples)/2].dollarsPerPoint
	var dollars, weights float64
	var used int
	for _, sample := range samples {
		if sample.dollarsPerPoint < median*0.5 || sample.dollarsPerPoint > median*2 {
			continue
		}
		// Multi-point changes reduce rounding error; cap their influence.
		weight := math.Min(sample.points, 5)
		dollars += sample.dollarsPerPoint * weight
		weights += weight
		used++
	}
	if used >= quotaMinimumSamples {
		result.USDPerPercent = dollars / weights
	}
	return result
}

func (db *DB) codexQuotaCalibrations(ctx context.Context, now time.Time) (map[string]UsageQuotaPlan, error) {
	cache := &db.quotaCalibration
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if now.Before(cache.until) {
		return cache.plans, nil
	}
	// Wait for the request-log batch to land before using recent observations.
	settling := max(30*time.Second, 2*time.Duration(db.GetUsageLogFlushIntervalSeconds())*time.Second)
	observations, err := db.quotaObservations(ctx, now.AddDate(0, 0, -quotaCalibrationDays), now.Add(-settling))
	if err != nil {
		return nil, err
	}
	samples := make(map[string][]quotaSample)
	anchorIndex := 0
	for index := 1; index < len(observations); index++ {
		item, anchor := observations[index], observations[anchorIndex]
		if !sameQuotaCycle(observations[index-1], item) || item.at-anchor.at > int64(quotaMaxSampleSpan/time.Second) {
			anchorIndex = index
			continue
		}
		delta := item.used - anchor.used
		if delta < 1 {
			continue
		}
		cost, err := db.quotaSampleCost(ctx, anchor, item, observations[anchorIndex+1:index])
		if err != nil {
			return nil, err
		}
		if cost > 0 && !math.IsInf(cost, 0) && !math.IsNaN(cost) && item.used < 100 {
			samples[item.plan] = append(samples[item.plan], quotaSample{dollarsPerPoint: cost / delta, points: delta})
		}
		anchorIndex = index
	}
	plans := make(map[string]UsageQuotaPlan, len(samples))
	for plan, planSamples := range samples {
		plans[plan] = fitQuotaPlan(plan, planSamples)
	}
	cache.plans, cache.until = plans, now.Add(time.Minute)
	return plans, nil
}

// Quota percentages follow the same date and dimension filters as the model
// panel. The fit itself always uses all observed gateway traffic for a plan.
func (db *DB) GetCodexQuotaModelStats(ctx context.Context, start, end time.Time, dim UsageLogFilter) ([]UsageQuotaPlan, error) {
	now := time.Now()
	if start.IsZero() {
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	plans, err := db.codexQuotaCalibrations(ctx, now)
	if err != nil {
		return nil, err
	}
	planExpr := "credentials->>'plan_type'"
	if db.isSQLite() {
		planExpr = "json_extract(NULLIF(credentials, ''), '$.plan_type')"
	}
	query := `SELECT DISTINCT LOWER(TRIM(COALESCE(` + planExpr + `, ''))) FROM accounts
		WHERE platform = 'openai' AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	var args []interface{}
	if dim.AccountID != nil {
		query += " AND id = $1"
		args = append(args, *dim.AccountID)
	}
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := make(map[string]UsageQuotaPlan)
	for rows.Next() {
		var plan string
		if err := rows.Scan(&plan); err != nil {
			rows.Close()
			return nil, err
		}
		if plan != "" {
			value := plans[plan]
			value.Plan = plan
			value.Models = []UsageModelStat{}
			result[plan] = value
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hasRate := false
	for _, plan := range plans {
		if plan.USDPerPercent > 0 {
			hasRate = true
			break
		}
	}
	if hasRate {
		where, args := db.usageSpanWhere("u.created_at", usageTimeSpan{start, end}, nil)
		where, args = usageStatsFilterWhere(where+` AND u.quota_plan <> '' AND u.status_code <> 499
			AND TRIM(COALESCE(u.internal_reason, '')) = ''`, args, UpstreamChannelCodex, dim)
		rows, err := db.conn.QueryContext(ctx, `
			SELECT u.quota_plan, COALESCE(NULLIF(u.effective_model, ''), NULLIF(u.model, ''), 'unknown'),
			       COALESCE(SUM(u.account_billed), 0), COUNT(*), COALESCE(SUM(u.total_tokens), 0),
			       COALESCE(SUM(CASE WHEN u.status_code >= 400 THEN 1 ELSE 0 END), 0)
			FROM usage_logs u WHERE `+where+` GROUP BY 1, 2`, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var plan string
			var item UsageModelStat
			if err := rows.Scan(&plan, &item.Model, &item.AccountBilled, &item.Requests, &item.Tokens, &item.ErrorCount); err != nil {
				return nil, err
			}
			group, ok := result[plan]
			if !ok {
				group = plans[plan]
				group.Plan = plan
				group.Models = []UsageModelStat{}
			}
			if group.USDPerPercent > 0 && item.AccountBilled > 0 {
				item.QuotaPercent = item.AccountBilled / group.USDPerPercent
				group.Models = append(group.Models, item)
			}
			result[plan] = group
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]UsageQuotaPlan, 0, len(result))
	for _, group := range result {
		sort.Slice(group.Models, func(i, j int) bool {
			a, b := group.Models[i], group.Models[j]
			if a.QuotaPercent != b.QuotaPercent {
				return a.QuotaPercent > b.QuotaPercent
			}
			return a.Model < b.Model
		})
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Plan < out[j].Plan })
	return out, nil
}
