package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/time/rate"
)

var (
	db          *sql.DB
	apiBase     string
	apiKey      string
	model       string
	limiter     *rate.Limiter
	pricing     map[string]ModelPricing
	pricingFile string
	host        string
	port        int
	rpm         int
)

// ModelPricing represents pricing per 1M tokens for a model
type ModelPricing struct {
	InputPrice  float64 `json:"InputPrice"`
	CachedPrice float64 `json:"CachedPrice"`
	OutputPrice float64 `json:"OutputPrice"`
}

// RequestLog represents a log entry in the database
type RequestLog struct {
	ID           int     `json:"id"`
	Timestamp    string  `json:"timestamp"`
	ClientIP     string  `json:"client_ip"`
	UpstreamURL  string  `json:"upstream_url"`
	Method       string  `json:"method"`
	Path         string  `json:"path"`
	StatusCode   int     `json:"status_code"`
	LatencyMs    int     `json:"latency_ms"`
	RequestBody  string  `json:"request_body"`
	ResponseBody string  `json:"response_body"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TotalTokens  int     `json:"total_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	CostINR      float64 `json:"cost_inr"`
	Model        string  `json:"model"`
}

// Usage represents the usage field in responses (supports both formats)
type Usage struct {
	InputTokens      int          `json:"input_tokens"`
	OutputTokens     int          `json:"output_tokens"`
	TotalTokens      int          `json:"total_tokens"`
	PromptTokens     int          `json:"prompt_tokens,omitempty"`
	CompletionTokens int          `json:"completion_tokens,omitempty"`
	CachedTokens     int          `json:"cached_tokens,omitempty"`
	PromptDetails    TokenDetails `json:"prompt_tokens_details,omitempty"`
	InputDetails     TokenDetails `json:"input_tokens_details,omitempty"`
}
type TokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// Response represents a generic API response with usage
type Response struct {
	Usage Usage  `json:"usage"`
	Model string `json:"model,omitempty"`
}

