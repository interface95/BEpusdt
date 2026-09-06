package task

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/model"
)

type evmRPCTransaction struct {
	Hash        string  `json:"hash"`
	BlockHash   string  `json:"blockHash"`
	BlockNumber string  `json:"blockNumber"`
	Index       string  `json:"transactionIndex"`
	From        string  `json:"from"`
	To          *string `json:"to"`
	Input       string  `json:"input"`
	Value       string  `json:"value"`
}

type evmRPCBlock struct {
	Number       string          `json:"number"`
	Hash         string          `json:"hash"`
	ParentHash   string          `json:"parentHash"`
	Timestamp    string          `json:"timestamp"`
	Transactions json.RawMessage `json:"transactions"`
}

type evmDecodedBlock struct {
	number       int64
	hash         string
	parentHash   string
	at           time.Time
	transactions []evmRPCTransaction
	indexes      map[string]int
}

type evmRPCLog struct {
	Address          string   `json:"address"`
	Topics           []string `json:"topics"`
	Data             string   `json:"data"`
	TransactionHash  string   `json:"transactionHash"`
	BlockHash        string   `json:"blockHash"`
	BlockNumber      string   `json:"blockNumber"`
	TransactionIndex string   `json:"transactionIndex"`
	LogIndex         string   `json:"logIndex"`
	Removed          bool     `json:"removed"`
}

type evmRPCReceipt struct {
	TransactionHash  string          `json:"transactionHash"`
	BlockHash        string          `json:"blockHash"`
	BlockNumber      string          `json:"blockNumber"`
	TransactionIndex string          `json:"transactionIndex"`
	Status           string          `json:"status"`
	From             string          `json:"from"`
	To               *string         `json:"to"`
	Logs             json.RawMessage `json:"logs"`
}

type evmToken struct {
	trade    model.TradeType
	decimals int32
}

func (e *evm) evmTokens() map[string]evmToken {
	tokens := make(map[string]evmToken)
	for trade, config := range model.GetAllTradeConfig() {
		if string(config.Network) == e.Network && !config.Native && evmData(config.Contract, 20) {
			tokens[strings.ToLower(config.Contract)] = evmToken{trade: model.TradeType(trade), decimals: config.Decimal}
		}
	}
	return tokens
}

func evmData(value string, size int) bool {
	if !strings.HasPrefix(value, "0x") || len(value)%2 != 0 || (size >= 0 && len(value) != 2+size*2) {
		return false
	}
	_, err := hex.DecodeString(value[2:])
	return err == nil
}

func evmHash(value string) bool {
	return evmData(value, 32) && value != "0x"+strings.Repeat("0", 64)
}

func evmQuantity(value string) (*big.Int, error) {
	if !strings.HasPrefix(value, "0x") || len(value) < 3 || len(value) > 66 || (len(value) > 3 && value[2] == '0') {
		return nil, evmError("decode", "invalid_response")
	}
	for _, ch := range value[2:] {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return nil, evmError("decode", "invalid_response")
		}
	}
	parsed, ok := new(big.Int).SetString(value[2:], 16)
	if !ok {
		return nil, evmError("decode", "invalid_response")
	}
	return parsed, nil
}

func evmHeight(value string) (int64, error) {
	parsed, err := evmQuantity(value)
	if err != nil || !parsed.IsInt64() {
		return 0, evmError("decode", "invalid_response")
	}
	return parsed.Int64(), nil
}

func evmDecodeArray[T any](raw json.RawMessage) ([]T, error) {
	data := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(data, "[") {
		return nil, evmError("decode", "invalid_response")
	}
	var result []T
	if json.Unmarshal(raw, &result) != nil {
		return nil, evmError("decode", "invalid_response")
	}
	return result, nil
}

