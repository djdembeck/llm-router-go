package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/djdembeck/llm-router-go/web"
)

// ─── Config ───────────────────────────────────────────────────────────────

type Backend struct {
	Name                        string `json:"name"`
	URL                         string `json:"url"`
	MaxConcurrent               int    `json:"maxConcurrent"`               // 0 = unlimited
	Tier                        int    `json:"tier"`                        // 0 = king (always admitted), 1 = subject
	GpuWeight                   int    `json:"gpuWeight"`                   // GPU cost when tier-0 active; 0 = no cost
	BlockOnTier0                int    `json:"blockOnTier0"`                // block when tier0 in-flight >= this; 0 = disabled
	MaxQueueDepth               int    `json:"maxQueueDepth"`               // queued beyond maxConcurrent before 429 (default 2)
	MaxConcurrentLargePrefill   int    `json:"maxConcurrentLargePrefill"`   // max concurrent large prefills; 0 = disabled
	LargePrefillThresholdTokens int    `json:"largePrefillThresholdTokens"` // new-token count to qualify as "large prefill" (default 8192)
}

// ─── Parsed body (single-pass, heavy fields stay raw) ──────────────────────

type rawMessage struct {
	Content json.RawMessage `json:"content"`
}

type parsedBody struct {
	Model    string          `json:"model"`
	Messages []rawMessage    `json:"messages"`
	Prompt   json.RawMessage `json:"prompt"`
	Stream   bool            `json:"stream"`
}

// ─── Per-backend slot manager ─────────────────────────────────────────────

type slotManager struct {
	inflight int32 // atomic
	max      int32
	waiting  int32 // atomic; currently queued
	maxQueue int32
	notify   chan struct{}
}

func newSlot(max, maxQueue int32) *slotManager {
	return &slotManager{
		max:      max,
		maxQueue: maxQueue,
		notify:   make(chan struct{}, 1),
	}
}

// acquire returns true if a slot was obtained (immediately or after queued wait).
// false = queue full or timeout/cancel.
func (s *slotManager) acquire(grace time.Duration, cancel <-chan struct{}) bool {
	// Fast path: CAS a free slot, but never barge ahead of queued waiters.
	for {
		cur := atomic.LoadInt32(&s.inflight)
		if cur >= s.max || atomic.LoadInt32(&s.waiting) > 0 {
			break
		}
		if atomic.CompareAndSwapInt32(&s.inflight, cur, cur+1) {
			return true
		}
	}

	// Queue path: check depth, then wait.
	if atomic.AddInt32(&s.waiting, 1) > s.maxQueue {
		atomic.AddInt32(&s.waiting, -1)
		return false // queue full
	}
	defer atomic.AddInt32(&s.waiting, -1)

	// A slot may already be free (e.g. a waiter timed out without consuming
	// its notify). Try once before sleeping.
	select {
	case <-cancel:
		return false
	default:
	}
	for {
		cur := atomic.LoadInt32(&s.inflight)
		if cur >= s.max {
			break
		}
		if atomic.CompareAndSwapInt32(&s.inflight, cur, cur+1) {
			return true
		}
	}

	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	for {
		select {
		case <-s.notify:
		case <-deadline.C:
			return false
		case <-cancel:
			return false
		}
		// Retr-try CAS after wake.
		for {
			cur := atomic.LoadInt32(&s.inflight)
			if cur >= s.max {
				break // spurious, loop back to wait
			}
			if atomic.CompareAndSwapInt32(&s.inflight, cur, cur+1) {
				return true
			}
		}
	}
}

func (s *slotManager) release() {
	atomic.AddInt32(&s.inflight, -1)
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *slotManager) snapshot() (inflight, max, waiting, maxQueue int32) {
	return atomic.LoadInt32(&s.inflight), s.max, atomic.LoadInt32(&s.waiting), s.maxQueue
}

// ─── GPU budget (weighted semaphore, only active when tier-0 is busy) ──────

type gpuBudget struct {
	mu      sync.Mutex
	cond    *sync.Cond
	used    int
	max     int
	waiting int
}

func (g *gpuBudget) tryAcquire(weight, maxQueue int, grace time.Duration, cancel <-chan struct{}) bool {
	g.mu.Lock()
	if g.used+weight <= g.max {
		g.used += weight
		g.mu.Unlock()
		return true
	}
	// Queue.
	if g.waiting >= maxQueue {
		g.mu.Unlock()
		return false
	}
	g.waiting++
	var expired atomic.Bool
	done := make(chan struct{})
	timer := time.NewTimer(grace)
	go func() {
		select {
		case <-timer.C:
			expired.Store(true)
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-cancel:
			expired.Store(true)
			g.mu.Lock()
			g.cond.Broadcast()
			g.mu.Unlock()
		case <-done:
		}
	}()
	acquired := false
	for {
		g.cond.Wait()
		if g.used+weight <= g.max {
			// Fit wins even at the deadline boundary (same precedence as today).
			g.used += weight
			acquired = true
			break
		}
		if expired.Load() {
			break
		}
		// Woken by a partial release or lost the race to another waiter:
		// keep waiting until the deadline.
	}
	close(done)
	g.waiting--
	timer.Stop()
	g.mu.Unlock()
	return acquired
}

func (g *gpuBudget) release(weight int) {
	g.mu.Lock()
	g.used -= weight
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *gpuBudget) snapshot() (used, max, waiting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.max, g.waiting
}

// ─── EWMA duration tracker ────────────────────────────────────────────────

type durationTracker struct {
	mu    sync.Mutex
	ewma  float64 // seconds
	count int
}

func (d *durationTracker) record(dur time.Duration) {
	seconds := dur.Seconds()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		d.ewma = seconds
	} else {
		alpha := 0.3
		d.ewma = alpha*seconds + (1-alpha)*d.ewma
	}
	d.count++
}

func (d *durationTracker) avgSeconds() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		return 15.0 // default estimate
	}
	return d.ewma
}

// ─── Metrics registry (live observability) ────────────────────────────────

// requestSample is one completed (or rejected) request, kept in the burst
// ring and folded into per-backend counters. accepted marks the demand side
// that recordAccept registered at admission; only accepted samples are
// retired from the pending window on completion.
type requestSample struct {
	T            time.Time
	Backend      string
	Path         string
	Status       int
	Stream       bool
	DurMs        float64
	TTFTMs       float64
	NewTokensEst int
	CtxTok       int // est context tokens (all messages/prompt, ~4 bytes/token)
	BytesIn      int64
	BytesOut     int64
	accepted     bool
}

// backendMetrics holds per-backend lifetime counters (completed requests
// only), the streaming TTFT EWMA (updated at first response byte), and the
// pending window — requests accepted but not yet completed, which feeds the
// live rates. Rates measure arrival, not completion: a decode can run for
// minutes, but the request's demand began the moment it was admitted.
type backendMetrics struct {
	reqTotal      int64
	bytesInTotal  int64
	bytesOutTotal int64
	tokEstTotal   int64
	ttftEwma      float64 // ms — EWMA over first-byte times, updated at first byte
	ttftSamples   int
	ttftMsNow     float64 // ms — the most recent first-byte sample (no decay)
	reqRate       float64
	bytesRate     float64
	tokEstRate    float64
	// pending: demand recorded at admission, not yet completed. computeRates
	// divides it by the tick interval to get the live rate, then decays the
	// remainder back by (interval / window).
	pending       float64
	pendingReq    float64
	pendingTokEst float64
}

// rateWindowS is how far back the live-rate window reaches. A single tick's
// arrivals produce a full-window rate, so even sparse traffic shows a real
// sustained rate instead of a 2×-per-tick spike that returns to zero.
const rateWindowS = 30

// backendMetricSnapshot is the lock-free view of backendMetrics used by the
// frame builder.
type backendMetricSnapshot struct {
	TTFTMs          float64
	TTFTSampleCount int
	TTFTMsNow       float64
	ReqRate         float64
	BytesRate       float64
	TokEstRate      float64
	ReqTotal        int64
	BytesInTotal    int64
	BytesOutTotal   int64
	TokEstTotal     int64
}

// metricsRegistry is the mutex-guarded home for request counters, the TTFT
// EWMA, and the bounded burst ring (newest appended, oldest evicted).
type metricsRegistry struct {
	mu       sync.Mutex
	backends map[string]*backendMetrics
	ring     []requestSample
	ringCap  int
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{backends: map[string]*backendMetrics{}, ringCap: 500}
}

var metrics = newMetricsRegistry()

// recordAccept records the demand side of a request the moment it is
// admitted for proxying: the pending window counters that feed the live
// rates. Rejections (429/413) are not "accepted" — they never enter a
// backend — and are not recorded here.
func (m *metricsRegistry) recordAccept(backend string, bytesIn int64, tokEst int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		bm = &backendMetrics{}
		m.backends[backend] = bm
	}
	bm.pendingReq += 1
	bm.pending += float64(bytesIn)
	bm.pendingTokEst += float64(tokEst)
}

// recordFirstByte captures TTFT at first response byte, while the request is
// still live — not at completion, which would lag the operator by the full
// decode. Only streaming requests carry a first-byte time; the EWMA updates
// here so the dashboard sees the real current value as it arrives.
func (m *metricsRegistry) recordFirstByte(backend string, ttftMs float64) {
	if ttftMs <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		return
	}
	if bm.ttftSamples == 0 {
		bm.ttftEwma = ttftMs
	} else {
		const alpha = 0.3
		bm.ttftEwma = alpha*ttftMs + (1-alpha)*bm.ttftEwma
	}
	bm.ttftSamples++
	bm.ttftMsNow = ttftMs
}

// recordRequest folds one COMPLETED (or rejected) request into per-backend
// lifetime counters and the burst ring, and retires it from the pending
// window if it had been accepted. Lifetime totals count completed work only;
// the pending counters are the live-rate source until completion.
func (m *metricsRegistry) recordRequest(backend string, s requestSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		bm = &backendMetrics{}
		m.backends[backend] = bm
	}
	// Retire the pending demand for requests that were actually accepted and
	// proxied. Router-originated rejections (413/429) never enter the
	// pending window — counting them would inflate the live rate with
	// demand that never reached a backend.
	if s.accepted {
		bm.pendingReq--
		bm.pending -= float64(s.BytesIn)
		bm.pendingTokEst -= float64(s.NewTokensEst)
	}
	bm.reqTotal++
	bm.bytesInTotal += s.BytesIn
	bm.bytesOutTotal += s.BytesOut
	bm.tokEstTotal += int64(s.NewTokensEst)
	m.ring = append(m.ring, s)
	if len(m.ring) > m.ringCap {
		m.ring = m.ring[len(m.ring)-m.ringCap:]
	}
}

// burst returns up to limit ring samples, oldest first (newest last), capped
// at the ring length.
func (m *metricsRegistry) burst(limit int) []requestSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.ring)
	if limit > n {
		limit = n
	}
	if limit < 0 {
		limit = 0
	}
	out := make([]requestSample, limit)
	copy(out, m.ring[n-limit:])
	return out
}

// computeRates refreshes per-backend req/bytes/tok-est live rates. Each tick,
// the whole pending window is divided by the tick interval — a request
// admitted this tick contributes a full window-length rate — and then the
// window decays back by (interval / window) so its tail lingers for the
// rest of rateWindowS instead of vanishing on the next tick. The result is a
// sustained rate that tracks arrivals, not completion deltas: a 10-minute
// decode shows its true admission rate from the moment it was accepted.
func (m *metricsRegistry) computeRates(dt time.Duration) {
	if dt <= 0 {
		return
	}
	dtS := dt.Seconds()
	window := float64(rateWindowS)
	if window < dtS {
		window = dtS
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bm := range m.backends {
		bm.reqRate = bm.pendingReq / dtS
		bm.bytesRate = float64(bm.pending) / dtS
		bm.tokEstRate = bm.pendingTokEst / dtS
		decay := dtS / window
		bm.pendingReq *= 1 - decay
		bm.pending *= 1 - decay
		bm.pendingTokEst *= 1 - decay
	}
}

func (m *metricsRegistry) snapshot(backend string) backendMetricSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var zero backendMetricSnapshot
	bm, ok := m.backends[backend]
	if !ok {
		return zero
	}
	return backendMetricSnapshot{
		TTFTMs:          bm.ttftEwma,
		TTFTSampleCount: bm.ttftSamples,
		TTFTMsNow:       bm.ttftMsNow,
		ReqRate:         bm.reqRate,
		BytesRate:       bm.bytesRate,
		TokEstRate:      bm.tokEstRate,
		ReqTotal:        bm.reqTotal,
		BytesInTotal:    bm.bytesInTotal,
		BytesOutTotal:   bm.bytesOutTotal,
		TokEstTotal:     bm.tokEstTotal,
	}
}

