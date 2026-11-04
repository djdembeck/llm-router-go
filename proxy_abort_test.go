package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAbortedProxyRetiresLiveSession(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		fmt.Fprint(w, "data: partial\n\n")
		w.(http.Flusher).Flush()
	}))
	defer engine.Close()
	withRouterEnv(t, []Backend{{Name: "engine", URL: engine.URL, MaxConcurrent: 1}}, map[string]int{"engine": 1}, nil)
	oldSessions, oldMetrics := sessions, metrics
	sessions, metrics = newSessionRegistry(), newTestMetrics()
	t.Cleanup(func() { sessions, metrics = oldSessions, oldMetrics })
	proxies["engine"] = newTestProxy(engine.URL)
	completed := make(chan struct{})
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(completed); handleProxy(w, r) }))
	defer router.Close()
	response, err := (&http.Client{Timeout: 3 * time.Second}).Post(router.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"engine","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err == nil {
		_, err = io.ReadAll(response.Body)
		response.Body.Close()
	}
	<-completed
	if err == nil {
		t.Fatal("truncated engine response unexpectedly completed")
	}
	if response == nil || response.StatusCode != http.StatusOK {
		t.Fatal("proxy did not start the expected upstream response")
	}
	feed := sessions.feed()
	if len(feed.Live) != 0 {
		t.Fatalf("aborted proxy left %d live requests after handler exited", len(feed.Live))
	}
	if len(feed.Sessions) != 1 || feed.Sessions[0].LiveN != 0 || feed.Sessions[0].N != 1 || feed.Sessions[0].LastStatus != http.StatusBadGateway {
		t.Fatalf("aborted request history = %+v", feed.Sessions)
	}
}

func TestCanceledProxyBeforeFirstByteReleasesAdmission(t *testing.T) {
	entered := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer engine.Close()
	withRouterEnv(t, []Backend{{Name: "engine", URL: engine.URL, MaxConcurrent: 1, MaxConcurrentLargePrefill: 1, LargePrefillThresholdTokens: 1}}, map[string]int{"engine": 1}, nil)
	prefillSlots["engine"] = newSlot(1, 1)
	oldSessions, oldMetrics := sessions, metrics
	sessions, metrics = newSessionRegistry(), newTestMetrics()
	t.Cleanup(func() { sessions, metrics = oldSessions, oldMetrics })
	proxies["engine"] = newTestProxy(engine.URL)
	completed := make(chan struct{})
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(completed); handleProxy(w, r) }))
	defer router.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", router.URL+"/v1/chat/completions", strings.NewReader(`{"model":"engine","stream":true,"messages":[{"role":"user","content":"hello world"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	clientDone := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(req)
		if response != nil {
			response.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached engine")
	}
	cancel()
	select {
	case err := <-clientDone:
		if err == nil {
			t.Fatal("canceled client request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not cancel")
	}
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("proxy did not retire canceled request")
	}
	feed := sessions.feed()
	if len(feed.Live) != 0 || len(feed.Sessions) != 1 || feed.Sessions[0].LiveN != 0 || feed.Sessions[0].N != 1 || feed.Sessions[0].LastStatus != 499 {
		t.Fatalf("canceled request feed = %+v", feed)
	}
	inFlight, _, _, _ := slots["engine"].snapshot()
	prefillInFlight, _, _, _ := prefillSlots["engine"].snapshot()
	if inFlight != 0 || prefillInFlight != 0 {
		t.Fatalf("canceled request leaked admission: slots=%d prefill=%d", inFlight, prefillInFlight)
	}
}
