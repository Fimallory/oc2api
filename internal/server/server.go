// Package server exposes the local OpenAI Responses-compatible API.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"oc2api/internal/api"
	"oc2api/internal/cache"
	"oc2api/internal/catalog"
	"oc2api/internal/config"
	"oc2api/internal/gateway"
	"oc2api/internal/zen"
)

// Server wires the catalog, exit pool and response cache into HTTP handlers.
type Server struct {
	cfg   config.Config
	cat   *catalog.Catalog
	pool  *gateway.Pool
	cache *cache.Cache
	log   *log.Logger
}

// New builds a server.
func New(cfg config.Config, cat *catalog.Catalog, pool *gateway.Pool, store *cache.Cache, logger *log.Logger) *Server {
	return &Server{cfg: cfg, cat: cat, pool: pool, cache: store, log: logger}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))
	mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/", s.handleIndex)
	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Oc2api-Cache, X-Session-Id, X-Opencode-Session")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.LocalKey != "" {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				token = r.Header.Get("x-api-key")
			}
			if token != s.cfg.LocalKey {
				writeError(w, http.StatusUnauthorized, "invalid local API key", "authentication_error")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "oc2api",
		"object":  "gateway",
		"version": "1.0",
		"endpoints": []string{
			"POST /v1/responses",
			"GET /v1/models",
			"GET /healthz",
		},
		"lane": s.cfg.Lane,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"lane":    s.cfg.Lane,
		"exits":   s.pool.States(),
		"cache":   s.cache.Stats(),
		"catalog": s.catalogInfo(),
	})
}

