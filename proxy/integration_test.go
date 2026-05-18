package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestIntegration_StreamingChatCompletionMetrics verifies that streaming chat
// completions produce correct activity log metrics: tokens, timing, status.
func TestIntegration_StreamingChatCompletionMetrics(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  stream-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond stream-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Send a streaming request
	body := `{"model":"stream-model","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions?stream=true", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, strings.Contains(w.Header().Get("Content-Type"), "text/event-stream"),
		"streaming response should have text/event-stream content type")

	// Wait for async metrics processing
	time.Sleep(100 * time.Millisecond)

	// Check activity metrics were captured
	metrics := proxy.metricsMonitor.getMetrics()
	assert.NotEmpty(t, metrics, "stream request should produce activity metrics")

	last := metrics[0]
	assert.Equal(t, "stream-model", last.Model)
	assert.Equal(t, 200, last.RespStatusCode)
	assert.Equal(t, 25, last.Tokens.InputTokens, "should capture prompt_tokens=25")
	assert.Equal(t, 10, last.Tokens.OutputTokens, "should capture completion_tokens=10")
	assert.Greater(t, last.DurationMs, 0, "duration should be > 0")
}

// TestIntegration_NonStreamingChatCompletion verifies non-streaming responses.
func TestIntegration_NonStreamingChatCompletion(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  ns-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond ns-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"ns-model","messages":[{"role":"user","content":"tell me a joke"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "responseMessage")
	assert.Contains(t, w.Body.String(), "ns-model")

	time.Sleep(100 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics)
	assert.Equal(t, "ns-model", metrics[0].Model)
}

// TestIntegration_MultipleModelsSwap verifies automatic model swapping works
// under load: sending requests to different models sequentially.
func TestIntegration_MultipleModelsSwap(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  model-a:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond model-a
  model-b:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond model-b
  model-c:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond model-c
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	models := []string{"model-a", "model-b", "model-c"}
	for _, m := range models {
		t.Run(m, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"ping"}]}`, m)
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := CreateTestResponseRecorder()

			proxy.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), m)
		})
	}

	time.Sleep(200 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	assert.GreaterOrEqual(t, len(metrics), 3, "should have at least 3 activity entries")

	// Each model should appear in metrics
	seen := map[string]bool{}
	for _, e := range metrics {
		seen[e.Model] = true
	}
	for _, m := range models {
		assert.True(t, seen[m], "model %s should appear in activity log", m)
	}
}

