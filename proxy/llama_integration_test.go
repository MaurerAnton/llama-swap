package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/proxy/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Real llama-server and model paths.
// Override with env vars: LLAMA_SERVER_PATH, GGUF_MODEL_PATH
var (
	llamaServerPath  = getLlamaServerPath()
	ggufModelPath    = getGgufModelPath()
	llamaServerReady = initLlamaServer()
)

func getLlamaServerPath() string {
	if p := os.Getenv("LLAMA_SERVER_PATH"); p != "" {
		return p
	}
	// Release binary from llama.cpp b9204 ARM64
	return "/tmp/llama-b9204/llama-server"
}

func getGgufModelPath() string {
	if p := os.Getenv("GGUF_MODEL_PATH"); p != "" {
		return p
	}
	return "/tmp/models/stories260K.gguf"
}

// initLlamaServer checks prerequisites and sets up the shared library path.
func initLlamaServer() bool {
	if _, err := os.Stat(llamaServerPath); err != nil {
		return false
	}
	if _, err := os.Stat(ggufModelPath); err != nil {
		return false
	}
	// Pre-built binary needs its bundled .so files
	if ldPath := filepath.Dir(llamaServerPath); ldPath != "" {
		os.Setenv("LD_LIBRARY_PATH", ldPath+":"+os.Getenv("LD_LIBRARY_PATH"))
	}
	return true
}

// startLlamaServer launches llama-server on a random port and returns its
// URL and a cleanup function.
func startLlamaServer(t *testing.T, port int) (string, func()) {
	t.Helper()

	if !llamaServerReady {
		t.Skip("llama-server or model not found — set LLAMA_SERVER_PATH and GGUF_MODEL_PATH")
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(llamaServerPath,
		"-m", ggufModelPath,
		"--port", fmt.Sprint(port),
		"--host", "127.0.0.1",
		"--no-webui",
		"--log-disable",
		"--no-warmup",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start llama-server: %v", err)
	}

	cleanup := func() {
		cmd.Process.Kill()
		cmd.Wait()
	}

	// Wait for server to be ready
	baseURL := fmt.Sprintf("http://%s", addr)
	for i := 0; i < 60; i++ {
		resp, err := http.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return baseURL, cleanup
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	cleanup()
	t.Fatal("llama-server did not start within 30s")
	return "", nil
}

// TestRealLLamaServer_BasicChatCompletion starts a real llama-server and
// sends a chat completion request, verifying the full response structure.
func TestRealLLamaServer_BasicChatCompletion(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	body := `{"messages":[{"role":"user","content":"Say hi"}],"max_tokens":5,"stream":false}`
	req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	result := gjson.ParseBytes(readAll(resp.Body))
	assert.Equal(t, "chat.completion", result.Get("object").String())
	assert.Greater(t, len(result.Get("choices").Array()), 0)

	msg := result.Get("choices.0.message")
	assert.Equal(t, "assistant", msg.Get("role").String())
	assert.NotEmpty(t, msg.Get("content").String(), "model should produce output")

	// Verify usage data
	usage := result.Get("usage")
	assert.Greater(t, usage.Get("prompt_tokens").Int(), int64(0), "should have prompt tokens")
	assert.Greater(t, usage.Get("completion_tokens").Int(), int64(0), "should have completion tokens")

	// Verify timing data
	timings := result.Get("timings")
	assert.True(t, timings.Exists(), "should have timing data")
	assert.Greater(t, timings.Get("predicted_n").Int(), int64(0))
}

// TestRealLLamaServer_StreamingChatCompletion tests streaming response parsing.
func TestRealLLamaServer_StreamingChatCompletion(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	body := `{"messages":[{"role":"user","content":"Tell me a one word answer"}],"max_tokens":3,"stream":true}`
	req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	data := readAll(resp.Body)
	dataStr := string(data)
	assert.Contains(t, dataStr, "data:", "streaming response should have SSE data lines")
	assert.Contains(t, dataStr, "[DONE]", "streaming should end with [DONE]")
}

// TestRealLLamaServer_ViaSwapProxy starts a real llama-server, configures
// llama-swap to proxy to it, and sends requests through the proxy.
func TestRealLLamaServer_ViaSwapProxy(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	// Start a real llama-server
	upstreamPort := getTestPort()
	upstreamURL, upstreamCleanup := startLlamaServer(t, upstreamPort)
	defer upstreamCleanup()

	// Use peer config to connect to external llama-server
	cfgYAML := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: error
peers:
  upstream:
    proxy: "%s"
    models:
      - stories-model
`, upstreamURL)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(cfgYAML))
	require.NoError(t, err)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Send a request through the proxy
	body := `{"model":"stories-model","messages":[{"role":"user","content":"Say hello"}],"max_tokens":5,"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	result := gjson.ParseBytes(w.Body.Bytes())
	t.Logf("Response: %s", w.Body.String())

	// Response should contain assistant message
	choices := result.Get("choices")
	assert.True(t, choices.Exists(), "should have choices")
	if choices.Exists() && len(choices.Array()) > 0 {
		msg := choices.Array()[0].Get("message")
		assert.Equal(t, "assistant", msg.Get("role").String())
		assert.NotEmpty(t, msg.Get("content").String(), "model should produce text")
	}

	// Usage should be captured
	usage := result.Get("usage")
	if usage.Exists() {
		assert.Greater(t, usage.Get("prompt_tokens").Int(), int64(0))
	}

	time.Sleep(200 * time.Millisecond)

	// Verify activity metrics were captured
	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics, "proxy should capture activity metrics")
	assert.Equal(t, "stories-model", metrics[0].Model)
	assert.Equal(t, 200, metrics[0].RespStatusCode)
}

// TestRealLLamaServer_StreamingViaSwapProxy tests streaming through llama-swap
// proxying to a real llama-server.
func TestRealLLamaServer_StreamingViaSwapProxy(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	upstreamPort := getTestPort()
	upstreamURL, upstreamCleanup := startLlamaServer(t, upstreamPort)
	defer upstreamCleanup()

	cfgYAML := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: error
peers:
  upstream:
    proxy: "%s"
    models:
      - stream-model
`, upstreamURL)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(cfgYAML))
	require.NoError(t, err)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"stream-model","messages":[{"role":"user","content":"One word please"}],"max_tokens":3,"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	respStr := w.Body.String()
	assert.Contains(t, respStr, "data:", "should be SSE streaming")
	assert.Contains(t, respStr, "[DONE]", "should end with [DONE]")

	time.Sleep(200 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics)
	assert.Greater(t, metrics[0].Tokens.InputTokens, 0, "should capture input tokens from streaming")
	assert.Greater(t, metrics[0].Tokens.OutputTokens, 0, "should capture output tokens from streaming")
}

// TestRealLLamaServer_ModelListing verifies /v1/models through the proxy.
func TestRealLLamaServer_ModelListing(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	upstreamPort := getTestPort()
	upstreamURL, upstreamCleanup := startLlamaServer(t, upstreamPort)
	defer upstreamCleanup()

	cfgYAML := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: error
peers:
  upstream:
    proxy: "%s"
    models:
      - listing-model
`, upstreamURL)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(cfgYAML))
	require.NoError(t, err)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	result := gjson.ParseBytes(w.Body.Bytes())
	assert.Equal(t, "list", result.Get("object").String())

	data := result.Get("data")
	assert.True(t, data.IsArray(), "data should be an array")
	assert.GreaterOrEqual(t, len(data.Array()), 1, "should list at least one model")

	model := data.Array()[0]
	assert.Equal(t, "listing-model", model.Get("id").String())
}

