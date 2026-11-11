package database

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Conversation stores application payloads only, never authentication headers.
type Conversation struct {
	RequestID    string `json:"request_id"`
	Endpoint     string `json:"endpoint"`
	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
	ContentType  string `json:"content_type"`
	Truncated    bool   `json:"truncated"`
	CreatedAt    int64  `json:"created_at"`
}

func (db *DB) EnsureConversationSchema(ctx context.Context) error {
	binaryType := "BYTEA"
	if db.isSQLite() {
		binaryType = "BLOB"
	}
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS conversation_retention (id INTEGER PRIMARY KEY, retention_days INTEGER NOT NULL DEFAULT 7)`,
		`INSERT INTO conversation_retention (id, retention_days) VALUES (1, 7) ON CONFLICT DO NOTHING`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS request_conversations (request_id TEXT PRIMARY KEY, created_at BIGINT NOT NULL, payload %s NOT NULL)`, binaryType),
		`CREATE INDEX IF NOT EXISTS request_conversations_created ON request_conversations(created_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ConversationRetentionDays(ctx context.Context) (int, error) {
	var days int
	err := db.conn.QueryRowContext(ctx, `SELECT retention_days FROM conversation_retention WHERE id=1`).Scan(&days)
	return days, err
}

func (db *DB) SetConversationRetentionDays(ctx context.Context, days int) error {
	if days < 1 || days > 365 {
		return fmt.Errorf("保留天数必须在 1 到 365 之间")
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE conversation_retention SET retention_days=$1 WHERE id=1`, days)
	return err
}

func (db *DB) StoreConversation(ctx context.Context, value *Conversation) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO request_conversations (request_id,created_at,payload) VALUES ($1,$2,$3)
 ON CONFLICT(request_id) DO UPDATE SET payload=excluded.payload`, value.RequestID, value.CreatedAt, compressed.Bytes())
	return err
}

func (db *DB) ConversationForUsageLog(ctx context.Context, id int64) (*Conversation, error) {
	days, err := db.ConversationRetentionDays(ctx)
	if err != nil {
		return nil, err
	}
	var compressed []byte
	err = db.conn.QueryRowContext(ctx, `SELECT c.payload FROM request_conversations c
 JOIN usage_logs u ON u.request_id=c.request_id WHERE u.id=$1 AND c.created_at >= $2`, id, time.Now().Add(-time.Duration(days)*24*time.Hour).Unix()).Scan(&compressed)
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var value Conversation
	// Capture limits bound both payloads; the decoder also bounds corrupt input.
	err = json.NewDecoder(io.LimitReader(reader, 40<<20)).Decode(&value)
	return &value, err
}

func (db *DB) PurgeConversations(ctx context.Context) error {
	days, err := db.ConversationRetentionDays(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	for {
		result, err := db.conn.ExecContext(ctx, `DELETE FROM request_conversations WHERE request_id IN
   (SELECT request_id FROM request_conversations WHERE created_at < $1 LIMIT 200)`, cutoff)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n < 200 {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