// ─── Engine metrics (the backends' own /metrics) ──────────────────────────
//
// The router measures demand at admission; the engine measures execution.
// In-engine running/waiting, KV-cache pressure, and REAL token throughput
// (prompt + completion — not the ~4 bytes/token body estimate) only exist
// inside the engine, so we scrape each backend's Prometheus /metrics with a
// stdlib text-exposition parser at 1s. vLLM always serves /metrics; SGLang
// requires --enable-metrics. When the endpoint is absent or down the view
// degrades to status "off"/"err" and the sheet renders "—" — never a guess.
//
// Aggregation rules (from the engines' source, verified 2026-08-20):
//   - vLLM gauges are labeled [model_name, engine]; request counts SUM
//     across data-parallel engines, KV usage is per-pool so MAX.
//   - SGLang scheduler gauges are per tp/pp/moe rank (multiprocess
//     mostrecent) and replicate the same batch state — MAX across ranks.
//   - Token counters are rate-differenced between scrapes; a counter reset
//     (engine restart) re-baselines instead of going negative.
//   - TTFT mean comes from the histogram's _sum/_count deltas.
//   - SGLang realtime_tokens_total{mode}: prefill = prefill_compute +
//     prefill_cache, decode = decode.

// engineView is the per-backend engine truth published in the frame (v2).
// Zero + status != "ok" means "no data" — the client renders "—".
//
// Field classes:
//   - gauges — live right now (running, waiting, kvPct, kvHeldPct, hitRate,
//     pool *Pct, *Tok pool stats, retracted, sloCap, ctxLen, *MemGB, hicache)
//   - interval means — histogram means over the last scrape interval
//     (ttftMs, itlMs, e2eMs, queueMs, prefillMs, decodeMs, perTokMs,
//     meanPromptTok, meanGenTok, ttftStreamMs, ttftNonStreamMs)
//   - rates — counter deltas over the last scrape interval (prefillTokS,
//     prefillCacheTokS, decodeTokS, retractedTokS)
//   - lifetime — engine-side counters since its own start (reqDoneTotal,
//     preemptedTotal, abortedTotal, finReasons, *Total token/req counters)
//
// On a non-ok scrape ("off"/"err") the interval fields and rates are zeroed
// (stale deltas would be misleading), while gauges and lifetime fields keep
// their last known values: they are lifetime facts, and an engine that
// restarts legitimately re-zeros them on its next ok scrape.
//
// Optional fields are pointers; nil → JSON null when the engine/feature
// doesn't report them (e.g. Mamba pool gauges absent on a non-hybrid model).
type engineView struct {
	Status string `json:"status"` // "ok" | "off" | "err"
	Engine string `json:"engine"` // "vllm" | "sglang" | ""

	// Core gauges + rates (frozen field names).
	Running          float64  `json:"running"`
	Waiting          float64  `json:"waiting"`
	KVPct            float64  `json:"kvPct"`     // 0..100 (live use only)
	KVHeldPct        *float64 `json:"kvHeldPct"` // 0..100; sglang: prefix-cached (held) share of pool pressure
	HitRate          float64  `json:"hitRate"`
	PrefillTokS      float64  `json:"prefillTokS"`
	PrefillCacheTokS float64  `json:"prefillCacheTokS"`
	DecodeTokS       float64  `json:"decodeTokS"`
	TTFTms           float64  `json:"ttftMs"`

	// Interval means (ms).
	ITLms   float64 `json:"itlMs"`
	E2EMs   float64 `json:"e2eMs"`
	QueueMs float64 `json:"queueMs"`

	// Optional per-stage means (ms) — pointer; nil when the family/feature
	// doesn't report them (prefill/decode/perTok are vLLM-only, the TTFT
	// stream split is SGLang-only).
	PrefillMs *float64 `json:"prefillMs"`
	DecodeMs  *float64 `json:"decodeMs"`
	PerTokMs  *float64 `json:"perTokMs"`

	// Per-request means, last interval (tokens, NOT ms).
	MeanPromptTok float64 `json:"meanPromptTok"`
	MeanGenTok    float64 `json:"meanGenTok"`

	// Optional gauges (nil = not reported).
	WaitCap            *float64 `json:"waitCap"`
	WaitDefer          *float64 `json:"waitDefer"`
	Retracted          *float64 `json:"retracted"`
	RetractedTokS      *float64 `json:"retractedTokS"`
	FullPct            *float64 `json:"fullPct"`
	SwaPct             *float64 `json:"swaPct"`
	MambaPct           *float64 `json:"mambaPct"`
	KVUsedTok          *float64 `json:"kvUsedTok"`
	KVCapTok           *float64 `json:"kvCapTok"`
	KVFreeTok          *float64 `json:"kvFreeTok"`
	KVEvictTok         *float64 `json:"kvEvictTok"`
	MambaUsedTok       *float64 `json:"mambaUsedTok"`
	MambaCapTok        *float64 `json:"mambaCapTok"`
	HiCacheHostUsedTok *float64 `json:"hicacheHostUsedTok"`
	HiCacheHostCapTok  *float64 `json:"hicacheHostCapTok"`
	TTFTStreamMs       *float64 `json:"ttftStreamMs"`
	TTFTNonStreamMs    *float64 `json:"ttftNonStreamMs"`
	KVMemGB            *float64 `json:"kvMemGB"`
	WeightMemGB        *float64 `json:"weightMemGB"`
	SLOCap             *float64 `json:"sloCap"`
	CTXLen             *float64 `json:"ctxLen"`

	// Optional lifetime counters (nil = not reported).
	PreemptedTotal     *int64 `json:"preemptedTotal"`
	AbortedTotal       *int64 `json:"abortedTotal"`
	StreamDoneTotal    *int64 `json:"streamDoneTotal"`
	NonStreamDoneTotal *int64 `json:"nonStreamDoneTotal"`
	PromptTokTotal     *int64 `json:"promptTokTotal"`
	GenTokTotal        *int64 `json:"genTokTotal"`
	HitQueriesTotal    *int64 `json:"hitQueriesTotal"`
	HitHitsTotal       *int64 `json:"hitHitsTotal"`
	MmQueriesTotal     *int64 `json:"mmQueriesTotal"`
	MmHitsTotal        *int64 `json:"mmHitsTotal"`

	ReqDoneTotal int64            `json:"reqDoneTotal"`
	FinReasons   map[string]int64 `json:"finReasons"` // vllm only; nil for sglang
}

// promSample is one line of a Prometheus text exposition.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

// histState tracks the previous _sum/_count pair for one histogram state
// key. A key is the base name (blended over all label sets) or
// base+"|"+is_streaming for SGLang's is_streaming-split histograms.
type histState struct {
	sum, cnt float64
	t        time.Time
	lastSum  float64
	have     bool // a delta mean has been computed (else lastSum is stale/zero)
}

type engineScrape struct {
	url    string
	status string
	view   engineView
	// counters: rate-differenced between scrapes, keyed by name+labels.
	counters map[string]float64
	cTimes   map[string]time.Time
	// histograms: the ones the dashboard reads, keyed per label set.
	hists   map[string]*histState
	lastT   time.Time
	seenEng map[string]bool
}

type engineRegistry struct {
	mu       sync.Mutex
	backends map[string]*engineScrape
	client   *http.Client
}

func newEngineRegistry() *engineRegistry {
	return &engineRegistry{
		backends: map[string]*engineScrape{},
		client:   &http.Client{Timeout: 2 * time.Second},
	}
}

func (e *engineRegistry) add(name, url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backends[name] = &engineScrape{
		url:      url,
		status:   "off",
		counters: map[string]float64{},
		cTimes:   map[string]time.Time{},
		hists:    map[string]*histState{},
		seenEng:  map[string]bool{},
	}
}

func (e *engineRegistry) view(backend string) engineView {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.backends[backend]; ok {
		return s.view
	}
	return engineView{}
}

// loop scrapes every backend every second until the process exits.
func (e *engineRegistry) loop() {
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		e.mu.Lock()
		names := make([]string, 0, len(e.backends))
		for n := range e.backends {
			names = append(names, n)
		}
		e.mu.Unlock()
		for _, n := range names {
			e.scrape(n)
		}
	}
}

func (e *engineRegistry) scrape(name string) {
	e.mu.Lock()
	s, ok := e.backends[name]
	if !ok {
		e.mu.Unlock()
		return
	}
	url := s.url
	e.mu.Unlock()

	body, gone, ok := func() ([]byte, bool, bool) {
		resp, err := e.client.Get(url)
		if err != nil {
			return nil, false, false
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			return nil, true, false
		}
		if resp.StatusCode != http.StatusOK {
			return nil, false, false
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, false, false
		}
		return b, false, true
	}()

	e.mu.Lock()
	defer e.mu.Unlock()
	if !ok {
		if gone {
			s.status = "off"
		} else {
			s.status = "err"
		}
		// Rates and interval means are deltas over a scrape interval; a stale
		// delta would mislead, so zero them. Gauges and lifetime counters keep
		// their last known values (lifetime facts).
		s.zeroIntervalFields()
		return
	}
	s.scrapeBody(parsePromText(body))
}

// zeroIntervalFields blanks the rate + interval-mean fields (stale deltas
// would mislead) while keeping gauges and lifetime fields at their last
// known values. status is read from the scrape state.
func (s *engineScrape) zeroIntervalFields() {
	v := s.view
	v.Status = s.status
	v.KVHeldPct = nil // derived from this scrape's gauges — not a lifetime fact
	v.PrefillTokS = 0
	v.PrefillCacheTokS = 0
	v.DecodeTokS = 0
	v.TTFTms = 0
	v.ITLms = 0
	v.E2EMs = 0
	v.QueueMs = 0
	v.MeanPromptTok = 0
	v.MeanGenTok = 0
	if v.TTFTStreamMs != nil {
		*v.TTFTStreamMs = 0
	}
	if v.TTFTNonStreamMs != nil {
		*v.TTFTNonStreamMs = 0
	}
	if v.RetractedTokS != nil {
		*v.RetractedTokS = 0
	}
	if v.PrefillMs != nil {
		*v.PrefillMs = 0
	}
	if v.DecodeMs != nil {
		*v.DecodeMs = 0
	}
	if v.PerTokMs != nil {
		*v.PerTokMs = 0
	}
	s.view = v
}

