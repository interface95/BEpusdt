package task

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/v03413/bepusdt/app/model"
)

func TestEVMRPCBatchRejectsMissingDuplicateAndUnknownIDs(t *testing.T) {
	calls := []evmRPCRequest{evmRequest("eth_getBlockByNumber", []any{"0x64", true}, 100), evmRequest("eth_getBlockByNumber", []any{"0x65", true}, 101)}
	for _, test := range []struct {
		name     string
		response string
		wantOK   bool
	}{
		{"out_of_order", `[{"jsonrpc":"2.0","id":101,"result":"second"},{"jsonrpc":"2.0","id":100,"result":"first"}]`, true},
		{"missing", `[{"jsonrpc":"2.0","id":100,"result":{}}]`, false},
		{"duplicate", `[{"jsonrpc":"2.0","id":100,"result":{}},{"jsonrpc":"2.0","id":100,"result":{}}]`, false},
		{"unknown", `[{"jsonrpc":"2.0","id":100,"result":{}},{"jsonrpc":"2.0","id":102,"result":{}}]`, false},
		{"string_id", `[{"jsonrpc":"2.0","id":"100","result":{}},{"jsonrpc":"2.0","id":101,"result":{}}]`, false},
		{"null_id", `[{"jsonrpc":"2.0","id":null,"result":{}},{"jsonrpc":"2.0","id":101,"result":{}}]`, false},
		{"wrong_version", `[{"jsonrpc":"1.0","id":100,"result":{}},{"jsonrpc":"2.0","id":101,"result":{}}]`, false},
		{"result_and_error", `[{"jsonrpc":"2.0","id":100,"result":{},"error":{"code":-32603,"message":"secret"}},{"jsonrpc":"2.0","id":101,"result":{}}]`, false},
		{"no_result", `[{"jsonrpc":"2.0","id":100},{"jsonrpc":"2.0","id":101,"result":{}}]`, false},
		{"non_array", `{"jsonrpc":"2.0","id":100,"result":{}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, test.response) }))
			defer server.Close()
			results, err := evmTestEngine().evmHTTP(context.Background(), server.URL, calls)
			if (err == nil) != test.wantOK {
				t.Fatalf("result=%v error=%v", results, err)
			}
			if test.wantOK && string(results[0]) != `"first"` {
				t.Fatal("batch response order was not restored")
			}
		})
	}
}

func TestEVMRPCFallbackValidatesBackupChain(t *testing.T) {
	var backupCalls atomic.Int32
	primary := evmTestServer(t, func(call evmTestCall) (any, map[string]any) { return "0x1", nil })
	backup := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		backupCalls.Add(1)
		if call.Method == "eth_chainId" {
			return "0x38", nil
		}
		return "0x64", nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: primary.URL, "rpc_endpoint_bsc_read_backups": backup.URL})
	if head, err := evmTestEngine().evmHead(context.Background()); err != nil || head != 100 {
		t.Fatalf("head=%d err=%v", head, err)
	}
	if backupCalls.Load() != 2 {
		t.Fatal("backup chain was not checked")
	}
}

func TestEVMRPCChainCacheExpires(t *testing.T) {
	var chain atomic.Int32
	chain.Store(56)
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return fmt.Sprintf("0x%x", chain.Load()), nil
		}
		return "0x64", nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	e := evmTestEngine()
	if _, err := e.evmHead(context.Background()); err != nil {
		t.Fatal(err)
	}
	chain.Store(1)
	evmValidatedChains.Lock()
	evmValidatedChains.entries[sha256.Sum256([]byte(server.URL))] = time.Now().Add(-time.Second)
	evmValidatedChains.Unlock()
	if _, err := e.evmHead(context.Background()); err == nil {
		t.Fatal("expired chain validation was reused")
	}
}

func TestEVMRPCLogStartGateSpacingAndCancellation(t *testing.T) {
	gate := &evmLogStartGate{}
	if err := gate.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	canceled, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := gate.wait(canceled); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel error=%v", err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("gate ignored cancellation")
	}
	if err := gate.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("log start spacing was not enforced")
	}
}

func TestEVMRPCReadsBypassPendingLogHTTP(t *testing.T) {
	logStarted := make(chan struct{})
	releaseLog := make(chan struct{})
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_getLogs":
			close(logStarted)
			<-releaseLog
			return []any{}, nil
		case "eth_blockNumber":
			return "0x64", nil
		default:
			return nil, nil
		}
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	e := evmTestEngine()
	logDone := make(chan error, 1)
	go func() {
		_, err := e.evmRangeLogs(context.Background(), evmBlock{From: 100, To: 100}, e.evmTokens())
		logDone <- err
	}()
	defer func() {
		close(releaseLog)
		if err := <-logDone; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-logStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("log did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if head, err := e.evmHead(ctx); err != nil || head != 100 {
		t.Fatalf("read blocked behind log I/O: head=%d err=%v", head, err)
	}
}

func TestEVMRPCContextAndInvalidEndpointsStayRedacted(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := []evmRPCRequest{evmRequest("eth_blockNumber", []any{}, 1)}
	if _, err := evmTestEngine().evmHTTP(ctx, server.URL+"/?password=secret", calls); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	for _, endpoint := range []string{"ftp://secret@host/key", "http://%secret", "http://127.0.0.1:1/secret"} {
		if _, err := evmTestEngine().evmHTTP(context.Background(), endpoint, calls); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe endpoint error: %v", err)
		}
	}
}

func TestEVMRPCHTTPClassificationAndRetryAfter(t *testing.T) {
	for _, test := range []struct {
		status int
		kind   string
	}{{401, "restricted"}, {403, "restricted"}, {404, "restricted"}, {405, "restricted"}, {408, "unavailable"}, {429, "rate_limited"}, {503, "unavailable"}, {400, "invalid_response"}} {
		t.Run(fmt.Sprint(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "90")
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			_, err := evmTestEngine().evmHTTP(context.Background(), server.URL, []evmRPCRequest{evmRequest("eth_getLogs", []any{}, 1)})
			var rpcError *EVMRPCError
			if !errors.As(err, &rpcError) || rpcError.Kind != test.kind || rpcError.RetryDelay() != 90*time.Second {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	if delay := evmRetryAfter(now.Add(120*time.Second).Format(http.TimeFormat), now); delay != 120*time.Second {
		t.Fatalf("date Retry-After=%v", delay)
	}
	for _, value := range []string{"", "bad", "-1", "0", now.Add(-time.Second).Format(http.TimeFormat)} {
		if evmRetryAfter(value, now) != 0 {
			t.Fatalf("invalid Retry-After accepted: %s", value)
		}
	}
}

func TestEVMRPCNoRedirectToAnotherEndpoint(t *testing.T) {
	var contacted atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := evmTestEngine().evmHTTP(context.Background(), server.URL, []evmRPCRequest{evmRequest("eth_blockNumber", []any{}, 1)}); err == nil {
		t.Fatal("redirect accepted")
	}
	if contacted.Load() != 0 {
		t.Fatal("RPC credentials could cross redirect boundary")
	}
}

func TestEVMRPCErrorResponseDoesNotLeakProviderMessage(t *testing.T) {
	for _, test := range []struct {
		code          int
		message, kind string
	}{{-32601, "unsupported", "restricted"}, {-32602, "Archive token=secret", "restricted"}, {-32005, "limit exceeded", "restricted"}, {-32000, "too many requests secret-url", "rate_limited"}, {-32603, "server secret", "unavailable"}, {-32602, "bad params", "invalid_request"}, {-1, "secret", "remote_error"}} {
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": test.code, "message": test.message}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = evmDecodeResponses(raw, []evmRPCRequest{evmRequest("eth_getLogs", []any{}, 1)})
		var rpcError *EVMRPCError
		if !errors.As(err, &rpcError) || rpcError.Kind != test.kind || strings.Contains(err.Error(), "secret") {
			t.Fatalf("error=%v expected=%s", err, test.kind)
		}
	}
}