func init() {

	// Load .env file
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: Error loading .env file: %v", err)
	}

	apiBase = os.Getenv("API_BASE")
	apiKey = os.Getenv("API_KEY")
	model = os.Getenv("MODEL")

	if apiBase == "" {
		log.Fatal("API_BASE is required")
	}
	if apiKey == "" {
		log.Fatal("API_KEY is required")
	}
	if model == "" {
		log.Fatal("MODEL is required")
	}

	// Define flags
	flag.StringVar(&pricingFile, "pricing", "pricing.json", "Path to pricing JSON file")
	flag.StringVar(&host, "host", "0.0.0.0", "Host to listen on")
	flag.IntVar(&port, "port", 8080, "Port to listen on")
	flag.IntVar(&rpm, "rpm", 60, "Maximum requests per minute")
	flag.Parse()

	// Initialize rate limiter
	limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), rpm)

	// Load pricing from JSON
	loadPricing()

	// Initialize SQLite database
	var err error
	db, err = sql.Open("sqlite3", "./tracer.db")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}

	// Create table if it doesn't exist
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
		client_ip TEXT,
		upstream_url TEXT,
		method TEXT,
		path TEXT,
		status_code INTEGER,
		latency_ms INTEGER,
		request_body TEXT,
		response_body TEXT,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		cached_tokens INTEGER DEFAULT 0,
		cost_inr REAL DEFAULT 0,
		model TEXT
	);`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Failed to create table:", err)
	}

	log.Printf("Loaded configuration:")
	log.Printf("  Upstream URL: %s", apiBase)
	log.Printf("  Model: %s", model)
	log.Printf("  Listening on: %s:%d", host, port)
	log.Printf("  Dashboard available at: http://%s:%d/dashboard", host, port)
	log.Printf("  Rate limit: %d requests/minute", rpm)
}

func loadPricing() {
	data, err := os.ReadFile(pricingFile)
	if err != nil {
		log.Printf("Warning: Could not read pricing file %s: %v. Starting with empty pricing.", pricingFile, err)
		pricing = make(map[string]ModelPricing)
		return
	}

	if err := json.Unmarshal(data, &pricing); err != nil {
		log.Fatalf("Error parsing pricing JSON: %v", err)
	}
	log.Printf("Loaded pricing for %d models from %s", len(pricing), pricingFile)
}

func main() {
	// Register handlers
	http.HandleFunc("/api/logs", logsHandler)
	http.HandleFunc("/api/stats", statsHandler)
	http.HandleFunc("/api/config", configHandler)
	http.HandleFunc("/dashboard", dashboardHandler)
	http.HandleFunc("/", rateLimitMiddleware(proxyHandler))

	// Start server
	addr := fmt.Sprintf("%s:%d", host, port)
	log.Printf("\nProxy server starting on %s...", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// rateLimitMiddleware applies rate limiting to the handler
func rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Ignore common browser requests
		if r.URL.Path == "/favicon.ico" || r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		// Skip rate limiting for dashboard and api routes
		if r.URL.Path == "/dashboard" || r.URL.Path == "/api/logs" || r.URL.Path == "/api/stats" || r.URL.Path == "/api/config" {
			switch r.URL.Path {
			case "/dashboard":
				dashboardHandler(w, r)
			case "/api/logs":
				logsHandler(w, r)
			case "/api/stats":
				statsHandler(w, r)
			case "/api/config":
				configHandler(w, r)
			}
			return
		}

		// Check rate limit
		if !limiter.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			errMsg := fmt.Sprintf(`{"error": "Rate limit exceeded. Maximum %d requests per minute."}`, rpm)
			w.Write([]byte(errMsg))
			log.Printf("Rate limit exceeded for %s", r.RemoteAddr)
			return
		}

		next(w, r)
	}
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Capture client IP
	clientIP := r.RemoteAddr

	// Read request body
	var requestBody []byte
	if r.Body != nil {
		requestBody, _ = io.ReadAll(r.Body)
		r.Body.Close()
		// Restore the body for forwarding
		r.Body = io.NopCloser(bytes.NewReader(requestBody))
	}

	if responseBody, ok := localMetadataResponse(r.URL.Path); ok {
		writeLocalResponse(w, r, startTime, clientIP, requestBody, responseBody)
		return
	}

	forwardedBody := requestBody
	if len(requestBody) > 0 {
		if body, changed, err := prepareRequestBody(requestBody, r.URL.Path); err != nil {
			log.Printf("Error preparing request body: %v", err)
		} else if changed {
			forwardedBody = body
		}
	}

	// Build upstream URL
	upstreamURL, err := url.Parse(apiBase)
	if err != nil {
		http.Error(w, "Invalid upstream URL", http.StatusInternalServerError)
		log.Printf("Error parsing upstream URL: %v", err)
		return
	}

	upstreamURL.Path = buildUpstreamPath(upstreamURL.Path, r.URL.Path)
	upstreamURL.RawQuery = r.URL.RawQuery
	upstreamFullPath := upstreamURL.String()

	// Create new request to upstream
	upstreamReq, err := http.NewRequest(r.Method, upstreamFullPath, bytes.NewReader(forwardedBody))
	if err != nil {
		http.Error(w, "Failed to create upstream request", http.StatusInternalServerError)
		log.Printf("Error creating upstream request: %v", err)
		return
	}

	// Copy headers from original request
	for key, values := range r.Header {
		if strings.EqualFold(key, "Authorization") ||
			strings.EqualFold(key, "Content-Length") ||
			strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			upstreamReq.Header.Add(key, value)
		}
	}

	// Inject/overwrite Authorization header with upstream API key
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	upstreamReq.ContentLength = int64(len(forwardedBody))

	// Execute the upstream request
	client := &http.Client{}
	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "Failed to reach upstream server", http.StatusBadGateway)
		log.Printf("Error executing upstream request: %v", err)
		return
	}
	defer upstreamResp.Body.Close()

	// Read response body
	responseBody, err := io.ReadAll(upstreamResp.Body)
	if err != nil {
		http.Error(w, "Failed to read upstream response", http.StatusInternalServerError)
		log.Printf("Error reading response body: %v", err)
		return
	}

	// Calculate latency
	latency := int(time.Since(startTime).Milliseconds())

	// Extract token usage and model from response
	inputTokens, outputTokens, totalTokens, cachedTokens := extractTokens(responseBody)
	responseModel := model

	// Calculate cost
	cost := calculateCost(responseModel, inputTokens, outputTokens, cachedTokens)

	// Log to database
	err = logToDatabase(RequestLog{
		ClientIP:     clientIP,
		UpstreamURL:  apiBase,
		Method:       r.Method,
		Path:         r.URL.Path,
		StatusCode:   upstreamResp.StatusCode,
		LatencyMs:    latency,
		RequestBody:  string(forwardedBody),
		ResponseBody: string(responseBody),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
		CachedTokens: cachedTokens,
		CostINR:      cost,
		Model:        responseModel,
	})
	if err != nil {
		log.Printf("Error logging to database: %v", err)
	}

	// Console logging
	log.Printf("\n========== REQUEST ==========")
	log.Printf("Method: %s | Path: %s | Client: %s", r.Method, r.URL.Path, clientIP)
	log.Printf("Upstream URL: %s", upstreamFullPath)
	log.Printf("Configured Model: %s", model)
	log.Printf("Path: %s", r.URL.Path)
	log.Printf("Request Body: %s", truncateString(string(forwardedBody), 200))
	log.Printf("========== RESPONSE ==========")
	log.Printf("Status: %d | Latency: %dms", upstreamResp.StatusCode, latency)
	log.Printf("Model: %s", responseModel)
	log.Printf("Tokens - Input: %d | Cached: %d | Output: %d | Total: %d", inputTokens, cachedTokens, outputTokens, totalTokens)
	log.Printf("Cost: $%.6f", cost)
	log.Printf("Response Body: %s", truncateString(string(responseBody), 200))
	log.Println("==============================")

	// Copy response headers
	for key, values := range upstreamResp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Write status code and body
	w.WriteHeader(upstreamResp.StatusCode)
	w.Write(responseBody)
}

func logsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	rows, err := db.Query(`
		SELECT id, timestamp, client_ip, upstream_url, method, path,
		       status_code, latency_ms, request_body, response_body,
		       input_tokens, output_tokens, total_tokens, cached_tokens, cost_inr, model
		FROM request_logs
		ORDER BY id DESC
		LIMIT 100
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var logs []RequestLog
	for rows.Next() {
		var reqLog RequestLog
		err := rows.Scan(
			&reqLog.ID, &reqLog.Timestamp, &reqLog.ClientIP, &reqLog.UpstreamURL,
			&reqLog.Method, &reqLog.Path, &reqLog.StatusCode, &reqLog.LatencyMs,
			&reqLog.RequestBody, &reqLog.ResponseBody,
			&reqLog.InputTokens, &reqLog.OutputTokens, &reqLog.TotalTokens,
			&reqLog.CachedTokens, &reqLog.CostINR, &reqLog.Model,
		)
		if err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}
		logs = append(logs, reqLog)
	}

	json.NewEncoder(w).Encode(logs)
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var stats struct {
		TotalRequests int     `json:"total_requests"`
		TotalTokens   int     `json:"total_tokens"`
		InputTokens   int     `json:"input_tokens"`
		OutputTokens  int     `json:"output_tokens"`
		CachedTokens  int     `json:"cached_tokens"`
		TotalCost     float64 `json:"total_cost_inr"`
	}

	err := db.QueryRow(`
		SELECT 
			COUNT(*) as total_requests,
			COALESCE(SUM(total_tokens), 0) as total_tokens,
			COALESCE(SUM(input_tokens), 0) as input_tokens,
			COALESCE(SUM(output_tokens), 0) as output_tokens,
			COALESCE(SUM(cached_tokens), 0) as cached_tokens,
			COALESCE(SUM(cost_inr), 0) as total_cost
		FROM request_logs
	`).Scan(
		&stats.TotalRequests,
		&stats.TotalTokens,
		&stats.InputTokens,
		&stats.OutputTokens,
		&stats.CachedTokens,
		&stats.TotalCost,
	)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(stats)
}

func dashboardHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "dashboard.html")
}

type configResponse struct {
	APIBase string `json:"api_base"`
	Model   string `json:"model"`
}

type configUpdate struct {
	APIBase *string `json:"api_base"`
	APIKey  *string `json:"api_key"`
	Model   *string `json:"model"`
}

func configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(configResponse{
			APIBase: apiBase,
			Model:   model,
		})
		return
	case http.MethodPost:
		var update configUpdate
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}

		updates := make(map[string]string)
		if update.APIBase != nil {
			value := strings.TrimSpace(*update.APIBase)
			if value == "" {
				http.Error(w, "api_base cannot be empty", http.StatusBadRequest)
				return
			}
			apiBase = value
			os.Setenv("API_BASE", value)
			updates["API_BASE"] = value
		}
		if update.Model != nil {
			value := strings.TrimSpace(*update.Model)
			if value == "" {
				http.Error(w, "model cannot be empty", http.StatusBadRequest)
				return
			}
			model = value
			os.Setenv("MODEL", value)
			updates["MODEL"] = value
		}
		if update.APIKey != nil {
			value := strings.TrimSpace(*update.APIKey)
			if value == "" {
				http.Error(w, "api_key cannot be empty", http.StatusBadRequest)
				return
			}
			apiKey = value
			os.Setenv("API_KEY", value)
			updates["API_KEY"] = value
		}
		if len(updates) == 0 {
			http.Error(w, "No settings to update", http.StatusBadRequest)
			return
		}

		if err := updateEnvFile(".env", updates); err != nil {
			http.Error(w, "Failed to update .env", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(configResponse{
			APIBase: apiBase,
			Model:   model,
		})
		return
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
}

func localMetadataResponse(path string) ([]byte, bool) {
	normalizedPath := normalizeRequestPath(path)

	switch normalizedPath {
	case "/models":
		return mustJSON(map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{
					"id":       model,
					"object":   "model",
					"created":  0,
					"owned_by": "llmproxy",
				},
			},
		}), true
	case "/api/tags":
		return mustJSON(map[string]interface{}{
			"models": []map[string]interface{}{
				{
					"name":        model,
					"model":       model,
					"modified_at": time.Now().UTC().Format(time.RFC3339Nano),
					"size":        0,
					"digest":      "",
					"details": map[string]interface{}{
						"family":             "openai",
						"families":           []string{"openai"},
						"parameter_size":     "",
						"quantization_level": "",
					},
				},
			},
		}), true
	case "/version":
		return mustJSON(map[string]string{"version": "llmproxy"}), true
	case "/props":
		return mustJSON(map[string]interface{}{
			"model": model,
			"models": []string{
				model,
			},
		}), true
	default:
		return nil, false
	}
}