// scrapeBody folds one parsed exposition into the scrape state.
//
// Lifetime counters (the *_Total fields, finReasons, reqDoneTotal) are
// assigned the LATEST raw value each ok scrape, with no reset logic: when an
// engine restarts its own lifetime counters re-zero, so the latest value is
// always the honest engine-side lifetime. Rates (prefillTokS, ...) and
// interval histogram means come from counterRate/updateHist deltas and are
// re-baselined on reset.
func (s *engineScrape) scrapeBody(samples []promSample) {
	now := time.Now()
	if len(samples) == 0 {
		s.status = "err"
		s.zeroIntervalFields()
		return
	}

	// First pass: family detection + gauge accumulation.
	vllm, sg := false, false
	var running, waiting float64
	var kvMax float64
	haveKV := false
	var waitCap, waitDefer float64
	haveCap, haveDefer := false, false
	var hitRate, sloCap, ctxLen float64
	var haveSLO, haveCTX bool
	var kvMemGB, weightMemGB float64
	var haveKVMem, haveWMem bool
	var retracted float64
	haveRetr := false
	var haveRetrTok bool
	var fullPct, swaPct, mambaPct float64
	var haveFull, haveSWA, haveMamba bool
	var kvUsed, kvCap, kvFree, kvEvict float64
	var haveKVUsed, haveKVCap, haveKVFree, haveKVEvict bool
	var mambaUsed, mambaAvail, mambaEvict float64
	var haveMambaUsed, haveMambaAvail, haveMambaEvict bool
	var hicacheUsed, hicacheCap float64
	var haveHCUsed, haveHCCap bool
	var preempted, aborted, streamDone, nonStreamDone int64
	var havePreempted, haveAborted, haveStreamDone, haveNonStreamDone bool
	var promptTok, genTok int64
	var havePromptTok, haveGenTok bool
	var hitQ, hitH, mmQ, mmH int64
	var haveHitQ, haveHitH, haveMMQ, haveMMH bool
	finReasons := map[string]int64{}
	haveFin := false
	for _, sm := range samples {
		switch {
		case strings.HasPrefix(sm.name, "vllm:"):
			vllm = true
			switch sm.name {
			case "vllm:num_requests_running":
				running += sm.value // sum across data-parallel engines
			case "vllm:num_requests_waiting":
				waiting += sm.value
			case "vllm:num_requests_waiting_by_reason":
				switch sm.labels["reason"] {
				case "capacity":
					waitCap += sm.value
					haveCap = true
				case "deferred":
					waitDefer += sm.value
					haveDefer = true
				}
			case "vllm:kv_cache_usage_perc":
				if sm.value > kvMax {
					kvMax = sm.value
					haveKV = true
				}
			case "vllm:num_preemptions_total":
				preempted += int64(sm.value)
				havePreempted = true
			case "vllm:request_success_total":
				finReasons[sm.labels["finished_reason"]] += int64(sm.value)
				haveFin = true
			case "vllm:prefix_cache_queries_total":
				hitQ += int64(sm.value)
				haveHitQ = true
			case "vllm:prefix_cache_hits_total":
				hitH += int64(sm.value)
				haveHitH = true
			case "vllm:mm_cache_queries_total":
				mmQ += int64(sm.value)
				haveMMQ = true
			case "vllm:mm_cache_hits_total":
				mmH += int64(sm.value)
				haveMMH = true
			}
		case strings.HasPrefix(sm.name, "sglang:"):
			sg = true
			switch sm.name {
			case "sglang:num_running_reqs":
				if sm.value > running {
					running = sm.value
				}
			case "sglang:num_queue_reqs":
				if sm.value > waiting {
					waiting = sm.value
				}
			case "sglang:token_usage":
				if sm.value > kvMax {
					kvMax = sm.value
					haveKV = true
				}
			case "sglang:full_token_usage":
				if sm.value > fullPct {
					fullPct = sm.value
					haveFull = true
				}
			case "sglang:swa_token_usage":
				if sm.value > swaPct {
					swaPct = sm.value
					haveSWA = true
				}
			case "sglang:mamba_usage":
				if sm.value > mambaPct {
					mambaPct = sm.value
					haveMamba = true
				}
			case "sglang:num_used_tokens":
				if sm.value > kvUsed {
					kvUsed = sm.value
					haveKVUsed = true
				}
			case "sglang:max_total_num_tokens":
				if sm.value > kvCap {
					kvCap = sm.value
					haveKVCap = true
				}
			case "sglang:kv_available_tokens":
				if sm.value > kvFree {
					kvFree = sm.value
					haveKVFree = true
				}
			case "sglang:kv_evictable_tokens":
				if sm.value > kvEvict {
					kvEvict = sm.value
					haveKVEvict = true
				}
			case "sglang:mamba_used_tokens":
				if sm.value > mambaUsed {
					mambaUsed = sm.value
					haveMambaUsed = true
				}
			case "sglang:mamba_available_tokens":
				if sm.value > mambaAvail {
					mambaAvail = sm.value
					haveMambaAvail = true
				}
			case "sglang:mamba_evictable_tokens":
				if sm.value > mambaEvict {
					mambaEvict = sm.value
					haveMambaEvict = true
				}
			case "sglang:num_retracted_reqs":
				if sm.value > retracted {
					retracted = sm.value
					haveRetr = true
				}
			case "sglang:cache_hit_rate":
				if sm.value > hitRate {
					hitRate = sm.value
				}
			case "sglang:max_running_requests_under_SLO":
				if sm.value > sloCap {
					sloCap = sm.value
					haveSLO = true
				}
			case "sglang:context_len":
				if sm.value > ctxLen {
					ctxLen = sm.value
					haveCTX = true
				}
			case "sglang:kv_cache_memory_usage_gb":
				if sm.value > kvMemGB {
					kvMemGB = sm.value
					haveKVMem = true
				}
			case "sglang:weight_memory_usage_gb":
				if sm.value > weightMemGB {
					weightMemGB = sm.value
					haveWMem = true
				}
			case "sglang:hicache_host_used_tokens":
				if sm.value > hicacheUsed {
					hicacheUsed = sm.value
					haveHCUsed = true
				}
			case "sglang:hicache_host_total_tokens":
				if sm.value > hicacheCap {
					hicacheCap = sm.value
					haveHCCap = true
				}
			case "sglang:num_aborted_requests_total":
				aborted += int64(sm.value)
				haveAborted = true
			case "sglang:num_retracted_input_tokens_total":
				haveRetrTok = true
			case "sglang:num_requests_total":
				if sm.labels["is_streaming"] == "true" {
					streamDone += int64(sm.value)
					haveStreamDone = true
				} else {
					nonStreamDone += int64(sm.value)
					haveNonStreamDone = true
				}
			case "sglang:prompt_tokens_total":
				promptTok += int64(sm.value)
				havePromptTok = true
			case "sglang:generation_tokens_total":
				genTok += int64(sm.value)
				haveGenTok = true
			}
		}
	}

	s.status = "ok"
	// A new ok scrape publishes a fresh view: optional fields default to nil
	// (null) and are re-filled only when the engine reports them.
	view := engineView{Status: "ok"}

	// Family: the most recently detected engine wins the view; both stay
	// recorded in seenEng for the Engine label.
	eng := ""
	if vllm {
		s.seenEng["vllm"] = true
		eng = "vllm"
	}
	if sg {
		s.seenEng["sglang"] = true
		eng = "sglang"
	}
	if eng == "" {
		if s.seenEng["vllm"] {
			eng = "vllm"
		} else if s.seenEng["sglang"] {
			eng = "sglang"
		}
	}
	view.Engine = eng
	if vllm || sg {
		view.Running = running
		view.Waiting = waiting
	}
	if haveKV {
		view.KVPct = kvMax * 100
	}

	// Optional gauges: pointer only when the metric appeared this scrape.
	if vllm {
		if haveCap {
			v := waitCap
			view.WaitCap = &v
		}
		if haveDefer {
			v := waitDefer
			view.WaitDefer = &v
		}
		if havePreempted {
			view.PreemptedTotal = &preempted
		}
		if haveFin {
			view.FinReasons = finReasons
		}
		if haveHitQ {
			view.HitQueriesTotal = &hitQ
		}
		if haveHitH {
			view.HitHitsTotal = &hitH
		}
		if hitQ != 0 {
			view.HitRate = float64(hitH) / float64(hitQ)
		}
		if haveMMQ {
			view.MmQueriesTotal = &mmQ
		}
		if haveMMH {
			view.MmHitsTotal = &mmH
		}
	} else if sg {
		if haveRetr {
			v := retracted
			view.Retracted = &v
		}
		if haveFull {
			v := fullPct * 100
			view.FullPct = &v
		}
		if haveSWA {
			v := swaPct * 100
			view.SwaPct = &v
		}
		if haveMamba {
			v := mambaPct * 100
			view.MambaPct = &v
		}
		if haveKVUsed {
			v := kvUsed
			view.KVUsedTok = &v
		}
		if haveKVCap {
			v := kvCap
			view.KVCapTok = &v
		}
		if haveKVFree {
			v := kvFree
			view.KVFreeTok = &v
		}
		if haveKVEvict {
			v := kvEvict
			view.KVEvictTok = &v
		}
		// Total KV pressure = live use (kvPct) + the prefix-cached (held)
		// portion. Held share = the cached part of the tokens the engine can
		// still hand out: 100·evictable/(available+evictable) — it reaches
		// 100 only when the hand-out pool is entirely warmed cache. Fallback
		// (evictable absent): the pool's residual, cap−free−used. vLLM never
		// sets this: its kv_cache_usage_perc already counts cached blocks.
		var held float64
		haveHeld := false
		if haveKVEvict && haveKVFree && haveKVUsed {
			if kvFree+kvEvict > 0 {
				held = 100 * kvEvict / (kvFree + kvEvict)
				haveHeld = true
			}
		} else if haveKVUsed && haveKVCap && haveKVFree {
			if kvCap > 0 {
				held = 100 * (kvCap - kvFree - kvUsed) / kvCap
				haveHeld = true
			}
		}
		if haveHeld {
			v := held
			view.KVHeldPct = &v
		}
		if haveMambaUsed || haveMambaAvail || haveMambaEvict {
			u := mambaUsed
			c := mambaUsed + mambaAvail + mambaEvict
			view.MambaUsedTok = &u
			view.MambaCapTok = &c
		}
		if haveHCUsed {
			v := hicacheUsed
			view.HiCacheHostUsedTok = &v
		}
		if haveHCCap {
			v := hicacheCap
			view.HiCacheHostCapTok = &v
		}
		if hitRate > 0 {
			if hitRate > 1 { // some builds report a percent, not a fraction
				hitRate /= 100
			}
			view.HitRate = hitRate
		}
		if haveSLO {
			v := sloCap
			view.SLOCap = &v
		}
		if haveCTX {
			v := ctxLen
			view.CTXLen = &v
		}
		if haveKVMem {
			v := kvMemGB
			view.KVMemGB = &v
		}
		if haveWMem {
			v := weightMemGB
			view.WeightMemGB = &v
		}
		if haveAborted {
			view.AbortedTotal = &aborted
		}
		if haveStreamDone {
			view.StreamDoneTotal = &streamDone
		}
		if haveNonStreamDone {
			view.NonStreamDoneTotal = &nonStreamDone
		}
		if havePromptTok {
			view.PromptTokTotal = &promptTok
		}
		if haveGenTok {
			view.GenTokTotal = &genTok
		}
		if haveStreamDone || haveNonStreamDone {
			view.ReqDoneTotal = streamDone + nonStreamDone
		}
	}

	// Second pass: token counter rates + histograms, in the detected family.
	if vllm {
		var promptTokS float64
		var bySrc bool
		for _, sm := range samples {
			switch sm.name {
			case "vllm:prompt_tokens_by_source_total":
				switch sm.labels["source"] {
				case "local_compute":
					view.PrefillTokS += s.counterRate(sm, now)
					bySrc = true
				case "local_cache_hit":
					view.PrefillCacheTokS += s.counterRate(sm, now)
					bySrc = true
				}
			case "vllm:prompt_tokens_total":
				promptTokS += s.counterRate(sm, now)
			case "vllm:generation_tokens_total":
				view.DecodeTokS += s.counterRate(sm, now)
			}
		}
		// Compute-only prefill comes from by_source when present; older vLLM
		// builds without it fall back to prompt_tokens_total (0 cache rate).
		if !bySrc {
			view.PrefillTokS = promptTokS
		}
		// Lifetime token totals: latest raw counter values (sum across engines).
		var vllmPrompt, vllmGen int64
		haveP, haveG := false, false
		for _, sm := range samples {
			switch sm.name {
			case "vllm:prompt_tokens_total":
				vllmPrompt += int64(sm.value)
				haveP = true
			case "vllm:generation_tokens_total":
				vllmGen += int64(sm.value)
				haveG = true
			}
		}
		if haveP {
			view.PromptTokTotal = &vllmPrompt
		}
		if haveG {
			view.GenTokTotal = &vllmGen
		}
		for _, base := range []string{
			"vllm:time_to_first_token_seconds",
			"vllm:inter_token_latency_seconds",
			"vllm:e2e_request_latency_seconds",
			"vllm:request_queue_time_seconds",
			"vllm:request_prefill_time_seconds",
			"vllm:request_decode_time_seconds",
			"vllm:request_time_per_output_token_seconds",
		} {
			s.updateHist(base, samples, now)
		}
		s.updateHist("vllm:request_prompt_tokens", samples, now)
		s.updateHist("vllm:request_generation_tokens", samples, now)
		view.TTFTms = s.histMean("vllm:time_to_first_token_seconds")
		view.ITLms = s.histMean("vllm:inter_token_latency_seconds")
		view.E2EMs = s.histMean("vllm:e2e_request_latency_seconds")
		view.QueueMs = s.histMean("vllm:request_queue_time_seconds")
		if v := s.histMean("vllm:request_prefill_time_seconds"); s.histHas("vllm:request_prefill_time_seconds") {
			view.PrefillMs = &v
		}
		if v := s.histMean("vllm:request_decode_time_seconds"); s.histHas("vllm:request_decode_time_seconds") {
			view.DecodeMs = &v
		}
		if v := s.histMean("vllm:request_time_per_output_token_seconds"); s.histHas("vllm:request_time_per_output_token_seconds") {
			view.PerTokMs = &v
		}
		// Token histograms: the mean is already in tokens, no ms conversion.
		view.MeanPromptTok = s.histMeanRaw("vllm:request_prompt_tokens")
		view.MeanGenTok = s.histMeanRaw("vllm:request_generation_tokens")
		if haveFin {
			for _, n := range finReasons {
				view.ReqDoneTotal += n
			}
		}
	} else if sg {
		var retrTokS float64
		// The retracted-token rate is published only once every rank's
		// counter has baselined; until then each scrape is baseline-only,
		// so a mid-scale-up rank never yields a bogus single-rank rate.
		retrTokReady := haveRetrTok
		for _, x := range samples {
			if x.name == "sglang:num_retracted_input_tokens_total" && !s.counterSeen(x) {
				retrTokReady = false
				break
			}
		}
		for _, sm := range samples {
			if sm.name == "sglang:realtime_tokens_total" {
				switch sm.labels["mode"] {
				case "prefill_compute", "prefill_cache":
					view.PrefillTokS += s.counterRate(sm, now)
				case "decode":
					view.DecodeTokS += s.counterRate(sm, now)
				}
			}
			if sm.name == "sglang:num_retracted_input_tokens_total" {
				if retrTokReady {
					retrTokS += s.counterRate(sm, now)
				} else {
					s.counterRate(sm, now) // baseline only
				}
			}
		}
		for _, base := range []string{
			"sglang:time_to_first_token_seconds",
			"sglang:inter_token_latency_seconds",
			"sglang:e2e_request_latency_seconds",
			"sglang:queue_time_seconds",
		} {
			s.updateHist(base, samples, now)
		}
		s.updateHist("sglang:prompt_tokens_histogram", samples, now)
		s.updateHist("sglang:generation_tokens_histogram", samples, now)
		view.TTFTms = s.histMean("sglang:time_to_first_token_seconds")
		view.ITLms = s.histMean("sglang:inter_token_latency_seconds")
		view.E2EMs = s.histMean("sglang:e2e_request_latency_seconds")
		view.QueueMs = s.histMean("sglang:queue_time_seconds")
		if v := s.histMean("sglang:time_to_first_token_seconds|true"); s.histHas("sglang:time_to_first_token_seconds|true") {
			view.TTFTStreamMs = &v
		}
		if v := s.histMean("sglang:time_to_first_token_seconds|false"); s.histHas("sglang:time_to_first_token_seconds|false") {
			view.TTFTNonStreamMs = &v
		}
		view.MeanPromptTok = s.histMeanRaw("sglang:prompt_tokens_histogram")
		view.MeanGenTok = s.histMeanRaw("sglang:generation_tokens_histogram")
		if haveRetrTok {
			v := retrTokS
			view.RetractedTokS = &v
		}
	}
	s.lastT = now
	s.view = view
}

