package task

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/v03413/bepusdt/app/model"
)

func TestEVMRPCFailedReceiptRequiresCanonicalEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(map[string]any, map[string]any)
		wantKind string
	}{
		{"canonical_failure", func(r, h map[string]any) {}, "failed_transaction"},
		{"wrong_receipt_hash", func(r, h map[string]any) { r["transactionHash"] = evmTestParentHash }, "inconsistent"},
		{"stale_fork", func(r, h map[string]any) { r["blockHash"] = evmTestParentHash }, "inconsistent"},
		{"wrong_header", func(r, h map[string]any) { h["hash"] = evmTestParentHash }, "inconsistent"},
		{"wrong_receipt_tx_index", func(r, h map[string]any) { r["transactionIndex"] = "0x1" }, "inconsistent"},
		{"missing_receipt_logs", func(r, h map[string]any) { delete(r, "logs") }, "invalid_response"},
		{"outside_valid_time", func(r, h map[string]any) { h["timestamp"] = "0x6553f000" }, "outside_payment_window"},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt, header := evmTestReceipt(), evmTestHeader()
			receipt["status"] = "0x0"
			receipt["logs"] = []any{}
			test.mutate(receipt, header)
			server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
				switch call.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_getTransactionReceipt":
					return receipt, nil
				case "eth_getBlockByNumber":
					return header, nil
				default:
					return nil, nil
				}
			})
			evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
			err := evmTestEngine().verifyEVMOrder(context.Background(), evmTestOrder(false))
			var rpcError *EVMRPCError
			if !errors.As(err, &rpcError) || rpcError.Kind != test.wantKind {
				t.Fatalf("error=%v want=%s", err, test.wantKind)
			}
		})
	}
}

func evmTestOrder(native bool) model.Order {
	created := time.Unix(1699999900, 0)
	order := model.Order{TradeType: model.UsdtBep20, Status: model.OrderStatusConfirming, RefHash: evmTestHash, RefBlockNum: 100, RefBlockHash: evmTestBlockHash, RefEventIndex: "log:2", RefAmount: "1", Amount: "1", Address: evmTestTo, FromAddress: evmTestFrom, ExpiredAt: created.Add(10 * time.Minute), AutoTimeAt: model.AutoTimeAt{CreatedAt: (*model.Datetime)(&created)}}
	if native {
		order.TradeType = model.BscBnb
		order.RefEventIndex = "native"
	}
	return order
}

func TestEVMRPCVerifyOrderRequiresRealTokenProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		wantOK bool
	}{
		{"valid", func(r, h map[string]any) {}, true},
		{"failed_receipt", func(r, h map[string]any) { r["status"] = "0x0"; r["logs"] = []any{} }, false},
		{"wrong_hash", func(r, h map[string]any) { r["transactionHash"] = evmTestParentHash }, false},
		{"wrong_receipt_block", func(r, h map[string]any) { r["blockHash"] = evmTestParentHash }, false},
		{"wrong_header", func(r, h map[string]any) { h["hash"] = evmTestParentHash }, false},
		{"no_token_logs", func(r, h map[string]any) { r["logs"] = []any{} }, false},
		{"wrong_amount", func(r, h map[string]any) {
			l := evmTestLog()
			l["data"] = fmt.Sprintf("0x%064x", uint64(2000000000000000000))
			r["logs"] = []any{l}
		}, false},
		{"wrong_receiver", func(r, h map[string]any) {
			l := evmTestLog()
			l["topics"] = []string{evmTransferEvent, "0x000000000000000000000000" + evmTestFrom[2:], "0x000000000000000000000000" + evmTestFrom[2:]}
			r["logs"] = []any{l}
		}, false},
		{"wrong_event", func(r, h map[string]any) { l := evmTestLog(); l["logIndex"] = "0x3"; r["logs"] = []any{l} }, false},
		{"duplicate_event", func(r, h map[string]any) { r["logs"] = []any{evmTestLog(), evmTestLog()} }, false},
		{"invalid_timestamp", func(r, h map[string]any) { h["timestamp"] = "0x0" }, false},
		{"outside_payment_window", func(r, h map[string]any) { h["timestamp"] = "0x6553f000" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt, header := evmTestReceipt(), evmTestHeader()
			test.mutate(receipt, header)
			server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
				switch call.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_getTransactionReceipt":
					return receipt, nil
				case "eth_getBlockByNumber":
					return header, nil
				default:
					t.Errorf("unexpected method %s", call.Method)
					return nil, nil
				}
			})
			evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL, model.PaymentMatchMode: string(model.Classic)})
			order := evmTestOrder(false)
			before := order
			err := evmTestEngine().verifyEVMOrder(context.Background(), order)
			if (err == nil) != test.wantOK {
				t.Fatalf("verification error=%v success expected=%v", err, test.wantOK)
			}
			if !reflect.DeepEqual(before, order) {
				t.Fatal("verification changed order")
			}
		})
	}
}

