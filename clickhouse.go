package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	openclickhouse "github.com/TheGrimmChester/open-clickhouse-go"
	openlogger "github.com/TheGrimmChester/open-logger-go"
)

// maxInFlightInserts bounds concurrent async ClickHouse INSERTs so a stalled
// ClickHouse cannot spawn unlimited goroutines and OOM the agent.
const maxInFlightInserts = 8

// ClickHouseWriter handles batch writes to ClickHouse
type ClickHouseWriter struct {
	url       string
	database  string // product DB (ora | osa | opl | opa)
	bufMu     sync.Mutex
	buf       bytes.Buffer
	fullBuf   bytes.Buffer
	count     int
	fullCount int
	batchSize int
	client    *http.Client
	// sem is a counting semaphore (token bucket) capping the number of
	// in-flight async INSERT goroutines. Callers block once it is full,
	// providing backpressure instead of unbounded goroutine growth.
	sem chan struct{}
}

// NewClickHouseWriter creates a new ClickHouse writer (legacy default DB "opa").
func NewClickHouseWriter(url string, batchSize int) *ClickHouseWriter {
	return NewClickHouseWriterDB(url, clickHouseDatabase(), batchSize)
}

// NewClickHouseWriterDB creates a writer targeting a specific database.
func NewClickHouseWriterDB(url, database string, batchSize int) *ClickHouseWriter {
	if database == "" {
		database = "opa"
	}
	return &ClickHouseWriter{
		url:       url,
		database:  database,
		batchSize: batchSize,
		sem:       make(chan struct{}, maxInFlightInserts),
		client: &http.Client{
			Timeout: 10 * time.Second,
			// Raise idle-connection pooling so drained keep-alive
			// connections are actually reused across the concurrent
			// INSERT fan-out (default MaxIdleConnsPerHost is only 2).
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 32,
				MaxConnsPerHost:     32,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// insert POSTs data as a single INSERT into <database>.<table>, with bounded retries
// and exponential backoff. It always fully drains and closes the response body
// so the underlying keep-alive connection can be reused. It returns true only
// on a confirmed HTTP 200; on final failure it logs loudly (never silent).
func (w *ClickHouseWriter) insert(table string, data []byte) bool {
	if len(data) == 0 {
		return true
	}
	db := w.database
	if db == "" {
		db = "opa"
	}
	endpoint := strings.TrimRight(w.url, "/") + "/?query=INSERT%20INTO%20" + db + "." + table + "%20FORMAT%20JSONEachRow"
	backoff := 100 * time.Millisecond
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader(data))
		if err != nil {
			openlogger.LogError(err, "Failed to build ClickHouse INSERT request", map[string]interface{}{
				"table": table,
			})
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		if err != nil {
			openlogger.LogError(err, "ClickHouse INSERT request failed", map[string]interface{}{
				"table":   table,
				"attempt": attempt,
			})
		} else if resp.StatusCode != 200 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			openlogger.LogError(nil, "ClickHouse INSERT returned non-200", map[string]interface{}{
				"table":       table,
				"status_code": resp.StatusCode,
				"body":        string(body),
				"attempt":     attempt,
			})
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return true
		}
		if attempt < attempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	// All retries exhausted: the batch is dropped, but loudly (not silently).
	openlogger.LogError(nil, "ClickHouse INSERT failed after retries; dropping batch", map[string]interface{}{
		"table": table,
		"bytes": len(data),
	})
	return false
}

// insertAsync runs insert() in a goroutine, but only after acquiring a
// semaphore token so no more than maxInFlightInserts run concurrently. The
// caller blocks on a full semaphore (backpressure). Empty payloads are no-ops.
func (w *ClickHouseWriter) insertAsync(table string, data []byte) {
	if len(data) == 0 {
		return
	}
	w.sem <- struct{}{}
	go func() {
		defer func() { <-w.sem }()
		w.insert(table, data)
	}()
}

// Add adds a row to the batch buffer
func (w *ClickHouseWriter) Add(minRow map[string]interface{}) {
	w.bufMu.Lock()
	j, _ := json.Marshal(minRow)
	w.buf.Write(j)
	w.buf.WriteString("\n")
	w.count++
	// Swap-under-lock: when the batch is full, snapshot the buffer into a
	// detached copy and reset it while still holding bufMu, then POST the
	// copy outside the lock. This avoids the data race of touching buf/count
	// from an unlocked goroutine.
	var dataCopy []byte
	if w.count >= w.batchSize {
		dataCopy = make([]byte, w.buf.Len())
		copy(dataCopy, w.buf.Bytes())
		w.buf.Reset()
		w.count = 0
	}
	w.bufMu.Unlock()

	w.insertAsync("spans_min", dataCopy)
}

// AddFull adds a full row (with detailed data) to the buffer. Like spans_min,
// spans_full is now batched: rows accumulate until batchSize (or the periodic
// Flush) is reached, then a single INSERT is issued. This avoids one tiny
// INSERT (and one ClickHouse part) per span, which triggers "too many parts".
func (w *ClickHouseWriter) AddFull(fullRow map[string]interface{}) {
	w.bufMu.Lock()
	j, _ := json.Marshal(fullRow)
	w.fullBuf.Write(j)
	w.fullBuf.WriteString("\n")
	w.fullCount++
	var dataCopy []byte
	if w.fullCount >= w.batchSize {
		dataCopy = make([]byte, w.fullBuf.Len())
		copy(dataCopy, w.fullBuf.Bytes())
		w.fullBuf.Reset()
		w.fullCount = 0
	}
	w.bufMu.Unlock()

	w.insertAsync("spans_full", dataCopy)
}

// flushLocked snapshots and clears the spans_min and spans_full buffers,
// returning detached copies of the pending data. The caller MUST hold bufMu.
// The returned copies are POSTed by the caller AFTER releasing the lock so the
// blocking HTTP request never runs while bufMu is held.
func (w *ClickHouseWriter) flushLocked() (minData, fullData []byte) {
	if w.buf.Len() > 0 {
		minData = make([]byte, w.buf.Len())
		copy(minData, w.buf.Bytes())
		w.buf.Reset()
		w.count = 0
	}
	if w.fullBuf.Len() > 0 {
		fullData = make([]byte, w.fullBuf.Len())
		copy(fullData, w.fullBuf.Bytes())
		w.fullBuf.Reset()
		w.fullCount = 0
	}
	return minData, fullData
}

// AddRUM adds RUM event to ClickHouse
func (w *ClickHouseWriter) AddRUM(rumEvent map[string]interface{}) {
	rumJSON, _ := json.Marshal(rumEvent)
	rumData := append(rumJSON, '\n')

	w.insertAsync("rum_events", rumData)
}

// AddError adds error instance and group to ClickHouse
func (w *ClickHouseWriter) AddError(errorInstance, errorGroup map[string]interface{}) {
	// Write error instance
	instanceJSON, _ := json.Marshal(errorInstance)
	instanceData := append(instanceJSON, '\n')

	// Write error group
	groupJSON, _ := json.Marshal(errorGroup)
	groupData := append(groupJSON, '\n')

	// Write both through the bounded async insert path.
	w.insertAsync("error_instances", instanceData)
	w.insertAsync("error_groups", groupData)
}

// AddLog adds a log entry to ClickHouse
func (w *ClickHouseWriter) AddLog(logEntry map[string]interface{}) {
	// Convert timestamp to DateTime64(3) format if it's a string
	if timestampStr, ok := logEntry["timestamp"].(string); ok {
		// Parse the timestamp string and reformat for ClickHouse DateTime64(3)
		if t, err := time.Parse("2006-01-02 15:04:05.000", timestampStr); err == nil {
			logEntry["timestamp"] = t.Format("2006-01-02 15:04:05.000")
		} else if t, err := time.Parse("2006-01-02 15:04:05", timestampStr); err == nil {
			logEntry["timestamp"] = t.Format("2006-01-02 15:04:05.000")
		} else {
			// If parsing fails, use current time
			logEntry["timestamp"] = time.Now().Format("2006-01-02 15:04:05.000")
		}
	} else {
		// If timestamp is missing, use current time
		logEntry["timestamp"] = time.Now().Format("2006-01-02 15:04:05.000")
	}

	// Ensure span_id is properly handled (can be empty string for NULL)
	if spanID, ok := logEntry["span_id"].(string); ok && spanID == "" {
		logEntry["span_id"] = nil
	}

	logJSON, _ := json.Marshal(logEntry)
	logData := append(logJSON, '\n')

	// Write through the bounded async insert path (retries + drained body).
	w.insertAsync("logs", logData)
}

// Flush flushes all buffered data to ClickHouse. The buffers are snapshotted
// and cleared under bufMu, then POSTed outside the lock via the bounded async
// insert path (no blocking HTTP while holding bufMu).
func (w *ClickHouseWriter) Flush() {
	w.bufMu.Lock()
	minData, fullData := w.flushLocked()
	w.bufMu.Unlock()

	w.insertAsync("spans_min", minData)
	w.insertAsync("spans_full", fullData)
}

// ClickHouseQuery handles queries to ClickHouse via Open-ClickHouse-Go.
// SkipLegacyRewrite: product SQL uses chTable(); hub SQL uses hubTable (opa.*).
type ClickHouseQuery struct {
	ch       *openclickhouse.Client
	database string
}

// NewClickHouseQuery creates a new ClickHouse query client (product DB from env).
func NewClickHouseQuery(url string) *ClickHouseQuery {
	return NewClickHouseQueryDB(url, clickHouseDatabase())
}

// NewClickHouseQueryDB creates a query client targeting a specific database.
func NewClickHouseQueryDB(url, database string) *ClickHouseQuery {
	if database == "" {
		database = "opa"
	}
	cfg := openclickhouse.Config{
		URL:               url,
		Database:          database,
		Username:          os.Getenv("CLICKHOUSE_USER"),
		Password:          os.Getenv("CLICKHOUSE_PASSWORD"),
		SkipLegacyRewrite: true,
	}
	return &ClickHouseQuery{
		ch:       openclickhouse.New(cfg),
		database: database,
	}
}

// Client exposes the underlying Open-ClickHouse-Go client.
func (q *ClickHouseQuery) Client() *openclickhouse.Client {
	if q == nil {
		return nil
	}
	return q.ch
}

// Query executes a query and returns results (no opa.* rewrite).
func (q *ClickHouseQuery) Query(query string) ([]map[string]interface{}, error) {
	if q == nil || q.ch == nil {
		return nil, fmt.Errorf("clickhouse query client not configured")
	}
	rows, err := q.ch.Query(query)
	if err != nil {
		openlogger.LogError(err, "ClickHouse query failed", nil)
		return nil, err
	}
	out := make([]map[string]interface{}, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out, nil
}

// Execute executes a query without returning results.
func (q *ClickHouseQuery) Execute(query string) error {
	if q == nil || q.ch == nil {
		return fmt.Errorf("clickhouse query client not configured")
	}
	if err := q.ch.Exec(query); err != nil {
		openlogger.LogError(err, "ClickHouse Execute failed", nil)
		return err
	}
	return nil
}