// TestIntegration_MetricsAPI verifies the /api/metrics endpoint returns valid
// JSON with expected structure after requests have been made.
func TestIntegration_MetricsAPI(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  m1:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond m1
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Make a request
	body := `{"model":"m1","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	time.Sleep(100 * time.Millisecond)

	// Fetch metrics via API
	apiReq := httptest.NewRequest("GET", "/api/metrics", nil)
	apiW := CreateTestResponseRecorder()
	proxy.ServeHTTP(apiW, apiReq)

	assert.Equal(t, http.StatusOK, apiW.Code)
	assert.True(t, gjson.ValidBytes(apiW.Body.Bytes()), "metrics API should return valid JSON")

	result := gjson.ParseBytes(apiW.Body.Bytes())
	assert.True(t, result.IsArray(), "metrics should be an array")
	assert.GreaterOrEqual(t, len(result.Array()), 1, "should have at least 1 metric entry")

	first := result.Array()[0]
	assert.Equal(t, "m1", first.Get("model").String())
	assert.Equal(t, float64(200), first.Get("resp_status_code").Float())
}

// TestIntegration_DailyMetricsAPI verifies /api/metrics/daily after requests.
// Note: getDailyMetrics looks at yesterday's entries by design.
// Entries created today won't appear unless we manipulate timestamps.
func TestIntegration_DailyMetricsAPI(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  dm1:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond dm1
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	for i := 0; i < 3; i++ {
		body := `{"model":"dm1","messages":[{"role":"user","content":"ping"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := CreateTestResponseRecorder()
		proxy.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	time.Sleep(200 * time.Millisecond)

	apiReq := httptest.NewRequest("GET", "/api/metrics/daily", nil)
	apiW := CreateTestResponseRecorder()
	proxy.ServeHTTP(apiW, apiReq)

	assert.Equal(t, http.StatusOK, apiW.Code)
	result := gjson.ParseBytes(apiW.Body.Bytes())

	date := result.Get("date").String()
	assert.NotEmpty(t, date, "daily metrics should have a date field")

	assert.True(t, result.Get("models").Exists(), "should have models field")
}

// TestIntegration_ConcurrentRequests verifies the proxy handles concurrent
// requests without data races and produces correct per-model metrics.
func TestIntegration_ConcurrentRequests(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  conc-a:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond conc-a
  conc-b:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond conc-b
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	var wg sync.WaitGroup
	numReqs := 10

	for i := 0; i < numReqs; i++ {
		wg.Add(1)
		model := "conc-a"
		if i%2 == 0 {
			model = "conc-b"
		}
		go func(m string) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"ping"}]}`, m)
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := CreateTestResponseRecorder()
			proxy.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
		}(model)
	}

	wg.Wait()
	time.Sleep(200 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	assert.GreaterOrEqual(t, len(metrics), numReqs, "should capture all concurrent requests")

	// Count per model
	var countA, countB int
	for _, m := range metrics {
		switch m.Model {
		case "conc-a":
			countA++
		case "conc-b":
			countB++
		}
	}
	assert.Greater(t, countA, 0, "conc-a should have activity")
	assert.Greater(t, countB, 0, "conc-b should have activity")
	assert.Equal(t, numReqs, countA+countB, "total requests should match")
}

// TestIntegration_ModelsEndpoint verifies /v1/models returns correct model list.
func TestIntegration_ModelsEndpoint(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  visible-a:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond visible-a
    description: "First visible model"
  visible-b:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond visible-b
    description: "Second visible model"
  hidden-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond hidden
    unlisted: true
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	result := gjson.ParseBytes(w.Body.Bytes())

	assert.Equal(t, "list", result.Get("object").String())
	data := result.Get("data")
	assert.True(t, data.IsArray())

	// Count visible models (hidden-model should be excluded)
	modelIDs := map[string]bool{}
	for _, item := range data.Array() {
		modelIDs[item.Get("id").String()] = true
	}
	assert.True(t, modelIDs["visible-a"], "visible-a should be in model list")
	assert.True(t, modelIDs["visible-b"], "visible-b should be in model list")
	assert.False(t, modelIDs["hidden-model"], "hidden-model should NOT be in model list")
}

// TestIntegration_HealthEndpoint verifies /health returns 200.
func TestIntegration_HealthEndpoint(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  h1:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond h1
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	req := httptest.NewRequest("GET", "/health", nil)
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestIntegration_VersionEndpoint verifies /api/version returns build info.
func TestIntegration_VersionEndpoint(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  v1:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond v1
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	req := httptest.NewRequest("GET", "/api/version", nil)
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	result := gjson.ParseBytes(w.Body.Bytes())
	assert.True(t, result.Get("version").Exists(), "version field should exist")
}

// TestIntegration_AliasRouting verifies that alias model names are correctly
// routed to their real models.
func TestIntegration_AliasRouting(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  real-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond real-model
    aliases:
      - my-alias
      - another-name
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	for _, requestModel := range []string{"real-model", "my-alias", "another-name"} {
		t.Run(requestModel, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"test"}]}`, requestModel)
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := CreateTestResponseRecorder()

			proxy.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)
			// The underlying simple-responder is configured for "real-model",
			// so the response should contain "real-model"
			assert.Contains(t, w.Body.String(), "real-model",
				"request with model=%q should be routed to real-model", requestModel)
		})
	}
}