func (s *Server) catalogInfo() map[string]any {
	info := map[string]any{
		"models":  len(s.cat.List()),
		"fetched": s.cat.Fetched().Format(time.RFC3339),
	}
	if err := s.cat.Err(); err != nil {
		info["error"] = err.Error()
	}
	return info
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.cat.List()
	sort.Slice(models, func(i, j int) bool { return models[i].PublicID < models[j].PublicID })
	data := make([]any, 0, len(models))
	for _, model := range models {
		if model.Protocol == catalog.Gemini {
			continue
		}
		data = append(data, map[string]any{
			"id":       model.PublicID,
			"object":   "model",
			"created":  0,
			"owned_by": "opencode",
		})
	}
	if len(data) == 0 {
		for _, id := range fallbackFreeModels {
			data = append(data, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "opencode"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

var fallbackFreeModels = []string{
	"deepseek-v4.1-flash",
	"glm-5.3-flash",
	"mimo-v2.6-flash",
	"nemotron-3.5-lightning",
	"big-pickle",
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required", "invalid_request_error")
		return
	}
	started := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body: "+err.Error(), "invalid_request_error")
		return
	}
	req, _, err := api.ParseResponses(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	clientModel := req.Model
	model, found := s.cat.Lookup(req.Model, s.cfg.FreeOnly)
	if !found || (s.cfg.FreeOnly && !model.Free) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("model %q is not an available free model", clientModel), "invalid_request_error")
		return
	}
	if model.Protocol == catalog.Gemini {
		writeError(w, http.StatusNotImplemented, "gemini models are not supported by this gateway", "invalid_request_error")
		return
	}
	// Upstream requests always use the real catalog id ("glm-5.3-flash-free"),
	// while responses echo the published name the client asked for.
	req.Model = model.ID

	sessionSignal := api.SessionSignalFromHeaders(r.Header)
	if sessionSignal == "" && req.Metadata != nil {
		sessionSignal = api.StringAt(req.Metadata, "session_id")
	}
	if sessionSignal == "" {
		sessionSignal = zen.Seed(req.Instructions, api.FirstUserText(req.Messages))
	}
	projectSignal := r.Header.Get("x-opencode-project")
	if projectSignal == "" && req.Metadata != nil {
		projectSignal = api.StringAt(req.Metadata, "project_id")
	}

	clientTools := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		clientTools = append(clientTools, tool.Name)
	}

	payload, injected, err := buildPayload(req, model.Protocol, s.cfg.ReasoningEffort)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot encode upstream body", "server_error")
		return
	}

	key := api.CacheKey(req)
	bypass := strings.EqualFold(r.Header.Get("x-oc2api-cache"), "bypass")
	if !bypass && s.cfg.CacheEnabled {
		if entry, ok := s.cache.Get(key); ok {
			w.Header().Set("x-oc2api-cache", "hit")
			w.Header().Set("x-oc2api-model", clientModel)
			w.Header().Set("Content-Type", entry.ContentType)
			w.WriteHeader(entry.Status)
			_, _ = w.Write(entry.Body)
			s.logf("cache hit model=%s bytes=%d", clientModel, len(entry.Body))
			return
		}
	}

	timeout := s.cfg.Timeout()
	maxAttempts := s.cfg.MaxAttempts
	candidates := s.pool.Candidates()
	if maxAttempts <= 0 || maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}

	var lastReason string
	var lastStatus int
	for attempt, exit := range candidates {
		if attempt >= maxAttempts {
			break
		}
		if !exit.Available(time.Now()) {
			continue
		}
		session := zen.SessionID(sessionSignal)
		if attempt > 0 {
			session = zen.NewSessionID(sessionSignal)
		}
		project := zen.ProjectID(projectSignal)
		wire := s.wireFor(session, project, model.Protocol)
		url := gateway.Endpoint(s.cfg, model, model.Protocol, req.Model)
		if wire.Query != "" {
			if strings.Contains(url, "?") {
				url += "&" + wire.Query
			} else {
				url += "?" + wire.Query
			}
		}

		s.logf("attempt=%d exit=%s protocol=%s model=%s->%s session=%s", attempt+1, exit.Name, model.Protocol, clientModel, req.Model, session)
		resp, err := s.pool.Do(r.Context(), gateway.Attempt{
			URL:     url,
			Headers: wire.Headers,
			Body:    body,
			Exit:    exit,
		}, timeout)
		if err != nil {
			lastReason = err.Error()
			lastStatus = http.StatusBadGateway
			s.logf("exit=%s transport error: %v", exit.Name, err)
			continue
		}
		if resp.StatusCode/100 == 2 {
			resp.Exit.Clear()
			s.respond(w, r, resp, model.Protocol, req, clientModel, injected, clientTools, key, started)
			return
		}
		reason, upstreamBody := gateway.Classify(resp, time.Duration(s.cfg.BlockSeconds)*time.Second, time.Duration(s.cfg.QuotaSeconds)*time.Second)
		lastReason = reason
		lastStatus = resp.StatusCode
		s.logf("exit=%s status=%d reason=%s body=%s", exit.Name, resp.StatusCode, reason, zen.Truncate(string(upstreamBody), 200))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
			writeError(w, resp.StatusCode, string(upstreamBody), "upstream_error")
			return
		}
	}

	status := http.StatusBadGateway
	if lastStatus == http.StatusTooManyRequests {
		status = http.StatusTooManyRequests
	} else if lastStatus == http.StatusForbidden {
		status = http.StatusForbidden
	} else if lastStatus == http.StatusUnauthorized {
		status = http.StatusUnauthorized
	}
	message := lastReason
	if message == "" {
		message = "every upstream exit is cooling down or unavailable"
	}
	s.logf("request failed model=%s: %s", clientModel, message)
	writeError(w, status, message, "upstream_error")
}

// wireFor renders the lane-specific headers.
func (s *Server) wireFor(session, project string, protocol catalog.Protocol) zen.Wire {
	anthropic := protocol == catalog.Anthropic
	if s.cfg.Lane == "v1" {
		return zen.V1Wire(session, project, zen.MessageID(), anthropic)
	}
	return zen.V2Wire(session, project, anthropic)
}

