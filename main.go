package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	listenAddr          = "127.0.0.1:38471"
	deepSeekMessagesURL = "https://api.deepseek.com/anthropic/v1/messages"
)

var upstreamHTTPClient = &http.Client{
	Transport: &http.Transport{
		DisableCompression: true,
	},
}

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	deepSeekAPIKey := os.Getenv("DEEPSEEK_API_KEY")
	if deepSeekAPIKey == "" {
		panic("DEEPSEEK_API_KEY is required")
	}
	deepSeekUserID := os.Getenv("DEEPSEEK_USER_ID")
	if deepSeekUserID == "" {
		panic("DEEPSEEK_USER_ID is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", handleMessages(deepSeekAPIKey, deepSeekUserID))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}

func handleMessages(deepSeekAPIKey, deepSeekUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("read request body: %v", err), http.StatusBadRequest)
			return
		}

		modifiedBody, err := rewriteMetadataUserID(body, deepSeekUserID)
		if err != nil {
			http.Error(w, fmt.Sprintf("rewrite metadata.user_id: %v", err), http.StatusBadRequest)
			return
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, deepSeekMessagesURL, bytes.NewReader(modifiedBody))
		if err != nil {
			http.Error(w, fmt.Sprintf("build upstream request: %v", err), http.StatusInternalServerError)
			return
		}

		copyRequestHeaders(upstreamReq.Header, r.Header)
		upstreamReq.Header.Del("x-api-key")
		upstreamReq.Header.Set("Authorization", "Bearer "+deepSeekAPIKey)
		upstreamReq.Header.Set("x-api-key", deepSeekAPIKey)
		upstreamReq.Header.Set("Content-Type", "application/json")
		if r.ContentLength >= 0 {
			upstreamReq.ContentLength = int64(len(modifiedBody))
		} else {
			upstreamReq.ContentLength = -1
		}

		resp, err := upstreamHTTPClient.Do(upstreamReq)
		if err != nil {
			http.Error(w, fmt.Sprintf("call DeepSeek: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		copyResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_ = streamResponse(w, resp.Body, resp.Header.Get("Content-Type"), resp.StatusCode)
	}
}

func rewriteMetadataUserID(body []byte, userID string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	rawMetadata, ok := payload["metadata"]
	if !ok || len(rawMetadata) == 0 {
		return body, nil
	}

	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(rawMetadata, &metadata); err != nil {
		return nil, fmt.Errorf("invalid metadata object: %w", err)
	}

	encodedUserID, err := json.Marshal(userID)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata.user_id: %w", err)
	}

	metadata["user_id"] = encodedUserID

	updatedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata object: %w", err)
	}

	payload["metadata"] = updatedMetadata

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal rewritten payload: %w", err)
	}

	return rewritten, nil
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "host", "content-length", "authorization", "x-api-key":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "transfer-encoding", "connection":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func streamResponse(w http.ResponseWriter, body io.Reader, contentType string, statusCode int) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	var captured bytes.Buffer

	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, writeErr := w.Write(chunk); writeErr != nil {
				return writeErr
			}
			_, _ = captured.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}

		if err == nil {
			continue
		}
		if err == io.EOF {
			logCacheStats(contentType, statusCode, captured.Bytes())
			return nil
		}
		return err
	}
}

type usageFields struct {
	InputTokens              *int64 `json:"input_tokens"`
	PromptTokens          *int64 `json:"prompt_tokens"`
	OutputTokens          *int64 `json:"output_tokens"`
	CompletionTokens      *int64 `json:"completion_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	PromptCacheHitTokens  *int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens"`
}

type usageEnvelope struct {
	Usage   usageFields `json:"usage"`
	Message struct {
		Usage usageFields `json:"usage"`
	} `json:"message"`
	Delta struct {
		Usage usageFields `json:"usage"`
	} `json:"delta"`
}

