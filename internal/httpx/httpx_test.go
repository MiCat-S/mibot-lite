package httpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionReuseAndBodyLimit(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	for range 3 {
		if _, err := Do(context.Background(), Request{URL: server.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("did not reuse connection: %d", connections.Load())
	}
	if _, err := Do(context.Background(), Request{URL: server.URL, MaxBytes: 3}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("body limit: %v", err)
	}
}

func TestCredentialRedirectBlocked(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.Write([]byte("ok")) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if _, err := Do(context.Background(), Request{URL: redirect.URL, Headers: map[string]string{"X-Api-Key": "SECRET"}}); !errors.Is(err, ErrCredentialRedirect) {
		t.Fatalf("redirect not rejected: %v", err)
	}
	if received.Load() != 0 {
		t.Fatal("secret reached redirect target")
	}
	// 不带凭证的公开下载仍然允许跨主机重定向。
	if _, err := Do(context.Background(), Request{URL: redirect.URL}); err != nil || received.Load() != 1 {
		t.Fatalf("public redirect failed: %v", err)
	}
	// URL 里带 Token 的请求即使没有敏感头，也能选择完全不跟随重定向。
	if res, err := Do(context.Background(), Request{URL: redirect.URL, NoRedirect: true}); err != nil || res.Status != 307 || received.Load() != 1 {
		t.Fatalf("NoRedirect failed: %v", err)
	}
}

func TestRequestCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Do(ctx, Request{URL: server.URL}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context not propagated: %v", err)
	}
}