// histMean is the mean (ms) over the most recent scrape interval for a
// state key; 0 until a delta has been computed.
func (s *engineScrape) histMean(key string) float64 {
	if h, ok := s.hists[key]; ok && h.have {
		return h.lastSum * 1000
	}
	return 0
}

// histMeanRaw is like histMean but the histogram is already in the unit we
// display (e.g. token counts — no seconds→ms conversion).
func (s *engineScrape) histMeanRaw(key string) float64 {
	if h, ok := s.hists[key]; ok && h.have {
		return h.lastSum
	}
	return 0
}

func (s *engineScrape) histHas(key string) bool {
	h, ok := s.hists[key]
	return ok && h.have
}

// counterKey distinguishes samples of the same metric with different
// labels (mode, is_streaming, engine, rank...).
func counterKey(sm promSample) string {
	keys := make([]string, 0, len(sm.labels))
	for k := range sm.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(sm.name)
	for _, k := range keys {
		b.WriteByte('\x00')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(sm.labels[k])
	}
	return b.String()
}

// counterRate returns Δ/Δt for one counter sample. First sight and counter
// resets (engine restart) re-baseline: rate 0 for this scrape.
func (s *engineScrape) counterRate(sm promSample, now time.Time) float64 {
	k := counterKey(sm)
	rate := 0.0
	if prev, ok := s.counters[k]; ok {
		if dt := now.Sub(s.cTimes[k]).Seconds(); dt > 0 {
			if sm.value >= prev {
				rate = (sm.value - prev) / dt
			} else {
				rate = 0 // reset: re-baseline below
			}
		}
	}
	s.counters[k] = sm.value
	s.cTimes[k] = now
	return rate
}

// updateHist folds _sum/_count samples into histogram state. Each sample
// feeds the blended base key (all label sets summed — the existing behavior
// for the core mean) and, when it carries an is_streaming label, also its
// per-label key base+"|"+value, so SGLang's split histograms get per-label
// means on top of the blended one.
func (s *engineScrape) updateHist(base string, samples []promSample, now time.Time) {
	type acc struct{ sum, cnt float64 }
	keyed := map[string]*acc{}
	for _, sm := range samples {
		if sm.name != base+"_sum" && sm.name != base+"_count" {
			continue
		}
		keys := []string{base}
		if v, ok := sm.labels["is_streaming"]; ok {
			keys = append(keys, base+"|"+v)
		}
		for _, k := range keys {
			a, ok := keyed[k]
			if !ok {
				a = &acc{}
				keyed[k] = a
			}
			if sm.name == base+"_sum" {
				a.sum += sm.value
			} else {
				a.cnt += sm.value
			}
		}
	}
	for key, a := range keyed {
		// A histogram with no observations yet emits no lines — skip.
		if a.sum == 0 && a.cnt == 0 {
			continue
		}
		h, ok := s.hists[key]
		if !ok {
			h = &histState{}
			s.hists[key] = h
		}
		if h.t.IsZero() {
			h.sum, h.cnt = a.sum, a.cnt
		} else if a.sum >= h.sum && a.cnt >= h.cnt {
			if d := a.cnt - h.cnt; d > 0 {
				h.lastSum = (a.sum - h.sum) / d
			}
			h.have = true
			h.sum, h.cnt = a.sum, a.cnt
		} else {
			h.sum, h.cnt = a.sum, a.cnt // reset: re-baseline
			h.have = false
		}
		h.t = now
	}
}

// counterSeen reports whether a counter sample was already baselined on a
// previous scrape.
func (s *engineScrape) counterSeen(sm promSample) bool {
	_, ok := s.counters[counterKey(sm)]
	return ok
}

// ─── Prometheus text parser (stdlib, dashboard needs only) ────────────────

// parsePromText parses the Prometheus text exposition format well enough
// for gauges, counters, and histogram _sum/_count parts. HELP/TYPE lines,
// openmetrics timestamps, and unparseable lines are skipped.
func parsePromText(body []byte) []promSample {
	var out []promSample
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, " \r")
		if line == "" || line[0] == '#' {
			continue
		}
		// Value is the last whitespace-delimited token; drop an optional
		// trailing epoch-seconds timestamp first.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val := fields[len(fields)-1]
		head := fields[0]
		// Some collectors emit "name{labels} value ts" — head already has
		// the labels; value is the token after it.
		if len(fields) >= 3 && strings.ContainsAny(head, "{}") {
			val = fields[len(fields)-2]
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			continue
		}
		name, labels, ok := splitPromHead(head)
		if !ok {
			continue
		}
		out = append(out, promSample{name: name, labels: labels, value: v})
	}
	return out
}

// splitPromHead splits "name{k=\"v\",...}" into name and labels.
func splitPromHead(head string) (string, map[string]string, bool) {
	brace := strings.IndexByte(head, '{')
	if brace < 0 {
		if head == "" || strings.ContainsAny(head, " \t") {
			return "", nil, false
		}
		return head, nil, true
	}
	name := head[:brace]
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", nil, false
	}
	inner := head[brace+1:]
	if !strings.HasSuffix(inner, "}") {
		return "", nil, false
	}
	inner = inner[:len(inner)-1]
	labels := map[string]string{}
	if inner != "" {
		rest := inner
		for {
			eq := strings.IndexByte(rest, '=')
			if eq <= 0 {
				return "", nil, false
			}
			key := rest[:eq]
			rest = rest[eq+1:]
			if len(rest) < 2 || rest[0] != '"' {
				return "", nil, false
			}
			var sb strings.Builder
			closed := false
			for j := 1; j < len(rest); j++ {
				c := rest[j]
				if c == '\\' && j+1 < len(rest) {
					j++
					sb.WriteByte(rest[j])
					continue
				}
				if c == '"' {
					rest = rest[j+1:]
					closed = true
					break
				}
				sb.WriteByte(c)
			}
			if !closed {
				return "", nil, false
			}
			labels[key] = sb.String()
			if rest == "" {
				break
			}
			if !strings.HasPrefix(rest, ",") {
				return "", nil, false
			}
			rest = rest[1:]
			if rest == "" {
				break
			}
		}
	}
	return name, labels, true
}

// ─── Sessions (live stack + conversation view) ───────────────────────────
//
// The router's in-flight request stack and per-conversation session
// aggregate, served at GET /metrics/sessions. Token figures here are
// router-side ESTIMATES (~4 bytes/token, body-based) — the honest-boundary
// counterpart to the engine's measured figures.
//
// Session identity: for chat requests, the sha1 of the raw JSON of every
// message except the last, joined by "\n" (a single message uses itself),
// truncated to 12 hex chars. Same conversation prefix → same session; a
// conversation forks a new session at its second turn. Non-chat requests
// (completions with a prompt string, unparseable bodies) get sess "".
//
// Live requests are registered at admission (phase "admitted"), flip to
// "streaming" on the first response body byte, and are removed on finish.
// Rejections (429/413) never enter live — they bypass it via
// finishRejection and land directly in the session ring/aggregates.
//
// Caps (evict-on-insert, no janitor): live 1024 (oldest start), feed live 256,
// session store 512 (LRU by lastMs), feed sessions 100 (least-recent), reqs
// ring 32 per session.

const (
	sessLiveCap      = 1024
	sessStoreCap     = 512
	sessFeedLiveCap  = 256
	sessFeedCap      = 100
	sessReqsRing     = 32
	sessReqsWindowMs = 60_000  // reqs included for sessions active within 60s
	sessActiveMs     = 120_000 // "active" = liveN>0 or lastMs within 120s
)

// sessReqFrame is one completed (or rejected) request in a session's ring.
type sessReqFrame struct {
	TMs       int64   `json:"tMs"`
	Status    int     `json:"status"`
	Stream    bool    `json:"stream"`
	DurMs     float64 `json:"durMs"`
	TTFTms    float64 `json:"ttftMs"`
	CtxTok    int     `json:"ctxTok"`
	NewTok    int     `json:"newTok"`
	CachedTok int     `json:"cachedTok"`
	TokOut    int     `json:"tokOut"`
	TokS      float64 `json:"tokS"`
}

