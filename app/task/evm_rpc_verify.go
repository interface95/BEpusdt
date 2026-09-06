package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/model"
)

// verifyEVMOrder proves an observed payment without modifying its database row.
// Null, failed, stale-fork or incomplete responses must never become payment ACKs.
func (e *evm) verifyEVMOrder(ctx context.Context, o model.Order) error {
	config, supported := model.GetAllTradeConfig()[string(o.TradeType)]
	if !supported || string(config.Network) != e.Network || !evmHash(o.RefHash) || o.RefBlockNum <= 0 || o.CreatedAt == nil || o.ExpiredAt.IsZero() {
		return evmError("verify", "invalid_order")
	}
	if o.RefBlockHash != "" && !evmHash(o.RefBlockHash) {
		return evmError("verify", "invalid_order")
	}
	if !evmData(orderMatchAddress(o), 20) || (o.FromAddress != "" && !evmData(o.FromAddress, 20)) {
		return evmError("verify", "invalid_order")
	}
	ctx, cancel := context.WithTimeout(ctx, evmReadBudget)
	defer cancel()
	results, err := e.evmRPC(ctx, false, []evmRPCRequest{evmRequest("eth_getTransactionReceipt", []any{o.RefHash}, 1)})
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(results[0])) == "null" {
		return evmError("eth_getTransactionReceipt", "pending")
	}
	var receipt evmRPCReceipt
	if json.Unmarshal(results[0], &receipt) != nil {
		return evmError("eth_getTransactionReceipt", "invalid_response")
	}
	if !strings.EqualFold(receipt.TransactionHash, o.RefHash) {
		return evmError("verify", "inconsistent")
	}
	if receipt.Status != "0x1" && receipt.Status != "0x0" {
		return evmError("verify", "invalid_response")
	}
	height, err := evmHeight(receipt.BlockNumber)
	if err != nil || height != int64(o.RefBlockNum) {
		return evmError("verify", "inconsistent")
	}
	if o.RefBlockHash != "" && !strings.EqualFold(o.RefBlockHash, receipt.BlockHash) {
		return evmError("verify", "inconsistent")
	}
	blocks, err := e.evmReadBlocks(ctx, evmBlock{From: height, To: height})
	if err != nil {
		return err
	}
	block := blocks[0]
	logs, err := evmValidateReceipt(receipt, block)
	if err != nil {
		return err
	}
	if !o.CreatedAt.Before(block.at) || !o.ExpiredAt.After(block.at) {
		return evmError("verify", "outside_payment_window")
	}
	if model.GetC(model.BlockOffsetConfirm) == "1" && e.Block.ConfirmedOffset > 0 {
		head, err := e.evmHead(ctx)
		if err != nil {
			return err
		}
		if head < height || head-height < int64(e.Block.ConfirmedOffset) {
			return evmError("verify", "pending")
		}
	}
	if receipt.Status == "0x0" {
		return evmError("verify", "failed_transaction")
	}
	if config.Native {
		return e.evmVerifyNative(o, receipt, block, config.Decimal)
	}
	tokens := e.evmTokens()
	matches := 0
	for _, event := range logs {
		item, eligible, err := e.evmLogTransfer(event, block, tokens)
		if err != nil {
			return err
		}
		if !eligible || item.TradeType != o.TradeType || !strings.EqualFold(item.RecvAddress, orderMatchAddress(o)) {
			continue
		}
		if o.FromAddress != "" && !strings.EqualFold(o.FromAddress, item.FromAddress) {
			continue
		}
		if o.RefEventIndex != "" && o.RefEventIndex != item.EventIndex {
			continue
		}
		matched, err := evmOrderAmountMatches(o, item.Amount)
		if err != nil {
			return err
		}
		if matched {
			matches++
		}
	}
	if matches != 1 {
		return evmError("verify", "payment_proof_missing_or_ambiguous")
	}
	return nil
}

func (e *evm) evmVerifyNative(o model.Order, receipt evmRPCReceipt, block evmDecodedBlock, decimals int32) error {
	if o.RefEventIndex != "" && o.RefEventIndex != "native" {
		return evmError("verify", "invalid_order")
	}
	index, err := evmHeight(receipt.TransactionIndex)
	if err != nil || index < 0 || index >= int64(len(block.transactions)) {
		return evmError("verify", "inconsistent")
	}
	tx := block.transactions[index]
	if tx.To == nil || tx.Input != "0x" || !strings.EqualFold(*tx.To, orderMatchAddress(o)) || (o.FromAddress != "" && !strings.EqualFold(o.FromAddress, tx.From)) {
		return evmError("verify", "payment_proof_missing_or_ambiguous")
	}
	value, err := evmQuantity(tx.Value)
	if err != nil || value.Sign() <= 0 {
		return evmError("verify", "payment_proof_missing_or_ambiguous")
	}
	matched, err := evmOrderAmountMatches(o, decimal.NewFromBigInt(value, decimals))
	if err != nil {
		return err
	}
	if !matched {
		return evmError("verify", "payment_proof_missing_or_ambiguous")
	}
	return nil
}

func evmOrderAmountMatches(o model.Order, amount decimal.Decimal) (bool, error) {
	if o.RefAmount != "" {
		expected, err := decimal.NewFromString(o.RefAmount)
		if err != nil || expected.Sign() <= 0 {
			return false, evmError("verify", "invalid_order")
		}
		return amount.Equal(expected), nil
	}
	// Legacy rows lack an exact chain amount; preserve the configured match mode.
	if _, err := decimal.NewFromString(o.Amount); err != nil {
		return false, evmError("verify", "invalid_order")
	}
	if !model.IsSupportedTradeType(o.TradeType) {
		return false, evmError("verify", "invalid_order")
	}
	return amountMatch(amount, o.Amount, fmt.Sprint(o.TradeType)), nil
}
