// Package openobserve is a small client for the OpenObserve APIs that are not
// covered by OTLP: SQL search and direct JSON/bulk ingestion.
package openobserve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxErrorBody = 1 << 20

// Config configures a Client.
type Config struct {
	BaseURL string // example: http://localhost:5080
	Org     string // defaults to "default"

	Username string
	Password string
	Token    string // bearer token; mutually exclusive with Username/Password

	HTTPClient *http.Client
	Timeout    time.Duration // applies when HTTPClient is nil
}

// Client calls the OpenObserve HTTP API.
type Client struct {
	baseURL *url.URL
	org     string
	auth    string
	http    *http.Client
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("openobserve: BaseURL is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("openobserve: parse BaseURL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("openobserve: BaseURL must use http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("openobserve: BaseURL must include a host")
	}
	if cfg.Token != "" && cfg.Username != "" {
		return nil, errors.New("openobserve: set Token or Username/Password, not both")
	}
	if cfg.Org == "" {
		cfg.Org = "default"
	}

	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}

	var auth string
	switch {
	case cfg.Token != "":
		auth = "Bearer " + cfg.Token
	case cfg.Username != "":
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.Username+":"+cfg.Password))
	}

	return &Client{baseURL: u, org: cfg.Org, auth: auth, http: hc}, nil
}

// StreamType selects the OpenObserve stream type for Search.
type StreamType string

// Stream types accepted by Search.
const (
	StreamLogs    StreamType = "logs"
	StreamMetrics StreamType = "metrics"
	StreamTraces  StreamType = "traces"
)

func (t StreamType) valid() bool {
	switch t {
	case "", StreamLogs, StreamMetrics, StreamTraces:
		return true
	default:
		return false
	}
}

// SearchOptions controls a SQL search. Zero Start/End default to the last hour.
type SearchOptions struct {
	Start time.Time
	End   time.Time
	Type  StreamType
	From  int
	Size  int
}

// SearchResult is the subset of the OpenObserve response callers usually need.
type SearchResult struct {
	Hits  []map[string]any `json:"hits"`
	Total int64            `json:"total"`
	Took  int              `json:"took"`
}

// Search runs SQL against OpenObserve.
func (c *Client) Search(ctx context.Context, sql string, opts SearchOptions) (*SearchResult, error) {
	if strings.TrimSpace(sql) == "" {
		return nil, errors.New("openobserve: sql is required")
	}
	if !opts.Type.valid() {
		return nil, fmt.Errorf("openobserve: invalid stream type %q", opts.Type)
	}

	end := opts.End
	if end.IsZero() {
		end = time.Now()
	}
	start := opts.Start
	if start.IsZero() {
		start = end.Add(-time.Hour)
	}
	if !start.Before(end) {
		return nil, errors.New("openobserve: Start must be before End")
	}

	query := map[string]any{
		"sql":        sql,
		"start_time": start.UnixMicro(),
		"end_time":   end.UnixMicro(),
	}
	if opts.From > 0 {
		query["from"] = opts.From
	}
	if opts.Size > 0 {
		query["size"] = opts.Size
	}
	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return nil, fmt.Errorf("openobserve: encode search request: %w", err)
	}

	endpoint, err := url.Parse(c.endpoint("_search"))
	if err != nil {
		return nil, fmt.Errorf("openobserve: build search URL: %w", err)
	}
	if opts.Type != "" {
		q := endpoint.Query()
		q.Set("type", string(opts.Type))
		endpoint.RawQuery = q.Encode()
	}

	resp, err := c.do(ctx, http.MethodPost, endpoint.String(), body, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openobserve: decode search response: %w", err)
	}
	return &out, nil
}

// IngestStatus describes the result for one stream in an ingest response.
type IngestStatus struct {
	Name       string `json:"name"`
	Successful int    `json:"successful"`
	Failed     int    `json:"failed"`
}

// IngestResult is the OpenObserve _json ingest response.
type IngestResult struct {
	Code   int            `json:"code"`
	Status []IngestStatus `json:"status"`
}

// IngestJSON writes rows to a stream using the _json endpoint.
func (c *Client) IngestJSON(ctx context.Context, stream string, rows any) (*IngestResult, error) {
	if strings.TrimSpace(stream) == "" {
		return nil, errors.New("openobserve: stream is required")
	}
	body, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("openobserve: encode ingest request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, c.endpoint(stream, "_json"), body, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out IngestResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openobserve: decode ingest response: %w", err)
	}
	return &out, nil
}

// BulkResult is the OpenObserve _bulk ingest response.
type BulkResult struct {
	Took   int               `json:"took"`
	Errors bool              `json:"errors"`
	Items  []json.RawMessage `json:"items"`
}

// IngestBulk writes documents to a stream using the Elasticsearch-compatible
// _bulk endpoint.
func (c *Client) IngestBulk(ctx context.Context, stream string, docs []map[string]any) (*BulkResult, error) {
	if strings.TrimSpace(stream) == "" {
		return nil, errors.New("openobserve: stream is required")
	}
	if len(docs) == 0 {
		return nil, errors.New("openobserve: docs must not be empty")
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, doc := range docs {
		meta := map[string]any{"index": map[string]any{"_index": stream}}
		if err := enc.Encode(meta); err != nil {
			return nil, fmt.Errorf("openobserve: encode bulk metadata: %w", err)
		}
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("openobserve: encode bulk document: %w", err)
		}
	}

	resp, err := c.do(ctx, http.MethodPost, c.endpoint("_bulk"), buf.Bytes(), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out BulkResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openobserve: decode bulk response: %w", err)
	}
	return &out, nil
}

// APIError is returned for non-2xx OpenObserve responses.
type APIError struct {
	StatusCode int
	Code       int
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("openobserve: HTTP %d (code %d): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("openobserve: HTTP %d: %s", e.StatusCode, e.Message)
}

func newAPIError(status int, body []byte) error {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	message := payload.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Code: payload.Code, Message: message}
}

func (c *Client) endpoint(parts ...string) string {
	u := *c.baseURL
	path := strings.TrimRight(u.Path, "/") + "/api/" + url.PathEscape(c.org)
	for _, part := range parts {
		path += "/" + url.PathEscape(part)
	}
	u.Path = path
	return u.String()
}

func (c *Client) do(ctx context.Context, method, endpoint string, body []byte, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openobserve: create request: %w", err)
	}
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openobserve: request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, newAPIError(resp.StatusCode, errBody)
	}
	return resp, nil
}
