package task

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
)

type ethereumLogTransport func(*http.Request) (*http.Response, error)

func (f ethereumLogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestEthereumEventScanFiltersContracts(t *testing.T) {
	for _, network := range []string{conf.Ethereum, conf.Polygon} {
		t.Run(network, func(t *testing.T) {
			client := &http.Client{Transport: ethereumLogTransport(func(r *http.Request) (*http.Response, error) {
				var call struct {
					Method string `json:"method"`
					Params []struct {
						FromBlock string   `json:"fromBlock"`
						ToBlock   string   `json:"toBlock"`
						Topics    []string `json:"topics"`
						Address   []string `json:"address"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					t.Error(err)
					return nil, err
				}
				if call.Method != "eth_getLogs" || len(call.Params) != 1 {
					t.Fatalf("unexpected call: %+v", call)
				}
				filter := call.Params[0]
				if filter.FromBlock != "0x64" || filter.ToBlock != "0x65" || !reflect.DeepEqual(filter.Topics, []string{evmTransferEvent}) {
					t.Errorf("scan range/topics changed: %+v", filter)
				}
				var addresses []string
				if network == conf.Ethereum {
					addresses = []string{conf.UsdtErc20, conf.UsdcErc20}
				}
				if !reflect.DeepEqual(filter.Address, addresses) {
					t.Errorf("address filter = %v, want %v", filter.Address, addresses)
				}
				logs := make([]map[string]any, 0, len(addresses))
				for _, address := range addresses {
					logs = append(logs, map[string]any{
						"address": address, "blockNumber": "0x64", "transactionHash": evmTestHash,
						"data": "0x0f4240", "topics": []string{evmTransferEvent,
							"0x" + strings.Repeat("0", 24) + evmTestFrom[2:],
							"0x" + strings.Repeat("0", 24) + evmTestTo[2:]},
					})
				}
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": logs})
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}
			e := evm{Network: network, Client: client}
			timestamp := time.Unix(1000, 0)
			transfers, err := e.parseEventTransfer(evmBlock{From: 100, To: 101}, map[string]time.Time{"0x64": timestamp})
			if err != nil {
				t.Fatal(err)
			}
			if network != conf.Ethereum {
				if len(transfers) != 0 {
					t.Fatal("unexpected transfers")
				}
				return
			}
			if len(transfers) != 2 {
				t.Fatalf("transfers = %d, want USDT and USDC", len(transfers))
			}
			for i, trade := range []model.TradeType{model.UsdtErc20, model.UsdcErc20} {
				got := transfers[i]
				if got.TradeType != trade || got.Amount.String() != "1" || got.Timestamp != timestamp || got.RecvAddress != evmTestTo {
					t.Errorf("incorrect token transfer: %+v", got)
				}
			}
		})
	}
}
