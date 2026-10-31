package main

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
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
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
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
	LargePrefillThresholdTokens int    `json:"largePrefillThresholdTokens"` // estimated prompt tokens to qualify as "large prefill" (default 8192)
}

// ─── Parsed body (single-pass, heavy fields stay raw) ──────────────────────

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type parsedBody struct {
	Model    string          `json:"model"`
	Messages []rawMessage    `json:"messages"`
	Prompt   json.RawMessage `json:"prompt"`
	Stream   bool            `json:"stream"`
	// Optional stable conversation identities (raw: a new non-string
	// identity value must not break decoding of unrelated fields).
	PromptCacheKey json.RawMessage `json:"prompt_cache_key"`
	User           json.RawMessage `json:"user"`
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
	T            time.Time // completion
	Start        time.Time // admission — the profile draws the request interval
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
// trailing arrival window that feeds the live rates. Rates measure arrival,
// not completion: a decode can run for minutes, but the request's demand
// began the moment it was admitted.
type backendMetrics struct {
	reqTotal      int64
	rejectedTotal int64
	bytesInTotal  int64
	bytesOutTotal int64
	tokEstTotal   int64
	ttftEwma      float64 // ms — EWMA over first-byte times, updated at first byte
	ttftSamples   int
	ttftMsNow     float64 // ms — the most recent first-byte sample (no decay)
	reqRate       float64
	bytesRate     float64
	tokEstRate    float64
	// arrivals: a fixed ring of 100 ms demand buckets over the trailing
	// rateWindowS. recordAccept writes the current bucket; computeRates sums
	// the buckets inside the window and divides by the full window. There is
	// no decay and completion never subtracts: the rate is a true trailing
	// arrival average with a hard cutoff.
	arrivals [rateBucketN]rateBucket
}

// The live rate is the trailing 30 s arrival average, evaluated every 500 ms
// from 300 × 100 ms buckets. See the dated contract revision in
// .impeccable/metrics-contract.md.
const (
	rateWindowS  = 30
	rateBucketMs = 100
	rateBucketN  = rateWindowS * 1000 / rateBucketMs
)

type rateBucket struct {
	t   int64 // bucket start, unix millis
	req float64
	byt float64
	tok float64
}

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
	RejectedTotal   int64
	BytesInTotal    int64
	BytesOutTotal   int64
	TokEstTotal     int64
}

// metricsRegistry is the mutex-guarded home for request counters, the TTFT
// EWMA, and the bounded burst ring (a fixed circular buffer: no per-append
// copies once full).
type metricsRegistry struct {
	mu       sync.Mutex
	backends map[string]*backendMetrics
	ring     []requestSample
	ringHead int // next write index
	ringN    int
	ringCap  int

	// ttft: the last 4096 eligible streaming first-byte samples, for the
	// router-side p50/p95 over a requested history window.
	ttft     [4096]ttftSample
	ttftHead int
	ttftN    int
}

type ttftSample struct {
	t  int64
	ms float64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{backends: map[string]*backendMetrics{}, ringCap: 2000}
}

var metrics = newMetricsRegistry()

// recordAccept records the demand side of a request the moment it is
// admitted for proxying: the 100 ms arrival bucket that feeds the live
// rates. Rejections (429/413) are not "accepted" — they never enter a
// backend — and are not recorded here.
func (m *metricsRegistry) recordAccept(backend string, bytesIn int64, tokEst int) {
	nowMs := time.Now().UnixMilli()
	start := nowMs / rateBucketMs * rateBucketMs
	idx := int(start/rateBucketMs) % rateBucketN
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		bm = &backendMetrics{}
		m.backends[backend] = bm
	}
	b := &bm.arrivals[idx]
	if b.t != start {
		*b = rateBucket{t: start}
	}
	b.req++
	b.byt += float64(bytesIn)
	b.tok += float64(tokEst)
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
	now := time.Now().UnixMilli()
	m.ttft[m.ttftHead] = ttftSample{t: now, ms: ttftMs}
	m.ttftHead = (m.ttftHead + 1) % len(m.ttft)
	if m.ttftN < len(m.ttft) {
		m.ttftN++
	}
}

// ttftPercentiles returns the eligible streaming first-byte sample count,
// actual coverage (oldest→newest), and p50/p95 over [from, to]. Percentiles
// are computed from real samples; when the ring covers less than the
// requested window, coverageMs says so.
func (m *metricsRegistry) ttftPercentiles(from, to int64) (n int, coverageMs int64, p50, p95 float64) {
	m.mu.Lock()
	vals := make([]float64, 0, m.ttftN)
	var oldest, newest int64
	for i := 0; i < m.ttftN; i++ {
		idx := (m.ttftHead - m.ttftN + i + len(m.ttft)) % len(m.ttft)
		s := m.ttft[idx]
		if s.t < from || s.t > to {
			continue
		}
		vals = append(vals, s.ms)
		if oldest == 0 || s.t < oldest {
			oldest = s.t
		}
		if s.t > newest {
			newest = s.t
		}
	}
	m.mu.Unlock()
	n = len(vals)
	if n == 0 {
		return 0, 0, 0, 0
	}
	sort.Float64s(vals)
	pick := func(q float64) float64 {
		i := int(q * float64(n-1))
		if i < 0 {
			i = 0
		}
		if i >= n {
			i = n - 1
		}
		return vals[i]
	}
	if newest > oldest {
		coverageMs = newest - oldest
	}
	return n, coverageMs, pick(0.5), pick(0.95)
}

// recordRequest folds one COMPLETED (or rejected) request into per-backend
// lifetime counters and the burst ring. Completion does not touch the rate
// window: rates measure arrivals, and their 30 s tail is a property of the
// window, not of the request's lifetime.
func (m *metricsRegistry) recordRequest(backend string, s requestSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bm, ok := m.backends[backend]
	if !ok {
		bm = &backendMetrics{}
		m.backends[backend] = bm
	}
	if !s.accepted {
		bm.rejectedTotal++
	}
	bm.reqTotal++
	bm.bytesInTotal += s.BytesIn
	bm.bytesOutTotal += s.BytesOut
	bm.tokEstTotal += int64(s.NewTokensEst)
	if m.ring == nil {
		m.ring = make([]requestSample, m.ringCap)
	}
	m.ring[m.ringHead] = s
	m.ringHead = (m.ringHead + 1) % m.ringCap
	if m.ringN < m.ringCap {
		m.ringN++
	}
}

// burst returns up to limit ring samples, oldest first (newest last), capped
// at the ring length.
func (m *metricsRegistry) burst(limit int) []requestSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit > m.ringN {
		limit = m.ringN
	}
	if limit < 0 {
		limit = 0
	}
	out := make([]requestSample, 0, limit)
	start := m.ringHead - limit
	for i := 0; i < limit; i++ {
		idx := ((start+i)%m.ringCap + m.ringCap) % m.ringCap
		out = append(out, m.ring[idx])
	}
	return out
}

// computeRates refreshes per-backend req/bytes/tok-est live rates from the
// trailing 30 s arrival buckets. The full window is the denominator even
// when traffic is sparse, so the value is a sustained arrival rate rather
// than a per-tick spike. A bucket older than the window is excluded with a
// hard cutoff (no decay tail).
func (m *metricsRegistry) computeRates(now time.Time) {
	cutoff := now.Add(-rateWindowS * time.Second).UnixMilli()
	nowMs := now.UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, bm := range m.backends {
		var req, byt, tok float64
		for i := range bm.arrivals {
			b := &bm.arrivals[i]
			if b.t > cutoff && b.t <= nowMs {
				req += b.req
				byt += b.byt
				tok += b.tok
			}
		}
		bm.reqRate = req / rateWindowS
		bm.bytesRate = byt / rateWindowS
		bm.tokEstRate = tok / rateWindowS
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
		RejectedTotal:   bm.rejectedTotal,
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
// stdlib text-exposition parser at 1s. A 404 endpoint backs off to 30s
// re-probes (engineOffBackoff) since the route only reappears on an engine
// restart; an unreachable ("err") endpoint is probed on every tick so a
// vanishing engine is detected within one interval. vLLM always serves
// /metrics; SGLang requires --enable-metrics. When the endpoint is absent
// or down the view degrades to status "off"/"err" and the sheet renders
// "—" — never a guess.
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
	PrefillTotalTokS float64  `json:"prefillTotalTokS"`
	TTFTms           float64  `json:"ttftMs"`
	HTTPStatus       int      `json:"httpStatus"`
	LastError        string   `json:"lastError,omitempty"`

	// Interval observation counts (additive). 0 = no observations in this
	// scrape interval, so a numeric mean of 0 renders as "—", never a real
	// zero. These are the honesty boundary for histogram means.
	TTFTSamples          int64 `json:"ttftSamples"`
	ITLSamples           int64 `json:"itlSamples"`
	E2ESamples           int64 `json:"e2eSamples"`
	QueueSamples         int64 `json:"queueSamples"`
	PrefillSamples       int64 `json:"prefillSamples"`
	DecodeSamples        int64 `json:"decodeSamples"`
	PerTokSamples        int64 `json:"perTokSamples"`
	PromptSamples        int64 `json:"promptSamples"`
	GenSamples           int64 `json:"genSamples"`
	TTFTStreamSamples    int64 `json:"ttftStreamSamples"`
	TTFTNonStreamSamples int64 `json:"ttftNonStreamSamples"`

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
	lastCnt  float64
	have     bool // a delta mean has been computed (else lastSum is stale/zero)
	seen     bool // this histogram appeared in the current scrape
}

type engineScrape struct {
	url    string
	status string
	// offNext: the next allowed probe while status is "off" (404/405).
	// The endpoint is structurally absent until the engine restarts, so
	// probes back off to engineOffBackoff instead of once per second.
	offNext    time.Time
	httpStatus int
	lastErr    string
	view       engineView
	// counters: rate-differenced between scrapes, keyed by name+labels.
	counters map[string]float64
	cTimes   map[string]time.Time
	rSeen    map[string]bool // counters baselined in the current scrape
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

// engineOffBackoff is the re-probe interval for a 404/405 /metrics endpoint
// ("off"): the route is absent until the engine restarts (e.g. SGLang gains
// --enable-metrics), so probing once per second only spams the engine's
// access log. "err" (unreachable) is never backed off — the engine may come
// back any moment and the sheet wants fast recovery detection.
const engineOffBackoff = 30 * time.Second

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
	// A 404 endpoint is structurally absent (SGLang without
	// --enable-metrics): it only reappears on an engine restart, so hold
	// the probe at engineOffBackoff instead of hammering once per second.
	// Unreachable ("err") endpoints are the opposite: a network blip can
	// end at any moment, so they are re-probed on every tick (no backoff)
	// and recovery is detected immediately.
	if s.status == "off" && time.Now().Before(s.offNext) {
		e.mu.Unlock()
		return
	}
	url := s.url
	e.mu.Unlock()

	var httpStatus int
	var fetchErr error
	body, gone, ok := func() ([]byte, bool, bool) {
		resp, err := e.client.Get(url)
		if err != nil {
			fetchErr = err
			return nil, false, false
		}
		defer resp.Body.Close()
		httpStatus = resp.StatusCode
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			return nil, true, false
		}
		if resp.StatusCode != http.StatusOK {
			fetchErr = fmt.Errorf("engine metrics HTTP %d", resp.StatusCode)
			return nil, false, false
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			fetchErr = err
			return nil, false, false
		}
		return b, false, true
	}()

	e.mu.Lock()
	defer e.mu.Unlock()
	if !ok {
		if gone {
			s.status = "off"
			s.lastErr = ""
			s.httpStatus = httpStatus
			s.offNext = time.Now().Add(engineOffBackoff)
		} else {
			// No backoff: unreachable endpoints are re-probed on the next
			// tick so a vanishing engine is detected within one interval.
			s.status = "err"
			s.httpStatus = httpStatus
			if fetchErr != nil {
				s.lastErr = fetchErr.Error()
			} else {
				s.lastErr = "engine metrics unavailable"
			}
		}
		// Rates and interval means are deltas over a scrape interval; a stale
		// delta would mislead, so zero them and re-baseline every counter and
		// histogram. Gauges and lifetime counters keep their last known
		// values (lifetime facts).
		s.resetBaselines()
		s.zeroIntervalFields()
		return
	}
	s.offNext = time.Time{}
	s.httpStatus = httpStatus
	s.lastErr = ""
	s.scrapeBody(body)
}

