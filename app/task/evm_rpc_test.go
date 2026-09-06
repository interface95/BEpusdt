package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
)

const (
	evmTestHash       = "0x1111111111111111111111111111111111111111111111111111111111111111"
	evmTestBlockHash  = "0x2222222222222222222222222222222222222222222222222222222222222222"
	evmTestParentHash = "0x3333333333333333333333333333333333333333333333333333333333333333"
	evmTestFrom       = "0x4444444444444444444444444444444444444444"
	evmTestTo         = "0x5555555555555555555555555555555555555555"
)

type evmTestCall struct {
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
	ID     int64             `json:"id"`
}

func evmTestConfig(t *testing.T, values map[model.ConfKey]string) {
	t.Helper()
	oldDB := model.Db
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "rpc.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Conf{}); err != nil {
		t.Fatal(err)
	}
	keys := []model.ConfKey{model.RpcEndpointBsc, "rpc_endpoint_bsc_read", "rpc_endpoint_bsc_read_backups", "rpc_endpoint_bsc_log_backups", model.PaymentMatchMode, model.BlockOffsetConfirm}
	original := make([]model.Conf, 0, len(keys))
	for _, key := range keys {
		original = append(original, model.Conf{K: key, V: model.GetC(key)})
		if err := db.Create(&model.Conf{K: key, V: values[key]}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model.Db = db
	model.RefreshC()
	t.Cleanup(func() {
		for _, c := range original {
			if err := db.Model(&model.Conf{}).Where("k = ?", c.K).Update("v", c.V).Error; err != nil {
				t.Error(err)
			}
		}
		model.RefreshC()
		model.Db = oldDB
		sqlDB, err := db.DB()
		if err == nil {
			sqlDB.Close()
		}
	})
}

func evmTestServer(t *testing.T, handle func(evmTestCall) (any, map[string]any)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		batch := len(raw) > 0 && raw[0] == '['
		var calls []evmTestCall
		if batch {
			if err := json.Unmarshal(raw, &calls); err != nil {
				t.Error(err)
				return
			}
		} else {
			var call evmTestCall
			if err := json.Unmarshal(raw, &call); err != nil {
				t.Error(err)
				return
			}
			calls = []evmTestCall{call}
		}
		responses := make([]any, 0, len(calls))
		for _, call := range calls {
			result, rpcError := handle(call)
			response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
			if rpcError != nil {
				response["error"] = rpcError
			} else {
				response["result"] = result
			}
			responses = append(responses, response)
		}
		w.Header().Set("Content-Type", "application/json")
		var output any = responses
		if !batch {
			output = responses[0]
		}
		if err := json.NewEncoder(w).Encode(output); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func evmTestEngine() *evm {
	return &evm{Network: conf.Bsc, Client: &http.Client{Timeout: 2 * time.Second}, Native: evmNative{Parse: true, Decimal: -18, TradeType: model.BscBnb}}
}

func evmTestTransaction() map[string]any {
	return map[string]any{"hash": evmTestHash, "blockHash": evmTestBlockHash, "blockNumber": "0x64", "transactionIndex": "0x0", "from": evmTestFrom, "to": evmTestTo, "input": "0x", "value": "0x0"}
}

func evmTestHeader() map[string]any {
	return map[string]any{"number": "0x64", "hash": evmTestBlockHash, "parentHash": evmTestParentHash, "timestamp": "0x6553f100", "transactions": []any{evmTestTransaction()}}
}

func evmTestLog() map[string]any {
	return map[string]any{"address": conf.UsdtBep20, "topics": []string{evmTransferEvent, "0x" + strings.Repeat("0", 24) + evmTestFrom[2:], "0x" + strings.Repeat("0", 24) + evmTestTo[2:]}, "data": fmt.Sprintf("0x%064x", uint64(1000000000000000000)), "transactionHash": evmTestHash, "blockHash": evmTestBlockHash, "blockNumber": "0x64", "transactionIndex": "0x0", "logIndex": "0x2", "removed": false}
}

func evmTestReceipt() map[string]any {
	return map[string]any{"transactionHash": evmTestHash, "blockHash": evmTestBlockHash, "blockNumber": "0x64", "transactionIndex": "0x0", "status": "0x1", "from": evmTestFrom, "to": evmTestTo, "logs": []any{evmTestLog()}}
}

func TestEVMRPCRejectsWrongChainBeforeHead(t *testing.T) {
	var headCalls atomic.Int32
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return "0x1", nil
		}
		headCalls.Add(1)
		return "0x64", nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	if _, err := evmTestEngine().evmHead(context.Background()); err == nil {
		t.Fatal("accepted Ethereum endpoint as BSC")
	}
	if headCalls.Load() != 0 {
		t.Fatal("read data before validating chain")
	}
}

func TestEVMRPCRateLimitClassifiedWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "secret-node-token")
	}))
	defer server.Close()
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL + "/secret-node-token"})
	_, err := evmTestEngine().evmHead(context.Background())
	var rpcError *EVMRPCError
	if !errors.As(err, &rpcError) || rpcError.Kind != "rate_limited" || rpcError.RetryAfter != 120*time.Second {
		t.Fatalf("wrong error: %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error leaked endpoint: %v", err)
	}
}