type liveReq struct {
	ID        uint64
	Sess      string
	Backend   string
	Path      string
	Stream    bool
	Streaming bool
	Start     time.Time
	StreamAt  time.Time // zero while admitted; set when Streaming flips
	CtxTok    int
	NewTok    int
}

type sessionAgg struct {
	ID          string
	Backend     string
	N           int
	FirstMs     int64
	LastMs      int64
	LiveN       int
	CtxTok      int
	NewTok      int
	TTFTSum     float64
	TTFTN       int
	TotalDurS   float64
	TokOutTotal int64
	LastStatus  int
	reqs        [sessReqsRing]sessReqFrame
	reqN        int // filled slots (≤ sessReqsRing)
	reqIdx      int // next write slot
}

type sessLiveFrame struct {
	ID      uint64 `json:"id"`
	Sess    string `json:"sess"`
	Backend string `json:"backend"`
	Path    string `json:"path"`
	Stream  bool   `json:"stream"`
	Phase   string `json:"phase"` // "admitted" | "streaming"
	StartMs int64  `json:"startMs"`
	CtxTok  int    `json:"ctxTok"`
	NewTok  int    `json:"newTok"`
	// EstPrefillMs: estimated prefill duration (newTok /
	// PREFILL_TOKENS_PER_SEC · 1000) — a router estimate, not a
	// measurement. 0 for non-streaming requests (their first byte marks
	// generation end, not prefill end) and for zero newTok.
	EstPrefillMs int `json:"estPrefillMs"`
	// StreamMs: UnixMilli of the first response body byte (first-byte
	// truth, measured); 0 while still admitted.
	StreamMs int64 `json:"streamMs"`
}

type sessFrame struct {
	ID          string         `json:"id"`
	Backend     string         `json:"backend"`
	N           int            `json:"n"`
	FirstMs     int64          `json:"firstMs"`
	LastMs      int64          `json:"lastMs"`
	Active      bool           `json:"active"`
	LiveN       int            `json:"liveN"`
	CtxTok      int            `json:"ctxTok"`
	NewTok      int            `json:"newTok"`
	CachedTok   int            `json:"cachedTok"`
	AvgTTFTMs   float64        `json:"avgTTFTMs"`
	TotalDurS   float64        `json:"totalDurS"`
	TokOutTotal int64          `json:"tokOutTotal"`
	LastStatus  int            `json:"lastStatus"`
	Reqs        []sessReqFrame `json:"reqs"` // nil unless active+recent+top-20
}

type sessionsFeed struct {
	T        int64           `json:"t"`
	Live     []sessLiveFrame `json:"live"`
	Sessions []sessFrame     `json:"sessions"`
}

type sessionRegistry struct {
	mu       sync.Mutex
	live     map[uint64]*liveReq
	sessions map[string]*sessionAgg
	nextID   atomic.Uint64
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{
		live:     map[uint64]*liveReq{},
		sessions: map[string]*sessionAgg{},
	}
}

var sessions = newSessionRegistry()

// put stores the aggregate under its key, evicting the least-recent
// session (by lastMs) when the store overflows its cap. Caller holds mu.
func (r *sessionRegistry) put(key string, a *sessionAgg) {
	if _, ok := r.sessions[key]; !ok && len(r.sessions) >= sessStoreCap {
		var oldestKey string
		var oldestMs int64 = math.MaxInt64
		for k, v := range r.sessions {
			if v.LastMs < oldestMs {
				oldestMs = v.LastMs
				oldestKey = k
			}
		}
		if oldestKey != "" {
			delete(r.sessions, oldestKey)
		}
	}
	r.sessions[key] = a
}

// start registers an admitted request in the live stack and returns its
// process-wide unique id.
func (r *sessionRegistry) start(backend, path string, stream bool, sess string, ctxTok, newTok int) uint64 {
	id := r.nextID.Add(1)
	now := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[id] = &liveReq{
		ID: id, Sess: sess, Backend: backend, Path: path,
		Stream: stream, Start: time.Now(), CtxTok: ctxTok, NewTok: newTok,
	}
	if len(r.live) > sessLiveCap {
		// IDs are assigned monotonically at admission, so the smallest
		// live ID is the oldest (ties are safe either way).
		var oldestID uint64
		for i := range r.live {
			if oldestID == 0 || i < oldestID {
				oldestID = i
			}
		}
		delete(r.live, oldestID)
	}
	if sess != "" {
		a, ok := r.sessions[sess]
		if !ok {
			a = &sessionAgg{ID: sess, Backend: backend, FirstMs: now}
			r.put(sess, a)
		}
		a.Backend = backend
		a.LiveN++
		a.CtxTok = ctxTok
		a.NewTok = newTok
	}
	return id
}

func (r *sessionRegistry) firstByte(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.live[id]; ok {
		l.Streaming = true
		l.StreamAt = time.Now()
	}
}

// appendReq folds one completed request into a session's aggregates and
// last-32 ring (oldest first).
func (a *sessionAgg) appendReq(req sessReqFrame) {
	a.N++
	if a.N == 1 {
		a.FirstMs = req.TMs
	}
	a.LastMs = req.TMs
	a.CtxTok = req.CtxTok
	a.NewTok = req.NewTok
	a.LastStatus = req.Status
	a.TotalDurS += req.DurMs / 1000
	a.TokOutTotal += int64(req.TokOut)
	if req.Stream && req.TTFTms > 0 {
		a.TTFTSum += req.TTFTms
		a.TTFTN++
	}
	a.reqs[a.reqIdx] = req
	a.reqIdx = (a.reqIdx + 1) % sessReqsRing
	if a.reqN < sessReqsRing {
		a.reqN++
	}
}

// finish removes a live request and folds it into its session.
func (r *sessionRegistry) finish(id uint64, status int, durMs, ttftMs float64, bytesOut int64) {
	r.mu.Lock()
	l, ok := r.live[id]
	if ok {
		delete(r.live, id)
	}
	if !ok {
		r.mu.Unlock()
		return
	}
	tokOut := int(bytesOut / 4)
	cached := l.CtxTok - l.NewTok
	if cached < 0 {
		cached = 0
	}
	req := sessReqFrame{
		TMs: l.Start.UnixMilli(), Status: status, Stream: l.Stream,
		DurMs: durMs, TTFTms: ttftMs, CtxTok: l.CtxTok, NewTok: l.NewTok,
		CachedTok: cached, TokOut: tokOut,
	}
	if durMs > 0 {
		req.TokS = float64(tokOut) / (durMs / 1000)
	}
	if l.Sess != "" {
		// The session aggregate may already be evicted from the store (LRU);
		// LiveN still decrements so no phantom liveness lingers, and the
		// ring/appends are best-effort against whatever survives.
		if a, ok := r.sessions[l.Sess]; ok {
			a.LiveN--
			a.appendReq(req)
			r.put(l.Sess, a)
		}
	}
	r.mu.Unlock()
}

// finishRejection folds a router-side rejection (429/413) directly into the
// session ring/aggregates — rejections never enter the live stack.
func (r *sessionRegistry) finishRejection(backend, path, sess string, ctxTok, newTok, status int, durMs float64) {
	if sess == "" {
		return
	}
	cached := ctxTok - newTok
	if cached < 0 {
		cached = 0
	}
	now := time.Now().UnixMilli()
	req := sessReqFrame{
		TMs: now, Status: status, Stream: false,
		DurMs: durMs, TTFTms: 0, CtxTok: ctxTok, NewTok: newTok, CachedTok: cached,
		TokOut: 0, TokS: 0,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.sessions[sess]
	if !ok {
		a = &sessionAgg{ID: sess, Backend: backend, FirstMs: now}
	}
	a.Backend = backend
	a.appendReq(req)
	r.put(sess, a)
}

// feed assembles the /metrics/sessions payload per the contract:
// live sorted startMs ASC (oldest on top), capped 256; sessions sorted
// active-first then lastMs DESC, capped 100; reqs included only for
// active+recent sessions among the first 20.
func (r *sessionRegistry) feed() sessionsFeed {
	r.mu.Lock()
	defer r.mu.Unlock()
	nowMs := time.Now().UnixMilli()

	live := make([]sessLiveFrame, 0, len(r.live))
	for _, l := range r.live {
		phase := "admitted"
		var streamMs int64
		var estPrefillMs int
		if l.Streaming {
			phase = "streaming"
			if !l.StreamAt.IsZero() {
				streamMs = l.StreamAt.UnixMilli()
			}
		}
		// Non-streaming requests keep phase "admitted" until the WHOLE
		// response is written, so their first byte marks generation end,
		// not prefill end — the estimate only makes sense for streaming.
		if l.Stream && l.NewTok > 0 {
			estPrefillMs = int(float64(l.NewTok) / float64(prefillTokensPerSec) * 1000)
		}
		live = append(live, sessLiveFrame{
			ID: l.ID, Sess: l.Sess, Backend: l.Backend, Path: l.Path,
			Stream: l.Stream, Phase: phase, StartMs: l.Start.UnixMilli(),
			CtxTok: l.CtxTok, NewTok: l.NewTok,
			EstPrefillMs: estPrefillMs, StreamMs: streamMs,
		})
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].StartMs == live[j].StartMs {
			return live[i].ID < live[j].ID
		}
		return live[i].StartMs < live[j].StartMs
	})
	if len(live) > sessFeedLiveCap {
		live = live[len(live)-sessFeedLiveCap:]
	}

	// A fresh conversation whose first request is still in flight has no
	// completed requests yet (LastMs is its start time, but it must not
	// sink behind finished sessions) — its effective recency is the newest
	// live start it owns.
	liveStarts := map[string]int64{}
	for _, l := range r.live {
		if l.Sess != "" {
			if st := l.Start.UnixMilli(); st > liveStarts[l.Sess] {
				liveStarts[l.Sess] = st
			}
		}
	}

	type sessSort struct {
		a   *sessionAgg
		eff int64
		act bool
	}
	list := make([]sessSort, 0, len(r.sessions))
	for _, a := range r.sessions {
		act := a.LiveN > 0 || nowMs-a.LastMs < sessActiveMs
		eff := a.LastMs
		if ls, ok := liveStarts[a.ID]; ok && ls > eff {
			eff = ls
		}
		list = append(list, sessSort{a: a, eff: eff, act: act})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].act != list[j].act {
			return list[i].act
		}
		if list[i].eff == list[j].eff {
			return list[i].a.ID < list[j].a.ID
		}
		return list[i].eff > list[j].eff
	})
	if len(list) > sessFeedCap {
		list = list[:sessFeedCap]
	}
	out := make([]sessFrame, 0, len(list))
	for i, e := range list {
		a := e.a
		f := sessFrame{
			ID: a.ID, Backend: a.Backend, N: a.N,
			FirstMs: a.FirstMs, LastMs: a.LastMs,
			Active: e.act, LiveN: a.LiveN,
			CtxTok: a.CtxTok, NewTok: a.NewTok,
			TotalDurS: a.TotalDurS, TokOutTotal: a.TokOutTotal,
			LastStatus: a.LastStatus,
		}
		if c := a.CtxTok - a.NewTok; c > 0 {
			f.CachedTok = c
		}
		if a.TTFTN > 0 {
			f.AvgTTFTMs = a.TTFTSum / float64(a.TTFTN)
		}
		// reqs: only for sessions that are live or recently active (60s
		// window — tighter than the 120s "active" flag) and within the
		// first 20 after sorting; otherwise null to keep the feed small.
		include := (a.LiveN > 0 || nowMs-a.LastMs < sessReqsWindowMs) && i < 20
		if include {
			// Ring is newest-at-(reqIdx-1); emit oldest first.
			reqs := make([]sessReqFrame, a.reqN)
			start := (a.reqIdx - a.reqN + sessReqsRing) % sessReqsRing
			copy(reqs, a.reqs[start:start+a.reqN])
			f.Reqs = reqs
		}
		out = append(out, f)
	}
	return sessionsFeed{T: nowMs, Live: live, Sessions: out}
}

