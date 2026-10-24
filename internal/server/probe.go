package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"oc2api/internal/api"
	"oc2api/internal/catalog"
	"oc2api/internal/gateway"
	"oc2api/internal/zen"
)

// probeMaxTokens is deliberately tiny: the probe only needs a status code.
const probeMaxTokens = 16

// probeQuotaRetries is how many times a model is retried after every exit
// reported an exhausted free allowance, before the whole probe stops.
const probeQuotaRetries = 2

type probeStats struct {
	total     int
	available int32
	gone      int32
	failed    int32
	aborted   int32
}

// Probe tests every published model with a minimal request and removes the
// ones the upstream reports as deprecated or missing. It runs before the
// listener starts so /v1/models is accurate from the first request.
func (s *Server) Probe(ctx context.Context) {
	if !s.cfg.ProbeOnStart {
		return
	}
	models := s.cat.List()
	if len(models) == 0 {
		return
	}
	stats := &probeStats{total: len(models)}
	s.log.Printf("probing %d models with %d tokens each", len(models), probeMaxTokens)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	interval := time.Duration(s.cfg.ProbeIntervalMS) * time.Millisecond
	jobs := make(chan catalog.Model)
	var wg sync.WaitGroup
	workers := s.cfg.ProbeConcurrency
	if workers > len(models) {
		workers = len(models)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for model := range jobs {
				if ctx.Err() != nil {
					return
				}
				s.probeModel(ctx, model, stats, cancel)
				if interval > 0 {
					select {
					case <-time.After(interval):
					case <-ctx.Done():
					}
				}
			}
		}()
	}
	for _, model := range models {
		if ctx.Err() != nil {
			break
		}
		select {
		case jobs <- model:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()

	s.log.Printf("probe finished: %d available, %d removed, %d inconclusive, %d aborted",
		stats.available, stats.gone, stats.failed, stats.aborted)
}

func (s *Server) probeModel(ctx context.Context, model catalog.Model, stats *probeStats, cancel context.CancelFunc) {
	req := &api.Request{
		Model: model.ID,
		Messages: []api.Message{{
			Role:  "user",
			Parts: []api.Part{{Type: "text", Text: "hi"}},
		}},
		MaxOutputTokens: probeMaxTokens,
		Stream:          true,
	}
	payload, _, err := buildPayload(req, model.Protocol, "")
	if err != nil {
		atomic.AddInt32(&stats.failed, 1)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		atomic.AddInt32(&stats.failed, 1)
		return
	}

	session := zen.SessionID("probe:" + model.PublicID)
	project := zen.ProjectID("global")
	wire := s.wireFor(session, project, model.Protocol)
	url := gateway.Endpoint(s.cfg, model, model.Protocol, model.ID)
	if wire.Query != "" {
		if strings.Contains(url, "?") {
			url += "&" + wire.Query
		} else {
			url += "?" + wire.Query
		}
	}

	lastQuota := false
	for round := 0; round < probeQuotaRetries; round++ {
		quota := false
		for _, exit := range s.pool.Candidates() {
			if !exit.Available(time.Now()) {
				continue
			}
			resp, err := s.pool.Do(ctx, gateway.Attempt{
				URL:     url,
				Headers: wire.Headers,
				Body:    body,
				Exit:    exit,
			}, time.Duration(s.cfg.ProbeTimeoutSeconds)*time.Second)
			if err != nil {
				continue
			}
			if resp.StatusCode/100 == 2 {
				resp.Body.Close()
				atomic.AddInt32(&stats.available, 1)
				s.logf("probe ok model=%s exit=%s", model.PublicID, exit.Name)
				return
			}
			reason, _, modelGone := gateway.Classify(resp, time.Duration(s.cfg.BlockSeconds)*time.Second, time.Duration(s.cfg.QuotaSeconds)*time.Second)
			switch {
			case modelGone:
				s.cat.Disable(model.PublicID, reason)
				atomic.AddInt32(&stats.gone, 1)
				s.logf("probe removed model=%s reason=%s", model.PublicID, reason)
				return
			case resp.StatusCode == http.StatusTooManyRequests:
				quota = true
				continue
			case resp.StatusCode == http.StatusUnauthorized:
				atomic.AddInt32(&stats.aborted, 1)
				s.log.Printf("probe aborted: upstream rejected the credential")
				cancel()
				return
			case resp.StatusCode == http.StatusForbidden:
				// A shape/exit rejection is not a property of the model.
				atomic.AddInt32(&stats.failed, 1)
				return
			}
		}
		lastQuota = quota
		if !quota {
			break
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
	if lastQuota {
		atomic.AddInt32(&stats.aborted, 1)
		s.log.Printf("probe aborted: free usage limit reached on every exit")
		cancel()
		return
	}
	atomic.AddInt32(&stats.failed, 1)
}