func TestEVMRPCUsesReadEndpointAndCachesChainCheck(t *testing.T) {
	var chainCalls atomic.Int32
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			chainCalls.Add(1)
			return "0x38", nil
		}
		return "0x64", nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: "http://127.0.0.1:1", "rpc_endpoint_bsc_read": server.URL})
	e := evmTestEngine()
	for i := 0; i < 2; i++ {
		if head, err := e.evmHead(context.Background()); err != nil || head != 100 {
			t.Fatalf("head=%d err=%v", head, err)
		}
	}
	if chainCalls.Load() != 1 {
		t.Fatalf("chain checks=%d", chainCalls.Load())
	}
}

func TestEVMRPCLogRestrictionFallsBackToCompleteReceipts(t *testing.T) {
	var receipts atomic.Int32
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_getBlockByNumber":
			return evmTestHeader(), nil
		case "eth_getLogs":
			var filter struct {
				Address []string `json:"address"`
			}
			if err := json.Unmarshal(call.Params[0], &filter); err != nil {
				t.Error(err)
			}
			if len(filter.Address) != 2 {
				t.Errorf("BSC token filter=%v", filter.Address)
			}
			return nil, map[string]any{"code": -32602, "message": "Archive requests require a personal token at secret-url"}
		case "eth_getBlockReceipts":
			receipts.Add(1)
			return []any{evmTestReceipt()}, nil
		default:
			t.Errorf("unexpected method %s", call.Method)
			return nil, nil
		}
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	items, err := evmTestEngine().fetchEVMRange(context.Background(), evmBlock{From: 100, To: 100})
	if err != nil || len(items) != 1 {
		t.Fatalf("transfers=%v err=%v", items, err)
	}
	if items[0].Amount.String() != "1" || items[0].RecvAddress != evmTestTo || items[0].BlockNum != 100 {
		t.Fatalf("wrong transfer %+v", items[0])
	}
	if receipts.Load() != 1 {
		t.Fatalf("receipt calls=%d", receipts.Load())
	}
}

func TestEVMRPCRejectsIncompleteReceiptFallback(t *testing.T) {
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_getBlockByNumber":
			return evmTestHeader(), nil
		case "eth_getLogs":
			return nil, map[string]any{"code": -32601, "message": "unsupported"}
		case "eth_getBlockReceipts":
			return []any{}, nil
		default:
			return nil, nil
		}
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	items, err := evmTestEngine().fetchEVMRange(context.Background(), evmBlock{From: 100, To: 100})
	if err == nil || len(items) != 0 {
		t.Fatalf("incomplete receipt scan accepted: %v %v", items, err)
	}
}

func TestEVMRPCRejectsMalformedLogs(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"short_topic", func(l map[string]any) { l["topics"] = []string{evmTransferEvent, "0x1", "0x2"} }},
		{"bad_data", func(l map[string]any) { l["data"] = "0x123" }},
		{"wrong_block", func(l map[string]any) { l["blockHash"] = evmTestParentHash }},
		{"removed", func(l map[string]any) { l["removed"] = true }},
		{"foreign_contract", func(l map[string]any) { l["address"] = conf.UsdtErc20 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
				switch call.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_getBlockByNumber":
					return evmTestHeader(), nil
				case "eth_getLogs":
					l := evmTestLog()
					test.mutate(l)
					return []any{l}, nil
				default:
					return nil, nil
				}
			})
			evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
			if _, err := evmTestEngine().fetchEVMRange(context.Background(), evmBlock{From: 100, To: 100}); err == nil {
				t.Fatal("malformed log accepted")
			}
		})
	}
}

func TestEVMRPCRejectsOversizedRangeBeforeHTTP(t *testing.T) {
	if _, err := evmTestEngine().fetchEVMRange(context.Background(), evmBlock{From: 100, To: 110}); err == nil {
		t.Fatal("accepted eleven block receipt window")
	}
}
