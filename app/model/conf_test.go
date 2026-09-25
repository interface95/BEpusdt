package model

import "testing"

func TestDefaultEthereumRPCSupportsFullTransactionScanning(t *testing.T) {
	if got := defaultConf[RpcEndpointEthereum]; got != "https://ethereum-rpc.publicnode.com" {
		t.Fatalf("Ethereum RPC default = %q, want PublicNode", got)
	}
}