func evmDecodeBlock(raw json.RawMessage, height int64, full bool) (evmDecodedBlock, error) {
	var parsed evmRPCBlock
	if json.Unmarshal(raw, &parsed) != nil {
		return evmDecodedBlock{}, evmError("eth_getBlockByNumber", "invalid_response")
	}
	number, err := evmHeight(parsed.Number)
	if err != nil || number != height || !evmHash(parsed.Hash) || !evmData(parsed.ParentHash, 32) || (height > 0 && !evmHash(parsed.ParentHash)) {
		return evmDecodedBlock{}, evmError("eth_getBlockByNumber", "inconsistent")
	}
	timestamp, err := evmHeight(parsed.Timestamp)
	if err != nil || (height > 0 && timestamp <= 0) || timestamp > time.Now().Add(10*time.Minute).Unix() {
		return evmDecodedBlock{}, evmError("eth_getBlockByNumber", "invalid_response")
	}
	block := evmDecodedBlock{number: number, hash: strings.ToLower(parsed.Hash), parentHash: strings.ToLower(parsed.ParentHash), at: time.Unix(timestamp, 0), indexes: make(map[string]int)}
	if !full {
		return block, nil
	}
	block.transactions, err = evmDecodeArray[evmRPCTransaction](parsed.Transactions)
	if err != nil {
		return evmDecodedBlock{}, err
	}
	for i, tx := range block.transactions {
		if err := evmValidateTransaction(tx, block, i); err != nil {
			return evmDecodedBlock{}, err
		}
		key := strings.ToLower(tx.Hash)
		if _, duplicate := block.indexes[key]; duplicate {
			return evmDecodedBlock{}, evmError("eth_getBlockByNumber", "inconsistent")
		}
		block.indexes[key] = i
	}
	return block, nil
}

func evmValidateTransaction(tx evmRPCTransaction, block evmDecodedBlock, index int) error {
	number, err := evmHeight(tx.BlockNumber)
	if err != nil || number != block.number || !evmHash(tx.Hash) || !strings.EqualFold(tx.BlockHash, block.hash) {
		return evmError("transaction", "inconsistent")
	}
	position, err := evmHeight(tx.Index)
	if err != nil || position != int64(index) || !evmData(tx.From, 20) || (tx.To != nil && !evmData(*tx.To, 20)) || !evmData(tx.Input, -1) {
		return evmError("transaction", "invalid_response")
	}
	if _, err := evmQuantity(tx.Value); err != nil {
		return err
	}
	return nil
}