func writeLocalResponse(w http.ResponseWriter, r *http.Request, startTime time.Time, clientIP string, requestBody, responseBody []byte) {
	latency := int(time.Since(startTime).Milliseconds())

	if err := logToDatabase(RequestLog{
		ClientIP:     clientIP,
		UpstreamURL:  "local",
		Method:       r.Method,
		Path:         r.URL.Path,
		StatusCode:   http.StatusOK,
		LatencyMs:    latency,
		RequestBody:  string(requestBody),
		ResponseBody: string(responseBody),
		Model:        model,
	}); err != nil {
		log.Printf("Error logging local response to database: %v", err)
	}

	log.Printf("\n========== REQUEST ==========")
	log.Printf("Method: %s | Path: %s | Client: %s", r.Method, r.URL.Path, clientIP)
	log.Printf("Upstream URL: local metadata response")
	log.Printf("Configured Model: %s", model)
	log.Printf("Request Body: %s", truncateString(string(requestBody), 200))
	log.Printf("========== RESPONSE ==========")
	log.Printf("Status: %d | Latency: %dms", http.StatusOK, latency)
	log.Printf("Model: %s", model)
	log.Printf("Tokens - Input: 0 | Cached: 0 | Output: 0 | Total: 0")
	log.Printf("Cost: $0.000000")
	log.Printf("Response Body: %s", truncateString(string(responseBody), 200))
	log.Println("==============================")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(responseBody)
	}
}

func mustJSON(value interface{}) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		log.Fatalf("Failed to marshal local metadata response: %v", err)
	}
	return body
}