// TestRealLLamaServer_MultipleRequests tests sending several requests through
// the proxy to a real llama-server and verifying activity log accumulation.
func TestRealLLamaServer_MultipleRequests(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	upstreamPort := getTestPort()
	upstreamURL, upstreamCleanup := startLlamaServer(t, upstreamPort)
	defer upstreamCleanup()

	cfgYAML := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: error
peers:
  upstream:
    proxy: "%s"
    models:
      - multi-model
`, upstreamURL)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(cfgYAML))
	require.NoError(t, err)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Send 3 requests
	for i := 0; i < 3; i++ {
		body := `{"model":"multi-model","messages":[{"role":"user","content":"Hi"}],"max_tokens":3,"stream":false}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := CreateTestResponseRecorder()

		proxy.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, "request %d should succeed", i)
	}

	time.Sleep(300 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	assert.GreaterOrEqual(t, len(metrics), 3, "should have at least 3 activity entries")

	// All entries should be for the same model
	for _, m := range metrics {
		assert.Equal(t, "multi-model", m.Model)
		assert.Equal(t, 200, m.RespStatusCode)
	}
}

// TestRealLLamaServer_ActivityLogTokens verifies token counts are correctly
// captured from real llama-server usage data.
func TestRealLLamaServer_ActivityLogTokens(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	upstreamPort := getTestPort()
	upstreamURL, upstreamCleanup := startLlamaServer(t, upstreamPort)
	defer upstreamCleanup()

	cfgYAML := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: error
peers:
  upstream:
    proxy: "%s"
    models:
      - token-model
`, upstreamURL)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(cfgYAML))
	require.NoError(t, err)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	body := `{"model":"token-model","messages":[{"role":"user","content":"Count my tokens"}],"max_tokens":10,"stream":false}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := CreateTestResponseRecorder()

	proxy.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	time.Sleep(200 * time.Millisecond)

	metrics := proxy.metricsMonitor.getMetrics()
	require.NotEmpty(t, metrics)

	t.Logf("Captured tokens: input=%d output=%d cache=%d duration=%dms",
		metrics[0].Tokens.InputTokens,
		metrics[0].Tokens.OutputTokens,
		metrics[0].Tokens.CachedTokens,
		metrics[0].DurationMs)

	// From real llama-server, we expect:
	// - prompt_tokens > 0 (the user message + system template)
	// - completion_tokens > 0 (max_tokens=10 requested)
	// - timing data from the timings field
	assert.Greater(t, metrics[0].Tokens.InputTokens, 0, "should have non-zero input tokens")
	assert.Greater(t, metrics[0].Tokens.OutputTokens, 0, "should have non-zero output tokens")
	assert.Greater(t, metrics[0].DurationMs, 0, "should have non-zero duration")
}