// zeroF64Ptr returns a pointer to a freshly allocated zero float.
// Snapshot readers dereference view pointers outside e.mu, so published
// values must never be written through in place; replacing the pointer
// with a new one is the race-free way to zero a nil-able field.
func zeroF64Ptr() *float64 {
	z := 0.0
	return &z
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
	v.PrefillTotalTokS = 0
	v.TTFTSamples = 0
	v.ITLSamples = 0
	v.E2ESamples = 0
	v.QueueSamples = 0
	v.PrefillSamples = 0
	v.DecodeSamples = 0
	v.PerTokSamples = 0
	v.PromptSamples = 0
	v.GenSamples = 0
	v.TTFTStreamSamples = 0
	v.TTFTNonStreamSamples = 0
	v.HTTPStatus = s.httpStatus
	v.LastError = s.lastErr
	if v.TTFTStreamMs != nil {
		v.TTFTStreamMs = zeroF64Ptr()
	}
	if v.TTFTNonStreamMs != nil {
		v.TTFTNonStreamMs = zeroF64Ptr()
	}
	if v.RetractedTokS != nil {
		v.RetractedTokS = zeroF64Ptr()
	}
	if v.PrefillMs != nil {
		v.PrefillMs = zeroF64Ptr()
	}
	if v.DecodeMs != nil {
		v.DecodeMs = zeroF64Ptr()
	}
	if v.PerTokMs != nil {
		v.PerTokMs = zeroF64Ptr()
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
// scaledFinite reports whether v*100 stays finite. A healthy engine reports
// fractions (0..1); a pathological value past ~3.4e35 would overflow the ×100
// scale to ±Inf and json.Marshal fatals the live stream — treated as absent.
func scaledFinite(v float64) bool { return !math.IsInf(v*100, 0) }

func (s *engineScrape) scrapeBody(raw []byte) {
	now := time.Now()
	samples, truncated := parsePromText(raw)
	// A capped parse lost the tail of the needed series (a histogram
	// _count after its _sum skews updateHist baselines; a dropped
	// _Total understates a lifetime counter). Treat the scrape as
	// incomplete like the empty body — missing data, not ok with
	// understated sums.
	if truncated {
		s.status = "err"
		s.lastErr = "engine metrics body truncated (past sample cap)"
		s.resetBaselines()
		s.zeroIntervalFields()
		return
	}
	if s.seenEng == nil {
		s.seenEng = map[string]bool{}
	}
	if s.hists == nil {
		s.hists = map[string]*histState{}
	}
	if s.counters == nil {
		s.counters = map[string]float64{}
	}
	if s.cTimes == nil {
		s.cTimes = map[string]time.Time{}
	}
	// Baselines not re-observed this scrape are pruned after the body is
	// folded (see the rSeen pass at the end of scrapeBody).
	s.rSeen = map[string]bool{}
	if len(samples) == 0 {
		s.status = "err"
		s.lastErr = "empty engine metrics body"
		s.resetBaselines()
		s.zeroIntervalFields()
		return
	}
	for _, h := range s.hists {
		h.have = false
		h.lastCnt = 0
		h.seen = false
	}

	// First pass: family detection + gauge accumulation.
	vllm, sg := false, false
	var running, waiting float64
	var kvMax float64
	haveKV := false
	var waitCap, waitDefer float64
	haveCap, haveDefer := false, false
	var hitRate, sloCap, ctxLen float64
	var haveHitRate, haveSLO, haveCTX bool
	var kvMemGB, weightMemGB float64
	var haveKVMem, haveWMem bool
	var retracted float64
	haveRetr := false
	var haveRetrTok bool
	var fullPct, swaPct, mambaPct float64
	var haveFull, haveSWA, haveMamba bool
	var kvUsed, kvCap, kvFree, kvEvict float64
	var haveKVUsed, haveKVCap, haveKVFree, haveKVEvict bool
	var vllmKVCap float64
	var haveVllmKVCap bool
	vllmCapacityComplete := true
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
			case "vllm:cache_config_info":
				// Group-aware capacity is reported per DP engine. Block count
				// times block size is not equivalent for hybrid models.
				capacity, err := strconv.ParseFloat(sm.labels["kv_cache_size_tokens"], 64)
				if err != nil || capacity <= 0 || math.IsNaN(capacity) || math.IsInf(capacity, 0) || sm.value != 1 {
					vllmCapacityComplete = false
				} else {
					vllmKVCap += capacity
					haveVllmKVCap = true
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
			// Presence = the sample was seen, tracked separately from the
			// per-rank MAX: a valid measured zero (an exhausted pool) must
			// not publish as null.
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
				haveKV = true
				if sm.value > kvMax {
					kvMax = sm.value
				}
			case "sglang:full_token_usage":
				haveFull = true
				if sm.value > fullPct {
					fullPct = sm.value
				}
			case "sglang:swa_token_usage":
				haveSWA = true
				if sm.value > swaPct {
					swaPct = sm.value
				}
			case "sglang:mamba_usage":
				haveMamba = true
				if sm.value > mambaPct {
					mambaPct = sm.value
				}
			case "sglang:num_used_tokens":
				haveKVUsed = true
				if sm.value > kvUsed {
					kvUsed = sm.value
				}
			case "sglang:max_total_num_tokens":
				haveKVCap = true
				if sm.value > kvCap {
					kvCap = sm.value
				}
			case "sglang:kv_available_tokens":
				haveKVFree = true
				if sm.value > kvFree {
					kvFree = sm.value
				}
			case "sglang:kv_evictable_tokens":
				haveKVEvict = true
				if sm.value > kvEvict {
					kvEvict = sm.value
				}
			case "sglang:mamba_used_tokens":
				haveMambaUsed = true
				if sm.value > mambaUsed {
					mambaUsed = sm.value
				}
			case "sglang:mamba_available_tokens":
				haveMambaAvail = true
				if sm.value > mambaAvail {
					mambaAvail = sm.value
				}
			case "sglang:mamba_evictable_tokens":
				haveMambaEvict = true
				if sm.value > mambaEvict {
					mambaEvict = sm.value
				}
			case "sglang:num_retracted_reqs":
				haveRetr = true
				if sm.value > retracted {
					retracted = sm.value
				}
			case "sglang:cache_hit_rate":
				haveHitRate = true
				if sm.value > hitRate {
					hitRate = sm.value
				}
			case "sglang:max_running_requests_under_SLO":
				haveSLO = true
				if sm.value > sloCap {
					sloCap = sm.value
				}
			case "sglang:context_len":
				haveCTX = true
				if sm.value > ctxLen {
					ctxLen = sm.value
				}
			case "sglang:kv_cache_memory_usage_gb":
				haveKVMem = true
				if sm.value > kvMemGB {
					kvMemGB = sm.value
				}
			case "sglang:weight_memory_usage_gb":
				haveWMem = true
				if sm.value > weightMemGB {
					weightMemGB = sm.value
				}
			case "sglang:hicache_host_used_tokens":
				haveHCUsed = true
				if sm.value > hicacheUsed {
					hicacheUsed = sm.value
				}
			case "sglang:hicache_host_total_tokens":
				haveHCCap = true
				if sm.value > hicacheCap {
					hicacheCap = sm.value
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
	view := engineView{Status: "ok", HTTPStatus: s.httpStatus}

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
	if haveKV && scaledFinite(kvMax) {
		view.KVPct = kvMax * 100
	}

	// Optional gauges: pointer only when the metric appeared this scrape.
	if vllm {
		if haveVllmKVCap && vllmCapacityComplete && !math.IsInf(vllmKVCap, 0) {
			view.KVCapTok = &vllmKVCap
		}
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
		if haveFull && scaledFinite(fullPct) {
			v := fullPct * 100
			view.FullPct = &v
		}
		if haveSWA && scaledFinite(swaPct) {
			v := swaPct * 100
			view.SwaPct = &v
		}
		if haveMamba && scaledFinite(mambaPct) {
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
			view.MambaUsedTok = &u
			// Derived total of finite parsed values; an overflow to Inf
			// would kill json.Marshal (and the whole stream). Keep 0 over
			// a poisoned total, as the GPU memory total does.
			c := mambaUsed + mambaAvail + mambaEvict
			if math.IsInf(c, 0) {
				c = 0
			}
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
		if haveHitRate {
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
		var seenCompute, seenCache, seenPromptTotal bool
		for _, sm := range samples {
			switch sm.name {
			case "vllm:prompt_tokens_by_source_total":
				switch sm.labels["source"] {
				case "local_compute":
					view.PrefillTokS += s.counterRate(sm, now)
					seenCompute = true
				case "local_cache_hit":
					view.PrefillCacheTokS += s.counterRate(sm, now)
					seenCache = true
				}
			case "vllm:prompt_tokens_total":
				promptTokS += s.counterRate(sm, now)
				seenPromptTotal = true
			case "vllm:generation_tokens_total":
				view.DecodeTokS += s.counterRate(sm, now)
			}
		}
		// Compute-only prefill comes from by_source only when the split is
		// complete; a single source label must not pass for the whole. Older
		// vLLM builds without by_source fall back to prompt_tokens_total.
		switch {
		case seenCompute && seenCache:
			view.PrefillTotalTokS = view.PrefillTokS + view.PrefillCacheTokS
		case seenCompute:
			view.PrefillCacheTokS = 0
			view.PrefillTotalTokS = view.PrefillTokS
		case seenCache:
			view.PrefillTokS = 0
			view.PrefillTotalTokS = view.PrefillCacheTokS
		default:
			view.PrefillTokS = promptTokS
			view.PrefillCacheTokS = 0
			view.PrefillTotalTokS = promptTokS
		}
		if seenPromptTotal {
			view.PrefillTotalTokS = promptTokS
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
				case "prefill_compute":
					view.PrefillTokS += s.counterRate(sm, now)
				case "prefill_cache":
					view.PrefillCacheTokS += s.counterRate(sm, now)
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
		view.PrefillTotalTokS = view.PrefillTokS + view.PrefillCacheTokS
	}
	// Histogram observation counts for this interval (0 = no observations).
	view.TTFTSamples = s.histCount("vllm:time_to_first_token_seconds")
	view.ITLSamples = s.histCount("vllm:inter_token_latency_seconds")
	view.E2ESamples = s.histCount("vllm:e2e_request_latency_seconds")
	view.QueueSamples = s.histCount("vllm:request_queue_time_seconds")
	view.PrefillSamples = s.histCount("vllm:request_prefill_time_seconds")
	view.DecodeSamples = s.histCount("vllm:request_decode_time_seconds")
	view.PerTokSamples = s.histCount("vllm:request_time_per_output_token_seconds")
	view.PromptSamples = s.histCount("vllm:request_prompt_tokens")
	view.GenSamples = s.histCount("vllm:request_generation_tokens")
	if sg {
		view.TTFTSamples = s.histCount("sglang:time_to_first_token_seconds")
		view.ITLSamples = s.histCount("sglang:inter_token_latency_seconds")
		view.E2ESamples = s.histCount("sglang:e2e_request_latency_seconds")
		view.QueueSamples = s.histCount("sglang:queue_time_seconds")
		view.PrefillSamples = 0
		view.DecodeSamples = 0
		view.PerTokSamples = 0
		view.PromptSamples = s.histCount("sglang:prompt_tokens_histogram")
		view.GenSamples = s.histCount("sglang:generation_tokens_histogram")
		view.TTFTStreamSamples = s.histCount("sglang:time_to_first_token_seconds|true")
		view.TTFTNonStreamSamples = s.histCount("sglang:time_to_first_token_seconds|false")
	}
	// Drop histogram state for series that vanished this scrape: their
	// baseline must not survive a gap.
	for k, h := range s.hists {
		if !h.seen {
			delete(s.hists, k)
		}
	}
	// Same for counter baselines: label churn in the engine adds new keys
	// every scrape, and old keys would be retained forever.
	for k := range s.counters {
		if !s.rSeen[k] {
			delete(s.counters, k)
			delete(s.cTimes, k)
		}
	}
	s.rSeen = nil
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

// histCount is the number of observations behind the last interval mean;
// 0 means "no data", never a measured zero.
func (s *engineScrape) histCount(key string) int64 {
	h, ok := s.hists[key]
	if !ok || !h.have {
		return 0
	}
	return int64(h.lastCnt)
}

// resetBaselines forgets every counter/histogram baseline so the first
// scrape after an outage is baseline-only (rates/means 0) instead of a
// delta spanning the gap.
func (s *engineScrape) resetBaselines() {
	s.counters = map[string]float64{}
	s.cTimes = map[string]time.Time{}
	for _, h := range s.hists {
		h.sum, h.cnt, h.lastSum, h.lastCnt = 0, 0, 0, 0
		h.have, h.seen = false, false
		h.t = time.Time{}
	}
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
	if s.rSeen != nil {
		s.rSeen[k] = true // prune absent baselines after the scrape
	}
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
			d := a.cnt - h.cnt
			if d > 0 {
				h.lastSum = (a.sum - h.sum) / d
				h.lastCnt = d
				h.have = true
			}
			h.sum, h.cnt = a.sum, a.cnt
		} else {
			h.sum, h.cnt = a.sum, a.cnt // reset: re-baseline
			h.have = false
			h.lastCnt = 0
		}
		h.t = now
		h.seen = true
	}
}

// counterSeen reports whether a counter sample was already baselined on a
// previous scrape.
func (s *engineScrape) counterSeen(sm promSample) bool {
	_, ok := s.counters[counterKey(sm)]
	return ok
}

// ─── Prometheus text parser (stdlib, dashboard needs only) ────────────────

// promSampleCap bounds the samples RETAINED from one exposition. The body
// size cap alone admits ~1M short lines every second; the scrape only
// reads a handful of metric families, so anything past the cap is dead
// weight. With the name filter, the cap counts the families the router
// consumes — an exposition dominated by unrelated series can no longer
// push a needed series (e.g. a histogram _count after its _sum) past
// the cap and publish partial data as complete.
const promSampleCap = 8192

// scrapeSamples is the set of metric names the router's scrapes
// consume, enumerated EXACTLY from the two consumers: the vllm:/sglang:
// gauges, counters, and histogram base names folded by
// engineScrape.scrapeBody, and the DCGM/nvidia_smi names handled by
// parseGPUDevices. The filter must not admit whole namespaces (the old
// "vllm:*", "sglang:*", "gpu:" prefixes): any series the router ignores
// counts against promSampleCap and can truncate the series it reads,
// publishing partial data as complete.
var scrapeSamples = map[string]struct{}{
	// vLLM — gauge/counter names folded by engineScrape.scrapeBody.
	"vllm:num_requests_running":           {},
	"vllm:num_requests_waiting":           {},
	"vllm:num_requests_waiting_by_reason": {},
	"vllm:kv_cache_usage_perc":            {},
	"vllm:cache_config_info":              {},
	"vllm:num_preemptions_total":          {},
	"vllm:request_success_total":          {},
	"vllm:prefix_cache_queries_total":     {},
	"vllm:prefix_cache_hits_total":        {},
	"vllm:mm_cache_queries_total":         {},
	"vllm:mm_cache_hits_total":            {},
	"vllm:prompt_tokens_by_source_total":  {},
	"vllm:prompt_tokens_total":            {},
	"vllm:generation_tokens_total":        {},
	// vLLM — histogram families: updateHist reads the _sum/_count parts
	// (the _bucket lines are not consumed — they only count cap volume and
	// can push a healthy scrape past the cap).
	"vllm:time_to_first_token_seconds":                 {},
	"vllm:time_to_first_token_seconds_sum":             {},
	"vllm:time_to_first_token_seconds_count":           {},
	"vllm:inter_token_latency_seconds":                 {},
	"vllm:inter_token_latency_seconds_sum":             {},
	"vllm:inter_token_latency_seconds_count":           {},
	"vllm:e2e_request_latency_seconds":                 {},
	"vllm:e2e_request_latency_seconds_sum":             {},
	"vllm:e2e_request_latency_seconds_count":           {},
	"vllm:request_queue_time_seconds":                  {},
	"vllm:request_queue_time_seconds_sum":              {},
	"vllm:request_queue_time_seconds_count":            {},
	"vllm:request_prefill_time_seconds":                {},
	"vllm:request_prefill_time_seconds_sum":            {},
	"vllm:request_prefill_time_seconds_count":          {},
	"vllm:request_decode_time_seconds":                 {},
	"vllm:request_decode_time_seconds_sum":             {},
	"vllm:request_decode_time_seconds_count":           {},
	"vllm:request_time_per_output_token_seconds":       {},
	"vllm:request_time_per_output_token_seconds_sum":   {},
	"vllm:request_time_per_output_token_seconds_count": {},
	"vllm:request_prompt_tokens":                       {},
	"vllm:request_prompt_tokens_sum":                   {},
	"vllm:request_prompt_tokens_count":                 {},
	"vllm:request_generation_tokens":                   {},
	"vllm:request_generation_tokens_sum":               {},
	"vllm:request_generation_tokens_count":             {},
	// SGLang — gauge/counter names folded by engineScrape.scrapeBody.
	"sglang:num_running_reqs":                 {},
	"sglang:num_queue_reqs":                   {},
	"sglang:token_usage":                      {},
	"sglang:full_token_usage":                 {},
	"sglang:swa_token_usage":                  {},
	"sglang:mamba_usage":                      {},
	"sglang:num_used_tokens":                  {},
	"sglang:max_total_num_tokens":             {},
	"sglang:kv_available_tokens":              {},
	"sglang:kv_evictable_tokens":              {},
	"sglang:mamba_used_tokens":                {},
	"sglang:mamba_available_tokens":           {},
	"sglang:mamba_evictable_tokens":           {},
	"sglang:num_retracted_reqs":               {},
	"sglang:cache_hit_rate":                   {},
	"sglang:max_running_requests_under_SLO":   {},
	"sglang:context_len":                      {},
	"sglang:kv_cache_memory_usage_gb":         {},
	"sglang:weight_memory_usage_gb":           {},
	"sglang:hicache_host_used_tokens":         {},
	"sglang:hicache_host_total_tokens":        {},
	"sglang:num_aborted_requests_total":       {},
	"sglang:num_retracted_input_tokens_total": {},
	"sglang:num_requests_total":               {},
	"sglang:prompt_tokens_total":              {},
	"sglang:generation_tokens_total":          {},
	"sglang:realtime_tokens_total":            {},
	// SGLang — histogram families (same part coverage as vLLM).
	"sglang:time_to_first_token_seconds":       {},
	"sglang:time_to_first_token_seconds_sum":   {},
	"sglang:time_to_first_token_seconds_count": {},
	"sglang:inter_token_latency_seconds":       {},
	"sglang:inter_token_latency_seconds_sum":   {},
	"sglang:inter_token_latency_seconds_count": {},
	"sglang:e2e_request_latency_seconds":       {},
	"sglang:e2e_request_latency_seconds_sum":   {},
	"sglang:e2e_request_latency_seconds_count": {},
	"sglang:queue_time_seconds":                {},
	"sglang:queue_time_seconds_sum":            {},
	"sglang:queue_time_seconds_count":          {},
	"sglang:prompt_tokens_histogram":           {},
	"sglang:prompt_tokens_histogram_sum":       {},
	"sglang:prompt_tokens_histogram_count":     {},
	"sglang:generation_tokens_histogram":       {},
	"sglang:generation_tokens_histogram_sum":   {},
	"sglang:generation_tokens_histogram_count": {},
	// DCGM exporter — names handled by parseGPUDevices.
	"DCGM_FI_DEV_GPU_UTIL":    {},
	"DCGM_FI_DEV_FB_USED":     {},
	"DCGM_FI_DEV_FB_TOTAL":    {},
	"DCGM_FI_DEV_FB_FREE":     {},
	"DCGM_FI_DEV_GPU_TEMP":    {},
	"DCGM_FI_DEV_POWER_USAGE": {},
	// nvidia_gpu_exporter — names handled by parseGPUDevices.
	"nvidia_smi_utilization_gpu_ratio": {},
	"nvidia_smi_memory_used_bytes":     {},
	"nvidia_smi_memory_total_bytes":    {},
	"nvidia_smi_temperature_gpu":       {},
	"nvidia_smi_power_draw_watts":      {},
}

func isScrapeSample(name string) bool {
	_, ok := scrapeSamples[name]
	return ok
}

// parsePromText parses the Prometheus text exposition format well enough
// for gauges, counters, and histogram _sum/_count parts. HELP/TYPE lines,
// openmetrics timestamps, and unparseable lines are skipped. Only samples
// from the exact metric names in scrapeSamples are retained — the memory
// cost of the scrape is bounded by promSampleCap, not the body size. The
// scan always walks the whole body (line by line, no full-body split), so
// memory is bounded by that count, not the body. truncated is true when
// the exposition held more retained samples than the cap: in that case
// later series are lost and the consumer must treat the scrape as
// incomplete (missing data), never as ok with understated sums.
func parsePromText(body []byte) (samples []promSample, truncated bool) {
	var out []promSample
	// Scan line by line instead of strings.Split over the whole body: a
	// 4 MiB body of many short lines must not allocate a million-entry
	// slice; found samples past the cap are counted, not kept.
	for rest := body; len(rest) > 0; {
		nl := bytes.IndexByte(rest, '\n')
		var line, next []byte
		if nl >= 0 {
			line = rest[:nl]
			next = rest[nl+1:]
		} else {
			line = rest
		}
		rest = next
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		line = bytes.TrimRight(line, " \r")
		head, val, ok := splitPromLine(string(line))
		if !ok {
			continue
		}
		name, labels, hok := splitPromHead(head)
		if !hok || !isScrapeSample(name) {
			continue
		}
		v, err := strconv.ParseFloat(val, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			// Non-finite values are valid in the Prometheus format, but
			// json.Marshal refuses them — one poisoned sample would drop
			// the whole live stream. Treat it as an unparseable line.
			continue
		}
		if len(out) >= promSampleCap {
			truncated = true
			continue
		}
		out = append(out, promSample{name: name, labels: labels, value: v})
	}
	return out, truncated
}

// splitPromLine separates the metric head from its value without splitting
// inside a quoted label value or a {...} block. Label values legitimately
// contain spaces (e.g. modelName="NVIDIA H100 80GB HBM3"); the old
// strings.Fields approach dropped every such series. An optional trailing
// timestamp is ignored.
func splitPromLine(line string) (head, val string, ok bool) {
	var fields []string
	start := 0
	inQuote := false
	depth := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inQuote && c == '\\' {
			i++ // escaped character inside a label value
			continue
		}
		switch c {
		case '"':
			inQuote = !inQuote
		case '{':
			if !inQuote {
				depth++
			}
		case '}':
			if !inQuote && depth > 0 {
				depth--
			}
		case ' ', '\t':
			if !inQuote && depth == 0 {
				if i > start {
					fields = append(fields, line[start:i])
				}
				start = i + 1
			}
		}
	}
	if start < len(line) {
		fields = append(fields, line[start:])
	}
	if len(fields) < 2 {
		return "", "", false
	}
	return fields[0], fields[1], true
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
	// NewTok: WHOLE-conversation new-prefill estimate
	// (estimatePrefillTokens), matching the admission gate — the
	// last-message telemetry figure (estimateNewTokens) is a separate
	// burst-feed field.
	NewTok int `json:"newTok"`
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
	// AdmissionLease: the conversation currently holds a bounded admission
	// lease (active/accepted-queued = idleSince 0; idle = full-TTL
	// retention in progress, idleSinceMs = clock time). Absent/expired/
	// unkeyed/off = false/0. Both fields are always present.
	AdmissionLease       bool  `json:"admissionLease"`
	AdmissionIdleSinceMs int64 `json:"admissionIdleSinceMs"`
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
		// Never evict a conversation with a request in flight: its
		// aggregate is the only owner of LiveN and the finish path would
		// otherwise decrement a recreated aggregate (phantom/negative live).
		var oldestKey string
		var oldestMs int64 = math.MaxInt64
		for k, v := range r.sessions {
			if v.LiveN > 0 {
				continue
			}
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
		// The evicted request never runs finish(), so retire its liveness
		// here or its session stays marked live forever (put() can then
		// never evict it and the store grows past its cap).
		if l, ok := r.live[oldestID]; ok && l.Sess != "" {
			if a, ok := r.sessions[l.Sess]; ok && a.LiveN > 0 {
				a.LiveN--
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
		a.LastMs = now
		a.CtxTok = ctxTok
		a.NewTok = newTok
	}
	return id
}

func (r *sessionRegistry) firstByte(id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.live[id]; ok && l.Stream {
		// Non-streaming responses arrive in one body; their first byte
		// marks generation END, so they keep phase "admitted" throughout.
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
			if a.LiveN > 0 {
				a.LiveN--
			}
			a.appendReq(req)
			r.put(l.Sess, a)
		}
	}
	r.mu.Unlock()
}

// finishRejection folds a router-side rejection (429/413) directly into the
// session ring/aggregates — rejections never enter the live stack. newTok is
// the whole-conversation prefill estimate (the same figure the live/finish
// paths record); the last-message burst newTokensEst stays out of sessions.
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
	// NOTE: a single explicit Unlock below — the live/session assembly
	// runs locked, the lease snapshots (which lock the admission
	// managers) run UNlocked. No early returns in between.
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
	liveCounts := map[string]int{}
	for _, l := range r.live {
		if l.Sess != "" {
			liveCounts[l.Sess]++
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
		// The live map is the source of truth for liveness; the stored
		// counter is repaired here so a missed decrement cannot linger.
		a.LiveN = liveCounts[a.ID]
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
			// Ring is newest-at-(reqIdx-1); emit oldest first. After the
			// ring wraps (reqIdx caught up to the oldest slot) the oldest
			// request sits at reqIdx itself and the slice overruns, so
			// copy the two segments separately.
			reqs := make([]sessReqFrame, a.reqN)
			start := (a.reqIdx - a.reqN + sessReqsRing) % sessReqsRing
			first := sessReqsRing - start
			if first > a.reqN {
				first = a.reqN
			}
			copy(reqs[:first], a.reqs[start:])
			copy(reqs[first:], a.reqs[:a.reqIdx])
			f.Reqs = reqs
		}
		out = append(out, f)
	}
	r.mu.Unlock()

	// Lease status comes from the admission managers, never from this
	// registry: the bounded live map above may evict records and queued
	// accepted requests are absent from it entirely, but the lease map is
	// authoritative. Unlock the registry first so snapshot (which locks
	// the manager) cannot interleave registry mutation order. Out of
	// scope: unkeyed/off rows and backends without a manager stay
	// false/0. This endpoint is a bounded feed, not a lease inventory.
	for i := range out {
		mgr, ok := sessionAdmissions[out[i].Backend]
		if !ok || mgr == nil {
			continue
		}
		if held, idleSince := mgr.snapshot(out[i].ID); held {
			out[i].AdmissionLease = true
			if idleSince.UnixMilli() > 0 {
				out[i].AdmissionIdleSinceMs = idleSince.UnixMilli()
			}
		}
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
	// Hash the framed message pieces incrementally: the body is already
	// in memory once, and per-request keying runs it through sha1 without
	// building a second body-sized copy (builder + []byte) before the
	// caller's admission checks.
	h := sha1.New()
	for i, m := range prefix {
		if i > 0 {
			h.Write([]byte{'\n'})
		}
		// Length-framed role + content: different role partitions must not
		// collide, and concatenation ambiguity is impossible.
		fmt.Fprintf(h, "%d:%s:%d:", len(m.Role), m.Role, len(m.Content))
		h.Write(m.Content)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:12]
}

// ─── TTL-bounded session admission ────────────────────────────────────────
//
// Stable per-backend conversation leases retain a bounded admitted set
// across idle gaps: a conversation that completed (or was terminated)
// holds its admission slot for SESSION_IDLE_TTL before newcomers may take
// it. Disabled (SESSION_ADMISSION unset) and unkeyed traffic keep the
// legacy per-request behavior exactly.

type sessionAdmissionConfig struct {
	Enabled     bool
	MaxAdmitted int // per-backend cap; 0 until resolved by the loader
	MaxWaiting  int // distinct-by-arrival queue bound per backend; 0 = reject newcomers
	IdleTTL     time.Duration
	KeySource   string // canonical: prompt_cache_key | user | header:<Name>
	// keyName is the selected header name when KeySource is header:<Name>.
	keyName string
}

// loadSessionAdmissionConfig parses the SESSION_* environment. The enabled
// flag short-circuits: when off, nothing else is parsed, no managers exist.
func loadSessionAdmissionConfig(getenv func(string) string, backends []Backend) (sessionAdmissionConfig, error) {
	var cfg sessionAdmissionConfig

	switch v := getenv("SESSION_ADMISSION"); v {
	case "", "off":
		return cfg, nil
	case "on":
		cfg.Enabled = true
	default:
		return cfg, fmt.Errorf("invalid SESSION_ADMISSION %q", v)
	}

	// SESSION_MAX_ADMITTED: empty inherits each backend's positive
	// maxConcurrent; explicit must be a positive decimal integer.
	explicitCap := 0
	if v := getenv("SESSION_MAX_ADMITTED"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid SESSION_MAX_ADMITTED %q", v)
		}
		explicitCap = n
	}
	for _, b := range backends {
		if explicitCap > 0 {
			continue
		}
		if b.MaxConcurrent <= 0 {
			return cfg, fmt.Errorf("SESSION_MAX_ADMITTED is required for backend %q with unlimited maxConcurrent", b.Name)
		}
	}

	// SESSION_MAX_WAITING: empty inherits each backend's effective
	// maxQueueDepth (0 becomes 2); explicit must be a nonnegative
	// decimal integer. It bounds queued ADMISSION requests per backend
	// (distinct-by-arrival), independent of execution queue depth.
	explicitWaiting := -1
	if v := getenv("SESSION_MAX_WAITING"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid SESSION_MAX_WAITING %q", v)
		}
		explicitWaiting = n
	} else {
		for _, b := range backends {
			if mw := b.MaxQueueDepth; mw > 0 {
				if explicitWaiting == -1 {
					explicitWaiting = mw
				}
			} else if explicitWaiting == -1 {
				explicitWaiting = 2
			}
		}
	}
	if explicitWaiting == -1 || explicitWaiting < 0 {
		return cfg, fmt.Errorf("invalid SESSION_MAX_WAITING %q", "")
	}

	d := 5 * time.Minute
	if v := getenv("SESSION_IDLE_TTL"); v != "" {
		pd, err := time.ParseDuration(v)
		if err != nil || pd <= 0 {
			return cfg, fmt.Errorf("invalid SESSION_IDLE_TTL %q", v)
		}
		d = pd
	}

	switch src := getenv("SESSION_KEY_SOURCE"); src {
	case "":
		cfg.KeySource = "prompt_cache_key"
	case "prompt_cache_key", "user":
		cfg.KeySource = src
	case "header:":
		return cfg, fmt.Errorf("invalid SESSION_KEY_SOURCE %q", src)
	default:
		if strings.HasPrefix(src, "header:") {
			name := strings.TrimPrefix(src, "header:")
			if !isHTTPHeaderToken(name) {
				return cfg, fmt.Errorf("invalid SESSION_KEY_SOURCE %q", src)
			}
			// Lookup is case-insensitive (standard HTTP header API); the
			// canonical form canonicalizes KEY inside the hashed source.
			cfg.KeySource = "header:" + textproto.CanonicalMIMEHeaderKey(name)
			cfg.keyName = textproto.CanonicalMIMEHeaderKey(name)
		} else {
			return cfg, fmt.Errorf("invalid SESSION_KEY_SOURCE %q", src)
		}
	}

	cfg.MaxAdmitted = explicitCap // 0 → resolved per backend in main
	cfg.MaxWaiting = explicitWaiting
	cfg.IdleTTL = d
	return cfg, nil
}

// isHTTPHeaderToken reports whether name is a nonempty ASCII HTTP token.
func isHTTPHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
		if !ok {
			return false
		}
	}
	return true
}

// rawStringIdentity decodes a raw JSON field and returns its exact string
// value when it is a non-whitespace JSON string; "" otherwise. Raw fields
// keep a wrong-type/new-shape value from breaking request decoding.
func rawStringIdentity(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

// admissionKeyFor returns the opaque backend-scoped lease ID for a request
// or "" when no usable identity exists (off, unkeyed, or missing key).
// Identity is the exact decoded string (no trim/case-fold), framed with the
// backend name and selected source; selection is by priority: a usable
// prompt_cache_key always wins, then the configured fallback (user field or
// header). Nothing falls back implicitly to message content.
func admissionKeyFor(parsed *parsedBody, r *http.Request, backend, source, headerName string) string {
	sourceUsed, key := "", ""
	if k := rawStringIdentity(parsed.PromptCacheKey); k != "" {
		sourceUsed, key = "prompt_cache_key", k
	} else if source == "user" {
		if k := rawStringIdentity(parsed.User); k != "" {
			sourceUsed, key = "user", k
		}
	} else if source == "header:"+headerName {
		if k := r.Header.Get(headerName); k != "" && strings.TrimSpace(k) != "" {
			sourceUsed, key = source, k
		}
	}
	if key == "" {
		return ""
	}
	// Length-framed sha256 (sessionKeyFor's framing pattern), hashed
	// incrementally: no second body-sized copy is built.
	h := sha256.New()
	frame := func(v string) {
		fmt.Fprintf(h, "%d:", len(v))
		h.Write([]byte(v))
	}
	frame(backend)
	frame(sourceUsed)
	frame(key)
	return "sa:" + hex.EncodeToString(h.Sum(nil))
}

// admissionClock / admissionTimer make the lease TTL and waiter deadlines
// controllable in tests. Production delegates to time.
type admissionTimer interface{ Stop() bool }

type admissionClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) admissionTimer
}

type productionAdmissionClock struct{}

func (productionAdmissionClock) Now() time.Time { return time.Now() }
func (productionAdmissionClock) AfterFunc(d time.Duration, f func()) admissionTimer {
	return time.AfterFunc(d, f)
}

var (
	sessionAdmissionSettings sessionAdmissionConfig
	sessionAdmissions        map[string]*sessionAdmission // per resolved backend name
)

var (
	errSessionAdmissionQueueFull = errors.New("session admission queue full")
	errSessionAdmissionTimeout   = errors.New("session admission timeout")
)

// sessionAdmissionLease is one admitted identity's accepted set.
type sessionAdmissionLease struct {
	key       string
	count     int // accepted non-terminating requests currently holding the lease
	idle      bool
	idleSince time.Time // set when count reaches 0; meaningful only when idle
	gen       uint64    // bumped by every expiry/rearm; stale callbacks are dropped
	timer     admissionTimer
}

// sessionAdmissionWaiter is one queued admission request, FIFO-ordered.
type sessionAdmissionWaiter struct {
	key      string
	ctx      context.Context
	deadline time.Time  // absolute deadline (arrival + grace)
	result   chan error // exactly one terminal result
	el       *list.Element
	pending  bool
	timer    admissionTimer
	ticket   *sessionAdmissionTicket // set under mu when a grant is published
}

// sessionAdmission is the bounded per-backend lease gate: a lease map
// (authoritative for capacity) plus a strict-FIFO queue of blocked
// arrivals. All state moves under mu; timers carry the lease pointer and
// generation so stale callbacks can no-op.
type sessionAdmission struct {
	mu          sync.Mutex
	maxAdmitted int
	maxWaiting  int
	ttl         time.Duration
	clock       admissionClock
	leaseMap    map[string]*sessionAdmissionLease
	queue       *list.List
	waiting     int
}

func newSessionAdmission(maxAdmitted, maxWaiting int, ttl time.Duration, clock admissionClock) *sessionAdmission {
	if clock == nil {
		clock = productionAdmissionClock{}
	}
	return &sessionAdmission{
		maxAdmitted: maxAdmitted,
		maxWaiting:  maxWaiting,
		ttl:         ttl,
		clock:       clock,
		leaseMap:    map[string]*sessionAdmissionLease{},
		queue:       list.New(),
	}
}

// acquire blocks until the identity holds a lease, the queue is full, the
// grace deadline passes, or the context is cancelled. grace <= 0 permits an
// immediately eligible acquisition and never queues.
func (a *sessionAdmission) acquire(ctx context.Context, key string, grace time.Duration) (*sessionAdmissionTicket, error) {
	a.mu.Lock()
	if ctx.Err() != nil {
		a.mu.Unlock()
		return nil, ctx.Err()
	}
	a.pruneIdleLocked()
	a.drainLocked()

	// Existing identity: the lease is held regardless of remaining
	// capacity — retention is the point, and it may arrive even while
	// newcomers are queued. A DUE idle lease has no retained preference:
	// prune it and proceed as a fresh admission (it cannot bypass
	// currently-eligible queued newcomers added afterwards).
	existing := (*sessionAdmissionLease)(nil)
	if l, ok := a.leaseMap[key]; ok {
		if l.idle && !a.clock.Now().Before(l.idleSince.Add(a.ttl)) {
			if l.timer != nil {
				l.timer.Stop()
			}
			delete(a.leaseMap, key)
		} else {
			existing = l
		}
	}
	if existing != nil {
		existing.count++
		existing.idle = false
		existing.idleSince = time.Time{}
		if existing.timer != nil {
			existing.timer.Stop()
			existing.timer = nil
		}
		t := ticketFor(a, existing)
		a.mu.Unlock()
		return t, nil
	}

	// Fresh identity: admit now only when capacity exists AND no earlier
	// eligible newcomer (still queued, no lease of its own) is bypassed.
	if len(a.leaseMap) < a.maxAdmitted && !a.blockedByQueueLocked() {
		if ctx.Err() == nil {
			l := &sessionAdmissionLease{key: key, count: 1}
			a.leaseMap[key] = l
			t := ticketFor(a, l)
			a.mu.Unlock()
			return t, nil
		}
	}

	// Enqueue at the FIFO tail, or reject.
	if a.waiting >= a.maxWaiting || grace <= 0 {
		a.mu.Unlock()
		return nil, errSessionAdmissionQueueFull
	}
	w := &sessionAdmissionWaiter{
		key:      key,
		ctx:      ctx,
		deadline: a.clock.Now().Add(grace),
		result:   make(chan error, 1),
		pending:  true,
	}
	w.el = a.queue.PushBack(w)
	a.waiting++

	w.timer = a.clock.AfterFunc(grace, func() { a.waiterTimeout(w) })
	select {
	case <-ctx.Done():
		// Do not enqueue an already-cancelled request: no lease, no
		// terminal publication; the caller returns ctx.Err immediately.
		a.discardWaiterLocked(w)
		a.mu.Unlock()
		return nil, ctx.Err()
	default:
		// Watch the request context: a cancelled waiter is unlinked and
		// the next eligible waiter is served.
		go func() {
			<-ctx.Done()
			a.contextCancelled(ctx, w)
		}()
	}

	a.mu.Unlock()
	select {
	case err := <-w.result:
		if err != nil {
			return nil, err
		}
		// Race: the grant was published, then the context cancelled
		// before this goroutine woke. Release the ticket exactly once.
		if ctxErr := ctx.Err(); ctxErr != nil {
			w.ticket.release()
			return nil, ctxErr
		}
		return w.ticket, nil
	case <-ctx.Done():
		a.mu.Lock()
		cancelErr := ctx.Err()
		wasPending := w.pending
		t := w.ticket
		if wasPending {
			a.failWaiterLocked(w, cancelErr)
			a.drainLocked()
		} else if t != nil {
			// The grant raced the cancellation: the waiter is already
			// unlinked; retire the published ticket exactly once.
			w.ticket = nil
			a.drainLocked()
		}
		a.mu.Unlock()
		if t != nil {
			t.release()
		}
		return nil, cancelErr
	}
}

// blockedByQueueLocked reports whether a fresh arrival would bypass an
// earlier queued newcomer: some pending, live, unexpired waiter that does
// not already hold a lease (it blocks only while capacity is exhausted).
func (a *sessionAdmission) blockedByQueueLocked() bool {
	now := a.clock.Now()
	for e := a.queue.Front(); e != nil; e = e.Next() {
		w := e.Value.(*sessionAdmissionWaiter)
		if !w.pending {
			continue
		}
		if w.ctx.Err() != nil || !now.Before(w.deadline) {
			continue // dead; purged on the settle pass
		}
		if _, held := a.leaseMap[w.key]; !held {
			return true
		}
	}
	return false
}

// waiterCancel watches the request context; a cancelled waiter is unlinked
// and, if it was at the head, unblocks the next eligible waiter.
func (a *sessionAdmission) contextCancelled(ctx context.Context, w *sessionAdmissionWaiter) {
	a.mu.Lock()
	if w.pending {
		a.failWaiterLocked(w, ctx.Err())
		a.drainLocked()
	}
	a.mu.Unlock()
}

func (a *sessionAdmission) waiterTimeout(w *sessionAdmissionWaiter) {
	a.mu.Lock()
	if w.pending {
		a.failWaiterLocked(w, errSessionAdmissionTimeout)
		a.drainLocked()
	}
	a.mu.Unlock()
}

// ticketFor ties a lease to a one-release ticket.
func ticketFor(a *sessionAdmission, l *sessionAdmissionLease) *sessionAdmissionTicket {
	return &sessionAdmissionTicket{mgr: a, lease: l}
}

// sessionAdmissionTicket is a single admission grant; release is once-guarded.
type sessionAdmissionTicket struct {
	mgr   *sessionAdmission
	lease *sessionAdmissionLease
	once  sync.Once
}

// release returns the lease's accepted count by one; the final release of
// an identity starts its full-TTL idle expiry. Never starts, stops, or
// consults anything outside the manager: registry and router globals are
// deliberately out of reach (lease authority is independent of LiveN).
func (t *sessionAdmissionTicket) release() {
	if t == nil || t.mgr == nil {
		return
	}
	t.once.Do(func() {
		a := t.mgr
		a.mu.Lock()
		a.releaseLocked(t.lease)
		a.mu.Unlock()
	})
}

// releaseLocked decrements the accepted count; on the final release it arms
// the idle expiry. Caller holds mu.
func (a *sessionAdmission) releaseLocked(l *sessionAdmissionLease) {
	if l.count > 0 {
		l.count--
	}
	if l.count > 0 {
		return // accepted count still positive: no TTL
	}
	l.idle = true
	l.idleSince = a.clock.Now()
	l.gen++
	a.rearmIdleLocked(l)
	// A newly idle lease may unblock nothing directly (the queue drains on
	// any terminal path). drainLocked is a no-op when nothing is eligible.
	a.drainLocked()
}

// pruneIdleLocked removes every due idle lease (count 0, now >= idleSince+ttl)
// under the same lock the arrivals make their grants.
func (a *sessionAdmission) pruneIdleLocked() {
	now := a.clock.Now()
	for k, l := range a.leaseMap {
		if !l.idle {
			continue
		}
		if now.Sub(l.idleSince) >= a.ttl {
			if l.timer != nil {
				l.timer.Stop()
			}
			delete(a.leaseMap, k)
		}
	}
}

// drainLocked walks the FIFO in arrival order, granting every eligible
// waiter: an existing (unexpired) lease grants regardless of remaining
// identity capacity; a new identity grants only when capacity exists. An
// ineligible waiter blocks only itself — a later waiter for the same
// identity remains eligible behind it. Modifying the list while ranging
// over Start requires re-resolving Front after each removal.
func (a *sessionAdmission) drainLocked() {
	for front := a.queue.Front(); front != nil; {
		w := front.Value.(*sessionAdmissionWaiter)
		if !w.pending {
			// Already terminal (cancellation lost the race with a
			// published grant): unlink and continue.
			a.queue.Remove(front)
			front = a.queue.Front()
			continue
		}
		// Cancellation / deadline: unlink, terminate, continue.
		if ctxErr := w.ctx.Err(); ctxErr != nil {
			a.failWaiterLocked(w, ctxErr)
			front = a.queue.Front()
			continue
		}
		if !a.clock.Now().Before(w.deadline) {
			// Deadline reached: queue timeout (not admitted).
			a.failWaiterLocked(w, errSessionAdmissionTimeout)
			front = a.queue.Front()
			continue
		}
		// Grant decision: an existing (unexpired) lease turns around
		// immediately; a new identity needs a free identity slot.
		if l, ok := a.leaseMap[w.key]; ok {
			if l.idle && !a.clock.Now().Before(l.idleSince.Add(a.ttl)) {
				// Idle lease due: prune it in place; re-decide below as a
				// new identity.
				if l.timer != nil {
					l.timer.Stop()
				}
				delete(a.leaseMap, w.key)
			} else {
				a.queue.Remove(front)
				a.grantWaiterLocked(w, l)
				front = a.queue.Front()
				continue
			}
		}
		if len(a.leaseMap) < a.maxAdmitted {
			a.queue.Remove(front)
			// grantWaiterLocked bumps count to 1: start at 0 so the
			// grant's single ticket balances.
			l := &sessionAdmissionLease{key: w.key}
			a.leaseMap[w.key] = l
			a.grantWaiterLocked(w, l)
			front = a.queue.Front()
			continue
		}
		// Blocked here: no lease and identity capacity exhausted. A LATER
		// waiter for an already-leased identity remains eligible behind
		// this one, so keep scanning.
		front = front.Next()
	}
}

// grantWaiterLocked publishes the grant and the ticket atomically under mu,
// then wakes the waiter. The count/ticket is held BEFORE publication so a
// cancelled acquire walks the once-release path exactly once.
func (a *sessionAdmission) grantWaiterLocked(w *sessionAdmissionWaiter, l *sessionAdmissionLease) {
	l.count++
	l.idle = false
	l.idleSince = time.Time{}
	if l.timer != nil {
		l.timer.Stop()
	}
	w.ticket = ticketFor(a, l)
	w.pending = false
	if w.timer != nil {
		w.timer.Stop()
	}
	a.waiting--
	select {
	case w.result <- nil:
	default:
		// Already consumed: the acquire goroutine saw ctx.Done racing the
		// grant and released w.ticket exactly once on its own path.
	}
}

// failWaiterLocked terminates a pending waiter with an error and unlinks it.
func (a *sessionAdmission) failWaiterLocked(w *sessionAdmissionWaiter, err error) {
	a.queue.Remove(w.el)
	w.pending = false
	if w.timer != nil {
		w.timer.Stop()
	}
	a.waiting--
	select {
	case w.result <- err:
	default:
	}
}

// discardWaiterLocked is the variant for a waiter that never became
// publishable (context already done at enqueue time).
func (a *sessionAdmission) discardWaiterLocked(w *sessionAdmissionWaiter) {
	if w.el != nil {
		a.queue.Remove(w.el)
	}
	w.pending = false
	if w.timer != nil {
		w.timer.Stop()
	}
	a.waiting--
}

// rearmIdleLocked arms the full-ttl expiry callback for the given lease;
// the lease pointer and generation are captured so stale callbacks no-op.
func (a *sessionAdmission) rearmIdleLocked(l *sessionAdmissionLease) {
	gen := l.gen
	l.timer = a.clock.AfterFunc(a.ttl, func() { a.idleExpiry(l, gen) })
}

// idleExpiry is the sole expiry driver: this timer — not new traffic and
// not metrics polling — wakes the queue when an idle lease's full TTL
// elapses.
func (a *sessionAdmission) idleExpiry(lease *sessionAdmissionLease, gen uint64) {
	a.mu.Lock()
	cur, ok := a.leaseMap[lease.key]
	// Same lease, same generation, still idle, full TTL elapsed: due.
	// Stale callbacks (reactivated/rearmed lease or already pruned) do
	// nothing.
	if !ok || cur != lease || lease.gen != gen || !lease.idle {
		a.mu.Unlock()
		return
	}
	if a.clock.Now().Sub(lease.idleSince) < a.ttl {
		// Invoked early (remaining-duration rearm): arm for the rest.
		rem := a.ttl - a.clock.Now().Sub(lease.idleSince)
		if rem > 0 {
			lease.timer = a.clock.AfterFunc(rem, func() { a.idleExpiry(lease, lease.gen) })
			a.mu.Unlock()
			return
		}
	}
	delete(a.leaseMap, lease.key)
	lease.timer = nil
	a.drainLocked()
	a.mu.Unlock()
}

// snapshot is the registry-side read: does this key currently hold a
// lease, active or idle?
func (a *sessionAdmission) snapshot(key string) (held bool, idleSince time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.leaseMap[key]
	if !ok || l.count == 0 && !l.idle {
		return false, time.Time{}
	}
	if l.idle {
		return true, l.idleSince
	}
	return true, time.Time{}
}

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
	metricsListen       string                      = "127.0.0.1:9100" // METRICS_LISTEN
	durations           map[string]*durationTracker                    // per backend name
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

// buildMux assembles the router's external route table. /v1/ is the proxy
// prefix; /health, /stats, and /v1/models stay public. The telemetry
// surface (/metrics/*, session data included) lives on a separate
// management listener — see buildMetricsMux / metricsListen. A future
// web.Handler() would be mounted at "/" by the caller, after this mux is
// built.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "vLLM router OK")
	})
	mux.HandleFunc("/stats", handleStats)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/", handleProxy)
	return mux
}