func TestEVMRPCVerifyNativeUsesTransactionValueAndRecipient(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
		wantOK bool
	}{
		{"native_without_erc20_logs", func(tx, r map[string]any) {}, true},
		{"zero_value_with_token_log", func(tx, r map[string]any) { tx["value"] = "0x0"; r["logs"] = []any{evmTestLog()} }, false},
		{"wrong_native_recipient", func(tx, r map[string]any) { tx["to"] = evmTestFrom; r["to"] = evmTestFrom }, false},
		{"contract_call_excluded", func(tx, r map[string]any) { tx["input"] = "0x1234" }, false},
		{"failed_native_receipt", func(tx, r map[string]any) { r["status"] = "0x0" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, receipt, header := evmTestTransaction(), evmTestReceipt(), evmTestHeader()
			tx["value"] = "0xde0b6b3a7640000"
			receipt["logs"] = []any{}
			test.mutate(tx, receipt)
			header["transactions"] = []any{tx}
			server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
				switch call.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_getTransactionReceipt":
					return receipt, nil
				case "eth_getBlockByNumber":
					return header, nil
				default:
					t.Errorf("unexpected method %s", call.Method)
					return nil, nil
				}
			})
			evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
			if err := evmTestEngine().verifyEVMOrder(context.Background(), evmTestOrder(true)); (err == nil) != test.wantOK {
				t.Fatalf("verification error=%v success expected=%v", err, test.wantOK)
			}
		})
	}
}

func TestEVMRPCVerifyNullReceiptDoesNotConfirm(t *testing.T) {
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return "0x38", nil
		}
		return nil, nil
	})
	evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL})
	if err := evmTestEngine().verifyEVMOrder(context.Background(), evmTestOrder(false)); err == nil {
		t.Fatal("null receipt confirmed payment")
	}
}

func TestEVMRPCVerifyLegacyAmountMatchModes(t *testing.T) {
	for _, test := range []struct {
		name   string
		mode   model.MatchMode
		target string
		amount uint64
	}{
		{"prefix", model.HasPrefix, "1.00", 1004000000000000000},
		{"round", model.RoundOff, "1.00", 1004000000000000000},
		{"classic", model.Classic, "1", 1000000000000000000},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
				switch call.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_getBlockByNumber":
					return evmTestHeader(), nil
				case "eth_getTransactionReceipt":
					r, l := evmTestReceipt(), evmTestLog()
					l["data"] = fmt.Sprintf("0x%064x", test.amount)
					r["logs"] = []any{l}
					return r, nil
				default:
					return nil, nil
				}
			})
			evmTestConfig(t, map[model.ConfKey]string{model.RpcEndpointBsc: server.URL, model.PaymentMatchMode: string(test.mode)})
			order := evmTestOrder(false)
			order.RefAmount = ""
			order.RefEventIndex = ""
			order.RefBlockHash = ""
			order.Amount = test.target
			if err := evmTestEngine().verifyEVMOrder(context.Background(), order); err != nil {
				t.Fatalf("legacy %s failed: %v", test.name, err)
			}
			order.RefAmount = test.target
			if test.amount != 1000000000000000000 {
				if err := evmTestEngine().verifyEVMOrder(context.Background(), order); err == nil {
					t.Fatal("exact reference amount must override approximate matching")
				}
			}
		})
	}
}