func readAll(r interface{ Read([]byte) (int, error) }) []byte {
	buf := new(bytes.Buffer)
	buf.ReadFrom(r)
	return buf.Bytes()
}

func TestRealLLamaServer_NonStreamingJSONParsing(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	// Verify raw non-streaming response directly from llama-server
	body := `{"messages":[{"role":"user","content":"test"}],"max_tokens":1,"stream":false}`
	req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	result := gjson.ParseBytes(readAll(resp.Body))

	// Real llama-server response should have all these fields
	object := result.Get("object").String()
	assert.Equal(t, "chat.completion", object)

	choices := result.Get("choices").Array()
	assert.GreaterOrEqual(t, len(choices), 1)

	// The model field should be present
	assert.NotEmpty(t, result.Get("model").String())

	// Verify usage in non-streaming mode
	usage := result.Get("usage")
	assert.True(t, usage.Exists(), "non-streaming response must have usage")
	assert.Greater(t, usage.Get("prompt_tokens").Int(), int64(0))
	assert.Greater(t, usage.Get("completion_tokens").Int(), int64(0))
}

func TestRealLLamaServer_VersionInfo(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	// Send a request and check the system_fingerprint field
	body := `{"messages":[{"role":"user","content":"x"}],"max_tokens":1,"stream":false}`
	req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	result := gjson.ParseBytes(readAll(resp.Body))
	fp := result.Get("system_fingerprint").String()
	assert.NotEmpty(t, fp, "should have system_fingerprint")
	t.Logf("llama.cpp system_fingerprint: %s", fp)
}

func TestRealLLamaServer_TimingAccuracy(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	// Request with enough tokens to get measurable timing
	body := `{"messages":[{"role":"user","content":"Write a short sentence about cats"}],"max_tokens":20,"stream":false}`
	req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	wallMs := int(time.Since(start).Milliseconds())
	require.NoError(t, err)
	defer resp.Body.Close()

	result := gjson.ParseBytes(readAll(resp.Body))
	timings := result.Get("timings")
	assert.True(t, timings.Exists(), "should have timing data")

	promptMs := timings.Get("prompt_ms").Float()
	predictedMs := timings.Get("predicted_ms").Float()
	totalModelMs := promptMs + predictedMs

	t.Logf("Wall time: %dms, Model time: %.0fms (prompt: %.0fms, generation: %.0fms)",
		wallMs, totalModelMs, promptMs, predictedMs)

	// Model time should be close to wall time (within reason — HTTP overhead)
	assert.Greater(t, totalModelMs, 0.0, "model should take some time")
	assert.LessOrEqual(t, totalModelMs, float64(wallMs)+2000,
		"model time should not vastly exceed wall time")
}

func TestRealLLamaServer_ErrorMessageHandling(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	// Send invalid JSON
	req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions",
		bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.GreaterOrEqual(t, resp.StatusCode, 400,
		"llama-server should return error for invalid JSON, got %d", resp.StatusCode)

	result := gjson.ParseBytes(readAll(resp.Body))
	t.Logf("Error response: %s", result.Raw)
}

func TestRealLLamaServer_ParallelRequests(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	port := getTestPort()
	baseURL, cleanup := startLlamaServer(t, port)
	defer cleanup()

	// Send 2 concurrent requests and verify both succeed
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			body := `{"messages":[{"role":"user","content":"Hi"}],"max_tokens":3,"stream":false}`
			req, _ := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("expected 200, got %d", resp.StatusCode)
				return
			}
			result := gjson.ParseBytes(readAll(resp.Body))
			if len(result.Get("choices").Array()) == 0 {
				errs <- fmt.Errorf("no choices in response")
				return
			}
			errs <- nil
		}()
	}

	for i := 0; i < 2; i++ {
		assert.NoError(t, <-errs, "parallel request %d should succeed", i)
	}
}

// TestRealLLamaServer_HealthCheck verifies the proxy's health check against
// a real upstream server.
func TestRealLLamaServer_HealthCheck(t *testing.T) {
	if !llamaServerReady {
		t.Skip("llama-server or model not found")
	}

	upstreamPort := getTestPort()
	upstreamURL, upstreamCleanup := startLlamaServer(t, upstreamPort)
	defer upstreamCleanup()

	cfgYAML := fmt.Sprintf(`
healthCheckTimeout: 60
logLevel: error
peers:
  upstream:
    proxy: "%s"
    models:
      - hc-model
`, upstreamURL)

	cfg, err := config.LoadConfigFromReader(strings.NewReader(cfgYAML))
	require.NoError(t, err)

	proxy := New(cfg)
	defer proxy.StopProcesses(StopWaitForInflightRequest)

	// Health endpoint through proxy
	req := httptest.NewRequest("GET", "/health", nil)
	w := CreateTestResponseRecorder()
	proxy.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}
