package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/v03413/bepusdt/app/model"
)

func TestEVMRPCReceiptFallbackRejectsCorruptBlockContents(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"wrong_receipt_height", func(r, h map[string]any) { r["blockNumber"] = "0x65" }},
		{"wrong_receipt_hash", func(r, h map[string]any) { r["transactionHash"] = evmTestParentHash }},
		{"wrong_receipt_block_hash", func(r, h map[string]any) { r["blockHash"] = evmTestParentHash }},
		{"missing_transaction_receipt", func(r, h map[string]any) {
			tx := evmTestTransaction()
			tx["hash"] = evmTestParentHash
			tx["transactionIndex"] = "0x1"
			h["transactions"] = []any{evmTestTransaction(), tx}
		}},
		{"receipt_bad_index", func(r, h map[string]any) { r["transactionIndex"] = "0x1" }},
		{"receipt_bad_status", func(r, h map[string]any) { r["status"] = "0x2" }},
		{"receipt_logs_null", func(r, h map[string]any) { r["logs"] = nil }},
		{"receipt_logs_wrong_tx", func(r, h map[string]any) {
			event := evmTestLog()
			event["transactionHash"] = evmTestParentHash
			r["logs"] = []any{event}
		}},
		{"duplicate_log_index", func(r, h map[string]any) { r["logs"] = []any{evmTestLog(), evmTestLog()} }},
		{"failed_receipt_with_logs", func(r, h map[string]any) { r["status"] = "0x0" }},
		{"bad_event_address", func(r, h map[string]any) { event := evmTestLog(); event["address"] = "0x11"; r["logs"] = []any{event} }},
		{"duplicate_block_transactions", func(r, h map[string]any) {
			tx := evmTestTransaction()
			tx["transactionIndex"] = "0x1"
			h["transactions"] = []any{evmTestTransaction(), tx}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt, header := evmTestReceipt(), evmTestHeader()
			test.mutate(receipt, header)
			server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
				switch call.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_getBlockByNumber":
					return header, nil
				case "eth_getBlockReceipts":
					return []any{receipt}, nil
				default:
					return nil, nil
				}
			})
			evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
			e := evmTestEngine()
			blocks, err := e.evmReadBlocks(context.Background(), evmBlock{From: 100, To: 100})
			if err == nil {
				_, err = e.evmReceiptLogs(context.Background(), blocks)
			}
			if err == nil {
				t.Fatal("incomplete or corrupt block was accepted")
			}
		})
	}
}

func TestEVMRPCReceiptFallbackRequiresEveryBatchBlock(t *testing.T) {
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return "0x38", nil
		}
		if call.ID == 100 {
			return []any{evmTestReceipt()}, nil
		}
		return nil, nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	raw, err := json.Marshal(evmTestHeader())
	if err != nil {
		t.Fatal(err)
	}
	block, err := evmDecodeBlock(raw, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	second := block
	second.number = 101
	if _, err := evmTestEngine().evmReceiptLogs(context.Background(), []evmDecodedBlock{block, second}); err == nil {
		t.Fatal("null receipts for second block accepted")
	}
}

func TestEVMRPCHeaderAndQuantityValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong_number", func(h map[string]any) { h["number"] = "0x65" }},
		{"missing_hash", func(h map[string]any) { delete(h, "hash") }},
		{"zero_hash", func(h map[string]any) { h["hash"] = "0x" + strings.Repeat("0", 64) }},
		{"invalid_parent", func(h map[string]any) { h["parentHash"] = "0x1" }},
		{"zero_parent", func(h map[string]any) { h["parentHash"] = "0x" + strings.Repeat("0", 64) }},
		{"bad_time", func(h map[string]any) { h["timestamp"] = "0xwrong" }},
		{"future_time", func(h map[string]any) { h["timestamp"] = "0x7fffffffffffffff" }},
		{"null_transactions", func(h map[string]any) { h["transactions"] = nil }},
		{"string_transactions", func(h map[string]any) { h["transactions"] = []string{evmTestHash} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := evmTestHeader()
			test.mutate(h)
			raw, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := evmDecodeBlock(raw, 100, true); err == nil {
				t.Fatal("invalid header accepted")
			}
		})
	}
	for _, value := range []string{"", "0x", "0x00", "-1", "0x-1", "1", "0x1g", "0x" + strings.Repeat("f", 65)} {
		if _, err := evmQuantity(value); err == nil {
			t.Fatalf("invalid quantity accepted: %s", value)
		}
	}
	if _, err := evmHeight("0xffffffffffffffff"); err == nil {
		t.Fatal("height overflow accepted")
	}
}

func TestEVMRPCFetchSimpleNativeTransferAndBlockTime(t *testing.T) {
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_getBlockByNumber":
			h, tx := evmTestHeader(), evmTestTransaction()
			tx["value"] = "0xde0b6b3a7640000"
			h["transactions"] = []any{tx}
			return h, nil
		case "eth_getLogs":
			return []any{}, nil
		default:
			return nil, nil
		}
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	e := evmTestEngine()
	items, err := e.fetchEVMRange(context.Background(), evmBlock{From: 100, To: 100})
	if err != nil || len(items) != 1 || items[0].TradeType != model.BscBnb || items[0].EventIndex != "native" || items[0].BlockHash != evmTestBlockHash || items[0].Amount.String() != "1" {
		t.Fatalf("native transfers=%v err=%v", items, err)
	}
	if at, err := e.evmBlockTime(context.Background(), 100); err != nil || at.Unix() != 1700000000 {
		t.Fatalf("time=%v err=%v", at, err)
	}
}

func TestEVMRPCDuplicateReceiptsCannotFillMissingTransaction(t *testing.T) {
	header := evmTestHeader()
	tx := evmTestTransaction()
	tx["hash"] = evmTestParentHash
	tx["transactionIndex"] = "0x1"
	header["transactions"] = []any{evmTestTransaction(), tx}
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_getBlockByNumber":
			return header, nil
		case "eth_getBlockReceipts":
			return []any{evmTestReceipt(), evmTestReceipt()}, nil
		default:
			return nil, nil
		}
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	e := evmTestEngine()
	blocks, err := e.evmReadBlocks(context.Background(), evmBlock{From: 100, To: 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.evmReceiptLogs(context.Background(), blocks); err == nil {
		t.Fatal("duplicate receipts filled missing transaction")
	}
}

func TestEVMRPCRejectsDisconnectedBatchHeaders(t *testing.T) {
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return "0x38", nil
		}
		header := evmTestHeader()
		header["number"] = fmt.Sprintf("0x%x", call.ID)
		header["transactions"] = []any{}
		return header, nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	if _, err := evmTestEngine().evmReadBlocks(context.Background(), evmBlock{From: 100, To: 101}); err == nil {
		t.Fatal("disconnected parent hash accepted")
	}
}
