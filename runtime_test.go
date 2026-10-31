package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func startBoundedServer(t *testing.T, limits serverLimits, handler http.Handler) (*http.Server, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := limits.server(listener.Addr().String(), handler)
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return server, listener.Addr().String()
}

func TestRuntimeIncomingBounds(t *testing.T) {
	limits := serverLimits{readHeader: 100 * time.Millisecond, readBody: 200 * time.Millisecond, idle: 100 * time.Millisecond, maxHeader: 1024}
	_, addr := startBoundedServer(t, limits, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "body read timeout", http.StatusRequestTimeout)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	dial := func(t *testing.T) net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	t.Run("large header", func(t *testing.T) {
		conn := dial(t)
		fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: local\r\nX-Large: %s\r\n\r\n", strings.Repeat("x", 8192))
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 431 {
			t.Fatalf("status = %d", response.StatusCode)
		}
	})
	t.Run("slow header", func(t *testing.T) {
		conn := dial(t)
		fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost:")
		started := time.Now()
		_, err := bufio.NewReader(conn).ReadByte()
		if err == nil {
			t.Fatal("incomplete header accepted")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("header not bounded: %s", elapsed)
		}
	})
	t.Run("slow body", func(t *testing.T) {
		conn := dial(t)
		fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: local\r\nContent-Length: 10\r\n\r\nx")
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("status = %d", response.StatusCode)
		}
	})
	t.Run("idle keepalive", func(t *testing.T) {
		conn := dial(t)
		fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: local\r\n\r\n")
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		started := time.Now()
		_, err = reader.ReadByte()
		if err == nil {
			t.Fatal("idle connection remained open")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("idle not bounded: %s", elapsed)
		}
	})
}

func TestRuntimeStreamDrain(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprintf("forced=%v", forced), func(t *testing.T) {
			release := make(chan struct{})
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					fmt.Fprint(w, "data: last\n\n")
				case <-r.Context().Done():
					close(canceled)
				}
			}))
			defer upstream.Close()
			withRouterEnv(t, []Backend{{Name: "engine", URL: upstream.URL}}, nil, nil)
			proxies["engine"] = newTestProxy(upstream.URL)
			limits := serverLimits{readHeader: time.Second, readBody: 50 * time.Millisecond, idle: 50 * time.Millisecond, maxHeader: 1 << 20}
			server, addr := startBoundedServer(t, limits, buildMux())
			client := &http.Client{Timeout: 4 * time.Second}
			response, err := client.Post("http://"+addr+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"engine","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			reader := bufio.NewReader(response.Body)
			first, err := reader.ReadString('\n')
			if err != nil || first != "data: first\n" {
				t.Fatalf("first chunk = %q, %v", first, err)
			}
			// Active SSE outlives both incoming-read and idle limits.
			time.Sleep(120 * time.Millisecond)
			done := make(chan struct{})
			started := time.Now()
			go func() { drainServers(400*time.Millisecond, server); close(done) }()
			if !forced {
				select {
				case <-done:
					t.Fatal("shutdown returned before active stream completed")
				case <-time.After(50 * time.Millisecond):
				}
				close(release)
			}
			rest, readErr := io.ReadAll(reader)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("drain hung")
			}
			if forced {
				if readErr == nil {
					t.Fatal("forced stream ended without transport error")
				}
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("upstream not canceled")
				}
				if time.Since(started) > time.Second {
					t.Fatal("forced drain exceeded bound")
				}
			} else if readErr != nil || !strings.Contains(string(rest), "data: last\n\n") {
				t.Fatalf("stream failed graceful drain: %q, %v", rest, readErr)
			}
		})
	}
}