func logCacheStats(contentType string, statusCode int, body []byte) {
	hit, miss, prompt, output, hasCache, ok := extractCacheStats(body, contentType)
	if !ok {
		return
	}

	if hasCache {
		log.Printf("deepseek token stats: hit=%d miss=%d prompt=%d output=%d status=%d", hit, miss, prompt, output, statusCode)
		return
	}

	log.Printf("deepseek token stats: prompt=%d output=%d status=%d", prompt, output, statusCode)
}

func extractCacheStats(body []byte, contentType string) (int64, int64, int64, int64, bool, bool) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return extractCacheStatsFromSSE(body)
	}

	if hit, miss, prompt, output, hasCache, ok := extractCacheStatsFromJSON(body); ok {
		return hit, miss, prompt, output, hasCache, true
	}

	return extractCacheStatsFromSSE(body)
}

func extractCacheStatsFromSSE(body []byte) (int64, int64, int64, int64, bool, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		lastHit    int64
		lastMiss   int64
		lastPrompt int64
		lastOutput int64
		lastHasCache bool
		found      bool
	)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}

		hit, miss, prompt, output, hasCache, ok := extractCacheStatsFromJSON([]byte(payload))
		if !ok {
			continue
		}

		lastHit = hit
		lastMiss = miss
		lastPrompt = prompt
		lastOutput = output
		lastHasCache = hasCache
		found = true
	}

	return lastHit, lastMiss, lastPrompt, lastOutput, lastHasCache, found
}

func extractCacheStatsFromJSON(body []byte) (int64, int64, int64, int64, bool, bool) {
	var envelope usageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0, 0, 0, 0, false, false
	}

	if hit, miss, prompt, output, ok := usageToCacheStats(envelope.Usage); ok {
		return hit, miss, prompt, output, usageHasCacheStats(envelope.Usage), true
	}

	if hit, miss, prompt, output, ok := usageToCacheStats(envelope.Message.Usage); ok {
		return hit, miss, prompt, output, usageHasCacheStats(envelope.Message.Usage), true
	}

	hit, miss, prompt, output, ok := usageToCacheStats(envelope.Delta.Usage)
	return hit, miss, prompt, output, usageHasCacheStats(envelope.Delta.Usage), ok
}

func usageToCacheStats(usage usageFields) (int64, int64, int64, int64, bool) {
	output := int64(0)
	switch {
	case usage.OutputTokens != nil:
		output = *usage.OutputTokens
	case usage.CompletionTokens != nil:
		output = *usage.CompletionTokens
	}

	if usage.InputTokens != nil || usage.CacheCreationInputTokens != nil || usage.CacheReadInputTokens != nil {
		hit := int64(0)
		miss := int64(0)

		if usage.CacheReadInputTokens != nil {
			hit = *usage.CacheReadInputTokens
		}
		if usage.InputTokens != nil {
			miss += *usage.InputTokens
		}
		if usage.CacheCreationInputTokens != nil {
			miss += *usage.CacheCreationInputTokens
		}

		prompt := hit + miss
		if prompt == 0 && output == 0 {
			return 0, 0, 0, 0, false
		}

		return hit, miss, prompt, output, true
	}

	prompt := int64(0)
	switch {
	case usage.PromptTokens != nil:
		prompt = *usage.PromptTokens
	case usage.PromptCacheHitTokens != nil || usage.PromptCacheMissTokens != nil:
		if usage.PromptCacheHitTokens != nil {
			prompt += *usage.PromptCacheHitTokens
		}
		if usage.PromptCacheMissTokens != nil {
			prompt += *usage.PromptCacheMissTokens
		}
	}

	if prompt == 0 && output == 0 {
		return 0, 0, 0, 0, false
	}

	hit := int64(0)
	miss := int64(0)
	if usage.PromptCacheHitTokens != nil {
		hit = *usage.PromptCacheHitTokens
	}
	if usage.PromptCacheMissTokens != nil {
		miss = *usage.PromptCacheMissTokens
	}

	return hit, miss, prompt, output, true
}

func usageHasCacheStats(usage usageFields) bool {
	return usage.CacheCreationInputTokens != nil ||
		usage.CacheReadInputTokens != nil ||
		usage.PromptCacheHitTokens != nil ||
		usage.PromptCacheMissTokens != nil
}