func buildUpstreamPath(basePath, requestPath string) string {
	requestPath = normalizeRequestPath(requestPath)
	if basePath == "" || basePath == "/" {
		return requestPath
	}
	if requestPath == "" || requestPath == "/" {
		return basePath
	}

	basePath = strings.TrimRight(basePath, "/")
	if requestPath == basePath || strings.HasPrefix(requestPath, basePath+"/") {
		return requestPath
	}

	return basePath + "/" + strings.TrimLeft(requestPath, "/")
}

func normalizeRequestPath(path string) string {
	path = "/" + strings.TrimLeft(path, "/")
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}

	switch {
	case path == "/api/tags":
		return path
	case path == "/api/version":
		return "/version"
	case path == "/api/props":
		return "/props"
	case path == "/api/v1":
		return "/"
	case strings.HasPrefix(path, "/api/v1/"):
		return "/" + strings.TrimPrefix(path, "/api/v1/")
	case path == "/v1":
		return "/"
	case strings.HasPrefix(path, "/v1/"):
		return "/" + strings.TrimPrefix(path, "/v1/")
	default:
		return path
	}
}

func prepareRequestBody(requestBody []byte, path string) ([]byte, bool, error) {
	var reqBody map[string]interface{}
	if err := json.Unmarshal(requestBody, &reqBody); err != nil {
		return nil, false, err
	}

	changed := false
	if reqBody["model"] != model {
		reqBody["model"] = model
		changed = true
	}

	if isChatCompletionsPath(path) {
		if stream, ok := reqBody["stream"].(bool); ok && stream {
			streamOptions, _ := reqBody["stream_options"].(map[string]interface{})
			if streamOptions == nil {
				streamOptions = make(map[string]interface{})
				reqBody["stream_options"] = streamOptions
				changed = true
			}
			if streamOptions["include_usage"] != true {
				streamOptions["include_usage"] = true
				changed = true
			}
		}
	}

	if !changed {
		return requestBody, false, nil
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, false, err
	}
	return body, true, nil
}

func isChatCompletionsPath(path string) bool {
	return path == "/chat/completions" || strings.HasSuffix(path, "/chat/completions")
}

func extractTokens(responseBody []byte) (input, output, total, cached int) {
	// Default to 0 if not found
	input, output, total, cached = 0, 0, 0, 0

	// Try to parse as Response API format (input_tokens/output_tokens)
	var resp Response
	if err := json.Unmarshal(responseBody, &resp); err == nil {
		if usageHasTokens(resp.Usage) {
			input, output, total, cached = tokensFromUsage(resp.Usage)
			return
		}
	}

	if input, output, total, cached, ok := extractTokensFromSSE(responseBody); ok {
		return input, output, total, cached
	}

	// Try parsing as a generic map for flexibility
	var wrappedResp map[string]interface{}
	if err := json.Unmarshal(responseBody, &wrappedResp); err == nil {
		if usage, ok := wrappedResp["usage"].(map[string]interface{}); ok {
			input, output, total, cached = tokensFromUsageMap(usage)
			return
		}
	}

	return
}

func extractTokensFromSSE(responseBody []byte) (input, output, total, cached int, ok bool) {
	lines := strings.Split(string(responseBody), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk Response
		if err := json.Unmarshal([]byte(payload), &chunk); err == nil && usageHasTokens(chunk.Usage) {
			input, output, total, cached = tokensFromUsage(chunk.Usage)
			ok = true
			continue
		}

		var wrappedChunk map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &wrappedChunk); err == nil {
			if usage, found := wrappedChunk["usage"].(map[string]interface{}); found {
				input, output, total, cached = tokensFromUsageMap(usage)
				if input > 0 || output > 0 || total > 0 || cached > 0 {
					ok = true
				}
			}
		}
	}

	return
}

func usageHasTokens(usage Usage) bool {
	return usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.PromptTokens > 0 ||
		usage.CompletionTokens > 0 ||
		usage.TotalTokens > 0 ||
		usage.CachedTokens > 0 ||
		usage.PromptDetails.CachedTokens > 0 ||
		usage.InputDetails.CachedTokens > 0
}

