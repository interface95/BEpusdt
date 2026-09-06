package task

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
)

const (
	evmHTTPRequestTimeout = 8 * time.Second
	evmReadBudget         = 30 * time.Second
	evmRangeBudget        = 90 * time.Second
	evmChainCacheTTL      = 5 * time.Minute
	evmRPCBodyLimit       = 32 << 20
)

// EVMRPCError deliberately excludes endpoint URLs, provider messages and bodies.
// The durable scheduler owns retries; RetryAfter is a minimum scheduling delay.
type EVMRPCError struct {
	Kind       string
	Method     string
	HTTPStatus int
	RPCCode    int
	RetryAfter time.Duration
	cause      error
}

func (e *EVMRPCError) Error() string {
	return fmt.Sprintf("EVM RPC %s: %s (http=%d rpc=%d)", e.Method, e.Kind, e.HTTPStatus, e.RPCCode)
}

func (e *EVMRPCError) Unwrap() error { return e.cause }

func (e *EVMRPCError) RetryDelay() time.Duration { return e.RetryAfter }

func (e *EVMRPCError) Retryable() bool {
	return e.Kind == "rate_limited" || e.Kind == "unavailable" || e.Kind == "timeout" || e.Kind == "pending" || e.Kind == "inconsistent"
}

func evmError(method, kind string) *EVMRPCError { return &EVMRPCError{Method: method, Kind: kind} }

type evmRPCRequest struct {
	Version string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
	ID      int64  `json:"id"`
}

type evmRPCResponse struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func evmRequest(method string, params any, id int64) evmRPCRequest {
	return evmRPCRequest{Version: "2.0", Method: method, Params: params, ID: id}
}

var evmValidatedChains = struct {
	sync.Mutex
	entries map[[32]byte]time.Time
}{entries: make(map[[32]byte]time.Time)}

type evmLogStartGate struct {
	sync.Mutex
	next time.Time
}

var evmBSCLogGate evmLogStartGate