func (e *evm) evmReadBlocks(ctx context.Context, b evmBlock) ([]evmDecodedBlock, error) {
	calls := make([]evmRPCRequest, 0, b.To-b.From+1)
	for height := b.From; height <= b.To; height++ {
		calls = append(calls, evmRequest("eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", height), true}, height))
	}
	results, err := e.evmRPC(ctx, false, calls)
	if err != nil {
		return nil, err
	}
	blocks := make([]evmDecodedBlock, 0, len(results))
	for i, raw := range results {
		block, err := evmDecodeBlock(raw, b.From+int64(i), true)
		if err != nil {
			return nil, err
		}
		if i > 0 && (block.parentHash != blocks[i-1].hash || block.at.Before(blocks[i-1].at)) {
			return nil, evmError("eth_getBlockByNumber", "inconsistent")
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// A range succeeds only after every block and every returned event is validated.
// It returns data, never queues transfers or changes order state.
func (e *evm) fetchEVMRange(ctx context.Context, b evmBlock) ([]transfer, error) {
	if b.From < 0 || b.To < b.From || b.To-b.From >= blockParseMaxNum || b.To == int64(^uint64(0)>>1) {
		return nil, evmError("range", "invalid_request")
	}
	ctx, cancel := context.WithTimeout(ctx, evmRangeBudget)
	defer cancel()
	blocks, err := e.evmReadBlocks(ctx, b)
	if err != nil {
		return nil, err
	}
	tokens := e.evmTokens()
	var logs []evmRPCLog
	if len(tokens) > 0 {
		logs, err = e.evmRangeLogs(ctx, b, tokens)
		if err != nil {
			var rpcError *EVMRPCError
			if !errors.As(err, &rpcError) || rpcError.Kind != "restricted" {
				return nil, err
			}
			logs, err = e.evmReceiptLogs(ctx, blocks)
			if err != nil {
				return nil, err
			}
		}
	}
	byHeight := make(map[int64]evmDecodedBlock, len(blocks))
	for _, block := range blocks {
		byHeight[block.number] = block
	}
	transfers := make([]transfer, 0, len(logs))
	seen := make(map[string]bool, len(logs))
	for _, event := range logs {
		height, err := evmHeight(event.BlockNumber)
		if err != nil {
			return nil, err
		}
		block, exists := byHeight[height]
		if !exists {
			return nil, evmError("eth_getLogs", "inconsistent")
		}
		item, eligible, err := e.evmLogTransfer(event, block, tokens)
		if err != nil {
			return nil, err
		}
		index, err := evmHeight(event.LogIndex)
		if err != nil {
			return nil, err
		}
		key := block.hash + ":" + strconv.FormatInt(index, 10)
		if seen[key] {
			return nil, evmError("eth_getLogs", "inconsistent")
		}
		seen[key] = true
		if eligible {
			transfers = append(transfers, item)
		}
	}
	if e.Native.Parse {
		for _, block := range blocks {
			for _, tx := range block.transactions {
				if tx.To == nil || tx.Input != "0x" {
					continue
				}
				value, err := evmQuantity(tx.Value)
				if err != nil {
					return nil, err
				}
				if value.Sign() == 0 {
					continue
				}
				transfers = append(transfers, transfer{Network: e.Network, TxHash: strings.ToLower(tx.Hash), Amount: decimal.NewFromBigInt(value, e.Native.Decimal), FromAddress: strings.ToLower(tx.From), RecvAddress: strings.ToLower(*tx.To), Timestamp: block.at, TradeType: e.Native.TradeType, BlockNum: int(block.number), EventIndex: "native", BlockHash: block.hash})
			}
		}
	}
	return transfers, nil
}

func (e *evm) evmRangeLogs(ctx context.Context, b evmBlock, tokens map[string]evmToken) ([]evmRPCLog, error) {
	addresses := make([]string, 0, len(tokens))
	for address := range tokens {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	filter := map[string]any{"fromBlock": fmt.Sprintf("0x%x", b.From), "toBlock": fmt.Sprintf("0x%x", b.To), "address": addresses, "topics": []string{evmTransferEvent}}
	results, err := e.evmRPC(ctx, true, []evmRPCRequest{evmRequest("eth_getLogs", []any{filter}, 1)})
	if err != nil {
		return nil, err
	}
	logs, err := evmDecodeArray[evmRPCLog](results[0])
	if err != nil {
		return nil, err
	}
	for _, event := range logs {
		if _, supported := tokens[strings.ToLower(event.Address)]; !supported {
			return nil, evmError("eth_getLogs", "inconsistent")
		}
		if len(event.Topics) == 0 || !strings.EqualFold(event.Topics[0], evmTransferEvent) {
			return nil, evmError("eth_getLogs", "inconsistent")
		}
	}
	return logs, nil
}

func evmValidateLog(event evmRPCLog, block evmDecodedBlock) error {
	height, err := evmHeight(event.BlockNumber)
	if err != nil || height != block.number || !strings.EqualFold(event.BlockHash, block.hash) || event.Removed {
		return evmError("log", "inconsistent")
	}
	index, err := evmHeight(event.TransactionIndex)
	if err != nil || index < 0 || index >= int64(len(block.transactions)) || !strings.EqualFold(block.transactions[index].Hash, event.TransactionHash) {
		return evmError("log", "inconsistent")
	}
	if _, err := evmHeight(event.LogIndex); err != nil {
		return err
	}
	if !evmData(event.Address, 20) || !evmData(event.Data, -1) || len(event.Topics) > 4 {
		return evmError("log", "invalid_response")
	}
	for _, topic := range event.Topics {
		if !evmData(topic, 32) {
			return evmError("log", "invalid_response")
		}
	}
	return nil
}

func (e *evm) evmLogTransfer(event evmRPCLog, block evmDecodedBlock, tokens map[string]evmToken) (transfer, bool, error) {
	if err := evmValidateLog(event, block); err != nil {
		return transfer{}, false, err
	}
	token, supported := tokens[strings.ToLower(event.Address)]
	if !supported || len(event.Topics) == 0 || !strings.EqualFold(event.Topics[0], evmTransferEvent) {
		return transfer{}, false, nil
	}
	if len(event.Topics) != 3 || !evmData(event.Data, 32) || event.Topics[1][2:26] != strings.Repeat("0", 24) || event.Topics[2][2:26] != strings.Repeat("0", 24) {
		return transfer{}, false, evmError("log", "invalid_response")
	}
	amount, ok := new(big.Int).SetString(event.Data[2:], 16)
	if !ok {
		return transfer{}, false, evmError("log", "invalid_response")
	}
	if amount.Sign() == 0 {
		return transfer{}, false, nil
	}
	index, err := evmHeight(event.LogIndex)
	if err != nil {
		return transfer{}, false, err
	}
	return transfer{Network: e.Network, TxHash: strings.ToLower(event.TransactionHash), Amount: decimal.NewFromBigInt(amount, token.decimals), FromAddress: "0x" + strings.ToLower(event.Topics[1][26:]), RecvAddress: "0x" + strings.ToLower(event.Topics[2][26:]), Timestamp: block.at, TradeType: token.trade, BlockNum: int(block.number), EventIndex: "log:" + strconv.FormatInt(index, 10), BlockHash: block.hash}, true, nil
}

func evmValidateReceipt(receipt evmRPCReceipt, block evmDecodedBlock) ([]evmRPCLog, error) {
	height, err := evmHeight(receipt.BlockNumber)
	if err != nil || height != block.number || !strings.EqualFold(receipt.BlockHash, block.hash) {
		return nil, evmError("receipt", "inconsistent")
	}
	index, err := evmHeight(receipt.TransactionIndex)
	if err != nil || index < 0 || index >= int64(len(block.transactions)) {
		return nil, evmError("receipt", "inconsistent")
	}
	tx := block.transactions[index]
	if !strings.EqualFold(receipt.TransactionHash, tx.Hash) || !strings.EqualFold(receipt.From, tx.From) || (receipt.To == nil) != (tx.To == nil) || (receipt.To != nil && !strings.EqualFold(*receipt.To, *tx.To)) {
		return nil, evmError("receipt", "inconsistent")
	}
	if receipt.Status != "0x1" && receipt.Status != "0x0" {
		return nil, evmError("receipt", "invalid_response")
	}
	logs, err := evmDecodeArray[evmRPCLog](receipt.Logs)
	if err != nil {
		return nil, err
	}
	if receipt.Status == "0x0" && len(logs) != 0 {
		return nil, evmError("receipt", "inconsistent")
	}
	seen := make(map[string]bool, len(logs))
	for _, event := range logs {
		if !strings.EqualFold(event.TransactionHash, receipt.TransactionHash) || !strings.EqualFold(event.TransactionIndex, receipt.TransactionIndex) {
			return nil, evmError("receipt", "inconsistent")
		}
		if err := evmValidateLog(event, block); err != nil {
			return nil, err
		}
		index, err := evmHeight(event.LogIndex)
		if err != nil {
			return nil, err
		}
		key := strconv.FormatInt(index, 10)
		if seen[key] {
			return nil, evmError("receipt", "inconsistent")
		}
		seen[key] = true
	}
	return logs, nil
}

func (e *evm) evmReceiptLogs(ctx context.Context, blocks []evmDecodedBlock) ([]evmRPCLog, error) {
	if len(blocks) == 0 || len(blocks) > blockParseMaxNum {
		return nil, evmError("eth_getBlockReceipts", "invalid_request")
	}
	calls := make([]evmRPCRequest, 0, len(blocks))
	for _, block := range blocks {
		calls = append(calls, evmRequest("eth_getBlockReceipts", []any{fmt.Sprintf("0x%x", block.number)}, block.number))
	}
	results, err := e.evmRPC(ctx, false, calls)
	if err != nil {
		return nil, err
	}
	var logs []evmRPCLog
	for i, raw := range results {
		receipts, err := evmDecodeArray[evmRPCReceipt](raw)
		if err != nil {
			return nil, err
		}
		block := blocks[i]
		if len(receipts) != len(block.transactions) {
			return nil, evmError("eth_getBlockReceipts", "incomplete_block")
		}
		seen := make(map[string]bool, len(receipts))
		for _, receipt := range receipts {
			key := strings.ToLower(receipt.TransactionHash)
			if seen[key] {
				return nil, evmError("eth_getBlockReceipts", "incomplete_block")
			}
			seen[key] = true
			events, err := evmValidateReceipt(receipt, block)
			if err != nil {
				return nil, err
			}
			logs = append(logs, events...)
		}
	}
	return logs, nil
}