func tokensFromUsage(usage Usage) (input, output, total, cached int) {
	input = usage.InputTokens
	output = usage.OutputTokens
	if input == 0 {
		input = usage.PromptTokens
	}
	if output == 0 {
		output = usage.CompletionTokens
	}

	total = usage.TotalTokens
	if total == 0 {
		total = input + output
	}

	cached = usage.CachedTokens
	if cached == 0 {
		cached = usage.PromptDetails.CachedTokens
	}
	if cached == 0 {
		cached = usage.InputDetails.CachedTokens
	}

	return
}

func tokensFromUsageMap(usage map[string]interface{}) (input, output, total, cached int) {
	input = intFromMap(usage, "input_tokens")
	output = intFromMap(usage, "output_tokens")
	if input == 0 {
		input = intFromMap(usage, "prompt_tokens")
	}
	if output == 0 {
		output = intFromMap(usage, "completion_tokens")
	}

	total = intFromMap(usage, "total_tokens")
	if total == 0 {
		total = input + output
	}

	cached = intFromMap(usage, "cached_tokens")
	if cached == 0 {
		cached = cachedTokensFromDetails(usage, "prompt_tokens_details")
	}
	if cached == 0 {
		cached = cachedTokensFromDetails(usage, "input_tokens_details")
	}

	return
}

func cachedTokensFromDetails(usage map[string]interface{}, key string) int {
	details, ok := usage[key].(map[string]interface{})
	if !ok {
		return 0
	}
	return intFromMap(details, "cached_tokens")
}

func intFromMap(values map[string]interface{}, key string) int {
	value, ok := values[key]
	if !ok {
		return 0
	}

	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}

// calculateCost calculates the cost in USD based on model and token usage
func calculateCost(modelName string, inputTokens, outputTokens, cachedTokens int) float64 {
	pricingInfo, exists := pricingForModel(modelName)
	if !exists {
		if inputTokens == 0 && outputTokens == 0 && cachedTokens == 0 {
			return 0
		}
		log.Printf("Warning: No pricing configured for model %q; cost recorded as 0", modelName)
		return 0
	}

	// Calculate costs (prices are per 1M tokens)
	inputCost := float64(inputTokens) / 1000000.0 * pricingInfo.InputPrice
	cachedCost := float64(cachedTokens) / 1000000.0 * pricingInfo.CachedPrice
	outputCost := float64(outputTokens) / 1000000.0 * pricingInfo.OutputPrice

	totalCost := inputCost + cachedCost + outputCost
	return totalCost
}

func pricingForModel(modelName string) (ModelPricing, bool) {
	if pricingInfo, exists := pricing[modelName]; exists {
		return pricingInfo, true
	}

	parts := strings.Split(modelName, "-")
	for len(parts) > 1 {
		parts = parts[:len(parts)-1]
		candidate := strings.Join(parts, "-")
		if pricingInfo, exists := pricing[candidate]; exists {
			return pricingInfo, true
		}
	}

	return ModelPricing{}, false
}

func logToDatabase(logEntry RequestLog) error {
	_, err := db.Exec(`
		INSERT INTO request_logs
		(timestamp, client_ip, upstream_url, method, path, status_code,
		 latency_ms, request_body, response_body, input_tokens,
		 output_tokens, total_tokens, cached_tokens, cost_inr, model)
		VALUES (datetime('now'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		logEntry.ClientIP, logEntry.UpstreamURL, logEntry.Method,
		logEntry.Path, logEntry.StatusCode, logEntry.LatencyMs,
		logEntry.RequestBody, logEntry.ResponseBody,
		logEntry.InputTokens, logEntry.OutputTokens, logEntry.TotalTokens,
		logEntry.CachedTokens, logEntry.CostINR, logEntry.Model,
	)
	return err
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func updateEnvFile(path string, updates map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	lines := []string{}
	if err == nil {
		lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	}

	seen := make(map[string]bool)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if value, ok := updates[key]; ok {
			lines[i] = fmt.Sprintf("%s=%s", key, formatEnvValue(value))
			seen[key] = true
		}
	}

	for key, value := range updates {
		if seen[key] {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s=%s", key, formatEnvValue(value)))
	}

	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func formatEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t\n#=") {
		escaped := strings.ReplaceAll(value, "\\", "\\\\")
		escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
		return "\"" + escaped + "\""
	}
	return value
}