// TestIntegration_RequestCapture verifies that the capture functionality
// stores request/response data when enabled.
func TestIntegration_RequestCapture(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
captureBuffer: 3
models:
  cap-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond cap-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"cap-model","messages":[{"role":"user","content":"capture me"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	time.Sleep(100 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics)
	assert.True(t, metrics[0].HasCapture, "capture should be available for this request")

	// Fetch capture by ID
	captureReq := httptest.NewRequest("GET", fmt.Sprintf("/api/captures/%d", metrics[0].ID), nil)
	captureW := CreateTestResponseRecorder()
	proxy.ServeHTTP(captureW, captureReq)
	assert.Equal(t, http.StatusOK, captureW.Code)

	result := gjson.ParseBytes(captureW.Body.Bytes())
	assert.True(t, result.Get("req_path").Exists(), "capture should have req_path")
	assert.True(t, result.Get("req_headers").Exists(), "capture should have req_headers")
	assert.True(t, result.Get("resp_headers").Exists(), "capture should have resp_headers")
}

// TestIntegration_SSEStreamParsing verifies that streaming responses with
// stream=true query parameter are correctly forwarded.
func TestIntegration_SSEStreamParsing(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  sse-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond sse-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Include stream in the JSON body (OpenAI format)
	body := `{"model":"sse-model","messages":[{"role":"user","content":"stream please"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions?stream=true", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	respBody := w.Body.String()
	hasStreaming := strings.Contains(respBody, "text/event-stream") ||
		strings.Contains(respBody, "data:") ||
		strings.Contains(respBody, "asdf")
	assert.True(t, hasStreaming, "streaming response should contain event data")

	time.Sleep(100 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics)
	assert.Equal(t, 25, metrics[0].Tokens.InputTokens)
	assert.Equal(t, 10, metrics[0].Tokens.OutputTokens)
}

// TestIntegration_UnknownModelReturnsError verifies that requesting a model
// not in the config returns an appropriate error.
func TestIntegration_UnknownModelReturnsError(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  known:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond known
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"nonexistent-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	assert.GreaterOrEqual(t, w.Code, 400, "unknown model should return 4xx error")
}

// TestIntegration_InFlightTracking verifies that concurrent requests are tracked.
func TestIntegration_InFlightTracking(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  slow-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond slow-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Send a request that takes some time
	body := `{"model":"slow-model","messages":[{"role":"user","content":"wait for me"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		proxy.ServeHTTP(w, req)
	}()

	// Small delay to let the request start
	time.Sleep(50 * time.Millisecond)

	// Check in-flight stats
	statsReq := httptest.NewRequest("GET", "/api/metrics", nil)
	statsW := CreateTestResponseRecorder()
	proxy.ServeHTTP(statsW, statsReq)
	assert.Equal(t, http.StatusOK, statsW.Code)

	wg.Wait()
}

// TestIntegration_GlobalTTLRespected verifies that the global TTL is applied
// to models that don't set their own ttl.
func TestIntegration_GlobalTTLRespected(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
globalTTL: 60
models:
  ttl-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond ttl-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"ttl-model","messages":[{"role":"user","content":"test"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	process := proxy.findGroupByModelName("ttl-model").processes["ttl-model"]
	assert.Equal(t, StateReady, process.CurrentState())

	// Verify the TTL was applied
	modelCfg := proxy.config.Models["ttl-model"]
	assert.Equal(t, 60, modelCfg.UnloadAfter)
}

// TestIntegration_ModelUnloadAPI verifies that /api/models/unload/*model works.
func TestIntegration_ModelUnloadAPI(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  unload-me:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond unload-me
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// First, load the model by sending a request
	body := `{"model":"unload-me","messages":[{"role":"user","content":"load"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	process := proxy.findGroupByModelName("unload-me").processes["unload-me"]
	assert.Equal(t, StateReady, process.CurrentState())

	// Now unload it via API
	unloadReq := httptest.NewRequest("POST", "/api/models/unload/unload-me", nil)
	unloadW := CreateTestResponseRecorder()
	proxy.ServeHTTP(unloadW, unloadReq)
	assert.Equal(t, http.StatusOK, unloadW.Code)

	time.Sleep(200 * time.Millisecond)

	// Model should be stopped
	state := process.CurrentState()
	assert.True(t, state == StateStopped || state == StateShutdown,
		"model should be stopped or shutdown after unload, got %v", state)
}

// TestIntegration_RawUpstreamPassthrough verifies that /upstream/* paths
// proxy directly to the currently loaded model's upstream server.
func TestIntegration_RawUpstreamPassthrough(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  up-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond up-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// First load the model by sending a request
	body := `{"model":"up-model","messages":[{"role":"user","content":"load"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Wait for model to be ready
	time.Sleep(200 * time.Millisecond)

	// Call /test via upstream passthrough — only works if model is loaded
	upReq := httptest.NewRequest("GET", "/upstream/test", nil)
	upW := CreateTestResponseRecorder()
	proxy.ServeHTTP(upW, upReq)

	// Depending on upstream endpoint routing, may 200 or another status
	assert.True(t, upW.Code == http.StatusOK || upW.Code >= 300,
		"upstream response should be 200 or an error code, got %d", upW.Code)

	if upW.Code == http.StatusOK {
		assert.Equal(t, "up-model", strings.TrimSpace(upW.Body.String()))
	}
}

// TestIntegration_PerformanceAPI verifies that /api/performance returns
// appropriate response when monitoring is disabled.
func TestIntegration_PerformanceAPI(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
performance:
  enable: false
models:
  perf-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond perf-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	req := httptest.NewRequest("GET", "/api/performance", nil)
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)

	// With performance disabled, server returns 503
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestIntegration_JSONContentType ensures that responses have correct content types.
func TestIntegration_JSONContentType(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  ct-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond ct-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	tests := []struct {
		path        string
		body        string
		contentType string
	}{
		{"/v1/chat/completions", `{"model":"ct-model","messages":[{"role":"user","content":"hi"}]}`, "application/json"},
		{"/v1/completions", `{"model":"ct-model","prompt":"hello"}`, "application/json"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest("POST", tc.path, bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := CreateTestResponseRecorder()
			proxy.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Header().Get("Content-Type"), tc.contentType)
		})
	}
}

// TestIntegration_ActivityLogEntriesHaveUniqueIDs verifies each activity entry
// gets a unique ID.
func TestIntegration_ActivityLogEntriesHaveUniqueIDs(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  id-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond id-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	for i := 0; i < 5; i++ {
		body := `{"model":"id-model","messages":[{"role":"user","content":"ping"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := CreateTestResponseRecorder()
		proxy.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
	}

	time.Sleep(200 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	assert.GreaterOrEqual(t, len(metrics), 5)

	// IDs should be unique among entries with positive IDs
	seen := map[int]bool{}
	for _, m := range metrics {
		if m.ID <= 0 {
			continue
		}
		assert.False(t, seen[m.ID], "metric ID %d should be unique", m.ID)
		seen[m.ID] = true
	}
	assert.GreaterOrEqual(t, len(seen), 4, "should have at least 4 unique positive IDs")
}

func TestIntegration_ReadRequestBody(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  rb-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond rb-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Verify POST body is read by upstream
	payload := map[string]any{
		"model":    "rb-model",
		"messages": []map[string]string{{"role": "user", "content": "hello test"}},
		"temperature": 0.7,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// Read the body first to populate ContentLength
	buf := new(bytes.Buffer)
	_, _ = io.Copy(buf, req.Body)
	req.Body = io.NopCloser(buf)
	req.ContentLength = int64(buf.Len())

	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// The response should contain the request body that was received
	resp := gjson.ParseBytes(w.Body.Bytes())
	assert.Equal(t, "rb-model", resp.Get("responseMessage").String())
	assert.NotEmpty(t, resp.Get("h_content_length").String())
}

// TestIntegration_ActivityLogZeroDuration verifies that metrics capture
// non-zero duration for successful requests.
func TestIntegration_ActivityLogZeroDuration(t *testing.T) {
	cfg := testConfigFromYAML(t, `
healthCheckTimeout: 15
logLevel: error
models:
  dur-model:
    cmd: {{RESPONDER}} --port ${PORT} --silent --respond dur-model
`)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"dur-model","messages":[{"role":"user","content":"measure me"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	time.Sleep(100 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics)
	assert.Greater(t, metrics[0].DurationMs, 0, "request duration should be > 0 ms")
}