func buildPayload(req *api.Request, protocol catalog.Protocol, effort string) (map[string]any, []string, error) {
	switch protocol {
	case catalog.Chat:
		payload := api.BuildChat(req)
		return payload, zen.ShapeChat(payload, effort), nil
	case catalog.Anthropic:
		payload := api.BuildAnthropic(req)
		return payload, zen.ShapeAnthropic(payload, ""), nil
	case catalog.Responses:
		payload := api.BuildResponses(req)
		return payload, zen.ShapeResponses(payload, effort), nil
	default:
		return nil, nil, fmt.Errorf("protocol %q is not supported", protocol)
	}
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, resp *gateway.Response, protocol catalog.Protocol, req *api.Request, clientModel string, injected, clientTools []string, key string, started time.Time) {
	w.Header().Set("x-oc2api-model", clientModel)
	w.Header().Set("x-oc2api-protocol", string(protocol))
	w.Header().Set("x-oc2api-exit", resp.Exit.Name)
	w.Header().Set("x-oc2api-cache", "miss")

	if req.Stream {
		s.streamSSE(w, r, resp, protocol, req, clientModel, injected, clientTools, key, started)
		return
	}

	writer := api.NewResponsesWriter(nil, nil, zen.RandomID("resp", 12), clientModel, clientTools, injected)
	if err := parseUpstream(resp.Body, protocol, writer.Emit); err != nil {
		writer.Fail(err)
	}
	final := writer.Final()
	if writer.Failed() {
		writeError(w, http.StatusBadGateway, writer.ErrorMessage(), "upstream_error")
		return
	}
	encoded, err := json.Marshal(final)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot encode response", "server_error")
		return
	}
	if s.cfg.CacheEnabled {
		s.cache.Set(key, &cache.Entry{ContentType: "application/json", Body: encoded, Status: http.StatusOK})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
	s.logf("done model=%s stream=false status=200 duration=%s", clientModel, time.Since(started).Round(time.Millisecond))
}

func (s *Server) streamSSE(w http.ResponseWriter, r *http.Request, resp *gateway.Response, protocol catalog.Protocol, req *api.Request, clientModel string, injected, clientTools []string, key string, started time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var captured bytes.Buffer
	limit := 8 << 20
	out := io.Writer(w)
	if s.cfg.CacheEnabled {
		out = &teeWriter{w: w, buf: &captured, limit: limit}
	}
	writer := api.NewResponsesWriter(out, flusher.Flush, zen.RandomID("resp", 12), clientModel, clientTools, injected)
	if err := parseUpstream(resp.Body, protocol, writer.Emit); err != nil {
		writer.Fail(err)
	}
	writer.Finish("completed")
	if s.cfg.CacheEnabled && !writer.Failed() && captured.Len() < limit {
		s.cache.Set(key, &cache.Entry{ContentType: "text/event-stream", Body: append([]byte(nil), captured.Bytes()...), Status: http.StatusOK, Stream: true})
	}
	s.logf("done model=%s stream=true status=200 duration=%s", clientModel, time.Since(started).Round(time.Millisecond))
}

func parseUpstream(body io.ReadCloser, protocol catalog.Protocol, emit func(api.Event)) error {
	defer body.Close()
	switch protocol {
	case catalog.Chat:
		return api.ParseChatStream(body, emit)
	case catalog.Anthropic:
		return api.ParseAnthropicStream(body, emit)
	case catalog.Responses:
		return api.ParseResponsesStream(body, emit)
	default:
		return fmt.Errorf("protocol %q is not supported", protocol)
	}
}

type teeWriter struct {
	w     io.Writer
	buf   *bytes.Buffer
	limit int
}

func (t *teeWriter) Write(p []byte) (int, error) {
	if t.buf.Len() < t.limit {
		room := t.limit - t.buf.Len()
		if room > len(p) {
			room = len(p)
		}
		t.buf.Write(p[:room])
	}
	return t.w.Write(p)
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Debug {
		s.log.Printf(format, args...)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func writeError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"code":    kind,
		},
	})
}
