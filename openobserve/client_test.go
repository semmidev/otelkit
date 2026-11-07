package openobserve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := New(Config{
		BaseURL:  srv.URL,
		Org:      "default",
		Username: "user",
		Password: "pass",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func TestNew(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		wantErr  string
		wantOrg  string
		wantAuth string
	}{
		{
			name:     "given basic auth when creating a client then credentials are encoded",
			cfg:      Config{BaseURL: "http://localhost:5080", Username: "user", Password: "pass"},
			wantOrg:  "default",
			wantAuth: "Basic dXNlcjpwYXNz",
		},
		{
			name:     "given a bearer token when creating a client then the token is used",
			cfg:      Config{BaseURL: "http://localhost:5080", Org: "prod", Token: "abc"},
			wantOrg:  "prod",
			wantAuth: "Bearer abc",
		},
		{
			name:    "given no base URL when creating a client then it fails",
			cfg:     Config{},
			wantErr: "BaseURL is required",
		},
		{
			name:    "given a base URL without a scheme when creating a client then it fails",
			cfg:     Config{BaseURL: "localhost:5080"},
			wantErr: "http or https",
		},
		{
			name:    "given a base URL without a host when creating a client then it fails",
			cfg:     Config{BaseURL: "http:///api"},
			wantErr: "include a host",
		},
		{
			name:    "given both token and basic auth when creating a client then it fails",
			cfg:     Config{BaseURL: "http://localhost:5080", Token: "abc", Username: "user"},
			wantErr: "not both",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("New() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if c.org != tt.wantOrg {
				t.Errorf("org = %q, want %q", c.org, tt.wantOrg)
			}
			if c.auth != tt.wantAuth {
				t.Errorf("auth = %q, want %q", c.auth, tt.wantAuth)
			}
		})
	}
}

func TestEndpoint(t *testing.T) {
	t.Run("given a base URL with a path when building an endpoint then the path is preserved", func(t *testing.T) {
		c, err := New(Config{BaseURL: "http://localhost:5080/prefix", Org: "default"})
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if got, want := c.endpoint("_search"), "http://localhost:5080/prefix/api/default/_search"; got != want {
			t.Fatalf("endpoint() = %q, want %q", got, want)
		}
	})
}

func TestSearch(t *testing.T) {
	start := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/default/_search" {
			t.Errorf("path = %s, want /api/default/_search", r.URL.Path)
		}
		if got := r.URL.Query().Get("type"); got != "traces" {
			t.Errorf("type = %q, want traces", got)
		}
		if got := r.Header.Get("Authorization"); got != "Basic dXNlcjpwYXNz" {
			t.Errorf("Authorization = %q, want basic auth", got)
		}

		var body struct {
			Query struct {
				SQL   string `json:"sql"`
				Start int64  `json:"start_time"`
				End   int64  `json:"end_time"`
				From  int    `json:"from"`
				Size  int    `json:"size"`
			} `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if body.Query.SQL != "SELECT * FROM default" {
			t.Errorf("sql = %q", body.Query.SQL)
		}
		if body.Query.Start != start.UnixMicro() || body.Query.End != end.UnixMicro() {
			t.Errorf("range = [%d,%d], want [%d,%d]", body.Query.Start, body.Query.End, start.UnixMicro(), end.UnixMicro())
		}
		if body.Query.From != 10 || body.Query.Size != 50 {
			t.Errorf("from/size = %d/%d, want 10/50", body.Query.From, body.Query.Size)
		}

		json.NewEncoder(w).Encode(map[string]any{
			"hits":  []map[string]any{{"message": "ok"}},
			"total": 1,
			"took":  2,
		})
	})

	res, err := c.Search(context.Background(), "SELECT * FROM default", SearchOptions{
		Start: start,
		End:   end,
		Type:  StreamTraces,
		From:  10,
		Size:  50,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if res.Total != 1 || len(res.Hits) != 1 || res.Hits[0]["message"] != "ok" {
		t.Fatalf("Search() = %+v", res)
	}
}

func TestSearchDefaultsToLastHour(t *testing.T) {
	before := time.Now()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query struct {
				Start int64 `json:"start_time"`
				End   int64 `json:"end_time"`
			} `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		start := time.UnixMicro(body.Query.Start)
		end := time.UnixMicro(body.Query.End)
		if got := end.Sub(start); got != time.Hour {
			t.Errorf("range = %v, want 1h", got)
		}
		if end.Before(before) || end.After(time.Now().Add(5*time.Second)) {
			t.Errorf("end = %v, want around now", end)
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": []any{}, "total": 0})
	})

	if _, err := c.Search(context.Background(), "SELECT * FROM default", SearchOptions{}); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
}

func TestSearchValidation(t *testing.T) {
	c, err := New(Config{BaseURL: "http://localhost:5080"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name    string
		sql     string
		opts    SearchOptions
		wantErr string
	}{
		{
			name:    "given an empty query when searching then it fails",
			sql:     " ",
			wantErr: "sql is required",
		},
		{
			name:    "given an invalid stream type when searching then it fails",
			sql:     "SELECT 1",
			opts:    SearchOptions{Type: "events"},
			wantErr: "invalid stream type",
		},
		{
			name:    "given a reversed time range when searching then it fails",
			sql:     "SELECT 1",
			opts:    SearchOptions{Start: time.Now(), End: time.Now().Add(-time.Hour)},
			wantErr: "Start must be before End",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Search(context.Background(), tt.sql, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Search() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSearchAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "message": "invalid time range"})
	})

	_, err := c.Search(context.Background(), "SELECT 1", SearchOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Search() error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != 400 {
		t.Errorf("APIError = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "invalid time range") {
		t.Errorf("APIError.Error() = %q", apiErr.Error())
	}
}

func TestIngestJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/default/go_app_logs/_json" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var rows []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&rows); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if len(rows) != 2 || rows[0]["message"] != "hello" {
			t.Errorf("rows = %+v", rows)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code":   200,
			"status": []map[string]any{{"name": "go_app_logs", "successful": 2, "failed": 0}},
		})
	})

	res, err := c.IngestJSON(context.Background(), "go_app_logs", []map[string]any{
		{"message": "hello"},
		{"message": "world"},
	})
	if err != nil {
		t.Fatalf("IngestJSON() error = %v", err)
	}
	if res.Code != 200 || len(res.Status) != 1 || res.Status[0].Successful != 2 {
		t.Fatalf("IngestJSON() = %+v", res)
	}
}

func TestIngestBulk(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/default/_bulk" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		if len(lines) != 4 {
			t.Errorf("bulk lines = %d, want 4", len(lines))
			return
		}
		if !strings.Contains(lines[0], `"_index":"go_app_logs"`) {
			t.Errorf("metadata line = %s", lines[0])
		}
		if !strings.Contains(lines[1], `"message":"hello"`) {
			t.Errorf("document line = %s", lines[1])
		}
		json.NewEncoder(w).Encode(map[string]any{"took": 1, "errors": false, "items": []any{}})
	})

	res, err := c.IngestBulk(context.Background(), "go_app_logs", []map[string]any{
		{"message": "hello"},
		{"message": "world"},
	})
	if err != nil {
		t.Fatalf("IngestBulk() error = %v", err)
	}
	if res.Errors || res.Took != 1 {
		t.Fatalf("IngestBulk() = %+v", res)
	}
}

func TestIngestBulkValidation(t *testing.T) {
	c, err := New(Config{BaseURL: "http://localhost:5080"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := c.IngestBulk(context.Background(), "stream", nil); err == nil {
		t.Fatal("IngestBulk() error = nil, want empty docs error")
	}
}