// sessionKeyFor derives the 12-hex conversation id: sha1 over the raw JSON
// of every message except the last (a single message uses itself), joined
// by "\n". Non-chat bodies return "".
func sessionKeyFor(parsed *parsedBody) string {
	if len(parsed.Messages) == 0 {
		return ""
	}
	prefix := parsed.Messages[:len(parsed.Messages)-1]
	if len(prefix) == 0 {
		prefix = parsed.Messages[:1]
	}
	var b strings.Builder
	for i, m := range prefix {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(m.Content)
	}
	sum := sha1.Sum([]byte(b.String()))
	return fmt.Sprintf("%x", sum)[:12]
}

// estimateCtxTokens estimates the conversation context (all messages, or
// the prompt) in tokens, ~4 bytes/token.
func estimateCtxTokens(parsed *parsedBody) int {
	if len(parsed.Messages) > 0 {
		chars := 0
		for _, m := range parsed.Messages {
			chars += rawContentLen(m.Content)
		}
		return chars / 4
	}
	return rawContentLen(parsed.Prompt) / 4
}

// ─── Global state ─────────────────────────────────────────────────────────

var (
	backends            []Backend
	client              = &http.Client{Timeout: 30 * time.Second}
	slots               map[string]*slotManager // per-backend concurrency slots
	prefillSlots        map[string]*slotManager // per-backend large-prefill slots (released on first response byte)
	gpu                 *gpuBudget
	queueTimeout                                    = 30 * time.Second
	maxBodyBytes        int64                       = 16 << 20
	prefillTokensPerSec int                         = 10000
	durations           map[string]*durationTracker // per backend name
	proxies             map[string]*httputil.ReverseProxy
	engineMetrics       = newEngineRegistry() // per-backend /metrics scrape
	proxyTransport      = &http.Transport{
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		DisableCompression:    true,
	}
)

// tier0Inflight returns total in-flight requests across all tier-0 backends.
func tier0Inflight() int32 {
	var total int32
	for _, b := range backends {
		if b.Tier == 0 {
			if s, ok := slots[b.Name]; ok {
				in, _, _, _ := s.snapshot()
				total += in
			}
		}
	}
	return total
}

// buildMux assembles the router's route table. /v1/ is the proxy prefix;
// management routes are registered before it. A future web.Handler() would
// be mounted at "/" by the caller, after this mux is built.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "vLLM router OK")
	})
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/metrics/stream", handleMetricsStream)
	mux.HandleFunc("/metrics/burst", handleBurst)
	mux.HandleFunc("/metrics/sessions", handleSessions)
	mux.HandleFunc("/v1/", handleProxy)
	return mux
}

func main() {
	port := flag.Int("port", 80, "listen port")
	flag.Parse()

	backendsJSON := os.Getenv("BACKENDS")
	if backendsJSON == "" {
		log.Fatal("BACKENDS env var required")
	}
	if err := json.Unmarshal([]byte(backendsJSON), &backends); err != nil {
		log.Fatalf("invalid BACKENDS JSON: %v", err)
	}
	if len(backends) == 0 {
		log.Fatal("at least one backend required")
	}

	if qt := os.Getenv("MAX_QUEUE_TIMEOUT"); qt != "" {
		if d, err := time.ParseDuration(qt); err == nil {
			queueTimeout = d
		}
	}

	slots = make(map[string]*slotManager)
	prefillSlots = make(map[string]*slotManager)
	durations = make(map[string]*durationTracker)
	totalWeight := 0
	for _, b := range backends {
		durations[b.Name] = &durationTracker{}
		if b.MaxConcurrent > 0 {
			mq := int32(b.MaxQueueDepth)
			if mq == 0 {
				mq = 2
			}
			slots[b.Name] = newSlot(int32(b.MaxConcurrent), mq)
		}
		if b.MaxConcurrentLargePrefill > 0 {
			prefillSlots[b.Name] = newSlot(int32(b.MaxConcurrentLargePrefill), 4)
		}
		if b.GpuWeight > 0 {
			totalWeight += b.GpuWeight
		}
		fields := []string{}
		if b.MaxConcurrent > 0 {
			fields = append(fields, fmt.Sprintf("maxConcurrent=%d", b.MaxConcurrent))
		} else {
			fields = append(fields, "unlimited")
		}
		if b.Tier > 0 {
			fields = append(fields, fmt.Sprintf("tier=%d", b.Tier))
			if b.BlockOnTier0 > 0 {
				fields = append(fields, fmt.Sprintf("blockOnTier0>=%d", b.BlockOnTier0))
			}
		} else {
			fields = append(fields, "tier=0(king)")
		}
		if b.GpuWeight > 0 {
			fields = append(fields, fmt.Sprintf("gpuWeight=%d", b.GpuWeight))
		}
		if b.MaxConcurrentLargePrefill > 0 {
			thresh := b.LargePrefillThresholdTokens
			if thresh == 0 {
				thresh = 8192
			}
			fields = append(fields, fmt.Sprintf("largePrefill=%d/thresh%d", b.MaxConcurrentLargePrefill, thresh))
		}
		log.Printf("  %s -> %s (%s)", b.Name, b.URL, strings.Join(fields, ", "))
	}
	// The engine's own /metrics: real in-engine running/waiting, KV pressure,
	// and prompt+completion token rates. vLLM serves it always; SGLang only
	// with --enable-metrics (the scrape degrades to "off" when absent).
	for _, b := range backends {
		engineMetrics.add(b.Name, b.URL+"/metrics")
	}
	go engineMetrics.loop()

	if totalWeight > 0 {
		maxBudget := 0
		mb := os.Getenv("MAX_GPU_BUDGET")
		if mb != "" {
			v, err := strconv.Atoi(mb)
			if err != nil {
				log.Fatalf("invalid MAX_GPU_BUDGET %q: %v", mb, err)
			}
			maxBudget = v
		}
		if mb == "" {
			maxBudget = 4 // default per docs
		}
		if maxBudget > 0 {
			gpu = &gpuBudget{max: maxBudget}
			gpu.cond = sync.NewCond(&gpu.mu)
			log.Printf("  GPU budget: max=%d (active only when tier-0 in-flight)", maxBudget)
		}
	}

	if mb := os.Getenv("MAX_BODY_BYTES"); mb != "" {
		v, err := strconv.ParseInt(mb, 10, 64)
		if err != nil || v <= 0 {
			log.Fatalf("invalid MAX_BODY_BYTES %q", mb)
		}
		maxBodyBytes = v
	}

	if pts := os.Getenv("PREFILL_TOKENS_PER_SEC"); pts != "" {
		v, err := strconv.Atoi(pts)
		if err != nil || v <= 0 {
			log.Fatalf("invalid PREFILL_TOKENS_PER_SEC %q", pts)
		}
		prefillTokensPerSec = v
	}

	// Build reverse proxies once at startup.
	proxies = make(map[string]*httputil.ReverseProxy)
	for _, b := range backends {
		u, err := url.Parse(b.URL)
		if err != nil {
			log.Fatalf("invalid backend URL for %q: %v", b.Name, err)
		}
		p := httputil.NewSingleHostReverseProxy(u)
		p.FlushInterval = -1 // flush immediately for SSE streaming
		p.Transport = proxyTransport
		name := b.Name
		p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			handleProxyError(w, r, err, name)
		}
		proxies[b.Name] = p
	}

	mux := buildMux()

	// Mount the embedded SPA dashboard as the lowest-priority catch-all.
	// buildMux() already registered the more specific management and proxy
	// routes (/health, /stats, /v1/models, /metrics/*, /v1/); web.Handler()
	// 404s any of those that reach it, so nothing is shadowed. Without the
	// webui build tag web.Handler() is a 404 stub, so this is a no-op there.
	mux.Handle("/", web.Handler())

	// Refresh per-backend + fleet rates for the live metrics feed. The
	// actual elapsed time between ticks feeds the window decay, so a
	// scheduler stall does not under-count the interval.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		last := time.Now()
		for range ticker.C {
			now := time.Now()
			metrics.computeRates(now.Sub(last))
			last = now
		}
	}()

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("vLLM router listening on %s with %d backends (queueTimeout=%s, gpuBudget=%v)",
		addr, len(backends), queueTimeout, gpu != nil)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// ─── /stats ───────────────────────────────────────────────────────────────