// buildMetricsMux assembles the management route table: the live metrics
// feeds and the session/history views. It is bound to METRICS_LISTEN
// (default 127.0.0.1:9100), never to the external listener, so other
// clients' session and conversation data is not exposed to the public
// surface.
func buildMetricsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics/stream", handleMetricsStream)
	mux.HandleFunc("/metrics/burst", handleBurst)
	mux.HandleFunc("/metrics/sessions", handleSessions)
	mux.HandleFunc("/metrics/history", handleHistory)
	// /stats is already public on the external listener; the dashboard's
	// last-resort /stats fallback is a relative URL, so it must also resolve
	// when the SPA is reached via the management listener.
	mux.HandleFunc("/stats", handleStats)
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

	admissionCfg, err := loadSessionAdmissionConfig(os.Getenv, backends)
	if err != nil {
		log.Fatalf("%v", err)
	}
	sessionAdmissionSettings = admissionCfg
	if admissionCfg.Enabled {
		sessionAdmissions = make(map[string]*sessionAdmission)
		for _, b := range backends {
			cap := admissionCfg.MaxAdmitted
			if cap <= 0 {
				cap = b.MaxConcurrent // loader guarantees positive here
			}
			sessionAdmissions[b.Name] = newSessionAdmission(cap, admissionCfg.MaxWaiting, admissionCfg.IdleTTL, nil)
			log.Printf("  session admission: %s cap=%d waiting=%d ttl=%s keySource=%s",
				b.Name, cap, admissionCfg.MaxWaiting, admissionCfg.IdleTTL, admissionCfg.KeySource)
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
	// with --enable-metrics (the scrape degrades to "off" when absent, and a
	// 404 endpoint is re-probed at engineOffBackoff, not once per second).
	for _, b := range backends {
		engineMetrics.add(b.Name, b.URL+"/metrics")
	}
	go engineMetrics.loop()

	// Optional device telemetry: a DCGM or nvidia_gpu_exporter Prometheus
	// endpoint. Absent by default; the dashboard shows an honest empty state.
	gpuURL := os.Getenv("GPU_METRICS_URL")
	gpuReaders.configure(gpuURL)
	if gpuURL != "" {
		go gpuReaders.loop()
		log.Printf("GPU device telemetry: scraping %s", gpuURL)
	}

	// Server-side history is independent of dashboard clients; it starts
	// here so the rings cover the process lifetime, not a browser tab.
	history.configure(os.Getenv("HISTORY_PATH"))
	if os.Getenv("HISTORY_PATH") != "" {
		log.Printf("history: persisting series to %s", os.Getenv("HISTORY_PATH"))
	}
	go history.loop()

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

	if v := os.Getenv("METRICS_LISTEN"); v != "" {
		if _, _, err := net.SplitHostPort(v); err != nil {
			log.Fatalf("invalid METRICS_LISTEN %q: %v", v, err)
		}
		metricsListen = v
	}

	// Management listener: the embedded SPA dashboard plus the live metrics
	// feeds, sharing one trust zone — it binds 127.0.0.1 by default
	// (METRICS_LISTEN) and is pointed at a LAN interface only when the
	// dashboard faces that network. Without the webui build tag
	// web.Handler() is a 404 stub, so the dashboard route is a no-op there.
	metricsMux := buildMetricsMux()
	metricsMux.Handle("/", web.Handler())
	go func() {
		log.Printf("management listener: dashboard + /metrics/* on %s", metricsListen)
		if err := http.ListenAndServe(metricsListen, metricsMux); err != nil {
			// The management surface (SPA dashboard, /metrics, /feeds,
			// /stats) is telemetry; a port collision or other bind failure
			// must not kill the data plane. Routing keeps serving without
			// the dashboard.
			log.Printf("management listener %s: %v — dashboard + /metrics feeds disabled, routing continues", metricsListen, err)
		}
	}()

	// Refresh per-backend + fleet rates for the live metrics feed. The
	// actual elapsed time between ticks feeds the window decay, so a
	// scheduler stall does not under-count the interval.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		for range ticker.C {
			metrics.computeRates(time.Now())
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

// estimateNewTokens estimates NEW prefill tokens for human-facing telemetry
// ("newTokensEst" in /metrics): multi-turn (>2 messages) counts only the last
// message, assuming prior context is cached; ≤2 messages count everything.
// Completions API: the prompt field. ~4 bytes per token (conservative for Qwen).
// The admission gate deliberately does NOT use this estimate — see
// estimatePrefillTokens.
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

// estimatePrefillTokens estimates prompt tokens that may need prefilling.
// The whole conversation is counted: the router cannot prove that a request's
// preceding prefix is cached. Prefix caches are keyed by exact token prefixes
// and can be cold, evicted, or invalidated by an edited turn, so a short final
// message is not evidence that earlier context will be a cache hit. Aggregate
// backend cache metrics also say nothing about an individual request.
// Completions API: the prompt field. ~4 bytes per token (conservative for Qwen).
func estimatePrefillTokens(b *parsedBody) int {
	chars := 0
	if len(b.Messages) > 0 {
		for _, m := range b.Messages {
			chars += rawContentLen(m.Content)
		}
	} else {
		chars = rawContentLen(b.Prompt)
	}
	return chars / 4
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

// ─── Server-side history ──────────────────────────────────────────────────
//
// One sampler goroutine (independent of SSE clients) takes a coherent frame
// every second and appends it to three rings per series: raw 1s × 1h,
// 1m × 7d, 1h × 30d. Rollups store averages for gauges/rates and maxima for
// pressure/TTFT. The live frame and the sampler share applyFleetAggregates,
// so the live edge and stored history cannot diverge.
//
// This is the durable truth the dashboard's history ranges read; the browser
// no longer owns the only copy. An optional HISTORY_PATH keeps the series
// across restarts (atomic snapshot, series only — lifetime counters stay
// process-scoped per the contract).

const (
	hiInFlight = iota
	hiWaiting
	hiMaxConc
	hiPrefillInFlight
	hiPrefillWaiting
	hiReqRate
	hiTokEstRate
	hiEWMA
	hiTTFT
	hiTTFTMax
	hiTTFTN
	hiEngRunning
	hiEngWaiting
	hiPrefillTokS
	hiPrefillCacheTokS
	hiDecodeTokS
	hiKVPct
	hiKVHeldPct
	hiKVMax
	hiKVUsedTok
	hiKVCapTok
	hiKVMemGB
	hiWeightMemGB
	hiHitRate
	hiEngTTFT
	histFieldN
)

var histFieldNames = [histFieldN]string{
	"inFlight", "waiting", "maxConcurrent", "prefillInFlight", "prefillWaiting",
	"reqRate", "tokEstRate", "ewmaMs", "ttftMs", "ttftMax", "ttftSamples",
	"engineRunning", "engineWaiting", "prefillTokS", "prefillCacheTokS",
	"decodeTokS", "kvPct", "kvHeldPct", "kvMax", "kvUsedTok", "kvCapTok",
	"kvMemGB", "weightMemGB", "hitRate", "engineTTFTMs",
}

var histMaxFields = [histFieldN]bool{hiTTFTMax: true, hiKVMax: true}

// historyPoint is compact and fixed-layout (~108 B). Flags records which
// fields were observed; an absent bit serializes as null, never 0.
type historyPoint struct {
	T     int64
	V     [histFieldN]float32
	Flags uint32
}

// hpSet stores v in field i, rejecting values that overflow float32:
// a finite float64 past float32 max narrows to ±Inf and json.Marshal
// fatals on /metrics/history. Rejection leaves the presence bit unset,
// so the field serializes as null.
func hpSet(p *historyPoint, i int, v float64) {
	fv := float32(v)
	if math.IsInf(float64(fv), 0) {
		return
	}
	p.V[i] = fv
	p.Flags |= 1 << uint(i)
}

func hpHas(p historyPoint, i int) bool { return p.Flags&(1<<uint(i)) != 0 }

type histAccum struct {
	t     int64
	sum   [histFieldN]float64
	max   [histFieldN]float32
	n     [histFieldN]int32
	flags uint32
}

func (a *histAccum) add(p historyPoint) {
	for i := 0; i < histFieldN; i++ {
		if !hpHas(p, i) {
			continue
		}
		v := p.V[i]
		a.sum[i] += float64(v)
		if v > a.max[i] {
			a.max[i] = v
		}
		a.n[i]++
		a.flags |= 1 << uint(i)
	}
}

func (a *histAccum) flush(t int64) historyPoint {
	var p historyPoint
	p.T = t
	p.Flags = a.flags
	for i := 0; i < histFieldN; i++ {
		if a.n[i] == 0 {
			continue
		}
		if histMaxFields[i] {
			p.V[i] = a.max[i]
		} else {
			p.V[i] = float32(a.sum[i] / float64(a.n[i]))
		}
	}
	return p
}

type histRing struct {
	p    []historyPoint
	head int
	n    int
}

func newHistRing(capacity int) *histRing {
	return &histRing{p: make([]historyPoint, capacity)}
}

func (r *histRing) add(p historyPoint) {
	r.p[r.head] = p
	r.head = (r.head + 1) % len(r.p)
	if r.n < len(r.p) {
		r.n++
	}
}

// ordered returns oldest→newest.
func (r *histRing) ordered() []historyPoint {
	out := make([]historyPoint, 0, r.n)
	start := r.head - r.n
	for i := 0; i < r.n; i++ {
		out = append(out, r.p[((start+i)%len(r.p)+len(r.p))%len(r.p)])
	}
	return out
}

func (r *histRing) load(pts []historyPoint) {
	for _, p := range pts {
		if p.T == 0 {
			continue
		}
		r.add(p)
	}
}

type seriesHistory struct {
	raw, minute, hour *histRing
	mA, hA            histAccum
	lastMin, lastHour int64
}

func newSeriesHistory() *seriesHistory {
	return &seriesHistory{
		raw:    newHistRing(3600),  // 1 h at 1 s
		minute: newHistRing(10080), // 7 d at 1 m
		hour:   newHistRing(720),   // 30 d at 1 h
	}
}

func (s *seriesHistory) add(p historyPoint, nowMs int64) {
	s.raw.add(p)

	minute := nowMs / 60000 * 60000
	if s.lastMin == 0 {
		s.lastMin = minute
	}
	if minute != s.lastMin {
		s.minute.add(s.mA.flush(s.lastMin))
		s.mA = histAccum{}
		s.lastMin = minute
	}
	s.mA.add(p)

	hour := nowMs / 3600000 * 3600000
	if s.lastHour == 0 {
		s.lastHour = hour
	}
	if hour != s.lastHour {
		s.hour.add(s.hA.flush(s.lastHour))
		s.hA = histAccum{}
		s.lastHour = hour
	}
	s.hA.add(p)
}

// ── GPU device series (small, 5 fields) ──────────────────────────────────

const (
	guUtil = iota
	guMemUsed
	guMemTotal
	guTemp
	guPower
	guFieldN
)

var gpuFieldNames = [guFieldN]string{"utilPct", "memUsedMB", "memTotalMB", "tempC", "powerW"}

type gpuPoint struct {
	T     int64
	V     [guFieldN]float32
	Flags uint8
}

// gpuSet is the float32-narrowing guard for gpuPoint fields, mirroring
// hpSet: Inf is rejected and the presence bit stays unset.
func gpuSet(p *gpuPoint, i int, v float64) {
	fv := float32(v)
	if math.IsInf(float64(fv), 0) {
		return
	}
	p.V[i] = fv
	p.Flags |= 1 << uint(i)
}

type gpuAccum struct {
	sum   [guFieldN]float64
	n     [guFieldN]int32
	flags uint8
}

func (a *gpuAccum) add(p gpuPoint) {
	for i := 0; i < guFieldN; i++ {
		if p.Flags&(1<<uint(i)) == 0 {
			continue
		}
		a.sum[i] += float64(p.V[i])
		a.n[i]++
		a.flags |= 1 << uint(i)
	}
}

func (a *gpuAccum) flush(t int64) gpuPoint {
	var p gpuPoint
	p.T = t
	p.Flags = a.flags
	for i := 0; i < guFieldN; i++ {
		if a.n[i] > 0 {
			p.V[i] = float32(a.sum[i] / float64(a.n[i]))
		}
	}
	return p
}

type gpuRing struct {
	p    []gpuPoint
	head int
	n    int
}

func newGPURing(capacity int) *gpuRing { return &gpuRing{p: make([]gpuPoint, capacity)} }

func (r *gpuRing) add(p gpuPoint) {
	r.p[r.head] = p
	r.head = (r.head + 1) % len(r.p)
	if r.n < len(r.p) {
		r.n++
	}
}

func (r *gpuRing) ordered() []gpuPoint {
	out := make([]gpuPoint, 0, r.n)
	start := r.head - r.n
	for i := 0; i < r.n; i++ {
		out = append(out, r.p[((start+i)%len(r.p)+len(r.p))%len(r.p)])
	}
	return out
}

func (r *gpuRing) load(pts []gpuPoint) {
	for _, p := range pts {
		if p.T != 0 {
			r.add(p)
		}
	}
}

type gpuSeries struct {
	raw, minute, hour *gpuRing
	mA, hA            gpuAccum
	lastMin, lastHour int64
}

func newGPUSeries() *gpuSeries {
	return &gpuSeries{
		raw:    newGPURing(3600),
		minute: newGPURing(10080),
		hour:   newGPURing(720),
	}
}

func (s *gpuSeries) add(p gpuPoint, nowMs int64) {
	s.raw.add(p)
	minute := nowMs / 60000 * 60000
	if s.lastMin == 0 {
		s.lastMin = minute
	}
	if minute != s.lastMin {
		s.minute.add(s.mA.flush(s.lastMin))
		s.mA = gpuAccum{}
		s.lastMin = minute
	}
	s.mA.add(p)
	hour := nowMs / 3600000 * 3600000
	if s.lastHour == 0 {
		s.lastHour = hour
	}
	if hour != s.lastHour {
		s.hour.add(s.hA.flush(s.lastHour))
		s.hA = gpuAccum{}
		s.lastHour = hour
	}
	s.hA.add(p)
}

// ── Store ────────────────────────────────────────────────────────────────

type historyStore struct {
	mu       sync.Mutex
	backends map[string]*seriesHistory
	fleet    *seriesHistory
	gpus     map[string]*gpuSeries
	path     string
	lastSave time.Time
	saveWarn bool
}

var history = newHistoryStore()

func newHistoryStore() *historyStore {
	return &historyStore{
		backends: map[string]*seriesHistory{},
		fleet:    newSeriesHistory(),
		gpus:     map[string]*gpuSeries{},
	}
}

func (h *historyStore) configure(path string) {
	h.mu.Lock()
	h.path = path
	h.mu.Unlock()
	if path != "" {
		h.loadSnapshot(path)
	}
}

func (h *historyStore) loop() {
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		h.sample()
		if h.path != "" && time.Since(h.lastSave) >= time.Minute {
			h.lastSave = time.Now()
			h.save()
		}
	}
}

func (h *historyStore) sample() {
	f := buildMetricsFrame()
	now := f.T
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range f.Backends {
		s, ok := h.backends[b.Name]
		if !ok {
			s = newSeriesHistory()
			h.backends[b.Name] = s
		}
		p := pointFromBackend(b)
		p.T = now
		s.add(p, now)
	}
	fp := pointFromTotals(f.Totals)
	fp.T = now
	h.fleet.add(fp, now)
	for _, d := range f.GPUDevices {
		s, ok := h.gpus[d.Key]
		if !ok {
			s = newGPUSeries()
			h.gpus[d.Key] = s
		}
		var p gpuPoint
		p.T = now
		gpuSet(&p, guUtil, d.UtilPct)
		if d.MemUsedMB > 0 {
			gpuSet(&p, guMemUsed, d.MemUsedMB)
		}
		if d.MemTotalMB > 0 {
			gpuSet(&p, guMemTotal, d.MemTotalMB)
		}
		if d.TempC != nil {
			gpuSet(&p, guTemp, *d.TempC)
		}
		if d.PowerW != nil {
			gpuSet(&p, guPower, *d.PowerW)
		}
		s.add(p, now)
	}
}

func pointFromBackend(b backendFrame) historyPoint {
	var p historyPoint
	hpSet(&p, hiInFlight, float64(b.InFlight))
	hpSet(&p, hiWaiting, float64(b.Waiting))
	hpSet(&p, hiMaxConc, float64(b.MaxConcurrent))
	hpSet(&p, hiPrefillInFlight, float64(b.PrefillInFlight))
	hpSet(&p, hiPrefillWaiting, float64(b.PrefillWaiting))
	hpSet(&p, hiReqRate, b.ReqRate)
	hpSet(&p, hiTokEstRate, b.TokEstRate)
	if b.EWMAms > 0 {
		hpSet(&p, hiEWMA, b.EWMAms)
	}
	if b.TTFTSampleCount > 0 {
		hpSet(&p, hiTTFT, b.TTFTms)
		hpSet(&p, hiTTFTMax, b.TTFTms)
		hpSet(&p, hiTTFTN, float64(b.TTFTSampleCount))
	}
	e := b.Engine
	if e.Status == "ok" {
		hpSet(&p, hiEngRunning, e.Running)
		hpSet(&p, hiEngWaiting, e.Waiting)
		hpSet(&p, hiPrefillTokS, e.PrefillTokS)
		hpSet(&p, hiPrefillCacheTokS, e.PrefillCacheTokS)
		hpSet(&p, hiDecodeTokS, e.DecodeTokS)
		hpSet(&p, hiKVPct, e.KVPct)
		hpSet(&p, hiKVMax, e.KVPct)
		hpSet(&p, hiHitRate, e.HitRate)
		if e.KVHeldPct != nil {
			hpSet(&p, hiKVHeldPct, *e.KVHeldPct)
		}
		if e.KVUsedTok != nil {
			hpSet(&p, hiKVUsedTok, *e.KVUsedTok)
		}
		if e.KVCapTok != nil {
			hpSet(&p, hiKVCapTok, *e.KVCapTok)
		}
		if e.KVMemGB != nil {
			hpSet(&p, hiKVMemGB, *e.KVMemGB)
		}
		if e.WeightMemGB != nil {
			hpSet(&p, hiWeightMemGB, *e.WeightMemGB)
		}
		if e.TTFTSamples > 0 {
			hpSet(&p, hiEngTTFT, e.TTFTms)
		}
	}
	return p
}

func pointFromTotals(t totalsFrame) historyPoint {
	var p historyPoint
	hpSet(&p, hiInFlight, float64(t.InFlight))
	hpSet(&p, hiWaiting, float64(t.Waiting))
	hpSet(&p, hiReqRate, t.ReqRate)
	hpSet(&p, hiTokEstRate, t.TokEstRate)
	if t.TTFTSampleCount > 0 {
		hpSet(&p, hiTTFT, t.TTFTMs)
		hpSet(&p, hiTTFTMax, t.TTFTMs)
		hpSet(&p, hiTTFTN, float64(t.TTFTSampleCount))
	}
	if t.EngineOK > 0 {
		hpSet(&p, hiEngRunning, t.EngineRunning)
		hpSet(&p, hiEngWaiting, t.EngineWaiting)
		hpSet(&p, hiPrefillTokS, t.PrefillTokS)
		hpSet(&p, hiPrefillCacheTokS, t.PrefillCacheTokS)
		hpSet(&p, hiDecodeTokS, t.DecodeTokS)
		hpSet(&p, hiKVPct, t.KVPeak)
		hpSet(&p, hiKVMax, t.KVPeak)
		if t.KVUsedTokN > 0 {
			hpSet(&p, hiKVUsedTok, t.KVUsedTok)
		}
		if t.KVCapTokN > 0 {
			hpSet(&p, hiKVCapTok, t.KVCapTok)
		}
		if t.KVMemGBN > 0 {
			hpSet(&p, hiKVMemGB, t.KVMemGB)
		}
		if t.WeightMemGBN > 0 {
			hpSet(&p, hiWeightMemGB, t.WeightMemGB)
		}
		hpSet(&p, hiHitRate, t.HitRate)
	}
	return p
}

// ── Query ────────────────────────────────────────────────────────────────

type historyRange struct {
	Dur    time.Duration
	Step   time.Duration
	Source string
}

var historyRanges = map[string]historyRange{
	"5m":  {5 * time.Minute, time.Second, "raw"},
	"1h":  {time.Hour, 5 * time.Second, "raw"},
	"6h":  {6 * time.Hour, time.Minute, "minute"},
	"24h": {24 * time.Hour, 2 * time.Minute, "minute"},
	"7d":  {7 * 24 * time.Hour, 15 * time.Minute, "minute"},
	"30d": {30 * 24 * time.Hour, time.Hour, "hour"},
}

type historySeriesOut struct {
	Name   string                `json:"name"`
	T      []int64               `json:"t"`
	Series map[string][]*float64 `json:"series"`
}

type historyGPUOut struct {
	Key    string                `json:"key"`
	Index  int                   `json:"index"`
	Name   string                `json:"name"`
	Source string                `json:"source"`
	T      []int64               `json:"t"`
	Series map[string][]*float64 `json:"series"`
}

type historyTTFTOut struct {
	N          int     `json:"n"`
	CoverageMs int64   `json:"coverageMs"`
	P50        float64 `json:"p50"`
	P95        float64 `json:"p95"`
}

type historyResponse struct {
	From     int64              `json:"from"`
	To       int64              `json:"to"`
	StepMs   int64              `json:"stepMs"`
	Range    string             `json:"range"`
	Backends []historySeriesOut `json:"backends"`
	Fleet    historySeriesOut   `json:"fleet"`
	GPU      []historyGPUOut    `json:"gpu"`
	GPUFeed  gpuFeedFrame       `json:"gpuFeed"`
	TTFT     historyTTFTOut     `json:"ttft"`
	Now      int64              `json:"now"`
}

func (h *historyStore) query(rangeName string, names []string) historyResponse {
	rng, ok := historyRanges[rangeName]
	if !ok {
		rng = historyRanges["1h"]
		rangeName = "1h"
	}
	now := time.Now()
	stepMs := rng.Step.Milliseconds()
	from := now.Add(-rng.Dur).UnixMilli()
	from = from / stepMs * stepMs
	n := int(rng.Dur.Milliseconds() / stepMs)
	if n < 1 {
		n = 1
	}
	to := from + int64(n-1)*stepMs

	h.mu.Lock()
	pick := func(s *seriesHistory) []historyPoint {
		var ring *histRing
		switch rng.Source {
		case "minute":
			ring = s.minute
		case "hour":
			ring = s.hour
		default:
			ring = s.raw
		}
		return ring.ordered()
	}
	fleetPts := pick(h.fleet)
	backendPts := map[string][]historyPoint{}
	if len(names) == 0 {
		for name := range h.backends {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	for _, name := range names {
		if s, ok := h.backends[name]; ok {
			backendPts[name] = pick(s)
		}
	}
	gpuPts := map[string][]gpuPoint{}
	gpuMeta := map[string]gpuDeviceFrame{}
	for key, s := range h.gpus {
		var ring *gpuRing
		switch rng.Source {
		case "minute":
			ring = s.minute
		case "hour":
			ring = s.hour
		default:
			ring = s.raw
		}
		gpuPts[key] = ring.ordered()
	}
	for _, d := range gpuReaders.view() {
		gpuMeta[d.Key] = d
	}
	h.mu.Unlock()

	resp := historyResponse{
		From: from, To: to, StepMs: stepMs, Range: rangeName, Now: now.UnixMilli(),
		Backends: make([]historySeriesOut, 0, len(names)),
		GPUFeed:  gpuReaders.feed(),
	}
	resp.Fleet = buildHistSeries("fleet", fleetPts, from, stepMs, n)
	for _, name := range names {
		pts, ok := backendPts[name]
		if !ok {
			continue
		}
		resp.Backends = append(resp.Backends, buildHistSeries(name, pts, from, stepMs, n))
	}
	for key, pts := range gpuPts {
		meta := gpuMeta[key]
		resp.GPU = append(resp.GPU, buildGPUSeries(key, meta, pts, from, stepMs, n))
	}
	sort.Slice(resp.GPU, func(i, j int) bool { return resp.GPU[i].Key < resp.GPU[j].Key })
	rn, cov, p50, p95 := metrics.ttftPercentiles(from, to+stepMs)
	resp.TTFT = historyTTFTOut{N: rn, CoverageMs: cov, P50: p50, P95: p95}
	return resp
}

func buildHistSeries(name string, pts []historyPoint, from, stepMs int64, n int) historySeriesOut {
	out := historySeriesOut{Name: name, T: make([]int64, n)}
	for i := 0; i < n; i++ {
		out.T[i] = from + int64(i)*stepMs
	}
	buckets := make([]histAccum, n)
	for _, p := range pts {
		if p.T < from || p.T >= from+int64(n)*stepMs {
			continue
		}
		idx := int((p.T - from) / stepMs)
		buckets[idx].add(p)
	}
	out.Series = map[string][]*float64{}
	for f := 0; f < histFieldN; f++ {
		arr := make([]*float64, n)
		for i := 0; i < n; i++ {
			if buckets[i].n[f] == 0 {
				continue
			}
			var v float64
			if histMaxFields[f] {
				v = float64(buckets[i].max[f])
			} else {
				v = buckets[i].sum[f] / float64(buckets[i].n[f])
			}
			arr[i] = &v
		}
		out.Series[histFieldNames[f]] = arr
	}
	return out
}

func buildGPUSeries(key string, meta gpuDeviceFrame, pts []gpuPoint, from, stepMs int64, n int) historyGPUOut {
	out := historyGPUOut{Key: key, Index: meta.Index, Name: meta.Name, Source: meta.Source, T: make([]int64, n)}
	if out.Key == "" {
		out.Key = "device"
	}
	if out.Name == "" {
		out.Name = out.Key
	}
	for i := 0; i < n; i++ {
		out.T[i] = from + int64(i)*stepMs
	}
	buckets := make([]gpuAccum, n)
	for _, p := range pts {
		if p.T < from || p.T >= from+int64(n)*stepMs {
			continue
		}
		idx := int((p.T - from) / stepMs)
		buckets[idx].add(p)
	}
	out.Series = map[string][]*float64{}
	for f := 0; f < guFieldN; f++ {
		arr := make([]*float64, n)
		for i := 0; i < n; i++ {
			if buckets[i].n[f] == 0 {
				continue
			}
			v := buckets[i].sum[f] / float64(buckets[i].n[f])
			arr[i] = &v
		}
		out.Series[gpuFieldNames[f]] = arr
	}
	return out
}

// ── Persistence (optional, atomic snapshot) ──────────────────────────────

const historySnapshotVersion = 2

type histSeriesSnap struct {
	Raw, Minute, Hour []historyPoint
}

type histGPUSnap struct {
	Raw, Minute, Hour []gpuPoint
}

type historySnapshot struct {
	Version  int
	SavedAt  int64
	Backends map[string]histSeriesSnap
	Fleet    histSeriesSnap
	GPUs     map[string]histGPUSnap
}

func (h *historyStore) snapshotLocked() historySnapshot {
	snap := historySnapshot{
		Version:  historySnapshotVersion,
		SavedAt:  time.Now().UnixMilli(),
		Backends: map[string]histSeriesSnap{},
		Fleet:    histSeriesSnap{Raw: h.fleet.raw.ordered(), Minute: h.fleet.minute.ordered(), Hour: h.fleet.hour.ordered()},
		GPUs:     map[string]histGPUSnap{},
	}
	for name, s := range h.backends {
		snap.Backends[name] = histSeriesSnap{Raw: s.raw.ordered(), Minute: s.minute.ordered(), Hour: s.hour.ordered()}
	}
	for key, s := range h.gpus {
		snap.GPUs[key] = histGPUSnap{Raw: s.raw.ordered(), Minute: s.minute.ordered(), Hour: s.hour.ordered()}
	}
	return snap
}

func (h *historyStore) save() {
	h.mu.Lock()
	snap := h.snapshotLocked()
	path := h.path
	h.mu.Unlock()
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			h.warnSave(err)
			return
		}
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		h.warnSave(err)
		return
	}
	enc := gob.NewEncoder(f)
	if err := enc.Encode(snap); err != nil {
		f.Close()
		os.Remove(tmp)
		h.warnSave(err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		h.warnSave(err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		h.warnSave(err)
	}
}

func (h *historyStore) warnSave(err error) {
	if h.saveWarn {
		return
	}
	h.saveWarn = true
	log.Printf("history: snapshot write failed (continuing in memory): %v", err)
}

func (h *historyStore) loadSnapshot(path string) {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("history: cannot open %s (continuing in memory): %v", path, err)
		}
		return
	}
	defer f.Close()
	var snap historySnapshot
	if err := gob.NewDecoder(f).Decode(&snap); err != nil || snap.Version != historySnapshotVersion {
		log.Printf("history: ignoring %s (bad snapshot or version)", path)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fleet.raw.load(snap.Fleet.Raw)
	h.fleet.minute.load(snap.Fleet.Minute)
	h.fleet.hour.load(snap.Fleet.Hour)
	for name, s := range snap.Backends {
		sh := newSeriesHistory()
		sh.raw.load(s.Raw)
		sh.minute.load(s.Minute)
		sh.hour.load(s.Hour)
		h.backends[name] = sh
	}
	for key, s := range snap.GPUs {
		sg := newGPUSeries()
		sg.raw.load(s.Raw)
		sg.minute.load(s.Minute)
		sg.hour.load(s.Hour)
		h.gpus[key] = sg
	}
	log.Printf("history: restored %d backend series, %d device series from %s", len(snap.Backends), len(snap.GPUs), path)
}

// applyFleetAggregates is the single fleet-aggregation implementation shared
// by the live frame and the history sampler. Engine sums only include
// status=="ok" backends; a dead engine's retained gauges are not live load.
func applyFleetAggregates(f *metricsFrame) {
	t := &f.Totals
	var ttftW, ttftN float64
	// Counter-derived hit totals (vLLM) are pooled directly and are the sole
	// hit source for counter-backed backends: their HitRate is the same
	// counters as a gauge, so blending it in again would double-count.
	// Gauge-only backends (SGLang) mix their HitRate in, weighted by their
	// query totals when available, so a mixed fleet reports one fleet-wide
	// figure.
	var hitH, hitQ, hitMean, hitDen float64
	for _, b := range f.Backends {
		e := b.Engine
		if e.Status == "ok" {
			t.EngineOK++
			t.EngineRunning += e.Running
			t.EngineWaiting += e.Waiting
			t.PrefillTokS += e.PrefillTokS
			t.PrefillCacheTokS += e.PrefillCacheTokS
			t.DecodeTokS += e.DecodeTokS
			if e.KVPct > t.KVPeak {
				t.KVPeak = e.KVPct
			}
			if e.KVUsedTok != nil {
				t.KVUsedTok += *e.KVUsedTok
				t.KVUsedTokN = 1
			}
			if e.KVCapTok != nil {
				t.KVCapTok += *e.KVCapTok
				t.KVCapTokN = 1
			}
			if e.KVMemGB != nil {
				t.KVMemGB += *e.KVMemGB
				t.KVMemGBN = 1
			}
			if e.WeightMemGB != nil {
				t.WeightMemGB += *e.WeightMemGB
				t.WeightMemGBN = 1
			}
			if e.HitQueriesTotal != nil && e.HitHitsTotal != nil && *e.HitQueriesTotal > 0 {
				hitH += float64(*e.HitHitsTotal)
				hitQ += float64(*e.HitQueriesTotal)
			}
			// Blend in only backends whose HitRate is not already the pooled
			// counter ratio: pooling a counter-backed backend's gauge too
			// adds its queries twice.
			counterBacked := e.HitQueriesTotal != nil && e.HitHitsTotal != nil
			if e.HitRate > 0 && !counterBacked {
				w := float64(1)
				if q := e.HitQueriesTotal; q != nil {
					w = float64(*q)
				}
				hitMean += e.HitRate * w
				hitDen += w
			}
		}
		if b.TTFTSampleCount > 0 {
			ttftW += b.TTFTms * float64(b.TTFTSampleCount)
			ttftN += float64(b.TTFTSampleCount)
		}
	}
	if ttftN > 0 {
		t.TTFTMs = ttftW / ttftN
		t.TTFTSampleCount = int(ttftN)
	}
	// Blend both where available: Σhits/Σqueries over the counter-derived
	// part plus the weighted gauge part, normalized by total queries.
	hits, queries := hitH, hitQ
	if hitDen > 0 {
		hits += hitMean
		queries += hitDen
	}
	if queries > 0 {
		t.HitRate = hits / queries
	}
}

// ─── Optional device telemetry (DCGM / nvidia_gpu_exporter) ───────────────
//
// The engines expose their own execution truth but not device-wide GPU
// utilization or VRAM. When GPU_METRICS_URL is set, the router scrapes that
// Prometheus text endpoint and publishes per-device util/memory/temp/power.
// Units are normalized: util 0..100, memory MiB, temp °C, power W. Devices
// are identified by UUID when present, else (hostname, gpu) — never by the
// bare index, which collides on a federated exporter.

type gpuDeviceFrame struct {
	Key        string   `json:"key"`
	Index      int      `json:"index"`
	Name       string   `json:"name"`
	UtilPct    float64  `json:"utilPct"`
	MemUsedMB  float64  `json:"memUsedMB"`
	MemTotalMB float64  `json:"memTotalMB"`
	TempC      *float64 `json:"tempC"`
	PowerW     *float64 `json:"powerW"`
	Source     string   `json:"source"`
}

type gpuFeedFrame struct {
	Status    string `json:"status"` // "" = not configured
	URL       string `json:"url"`
	LastError string `json:"lastError,omitempty"`
}

type gpuRegistry struct {
	mu       sync.Mutex
	url      string
	devices  []gpuDeviceFrame
	status   string
	lastErr  string
	offNext  time.Time
	errNext  time.Time
	errDelay time.Duration
	client   *http.Client
}

var gpuReaders = newGPURegistry()

func newGPURegistry() *gpuRegistry {
	return &gpuRegistry{
		status: "",
		client: &http.Client{Timeout: 2 * time.Second},
	}
}

func (g *gpuRegistry) configure(url string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.url = url
	if url == "" {
		g.status = ""
	}
}

func (g *gpuRegistry) feed() gpuFeedFrame {
	g.mu.Lock()
	defer g.mu.Unlock()
	return gpuFeedFrame{Status: g.status, URL: g.url, LastError: g.lastErr}
}

func (g *gpuRegistry) view() []gpuDeviceFrame {
	g.mu.Lock()
	defer g.mu.Unlock()
	// A failed scrape means the device values are not current; publish none
	// rather than stale gauges. gpuFeed carries the reason.
	if g.status != "ok" || len(g.devices) == 0 {
		return nil
	}
	out := make([]gpuDeviceFrame, len(g.devices))
	copy(out, g.devices)
	return out
}

func (g *gpuRegistry) loop() {
	ticker := time.NewTicker(time.Second)
	for range ticker.C {
		g.scrape()
	}
}

func (g *gpuRegistry) scrape() {
	g.mu.Lock()
	url := g.url
	if url == "" {
		g.mu.Unlock()
		return
	}
	now := time.Now()
	if g.status == "off" && now.Before(g.offNext) {
		g.mu.Unlock()
		return
	}
	if g.status == "err" && now.Before(g.errNext) {
		g.mu.Unlock()
		return
	}
	g.mu.Unlock()

	body, httpStatus, err := func() ([]byte, int, error) {
		resp, err := g.client.Get(url)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, resp.StatusCode, fmt.Errorf("gpu metrics %s", resp.Status)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return b, resp.StatusCode, err
	}()

	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.status = "err"
		g.lastErr = err.Error()
		if httpStatus == http.StatusNotFound || httpStatus == http.StatusMethodNotAllowed {
			g.status = "off"
			g.lastErr = ""
			g.offNext = time.Now().Add(engineOffBackoff)
		} else {
			if g.errDelay == 0 {
				g.errDelay = time.Second
			} else if g.errDelay < engineOffBackoff {
				g.errDelay *= 2
			}
			g.errNext = time.Now().Add(g.errDelay)
		}
		return
	}
	// A capped parse lost part of the exposition: the retained devices
	// are partial exactly like a truncated engine body, so the scrape is
	// incomplete (err) and the partial list is withheld — never "ok"
	// with devices silently missing from the frame.
	samples, truncated := parsePromText(body)
	if truncated {
		g.status = "err"
		g.lastErr = "gpu metrics body truncated (past sample cap)"
		g.errDelay = time.Second
		g.errNext = time.Now().Add(g.errDelay)
		return
	}
	g.status = "ok"
	g.lastErr = ""
	g.errDelay = 0
	g.errNext = time.Time{}
	g.devices = parseGPUDevices(samples)
}

// gpuBuilder accumulates one device across the family-specific metric names.
type gpuBuilder struct {
	frame    gpuDeviceFrame
	haveUtil bool
	haveMem  bool
	haveTemp bool
	havePow  bool
	haveTot  bool
	haveFree bool
	freeMB   float64
}

func parseGPUDevices(samples []promSample) []gpuDeviceFrame {
	devs := map[string]*gpuBuilder{}
	order := []string{}
	get := func(sm promSample) *gpuBuilder {
		uuid := sm.labels["UUID"]
		if uuid == "" {
			uuid = sm.labels["uuid"]
		}
		host := sm.labels["Hostname"]
		if host == "" {
			host = sm.labels["hostname"]
		}
		idx := sm.labels["gpu"]
		if idx == "" {
			idx = sm.labels["device"]
		}
		key := uuid
		if key == "" {
			key = host + "|" + idx
		}
		if key == "|" {
			key = "device"
		}
		b, ok := devs[key]
		if !ok {
			i := 0
			fmt.Sscanf(idx, "%d", &i)
			name := sm.labels["modelName"]
			if name == "" {
				name = sm.labels["name"]
			}
			if name == "" {
				name = sm.labels["gpu_name"]
			}
			if name == "" {
				name = fmt.Sprintf("GPU %d", i)
			}
			b = &gpuBuilder{frame: gpuDeviceFrame{Key: key, Index: i, Name: name}}
			devs[key] = b
			order = append(order, key)
		}
		return b
	}
	for _, sm := range samples {
		switch sm.name {
		case "DCGM_FI_DEV_GPU_UTIL":
			b := get(sm)
			b.frame.UtilPct = sm.value
			b.frame.Source = "dcgm"
			b.haveUtil = true
		case "DCGM_FI_DEV_FB_USED":
			b := get(sm)
			b.frame.MemUsedMB = sm.value
			b.frame.Source = "dcgm"
			b.haveMem = true
		case "DCGM_FI_DEV_FB_TOTAL":
			b := get(sm)
			b.frame.MemTotalMB = sm.value
			b.haveTot = true
		case "DCGM_FI_DEV_FB_FREE":
			b := get(sm)
			b.freeMB = sm.value
			b.haveFree = true
		case "DCGM_FI_DEV_GPU_TEMP":
			b := get(sm)
			v := sm.value
			b.frame.TempC = &v
			b.haveTemp = true
		case "DCGM_FI_DEV_POWER_USAGE":
			b := get(sm)
			v := sm.value
			b.frame.PowerW = &v
			b.havePow = true
		case "nvidia_smi_utilization_gpu_ratio":
			b := get(sm)
			u := sm.value * 100
			if math.IsInf(u, 0) { // finite input, overflowing scale
				continue
			}
			b.frame.UtilPct = u
			b.frame.Source = "nvidia"
			b.haveUtil = true
		case "nvidia_smi_memory_used_bytes":
			b := get(sm)
			b.frame.MemUsedMB = sm.value / (1 << 20)
			b.frame.Source = "nvidia"
			b.haveMem = true
		case "nvidia_smi_memory_total_bytes":
			b := get(sm)
			b.frame.MemTotalMB = sm.value / (1 << 20)
			b.haveTot = true
		case "nvidia_smi_temperature_gpu":
			b := get(sm)
			v := sm.value
			b.frame.TempC = &v
			b.haveTemp = true
		case "nvidia_smi_power_draw_watts":
			b := get(sm)
			v := sm.value
			b.frame.PowerW = &v
			b.havePow = true
		}
	}
	out := make([]gpuDeviceFrame, 0, len(order))
	for _, k := range order {
		b := devs[k]
		if !b.haveUtil && !b.haveMem {
			continue
		}
		if !b.haveTot && b.haveFree {
			total := b.frame.MemUsedMB + b.freeMB
			if !math.IsInf(total, 0) { // keep 0 over a poisoned total
				b.frame.MemTotalMB = total
			}
		}
		out = append(out, b.frame)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
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
	RejectTotal     int64      `json:"rejectTotal"`
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

// totalsFrame is the fleet aggregate. The router-slot fields are the
// admission view; the engine fields are gated on status=="ok" so a dead
// engine's retained gauges never count as live fleet load. buildMetricsFrame
// and the history sampler use the same applyFleetAggregates function, so the
// live edge and the stored series cannot diverge.
type totalsFrame struct {
	InFlight      int32   `json:"inFlight"`
	Waiting       int32   `json:"waiting"`
	ReqRate       float64 `json:"reqRate"`
	BytesRate     float64 `json:"bytesRate"`
	TokEstRate    float64 `json:"tokEstRate"`
	ReqTotal      int64   `json:"reqTotal"`
	RejectTotal   int64   `json:"rejectTotal"`
	BytesInTotal  int64   `json:"bytesInTotal"`
	BytesOutTotal int64   `json:"bytesOutTotal"`
	TokEstTotal   int64   `json:"tokEstTotal"`

	TTFTMs           float64 `json:"ttftMs"`
	TTFTSampleCount  int     `json:"ttftSampleCount"`
	EngineOK         int     `json:"engineOk"`
	EngineRunning    float64 `json:"engineRunning"`
	EngineWaiting    float64 `json:"engineWaiting"`
	PrefillTokS      float64 `json:"prefillTokS"`
	PrefillCacheTokS float64 `json:"prefillCacheTokS"`
	DecodeTokS       float64 `json:"decodeTokS"`
	KVPeak           float64 `json:"kvPeak"`
	KVUsedTok        float64 `json:"kvUsedTok"`
	KVCapTok         float64 `json:"kvCapTok"`
	KVMemGB          float64 `json:"kvMemGB"`
	WeightMemGB      float64 `json:"weightMemGB"`
	HitRate          float64 `json:"hitRate"`

	// Optional-gauge presence: set when any ok engine reported the field,
	// so history stays null (never 0) when no engine reports it at all.
	KVUsedTokN   int `json:"-"`
	KVCapTokN    int `json:"-"`
	KVMemGBN     int `json:"-"`
	WeightMemGBN int `json:"-"`
}

type metricsFrame struct {
	T             int64            `json:"t"`
	Backends      []backendFrame   `json:"backends"`
	GPU           gpuFrame         `json:"gpu"`
	GPUDevices    []gpuDeviceFrame `json:"gpuDevices"`
	GPUFeed       gpuFeedFrame     `json:"gpuFeed"`
	Tier0Inflight int32            `json:"tier0Inflight"`
	Totals        totalsFrame      `json:"totals"`
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
		bf.RejectTotal = ms.RejectedTotal
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
		f.Totals.RejectTotal += ms.RejectedTotal
		f.Totals.BytesInTotal += ms.BytesInTotal
		f.Totals.BytesOutTotal += ms.BytesOutTotal
		f.Totals.TokEstTotal += ms.TokEstTotal
	}
	applyFleetAggregates(&f)
	f.GPUDevices = gpuReaders.view()
	f.GPUFeed = gpuReaders.feed()

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
	Start        int64   `json:"start"`
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

// burstLimitMax bounds the number of recent requests ?limit= may return,
// well under the ring capacity (2000) so a single poll stays cheap.
const burstLimitMax = 1000

// handleBurst serves the recent-request ring as {"requests":[...]} (newest
// last), ?limit default 500, max 1000.
func handleBurst(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > burstLimitMax {
		limit = burstLimitMax
	}
	samples := metrics.burst(limit)
	out := make([]burstRequestFrame, 0, len(samples))
	for _, s := range samples {
		startMs := s.Start.UnixMilli()
		if s.Start.IsZero() {
			startMs = s.T.Add(-time.Duration(s.DurMs * float64(time.Millisecond))).UnixMilli()
		}
		out = append(out, burstRequestFrame{
			T:            s.T.UnixMilli(),
			Start:        startMs,
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

// handleHistory serves the server-side time series for one range. Series are
// dense, grid-aligned arrays (t[i] = from + i*step); a bucket with no
// samples in the ring is null, never forward-filled.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	rangeName := r.URL.Query().Get("range")
	if rangeName == "" {
		rangeName = "1h"
	}
	if _, ok := historyRanges[rangeName]; !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown range"})
		return
	}
	seen := map[string]bool{}
	var names []string
	for _, v := range r.URL.Query()["backend"] {
		for _, n := range strings.Split(v, ",") {
			n = strings.TrimSpace(n)
			if n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	resp := history.query(rangeName, names)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
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
//
// The JSON body carries a static message ("backend temporarily
// unavailable") for every 503/502: the full upstream error is logged server-
// side only, so transport details (internal hostnames, ports) never reach
// the client.
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
	var netErr net.Error
	if errors.As(err, &opErr) || errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
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
	// The full upstream error (transport details can include internal
	// hostnames/ports) stays in the server log only; the public message is
	// static so a dial refused/reset/timeout can never leak topology.
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "backend temporarily unavailable",
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
				Start:   reqStart,
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

	// Estimated NEW prefill tokens (request-body based, ~4 bytes/token)
	// for human-facing telemetry (the burst feed's newTokensEst):
	// last-message-only for multi-turn conversations. Computed once,
	// before any admission decision.
	newTokens := estimateNewTokens(&parsed)
	// The admission gate and the live-session prefill bar both measure
	// against the WHOLE-conversation estimate: the throttle and the bar
	// must agree, so both use this value (see estimatePrefillTokens).
	prefillTok := estimatePrefillTokens(&parsed)
	// Est context tokens (whole conversation/prompt) + the conversation
	// session id, for the live sessions view. Both are body estimates.
	ctxTok := estimateCtxTokens(&parsed)
	// Stable conversation identity: with session admission enabled the
	// opaque backend-scoped key is BOTH the lease ID and the registry's
	// sessKey (one row per conversation as history grows), while disabled
	// and unkeyed traffic keep the legacy 12-hex content digest.
	var admKey string
	if sessionAdmissionSettings.Enabled {
		admKey = admissionKeyFor(&parsed, r, target.Name, sessionAdmissionSettings.KeySource, sessionAdmissionSettings.keyName)
	}
	sessKey := sessionKeyFor(&parsed)
	if admKey != "" {
		sessKey = admKey
	}
	reqContext := requestSample{
		T:            time.Now(),
		Start:        reqStart,
		Backend:      target.Name,
		Path:         r.URL.Path,
		NewTokensEst: newTokens,
		CtxTok:       ctxTok,
	}
	recordRejection := func(status int) {
		reqContext.Status = status
		// Stamp T at completion, like recordRequest: a request that waited
		// in a queue before being rejected belongs in the burst feed at
		// the moment it was rejected, not at arrival. DurMs stays
		// arrival->completion, so both stay consistent.
		reqContext.T = time.Now()
		reqContext.DurMs = float64(time.Since(reqStart) / time.Millisecond)
		// Rejections never enter the live stack, but they DO belong to the
		// conversation — fold them into the session ring/aggregates with
		// the whole-conversation prefill estimate (the figure live/finish
		// record); the last-message newTokens stays in the burst feed only.
		sessions.finishRejection(reqContext.Backend, r.URL.Path, sessKey, ctxTok, prefillTok, status, reqContext.DurMs)
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

	// ── Step 1.5: Session admission (stable conversation leases) ─────────
	// Enabled + keyed traffic only: unkeyed and disabled requests bypass
	// this gate entirely (legacy per-request behavior). The lease spans the
	// whole accepted request — downstream GPU/backend/prefill queueing,
	// upstream response, client cancellation — and releases exactly once
	// on completion (including ErrAbortHandler unwinding via the defer).
	// No grant means no accepted count and no idle lease.
	if admKey != "" {
		if mgr := sessionAdmissions[target.Name]; mgr != nil {
			admitStart := time.Now()
			ticket, aerr := mgr.acquire(r.Context(), admKey, queueTimeout)
			if aerr != nil {
				status := http.StatusTooManyRequests
				reason := "session-admission-queue-full"
				if errors.Is(aerr, errSessionAdmissionTimeout) {
					reason = "session-admission-timeout"
				} else if r.Context().Err() != nil {
					// Cancellation: 499, recorded only — never write to a
					// closed client and never enter downstream admission.
					status = 499
					recordRejection(status)
					return
				} else {
					// A non-admission error cannot happen here (the manager
					// returns only the two sentinels or ctx.Err); treat it
					// like queue-full.
					log.Printf("%s %s -> %s (model=%s) admission error: %v",
						r.Method, r.URL.Path, target.Name, modelName, aerr)
				}
				if status == http.StatusTooManyRequests {
					log.Printf("%s %s -> %s (model=%s) 429: %s (admission wait=%v)",
						r.Method, r.URL.Path, target.Name, modelName, reason, time.Since(admitStart))
					write429(w, reason, target.Name, estimateRetryAfter(target.Name))
					recordRejection(http.StatusTooManyRequests)
				}
				return
			}
			waitMs := float64(time.Since(admitStart) / time.Millisecond)
			if waitMs > 1 {
				log.Printf("%s %s -> %s (model=%s) admitted by session lease (wait=%v)",
					r.Method, r.URL.Path, target.Name, modelName, time.Since(admitStart))
			}
			defer ticket.release()
			// The lease may have waited: re-snapshot tier-0 in-flight so
			// the GPU-budget decision below does not use the arrival-time
			// figure. The cross-tier policy check itself is not repeated.
			t0Inflight = tier0Inflight()
		}
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
	// prefill is one whose estimated prompt tokens exceed the backend's
	// threshold. The estimate covers the whole conversation because the router
	// has no per-request evidence that any prefix is cached; a short final turn
	// must not exempt a long prompt. The slot is released on the first response
	// byte for streaming requests, or after the estimated prefill duration
	// (prefillTokens / PREFILL_TOKENS_PER_SEC) for non-streaming requests.
	// Small requests skip this entirely. Soft-queued up to maxQueue, then 429.
	prefillSem := prefillSlots[target.Name]
	prefillRelease := func() {} // no-op by default
	if prefillSem != nil {
		threshold := target.LargePrefillThresholdTokens
		if threshold == 0 {
			threshold = 8192
		}
		prefillTokens := prefillTok
		if prefillTokens >= threshold {
			pin, pmaxC, pwaiting, _ := prefillSem.snapshot()
			if pin >= pmaxC {
				log.Printf("%s %s -> %s (model=%s) queued at prefill (%d/%d, waiting=%d, prefillTokens=%d)",
					r.Method, r.URL.Path, target.Name, modelName,
					pin, pmaxC, pwaiting+1, prefillTokens)
			}
			if !prefillSem.acquire(queueTimeout, cancelCh) {
				retry := estimateRetryAfter(target.Name)
				stage := "prefill-full"
				_, _, pqW, pqMax := prefillSem.snapshot()
				if pqW >= pqMax {
					stage = "prefill-queue-full"
				}
				log.Printf("%s %s -> %s (model=%s) 429: prefill at capacity (prefillTokens=%d)",
					r.Method, r.URL.Path, target.Name, modelName, prefillTokens)
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
				hold := time.Duration(float64(prefillTokens) / float64(prefillTokensPerSec) * float64(time.Second))
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
	// The session live figure (estPrefillMs) feeds the queue prefill bar;
	// it must measure the same estimate the gate throttles on — the
	// whole-conversation figure, not the last-message telemetry value
	// above.
	reqID := sessions.start(target.Name, r.URL.Path, parsed.Stream, sessKey, ctxTok, prefillTok)
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

	proxyReturned := false
	defer func() {
		// ReverseProxy aborts a failed body copy with ErrAbortHandler.
		// Finalize admission state during unwinding without swallowing the
		// panic or trying to replace an already-committed wire response.
		prefillRelease()
		durations[target.Name].record(time.Since(start))
		status := tracker.status
		if status < 400 {
			if r.Context().Err() != nil {
				status = 499 // client closed request; metrics only
			} else if !proxyReturned {
				status = http.StatusBadGateway
			}
		}
		// TTFT on completion follows the same success rule as the
		// first-byte sample in respTracker.Write: a non-2xx streaming
		// response (e.g. an upstream 5xx error body) has a body latency
		// but is not serving — 0 keeps it out of the session's
		// AvgTTFTMs and the request sample.
		var ttftMs float64
		if parsed.Stream && status >= 200 && status < 300 && !tracker.firstByteAt.IsZero() {
			ttftMs = float64(tracker.firstByteAt.Sub(start) / time.Millisecond)
		}
		tracker.finish(target.Name, r.URL.Path, status, ttftMs, tracker.bytesOut)
	}()

	proxy.ServeHTTP(tracker, r)
	proxyReturned = true
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
			// decoding, so the dashboard sees it the moment it happens. Only
			// the first byte of a route-accepted 2xx stream counts: router-
			// generated error bodies (handleProxyError writes 502/503 through
			// this same tracker) and upstream non-2xx responses must never
			// publish a TTFT sample, and an already-finalized sample (finish
			// ran on the error path) cannot record a second one.
			if t.stream && !t.ttftSent && !t.recorded && t.status >= 200 && t.status < 300 {
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
		Start:        t.reqStart,
		Backend:      backend,
		accepted:     true,
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