// Reserve only the next actual start, not a queue of future slots. No mutex is
// held while sleeping or during HTTP, and receipt/header reads bypass this gate.
func (g *evmLogStartGate) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.Lock()
		now := time.Now()
		delay := g.next.Sub(now)
		if delay <= 0 {
			g.next = now.Add(550 * time.Millisecond)
		}
		g.Unlock()
		if delay <= 0 {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (e *evm) evmEndpoints(logs bool) []string {
	primary := e.rpcEndpoint()
	backups := ""
	if e.Network == conf.Bsc {
		if logs {
			backups = model.GetC("rpc_endpoint_bsc_log_backups")
		} else {
			if configured := strings.TrimSpace(model.GetC("rpc_endpoint_bsc_read")); configured != "" {
				primary = configured
			}
			backups = model.GetC("rpc_endpoint_bsc_read_backups")
		}
	}
	var endpoints []string
	seen := make(map[string]bool)
	for _, endpoint := range append([]string{primary}, strings.Split(backups, ",")...) {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint != "" && !seen[endpoint] {
			endpoints = append(endpoints, endpoint)
			seen[endpoint] = true
		}
		if len(endpoints) == 4 {
			break
		}
	}
	return endpoints
}

func (e *evm) evmChainCheck(ctx context.Context, endpoint string) error {
	if e.Network != conf.Bsc {
		return nil
	}
	key := sha256.Sum256([]byte(endpoint))
	now := time.Now()
	evmValidatedChains.Lock()
	validUntil := evmValidatedChains.entries[key]
	evmValidatedChains.Unlock()
	if now.Before(validUntil) {
		return nil
	}
	results, err := e.evmHTTP(ctx, endpoint, []evmRPCRequest{evmRequest("eth_chainId", []any{}, 1)})
	if err != nil {
		return err
	}
	var encoded string
	if json.Unmarshal(results[0], &encoded) != nil {
		return evmError("eth_chainId", "invalid_response")
	}
	chain, err := evmQuantity(encoded)
	if err != nil || !chain.IsInt64() || chain.Int64() != 56 {
		return evmError("eth_chainId", "wrong_chain")
	}
	evmValidatedChains.Lock()
	for k, expires := range evmValidatedChains.entries {
		if !now.Before(expires) {
			delete(evmValidatedChains.entries, k)
		}
	}
	if len(evmValidatedChains.entries) >= 64 {
		clear(evmValidatedChains.entries)
	}
	evmValidatedChains.entries[key] = time.Now().Add(evmChainCacheTTL)
	evmValidatedChains.Unlock()
	return nil
}

func (e *evm) evmRPC(ctx context.Context, logs bool, calls []evmRPCRequest) ([]json.RawMessage, error) {
	if len(calls) == 0 || len(calls) > blockParseMaxNum {
		return nil, evmError("batch", "invalid_request")
	}
	last := error(evmError(calls[0].Method, "not_configured"))
	var restricted *EVMRPCError
	var retryAfter time.Duration
	for _, endpoint := range e.evmEndpoints(logs) {
		if ctx.Err() != nil {
			return nil, &EVMRPCError{Method: calls[0].Method, Kind: "timeout", cause: ctx.Err()}
		}
		err := e.evmChainCheck(ctx, endpoint)
		if err == nil {
			if e.Network == conf.Bsc && logs && calls[0].Method == "eth_getLogs" {
				if err := evmBSCLogGate.wait(ctx); err != nil {
					return nil, &EVMRPCError{Method: "eth_getLogs", Kind: "timeout", cause: err}
				}
			}
			var result []json.RawMessage
			result, err = e.evmHTTP(ctx, endpoint, calls)
			if err == nil {
				return result, nil
			}
		}
		last = err
		var rpcError *EVMRPCError
		if errors.As(err, &rpcError) {
			if rpcError.Kind == "restricted" && rpcError.Method == "eth_getLogs" {
				restricted = rpcError
			}
			if rpcError.RetryAfter > retryAfter {
				retryAfter = rpcError.RetryAfter
			}
		}
	}
	if restricted != nil {
		last = restricted
	}
	var rpcError *EVMRPCError
	if errors.As(last, &rpcError) {
		copyError := *rpcError
		copyError.RetryAfter = retryAfter
		last = &copyError
	}
	return nil, last
}

func (e *evm) evmHTTP(ctx context.Context, endpoint string, calls []evmRPCRequest) ([]json.RawMessage, error) {
	method := calls[0].Method
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, evmError(method, "invalid_endpoint")
	}
	var payload any = calls
	if len(calls) == 1 {
		payload = calls[0]
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, evmError(method, "invalid_request")
	}
	requestCtx, cancel := context.WithTimeout(ctx, evmHTTPRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, evmError(method, "invalid_endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: evmHTTPRequestTimeout}
	if e.Client != nil {
		client = *e.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, &EVMRPCError{Method: method, Kind: "timeout", cause: requestCtx.Err()}
		}
		return nil, evmError(method, "unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		kind := "invalid_response"
		switch {
		case resp.StatusCode == 429:
			kind = "rate_limited"
		case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 405 || resp.StatusCode == 404:
			kind = "restricted"
		case resp.StatusCode >= 500 || resp.StatusCode == 408:
			kind = "unavailable"
		}
		return nil, &EVMRPCError{Method: method, Kind: kind, HTTPStatus: resp.StatusCode, RetryAfter: evmRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, evmRPCBodyLimit+1))
	if err != nil {
		if requestCtx.Err() != nil {
			return nil, &EVMRPCError{Method: method, Kind: "timeout", cause: requestCtx.Err()}
		}
		return nil, evmError(method, "unavailable")
	}
	if len(data) > evmRPCBodyLimit {
		return nil, evmError(method, "response_too_large")
	}
	return evmDecodeResponses(data, calls)
}

func evmDecodeResponses(data []byte, calls []evmRPCRequest) ([]json.RawMessage, error) {
	method := calls[0].Method
	var responses []evmRPCResponse
	if len(calls) == 1 {
		var response evmRPCResponse
		if json.Unmarshal(data, &response) != nil {
			return nil, evmError(method, "invalid_response")
		}
		responses = []evmRPCResponse{response}
	} else if json.Unmarshal(data, &responses) != nil {
		return nil, evmError(method, "invalid_response")
	}
	if len(responses) != len(calls) {
		return nil, evmError(method, "incomplete_batch")
	}
	wanted := make(map[int64]int, len(calls))
	for i, call := range calls {
		if _, exists := wanted[call.ID]; exists {
			return nil, evmError(method, "invalid_request")
		}
		wanted[call.ID] = i
	}
	results := make([]json.RawMessage, len(calls))
	for _, response := range responses {
		var id int64
		if response.Version != "2.0" || json.Unmarshal(response.ID, &id) != nil || string(response.ID) == "null" {
			return nil, evmError(method, "invalid_response")
		}
		index, exists := wanted[id]
		if !exists {
			return nil, evmError(method, "incomplete_batch")
		}
		delete(wanted, id)
		if response.Error != nil {
			if len(response.Result) != 0 {
				return nil, evmError(method, "invalid_response")
			}
			return nil, evmProviderError(calls[index].Method, response.Error.Code, response.Error.Message)
		}
		if len(response.Result) == 0 {
			return nil, evmError(method, "invalid_response")
		}
		results[index] = response.Result
	}
	return results, nil
}

func evmProviderError(method string, code int, message string) *EVMRPCError {
	text := strings.ToLower(message)
	kind := "remote_error"
	switch {
	case code == -32601:
		kind = "restricted"
	case strings.Contains(text, "archive") || strings.Contains(text, "personal token") || strings.Contains(text, "subscription") || strings.Contains(text, "not supported") || strings.Contains(text, "not available") || strings.Contains(text, "disabled") || strings.Contains(text, "unauthorized") || strings.Contains(text, "forbidden") || strings.Contains(text, "access denied"):
		kind = "restricted"
	case code == 429 || strings.Contains(text, "rate limit") || strings.Contains(text, "too many requests"):
		kind = "rate_limited"
	case code == -32005 && method == "eth_getLogs":
		kind = "restricted"
	case code == -32603 || strings.Contains(text, "timeout") || strings.Contains(text, "temporarily"):
		kind = "unavailable"
	case code == -32602 || code == -32600:
		kind = "invalid_request"
	}
	return &EVMRPCError{Method: method, Kind: kind, RPCCode: code}
}

func evmRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > 86400 {
			seconds = 86400
		}
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(value); err == nil && until.After(now) {
		delay := until.Sub(now)
		if delay > 24*time.Hour {
			return 24 * time.Hour
		}
		return delay
	}
	return 0
}

// evmHead returns the unadjusted head; the scheduler applies RollDelayOffset.
func (e *evm) evmHead(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, evmReadBudget)
	defer cancel()
	results, err := e.evmRPC(ctx, false, []evmRPCRequest{evmRequest("eth_blockNumber", []any{}, 1)})
	if err != nil {
		return 0, err
	}
	var encoded string
	if json.Unmarshal(results[0], &encoded) != nil {
		return 0, evmError("eth_blockNumber", "invalid_response")
	}
	head, err := evmHeight(encoded)
	if err != nil || head <= 0 {
		return 0, evmError("eth_blockNumber", "invalid_response")
	}
	return head, nil
}

func (e *evm) evmBlockTime(ctx context.Context, height int64) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, evmReadBudget)
	defer cancel()
	if height < 0 {
		return time.Time{}, evmError("eth_getBlockByNumber", "invalid_request")
	}
	results, err := e.evmRPC(ctx, false, []evmRPCRequest{evmRequest("eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", height), false}, 1)})
	if err != nil {
		return time.Time{}, err
	}
	header, err := evmDecodeBlock(results[0], height, false)
	if err != nil {
		return time.Time{}, err
	}
	return header.at, nil
}