func handleStats(w http.ResponseWriter, r *http.Request) {
	type stat struct {
		Name            string  `json:"name"`
		URL             string  `json:"url"`
		Tier            int     `json:"tier"`
		INFlight        int32   `json:"inFlight"`
		MaxConcurrent   int32   `json:"maxConcurrent"`
		Waiting         int32   `json:"waiting"`
		MaxQueueDepth   int32   `json:"maxQueueDepth"`
		PrefillInFlight int32   `json:"prefillInFlight"`
		PrefillWaiting  int32   `json:"prefillWaiting"`
		PrefillMax      int32   `json:"prefillMax"`
		AvgDurationS    float64 `json:"avgDurationS"`
	}
	t0Inflight := tier0Inflight()

	stats := make([]stat, 0, len(backends))
	for _, b := range backends {
		in, max, waiting, mq := int32(0), int32(0), int32(0), int32(0)
		if s, ok := slots[b.Name]; ok {
			in, max, waiting, mq = s.snapshot()
		}
		pIn, pMax, pWaiting, _ := int32(0), int32(0), int32(0), int32(0)
		if s, ok := prefillSlots[b.Name]; ok {
			pIn, pMax, pWaiting, _ = s.snapshot()
		}
		stats = append(stats, stat{
			Name:            b.Name,
			URL:             b.URL,
			Tier:            b.Tier,
			INFlight:        in,
			MaxConcurrent:   max,
			Waiting:         waiting,
			MaxQueueDepth:   mq,
			PrefillInFlight: pIn,
			PrefillWaiting:  pWaiting,
			PrefillMax:      pMax,
			AvgDurationS:    durations[b.Name].avgSeconds(),
		})
	}

	gUsed, gMax, gWaiting := 0, 0, 0
	if gpu != nil {
		gUsed, gMax, gWaiting = gpu.snapshot()
	}

	resp := map[string]any{
		"backends":      stats,
		"gpuUsed":       gUsed,
		"gpuBudget":     gMax,
		"gpuWaiting":    gWaiting,
		"tier0Inflight": t0Inflight,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ─── /v1/models ───────────────────────────────────────────────────────────

func handleModels(w http.ResponseWriter, r *http.Request) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	type modelsResp struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}

	var wg sync.WaitGroup
	results := make([][]modelEntry, len(backends))
	for i, b := range backends {
		wg.Add(1)
		go func(idx int, backend Backend) {
			defer wg.Done()
			resp, err := client.Get(backend.URL + "/v1/models")
			if err != nil {
				results[idx] = []modelEntry{{ID: backend.Name, Object: "model", OwnedBy: "vllm"}}
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				results[idx] = []modelEntry{{ID: backend.Name, Object: "model", OwnedBy: "vllm"}}
				return
			}
			var mr modelsResp
			if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
				results[idx] = []modelEntry{{ID: backend.Name, Object: "model", OwnedBy: "vllm"}}
				return
			}
			results[idx] = mr.Data
		}(i, b)
	}
	wg.Wait()

	merged := modelsResp{Object: "list", Data: []modelEntry{}}
	for _, entries := range results {
		merged.Data = append(merged.Data, entries...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(merged)
}

// ─── 429 helper ───────────────────────────────────────────────────────────

func write429(w http.ResponseWriter, reason, backend string, retryAfter int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	w.Header().Set("X-Router-Reason", reason)
	w.Header().Set("X-Router-Backend", backend)
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprintf(w, `{"error":{"type":"%s","message":"backend at capacity","backend":"%s","retry_after":%d}}`,
		reason, backend, retryAfter)
}

// estimateNewTokens estimates NEW prefill tokens from the parsed body.
// Multi-turn (>2 messages): only the last message is new input; prior context
// is assumed cached (radix/prefix cache). ≤2 messages: all content is new.
// Completions API: the prompt field. ~4 bytes per token (conservative for Qwen).
func estimateNewTokens(b *parsedBody) int {
	if len(b.Messages) > 0 {
		chars := 0
		if len(b.Messages) <= 2 {
			for _, m := range b.Messages {
				chars += rawContentLen(m.Content)
			}
		} else {
			chars = rawContentLen(b.Messages[len(b.Messages)-1].Content)
		}
		return chars / 4
	}
	return rawContentLen(b.Prompt) / 4
}

// rawContentLen measures character content from raw JSON without decoding it.
// String: bytes minus the two quotes (escape sequences keep encoded length —
// slight overestimate, which errs toward throttling, the safe direction).
// Array: sum of each part's "text" field. Anything else: 0.
func rawContentLen(raw json.RawMessage) int {
	if len(raw) < 2 {
		return 0
	}
	if raw[0] == '"' {
		return len(raw) - 2
	}
	if raw[0] == '[' {
		var parts []struct {
			Text json.RawMessage `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return 0
		}
		total := 0
		for _, p := range parts {
			if len(p.Text) >= 2 {
				total += len(p.Text) - 2
			}
		}
		return total
	}
	return 0
}

// estimateRetryAfter returns a rough seconds estimate of when a slot frees.
func estimateRetryAfter(name string) int {
	avg := durations[name].avgSeconds()
	max := 1
	if s, ok := slots[name]; ok {
		_, mx, _, _ := s.snapshot()
		max = int(mx)
	}
	if max < 1 {
		max = 1
	}
	estimate := int(avg / float64(max))
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

// ─── /metrics/stream (SSE) + /metrics/burst ───────────────────────────────

type backendFrame struct {
	Name            string     `json:"name"`
	URL             string     `json:"url"`
	Tier            int        `json:"tier"`
	InFlight        int32      `json:"inFlight"`
	MaxConcurrent   int32      `json:"maxConcurrent"`
	Waiting         int32      `json:"waiting"`
	MaxQueueDepth   int32      `json:"maxQueueDepth"`
	PrefillInFlight int32      `json:"prefillInFlight"`
	PrefillWaiting  int32      `json:"prefillWaiting"`
	PrefillMax      int32      `json:"prefillMax"`
	AvgDurationS    float64    `json:"avgDurationS"`
	EWMAms          float64    `json:"ewmaMs"`
	TTFTms          float64    `json:"ttftMs"`
	TTFTSampleCount int        `json:"ttftSampleCount"`
	TTFTMsNow       float64    `json:"ttftMsNow"`
	ReqRate         float64    `json:"reqRate"`
	BytesRate       float64    `json:"bytesRate"`
	TokEstRate      float64    `json:"tokEstRate"`
	ReqTotal        int64      `json:"reqTotal"`
	BytesInTotal    int64      `json:"bytesInTotal"`
	BytesOutTotal   int64      `json:"bytesOutTotal"`
	TokEstTotal     int64      `json:"tokEstTotal"`
	Engine          engineView `json:"engine"`
}

type gpuFrame struct {
	Used    int  `json:"used"`
	Budget  int  `json:"budget"`
	Waiting int  `json:"waiting"`
	Active  bool `json:"active"`
}

type totalsFrame struct {
	InFlight      int32   `json:"inFlight"`
	Waiting       int32   `json:"waiting"`
	ReqRate       float64 `json:"reqRate"`
	BytesRate     float64 `json:"bytesRate"`
	TokEstRate    float64 `json:"tokEstRate"`
	ReqTotal      int64   `json:"reqTotal"`
	BytesInTotal  int64   `json:"bytesInTotal"`
	BytesOutTotal int64   `json:"bytesOutTotal"`
	TokEstTotal   int64   `json:"tokEstTotal"`
}

type metricsFrame struct {
	T             int64          `json:"t"`
	Backends      []backendFrame `json:"backends"`
	GPU           gpuFrame       `json:"gpu"`
	Tier0Inflight int32          `json:"tier0Inflight"`
	Totals        totalsFrame    `json:"totals"`
}

// buildMetricsFrame assembles one full live snapshot per the live metrics
// contract.
func buildMetricsFrame() metricsFrame {
	f := metricsFrame{T: time.Now().UnixMilli(), Backends: make([]backendFrame, 0, len(backends))}
	f.Tier0Inflight = tier0Inflight()

	for _, b := range backends {
		bf := backendFrame{
			Name:         b.Name,
			URL:          b.URL,
			Tier:         b.Tier,
			AvgDurationS: durations[b.Name].avgSeconds(),
		}
		if s, ok := slots[b.Name]; ok {
			bf.InFlight, bf.MaxConcurrent, bf.Waiting, bf.MaxQueueDepth = s.snapshot()
		}
		if s, ok := prefillSlots[b.Name]; ok {
			bf.PrefillInFlight, bf.PrefillMax, bf.PrefillWaiting, _ = s.snapshot()
		}
		ms := metrics.snapshot(b.Name)
		bf.EWMAms = bf.AvgDurationS * 1000
		bf.TTFTms = ms.TTFTMs
		bf.TTFTSampleCount = ms.TTFTSampleCount
		bf.TTFTMsNow = ms.TTFTMsNow
		bf.ReqRate = ms.ReqRate
		bf.BytesRate = ms.BytesRate
		bf.TokEstRate = ms.TokEstRate
		bf.ReqTotal = ms.ReqTotal
		bf.BytesInTotal = ms.BytesInTotal
		bf.BytesOutTotal = ms.BytesOutTotal
		bf.TokEstTotal = ms.TokEstTotal
		bf.Engine = engineMetrics.view(b.Name)
		f.Backends = append(f.Backends, bf)

		f.Totals.InFlight += bf.InFlight
		f.Totals.Waiting += bf.Waiting
		f.Totals.ReqRate += ms.ReqRate
		f.Totals.BytesRate += ms.BytesRate
		f.Totals.TokEstRate += ms.TokEstRate
		f.Totals.ReqTotal += ms.ReqTotal
		f.Totals.BytesInTotal += ms.BytesInTotal
		f.Totals.BytesOutTotal += ms.BytesOutTotal
		f.Totals.TokEstTotal += ms.TokEstTotal
	}

	// Active means the GPU budget mechanism is engaged: a budget exists AND
	// tier-0 has in-flight requests. No budget configured → active false.
	f.GPU.Active = gpu != nil && f.Tier0Inflight > 0
	if gpu != nil {
		f.GPU.Used, f.GPU.Budget, f.GPU.Waiting = gpu.snapshot()
	}

	return f
}

// handleMetricsStream pushes one JSON snapshot per SSE data frame every
// interval (default 500ms, clamped 200ms..10s) until the client disconnects.
func handleMetricsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	interval := 500 * time.Millisecond
	if v := r.URL.Query().Get("interval"); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			interval = d
		}
	}
	if interval < 200*time.Millisecond {
		interval = 200 * time.Millisecond
	}
	if interval > 10*time.Second {
		interval = 10 * time.Second
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		frame := buildMetricsFrame()
		buf, err := json.Marshal(frame)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

type burstRequestFrame struct {
	T            int64   `json:"t"`
	Backend      string  `json:"backend"`
	Path         string  `json:"path"`
	Status       int     `json:"status"`
	Stream       bool    `json:"stream"`
	DurMs        float64 `json:"durMs"`
	TTFTms       float64 `json:"ttftMs"`
	NewTokensEst int     `json:"newTokensEst"`
	BytesIn      int64   `json:"bytesIn"`
	BytesOut     int64   `json:"bytesOut"`
}

// handleBurst serves the recent-request ring as {"requests":[...]} (newest
// last), ?limit default 500, max 1000.
func handleBurst(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	samples := metrics.burst(limit)
	out := make([]burstRequestFrame, 0, len(samples))
	for _, s := range samples {
		out = append(out, burstRequestFrame{
			T:            s.T.UnixMilli(),
			Backend:      s.Backend,
			Path:         s.Path,
			Status:       s.Status,
			Stream:       s.Stream,
			DurMs:        s.DurMs,
			TTFTms:       s.TTFTMs,
			NewTokensEst: s.NewTokensEst,
			BytesIn:      s.BytesIn,
			BytesOut:     s.BytesOut,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"requests": out})
}

// handleSessions serves the live in-flight request stack plus the
// conversation-grouped session aggregates (est token semantics).
func handleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions.feed())
}

// ─── Proxy ────────────────────────────────────────────────────────────────

// handleProxyError converts upstream transport failures into clean,
// OpenAI-style JSON errors instead of Go's bare "http: proxy error" 502.
//
// Classification:
//   - client canceled: log quietly; nothing can be written
//   - response already started (engine died mid-stream): the status line is
//     fixed; log and end the stream
//   - transport failure (dial refused/reset, EOF, timeout): 503
//     backend_unavailable + Retry-After — the engine is down or restarting;
//     the request never reached it, so retrying is safe
//   - anything else (e.g. malformed upstream response): 502 bad_gateway
func handleProxyError(w http.ResponseWriter, r *http.Request, err error, backend string) {
	if errors.Is(err, context.Canceled) {
		log.Printf("%s %s -> %s client canceled during upstream roundtrip", r.Method, r.URL.Path, backend)
		return
	}
	if t, ok := w.(*respTracker); ok && t.wrote {
		log.Printf("%s %s -> %s upstream failed mid-response: %v", r.Method, r.URL.Path, backend, err)
		return
	}
	status := http.StatusBadGateway
	code := "bad_gateway"
	var opErr *net.OpError
	if errors.As(err, &opErr) || errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
		status = http.StatusServiceUnavailable
		code = "backend_unavailable"
	}
	log.Printf("%s %s -> %s %d %s: %v", r.Method, r.URL.Path, backend, status, code, err)
	// The request never produced an upstream response byte, so the sample
	// carries TTFT 0 and BytesOut 0. Record now (before the error body is
	// written through the tracker) so the error body doesn't pollute bytesOut.
	if t, ok := w.(*respTracker); ok {
		t.status = status
		t.finish(backend, r.URL.Path, status, 0, 0)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.Header().Set("X-Router-Reason", code)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": fmt.Sprintf("backend %q unavailable (engine down or restarting): %v", backend, err),
			"type":    "server_error",
			"code":    code,
		},
	})
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	// Production: unreachable (main fatals on an empty BACKENDS); tests swap
	// the global backends slice, so a request racing teardown gets a clean
	// 503 instead of an index panic.
	if len(backends) == 0 {
		http.Error(w, "no backends configured", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			w.Header().Set("X-Router-Reason", "body-too-large")
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			// Model is unknown at this point; attribute to the default backend.
			metrics.recordRequest(backends[0].Name, requestSample{
				T:       time.Now(),
				Backend: backends[0].Name,
				Path:    r.URL.Path,
				Status:  http.StatusRequestEntityTooLarge,
				DurMs:   float64(time.Since(reqStart) / time.Millisecond),
				BytesIn: int64(len(body)),
			})
			return
		}
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body.Close()

	// Resolve backend from model name.
	target := backends[0]
	var parsed parsedBody
	if len(body) > 0 {
		// Error tolerated: zero value keeps modelName "" and routes to backends[0],
		// identical to today's behavior on unmarshal failure.
		json.Unmarshal(body, &parsed)
	}
	modelName := parsed.Model
	if modelName != "" {
		for _, b := range backends {
			if b.Name == modelName {
				target = b
				break
			}
		}
	}
	if qModel := r.URL.Query().Get("model"); qModel != "" {
		for _, b := range backends {
			if b.Name == qModel {
				target = b
				break
			}
		}
	}

	// Estimated NEW prefill tokens (request-body based, ~4 bytes/token).
	// Computed once, before any admission decision: the large-prefill
	// throttle and metrics both use this value.
	newTokens := estimateNewTokens(&parsed)
	// Est context tokens (whole conversation/prompt) + the conversation
	// session id, for the live sessions view. Both are body estimates.
	ctxTok := estimateCtxTokens(&parsed)
	sessKey := sessionKeyFor(&parsed)
	reqContext := requestSample{
		T:            time.Now(),
		Backend:      target.Name,
		Path:         r.URL.Path,
		NewTokensEst: newTokens,
		CtxTok:       ctxTok,
	}
	recordRejection := func(status int) {
		reqContext.Status = status
		reqContext.DurMs = float64(time.Since(reqStart) / time.Millisecond)
		// Rejections never enter the live stack, but they DO belong to the
		// conversation — fold them into the session ring/aggregates.
		sessions.finishRejection(reqContext.Backend, r.URL.Path, sessKey, ctxTok, newTokens, status, reqContext.DurMs)
		metrics.recordRequest(reqContext.Backend, reqContext)
	}

	cancelCh := r.Context().Done()
	t0Inflight := tier0Inflight()

	// ── Step 1: Cross-tier blocking ────────────────────────────────────────
	// If tier-0 in-flight count meets or exceeds this backend's blockOnTier0
	// threshold, reject immediately. Don't queue — AxonHub handles cloud fallback.
	if target.BlockOnTier0 > 0 && int(t0Inflight) >= target.BlockOnTier0 {
		retry := 1
		for _, b := range backends {
			if b.Tier == 0 {
				avg := int(durations[b.Name].avgSeconds())
				if avg > retry {
					retry = avg
				}
			}
		}
		log.Printf("%s %s -> %s (model=%s) REJECTED: blocked by tier-0 (tier0Inflight=%d >= threshold %d)",
			r.Method, r.URL.Path, target.Name, modelName, t0Inflight, target.BlockOnTier0)
		write429(w, "blocked-by-tier0", target.Name, retry)
		recordRejection(http.StatusTooManyRequests)
		return
	}

	// ── Step 2: GPU budget (only for tier-1 subjects, only when tier-0 active) ──
	// The king (tier 0) is never constrained by GPU budget — it IS the budget.
	// Subjects with gpuWeight get squeezed into the remaining budget when king runs.
	// When king is idle, no budget check at all.
	if gpu != nil && target.Tier > 0 && target.GpuWeight > 0 && t0Inflight > 0 {
		used, maxB, _ := gpu.snapshot()
		if used+target.GpuWeight > maxB {
			log.Printf("%s %s -> %s (model=%s) queued at GPU budget (gpu=%d+%d/%d, tier0=%d)",
				r.Method, r.URL.Path, target.Name, modelName,
				used, target.GpuWeight, maxB, t0Inflight)
			if !gpu.tryAcquire(target.GpuWeight, 4, 5*time.Second, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				log.Printf("%s %s -> %s (model=%s) 429: GPU budget full", r.Method, r.URL.Path, target.Name, modelName)
				write429(w, "gpu-budget-full", target.Name, retry)
				recordRejection(http.StatusTooManyRequests)
				return
			}
		} else {
			if !gpu.tryAcquire(target.GpuWeight, 4, 5*time.Second, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				log.Printf("%s %s -> %s (model=%s) 429: GPU budget full (race)", r.Method, r.URL.Path, target.Name, modelName)
				write429(w, "gpu-budget-full", target.Name, retry)
				recordRejection(http.StatusTooManyRequests)
				return
			}
		}
		defer gpu.release(target.GpuWeight)
	}

	// ── Step 3: Per-backend slot ──────────────────────────────────────────
	sem := slots[target.Name]
	if sem != nil {
		in, maxC, waiting, _ := sem.snapshot()
		if in >= maxC {
			log.Printf("%s %s -> %s (model=%s) queued at backend (%d/%d, waiting=%d, tier0=%d)",
				r.Method, r.URL.Path, target.Name, modelName,
				in, maxC, waiting+1, t0Inflight)
		}
		if !sem.acquire(queueTimeout, cancelCh) {
			retry := estimateRetryAfter(target.Name)
			stage := "backend-full"
			_, _, qW, qMax := sem.snapshot()
			if qW >= qMax {
				stage = "backend-queue-full"
			}
			log.Printf("%s %s -> %s (model=%s) 429: backend at capacity", r.Method, r.URL.Path, target.Name, modelName)
			write429(w, stage, target.Name, retry)
			recordRejection(http.StatusTooManyRequests)
			return
		}
		defer sem.release()
	}

	// ── Step 3.5: Large prefill throttle ─────────────────────────────────
	// Limits how many concurrent large prefills a backend processes. A large
	// prefill is one whose estimated NEW tokens (last message only, not cached
	// context) exceed the backend's threshold. The slot is released on the
	// first response byte for streaming requests, or after the estimated
	// prefill duration (newTokens / PREFILL_TOKENS_PER_SEC) for non-streaming
	// requests. Small requests skip this entirely. Soft-queued up to maxQueue,
	// then 429.
	prefillSem := prefillSlots[target.Name]
	prefillRelease := func() {} // no-op by default
	if prefillSem != nil {
		threshold := target.LargePrefillThresholdTokens
		if threshold == 0 {
			threshold = 8192
		}
		if newTokens >= threshold {
			pin, pmaxC, pwaiting, _ := prefillSem.snapshot()
			if pin >= pmaxC {
				log.Printf("%s %s -> %s (model=%s) queued at prefill (%d/%d, waiting=%d, newTokens=%d)",
					r.Method, r.URL.Path, target.Name, modelName,
					pin, pmaxC, pwaiting+1, newTokens)
			}
			if !prefillSem.acquire(queueTimeout, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				stage := "prefill-full"
				_, _, pqW, pqMax := prefillSem.snapshot()
				if pqW >= pqMax {
					stage = "prefill-queue-full"
				}
				log.Printf("%s %s -> %s (model=%s) 429: prefill at capacity (newTokens=%d)",
					r.Method, r.URL.Path, target.Name, modelName, newTokens)
				write429(w, stage, target.Name, retry)
				recordRejection(http.StatusTooManyRequests)
				return
			}
			var prefillOnce sync.Once
			prefillRelease = func() { prefillOnce.Do(prefillSem.release) }
			if !parsed.Stream {
				// Non-streaming: response headers arrive only after full
				// generation, so first-byte release would hold the slot through
				// all of decode. Bound the hold to the estimated prefill duration;
				// whichever fires first (timer or first byte) wins via the Once.
				hold := time.Duration(float64(newTokens) / float64(prefillTokensPerSec) * float64(time.Second))
				timer := time.AfterFunc(hold, prefillRelease)
				defer timer.Stop()
			}
		}
	}

	// ── Step 4: Forward ───────────────────────────────────────────────────
	in, maxC, waiting, _ := int32(0), int32(0), int32(0), int32(0)
	if sem != nil {
		in, maxC, waiting, _ = sem.snapshot()
	}
	gUsed, gMax, _ := 0, 0, 0
	if gpu != nil {
		gUsed, gMax, _ = gpu.snapshot()
	}
	if target.Tier > 0 && target.GpuWeight > 0 && t0Inflight > 0 {
		log.Printf("%s %s -> %s (model=%s) inflight=%d/%d waiting=%d gpu=%d/%d tier0=%d",
			r.Method, r.URL.Path, target.Name, modelName, in, maxC, waiting, gUsed, gMax, t0Inflight)
	} else if sem != nil {
		log.Printf("%s %s -> %s (model=%s) inflight=%d/%d waiting=%d",
			r.Method, r.URL.Path, target.Name, modelName, in, maxC, waiting)
	} else {
		log.Printf("%s %s -> %s (model=%s)", r.Method, r.URL.Path, target.Name, modelName)
	}

	// Duration + TTFT are measured from here (after admission/queue wait),
	// matching the existing per-backend EWMA semantics. Rejections record
	// queue-wait time from reqStart instead.
	start := time.Now()
	// The request is now admitted: its demand enters the live-rate window.
	// Completion retires it again via the accepted flag.
	reqContext.accepted = true
	metrics.recordAccept(target.Name, int64(len(body)), newTokens)
	// The request enters the live session stack at admission.
	reqID := sessions.start(target.Name, r.URL.Path, parsed.Stream, sessKey, ctxTok, newTokens)
	proxy := proxies[target.Name]
	// Wrap ResponseWriter to capture status + detect completion for duration
	// tracking. Also releases the prefill slot on first response byte.
	tracker := &respTracker{
		ResponseWriter: w,
		status:         200,
		onFirstByte:    prefillRelease,
		backend:        target.Name,
		ttftStart:      start,
		reqStart:       start,
		stream:         parsed.Stream,
		newTokensEst:   newTokens,
		bytesIn:        int64(len(body)),
		reqID:          reqID,
		sessKey:        sessKey,
		ctxTok:         ctxTok,
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	proxy.ServeHTTP(tracker, r)

	prefillRelease() // fallback: release if no response was ever written
	durations[target.Name].record(time.Since(start))

	// TTFT is only meaningful for streaming requests (time to first byte).
	var ttftMs float64
	if parsed.Stream && !tracker.firstByteAt.IsZero() {
		ttftMs = float64(tracker.firstByteAt.Sub(start) / time.Millisecond)
	}
	tracker.finish(target.Name, r.URL.Path, tracker.status, ttftMs, tracker.bytesOut)
}

type respTracker struct {
	http.ResponseWriter
	status      int
	wrote       bool
	onFirstByte func() // called on first WriteHeader/Write; used to release prefill slot
	// firstByteAt: time of the first response byte; if only WriteHeader
	// ever happens (no body byte), the WriteHeader time is the fallback.
	firstByteAt time.Time
	bodyByte    bool // a Write with n>0 has happened
	bytesOut    int64

	// Request context for metrics recording (set at construction).
	backend      string
	ttftStart    time.Time // admission time; TTFT is first-byte minus this
	reqStart     time.Time
	stream       bool
	newTokensEst int
	bytesIn      int64
	reqID        uint64 // session registry live id (0 = none)
	sessKey      string // conversation id ("" = non-chat)
	ctxTok       int    // est context tokens
	ttftSent     bool   // first-byte TTFT already recorded (once)
	recorded     bool   // finish guard: exactly one sample per request
}

func (t *respTracker) WriteHeader(code int) {
	if !t.wrote {
		t.status = code
		t.wrote = true
		if !t.bodyByte {
			t.firstByteAt = time.Now() // fallback; first body byte overrides
		}
		if t.onFirstByte != nil {
			t.onFirstByte()
		}
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *respTracker) Write(b []byte) (int, error) {
	if !t.wrote {
		t.wrote = true
		if t.onFirstByte != nil {
			t.onFirstByte()
		}
	}
	n, err := t.ResponseWriter.Write(b)
	if n > 0 {
		if !t.bodyByte {
			t.firstByteAt = time.Now()
			t.bodyByte = true
			// TTFT is live truth: record it now, while the request is still
			// decoding, so the dashboard sees it the moment it happens.
			if t.stream && !t.ttftSent {
				t.ttftSent = true
				metrics.recordFirstByte(t.backend, float64(t.firstByteAt.Sub(t.ttftStart)/time.Millisecond))
			}
			// The live session stack flips to "streaming" on the first byte.
			if t.reqID != 0 {
				sessions.firstByte(t.reqID)
			}
		}
		t.bytesOut += int64(n)
	}
	return n, err
}

// finish records the metrics sample for this request exactly once. Called
// from the normal completion path (real status, TTFT, bytesOut) and from
// handleProxyError when it itself wrote a 502/503 (TTFTMs 0, BytesOut 0).
func (t *respTracker) finish(backend, path string, status int, ttftMs float64, bytesOut int64) {
	if t.recorded {
		return
	}
	t.recorded = true
	durMs := float64(time.Since(t.reqStart) / time.Millisecond)
	if t.reqID != 0 {
		sessions.finish(t.reqID, status, durMs, ttftMs, bytesOut)
	}
	metrics.recordRequest(backend, requestSample{
		T:            time.Now(),
		Backend:      backend,
		Path:         path,
		Status:       status,
		Stream:       t.stream,
		DurMs:        durMs,
		TTFTMs:       ttftMs,
		NewTokensEst: t.newTokensEst,
		CtxTok:       t.ctxTok,
		BytesIn:      t.bytesIn,
		BytesOut:     bytesOut,
	})
}

func (t *respTracker) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
