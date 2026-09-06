package task

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const scanTaskBudget = 90 * time.Second

func evmOrderWindow(orders []model.Order, snapshot time.Time) (int64, int64) {
	var start, end int64
	for _, o := range orders {
		if o.CreatedAt == nil {
			continue
		}
		created := o.CreatedAt.Time().Unix()
		if start == 0 || created < start {
			start = created
		}
		until := min(o.ExpiredAt.Unix(), snapshot.Unix())
		end = max(end, until)
	}
	return start, end
}

func (e *evm) discoverScanOrders(ctx context.Context) {
	var orders []model.Order
	existing := model.Db.Model(&model.EVMScanJob{}).Select("order_id").Where("network = ? AND kind = ?", e.Network, "order")
	err := model.Db.WithContext(ctx).Where("trade_type IN ? AND status IN ?", model.GetNetworkTrades(model.Network(e.Network)), []int{model.OrderStatusWaiting, model.OrderStatusExpired}).
		Where("expired_at > ? AND id NOT IN (?)", time.Now().Add(model.GetLookbackHour()), existing).
		Order("id").Limit(100).Find(&orders).Error
	if err != nil {
		log.Task.Warn("discover BSC scan jobs: ", err)
		return
	}
	for _, o := range orders {
		start, end := evmOrderWindow([]model.Order{o}, time.Now())
		job := model.EVMScanJob{Network: e.Network, Kind: "order", OrderID: o.ID, StartAt: start, EndAt: end, NextRetryAt: time.Now()}
		if err := model.Db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
			log.Task.Warn("persist BSC scan job: ", err)
			return
		}
	}
}

func (e *evm) syncDurableHead(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Existing unfinished jobs remain eligible even after the lookback age expires.
	var unfinished int64
	if err := model.Db.WithContext(ctx).Model(&model.EVMScanJob{}).Where("network = ? AND completed = ?", e.Network, false).Count(&unfinished).Error; err != nil {
		return
	}
	var confirming int64
	if err := model.Db.WithContext(ctx).Model(&model.Order{}).Where("trade_type IN ? AND status = ?", model.GetNetworkTrades(model.Network(e.Network)), model.OrderStatusConfirming).Count(&confirming).Error; err != nil {
		return
	}
	if unfinished == 0 && confirming == 0 && syncBreak(e.Network, 0) {
		return
	}
	head, err := e.evmHead(ctx)
	if err != nil {
		log.Task.Warn("BSC head read failed: ", err)
		return
	}
	chainBlockNum.Store(e.Network, head)
	var watched int64
	if err := model.Db.WithContext(ctx).Model(&model.Wallet{}).Where("other_notify = ? AND trade_type IN ?", model.WaOtherEnable, model.GetNetworkTrades(model.Network(e.Network))).Count(&watched).Error; err != nil {
		return
	}
	if watched == 0 && !mqttSubscribed(e.Network) && !hasLookbackOrders(model.GetNetworkTrades(model.Network(e.Network))) {
		return
	}
	job := model.EVMScanJob{Network: e.Network, Kind: "live", NextBlock: max(1, head-e.Block.RollDelayOffset), TargetBlock: head - e.Block.RollDelayOffset, NextRetryAt: time.Now()}
	if err := model.Db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
		log.Task.Warn("persist live BSC cursor: ", err)
		return
	}
	if err := e.promoteLiveWindow(model.Db.WithContext(ctx), head-e.Block.RollDelayOffset); err != nil {
		log.Task.Warn("persist BSC historical gap: ", err)
	}
}

func (e *evm) dispatchDurableScans(ctx context.Context) {
	// A historical outage cannot occupy the live or confirmation worker.
	done := make(chan struct{})
	go func() { defer close(done); e.scanWorker(ctx, "live") }()
	e.scanWorker(ctx, "order")
	<-done
}

func (e *evm) scanWorker(ctx context.Context, kind string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if kind == "live" && syncBreak(e.Network, 0) {
			continue
		}
		var job model.EVMScanJob
		kinds := []string{kind}
		if kind == "order" {
			kinds = append(kinds, "recovery")
		}
		res := model.Db.WithContext(ctx).Where("network = ? AND kind IN ? AND completed = ? AND next_retry_at <= ?", e.Network, kinds, false, time.Now()).Order("next_retry_at, id").Limit(1).Find(&job)
		if res.Error != nil {
			log.Task.Warn("read BSC scan jobs: ", res.Error)
			continue
		}
		if res.RowsAffected == 0 {
			continue
		}
		jobCtx, cancel := context.WithTimeout(ctx, scanTaskBudget)
		err := e.processScanJob(jobCtx, &job)
		cancel()
		if err != nil && ctx.Err() == nil {
			e.deferScanJob(job, err)
		}
	}
}

func (e *evm) deferScanJob(job model.EVMScanJob, err error) {
	delay := min(300, 1<<min(job.Attempts+1, 8))
	var retry interface{ RetryDelay() time.Duration }
	if errors.As(err, &retry) {
		delay = max(delay, min(86400, int(retry.RetryDelay().Seconds())))
	}
	// Keep failures durable, with capped retries per minute rather than dropping jobs.
	next := time.Now().Add(time.Duration(delay)*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond)
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	res := model.Db.Model(&model.EVMScanJob{}).Where("id = ? AND next_block = ? AND target_block = ?", job.ID, job.NextBlock, job.TargetBlock).Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "next_retry_at": next, "last_error": msg})
	if res.Error != nil {
		log.Task.Warn("persist BSC retry: ", res.Error)
	}
	if res.Error == nil && res.RowsAffected == 0 {
		return
	}
	conf.RecordFailure(e.Network)
	log.Task.Warn(fmt.Sprintf("BSC scan job %d retry in %ds: %s", job.ID, delay, msg))
}

func (e *evm) processScanJob(ctx context.Context, job *model.EVMScanJob) error {
	var owner model.Order
	if job.Kind == "order" {
		res := model.Db.WithContext(ctx).First(&owner, job.OrderID)
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return model.Db.Model(job).Update("completed", true).Error
		}
		if res.Error != nil {
			return res.Error
		}
		if owner.Status != model.OrderStatusWaiting && owner.Status != model.OrderStatusExpired {
			return model.Db.Model(job).Update("completed", true).Error
		}
	}
	if job.Kind != "recovery" && (job.NextBlock == 0 || job.NextBlock > job.TargetBlock) {
		oldNext, oldTarget := job.NextBlock, job.TargetBlock
		resolved := *job
		head, err := e.evmHead(ctx)
		if err != nil {
			return err
		}
		head -= e.Block.RollDelayOffset
		if head <= 0 {
			return errors.New("chain head unavailable")
		}
		chainBlockNum.Store(e.Network, head)
		if job.Kind == "live" {
			resolved.TargetBlock = head
		} else {
			tip, err := e.evmBlockTime(ctx, head)
			if err != nil {
				return err
			}
			end := min(owner.ExpiredAt.Unix(), time.Now().Unix(), tip.Unix())
			if end < job.StartAt {
				return errors.New("chain head is behind order creation")
			}
			resolved.EndAt = end
			if job.NextBlock == 0 {
				start, err := e.heightAtTime(ctx, job.StartAt, head)
				if err != nil {
					return err
				}
				resolved.NextBlock = max(1, start)
			}
			target, err := e.heightAtTime(ctx, end, head)
			if err != nil {
				return err
			}
			resolved.TargetBlock = target
		}
		res := model.Db.WithContext(ctx).Model(&resolved).Where("next_block = ? AND target_block = ?", oldNext, oldTarget).Updates(map[string]any{"next_block": resolved.NextBlock, "target_block": resolved.TargetBlock, "end_at": resolved.EndAt})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("scan cursor changed while resolving range")
		}
		*job = resolved
	}
	if job.NextBlock > job.TargetBlock {
		completed := job.Kind == "recovery" || (job.Kind == "order" && job.EndAt >= owner.ExpiredAt.Unix())
		return model.Db.WithContext(ctx).Model(job).Where("target_block = ? AND next_block = ?", job.TargetBlock, job.NextBlock).Updates(map[string]any{"completed": completed, "next_retry_at": time.Now().Add(3 * time.Second), "attempts": 0, "last_error": ""}).Error
	}
	b := evmBlock{From: job.NextBlock, To: min(job.TargetBlock, job.NextBlock+blockParseMaxNum-1)}
	transfers, err := e.fetchEVMRange(ctx, b)
	if err != nil {
		return err
	}
	if err := e.commitScanRange(model.Db.WithContext(ctx), *job, b, transfers); err != nil {
		return err
	}
	conf.RecordSuccess(e.Network, fmt.Sprint(b.To))
	// Non-order notifications retain their existing best-effort delivery semantics.
	for _, tr := range transfers {
		mqttPublish(tr)
	}
	if job.Kind == "live" && len(transfers) > 0 {
		var paid []model.Order
		if err := model.Db.Select("ref_hash", "ref_event_index").Where("trade_type IN ? AND ref_block_num BETWEEN ? AND ? AND status IN ?", model.GetNetworkTrades(model.Network(e.Network)), b.From, b.To, []int{model.OrderStatusConfirming, model.OrderStatusSuccess}).Find(&paid).Error; err != nil {
			log.Task.Warn("read non-order notification exclusions: ", err)
			return nil
		}
		claimed := make(map[string]bool, len(paid))
		for _, o := range paid {
			claimed[o.RefHash+"/"+o.RefEventIndex] = true
		}
		other := make([]transfer, 0, len(transfers))
		for _, tr := range transfers {
			if !claimed[tr.TxHash+"/"] && !claimed[tr.TxHash+"/"+tr.EventIndex] {
				other = append(other, tr)
			}
		}
		select {
		case notOrderQueue.In <- other:
		case <-ctx.Done():
		}
	}
	return nil
}

func (e *evm) heightAtTime(ctx context.Context, target, head int64) (int64, error) {
	hi := head
	at, err := e.evmBlockTime(ctx, hi)
	if err != nil {
		return 0, err
	}
	if at.Unix() <= target {
		return hi, nil
	}
	lo := hi
	for step := int64(1); ; step *= 2 {
		lo = max(1, head-step)
		at, err = e.evmBlockTime(ctx, lo)
		if err != nil {
			return 0, err
		}
		if at.Unix() <= target || lo == 1 {
			break
		}
		hi = lo
	}
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		at, err = e.evmBlockTime(ctx, mid)
		if err != nil {
			return 0, err
		}
		if at.Unix() <= target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

func (e *evm) commitScanRange(db *gorm.DB, job model.EVMScanJob, b evmBlock, transfers []transfer) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var current model.EVMScanJob
		if err := tx.First(&current, job.ID).Error; err != nil {
			return err
		}
		if current.NextBlock > b.To {
			return nil
		}
		if current.NextBlock != b.From {
			return errors.New("scan cursor changed")
		}
		byRecipient := make(map[string][]transfer)
		recipients := make(map[string]bool)
		var earliest, latest time.Time
		for _, tr := range transfers {
			if tr.EventIndex == "" {
				return errors.New("transfer event identity missing")
			}
			if !model.IsAmountValid(tr.TradeType, tr.Amount) {
				continue
			}
			address := strings.ToLower(tr.RecvAddress)
			recipients[address] = true
			key := address + "/" + string(tr.TradeType)
			byRecipient[key] = append(byRecipient[key], tr)
			if earliest.IsZero() || tr.Timestamp.Before(earliest) {
				earliest = tr.Timestamp
			}
			if tr.Timestamp.After(latest) {
				latest = tr.Timestamp
			}
		}
		addresses := make([]string, 0, len(recipients))
		for address := range recipients {
			addresses = append(addresses, address)
		}
		sort.Strings(addresses)
		used := make(map[string]bool)
		// Keyset pagination stays correct while matching removes rows from eligibility.
		for offset := 0; offset < len(addresses); offset += 64 {
			var after int64
			for {
				var orders []model.Order
				pinned := tx.Model(&model.EVMScanJob{}).Select("order_id").Where("network = ? AND kind = ? AND completed = ?", e.Network, "order", false)
				if err := tx.Where("id > ? AND trade_type IN ? AND status IN ?", after, model.GetNetworkTrades(model.Network(e.Network)), []int{model.OrderStatusWaiting, model.OrderStatusExpired}).
					Where("expired_at > ? OR id IN (?)", time.Now().Add(model.GetLookbackHour()), pinned).
					Where("created_at < ? AND expired_at > ?", latest, earliest).
					Where("LOWER(COALESCE(NULLIF(match_address, ''), address)) IN ?", addresses[offset:min(offset+64, len(addresses))]).Order("id").Limit(128).Find(&orders).Error; err != nil {
					return err
				}
				if len(orders) == 0 {
					break
				}
				for i := range orders {
					o := &orders[i]
					after = o.ID
					for _, tr := range byRecipient[strings.ToLower(orderMatchAddress(*o))+"/"+string(o.TradeType)] {
						identity := tr.TxHash + "/" + tr.EventIndex
						if used[identity] || !evmScanMatch(*o, tr) {
							continue
						}
						claimed, err := e.claimScanTransfer(tx, o, tr)
						if err != nil {
							return err
						}
						used[identity] = true
						if claimed {
							break
						}
					}
				}
			}
		}
		res := tx.Model(&model.EVMScanJob{}).Where("id = ? AND next_block = ?", job.ID, b.From).Updates(map[string]any{"next_block": b.To + 1, "attempts": 0, "last_error": "", "next_retry_at": time.Now().Add(time.Second)})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("scan commit lost cursor ownership")
		}
		return nil
	})
}

func (e *evm) claimScanTransfer(tx *gorm.DB, o *model.Order, tr transfer) (bool, error) {
	var legacy []model.Order
	if err := tx.Select("ref_event_index").Where("trade_type IN ? AND ref_hash = ? AND id <> ? AND (ref_event_index = ? OR ref_event_index = ?)", model.GetNetworkTrades(model.Network(e.Network)), tr.TxHash, o.ID, "", tr.EventIndex).Limit(2).Find(&legacy).Error; err != nil {
		return false, err
	}
	for _, previous := range legacy {
		if previous.RefEventIndex == "" {
			return false, errors.New("legacy payment event ambiguous; review existing transaction allocation before retry")
		}
		return false, nil
	}
	claim := model.EVMPaymentClaim{Network: e.Network, TxHash: strings.ToLower(tr.TxHash), EventIndex: tr.EventIndex, OrderID: o.ID}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, nil
	}
	if err := o.MarkConfirmingTx(tx, tr.BlockNum, tr.FromAddress, tr.TxHash, tr.Timestamp, tr.Amount); err != nil {
		return false, err
	}
	if err := tx.Model(o).Updates(map[string]any{"ref_event_index": tr.EventIndex, "ref_block_hash": tr.BlockHash, "ref_amount": tr.Amount.String()}).Error; err != nil {
		return false, err
	}
	return true, nil
}

func evmScanMatch(o model.Order, tr transfer) bool {
	if !model.IsAmountValid(tr.TradeType, tr.Amount) {
		return false
	}
	if o.CreatedAt == nil || !strings.EqualFold(orderMatchAddress(o), tr.RecvAddress) || o.TradeType != tr.TradeType {
		return false
	}
	if !o.CreatedAt.Before(tr.Timestamp) || !o.ExpiredAt.After(tr.Timestamp) {
		return false
	}
	if o.AddressLocked {
		return tr.Amount.IsPositive()
	}
	return amountMatch(tr.Amount, o.Amount, string(o.TradeType))
}

func (e *evm) confirmDurableOrders(ctx context.Context) {
	var orders []model.Order
	if err := model.Db.WithContext(ctx).Where("trade_type IN ? AND status = ?", model.GetNetworkTrades(model.Network(e.Network)), model.OrderStatusConfirming).Order("updated_at,id").Limit(32).Find(&orders).Error; err != nil {
		return
	}
	jobs := make(chan model.Order)
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for o := range jobs {
				jobCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err := e.verifyEVMOrder(jobCtx, o)
				cancel()
				if err == nil {
					model.Db.Model(&model.Order{}).Where("id = ? AND status = ?", o.ID, model.OrderStatusConfirming).Update("confirmation_error", "")
					markFinalConfirmed(o)
				} else {
					log.Task.Warn(fmt.Sprintf("BSC order %d confirmation deferred: %s", o.ID, err))
					message := err.Error()
					if len(message) > 500 {
						message = message[:500]
					}
					if err := model.Db.Model(&model.Order{}).Where("id = ? AND status = ?", o.ID, model.OrderStatusConfirming).Updates(map[string]any{"updated_at": time.Now(), "confirmation_error": message}).Error; err != nil {
						log.Task.Warn("persist confirmation error: ", err)
					}
					var rpcError *EVMRPCError
					if errors.As(err, &rpcError) && rpcError.Kind == "failed_transaction" {
						if err := o.SetFailed(); err != nil {
							log.Task.Warn("persist failed chain transaction: ", err)
						}
					}
				}
			}
		}()
	}
	for _, o := range orders {
		select {
		case jobs <- o:
		case <-ctx.Done():
			close(jobs)
			for range 4 {
				<-done
			}
			return
		}
	}
	close(jobs)
	for range 4 {
		<-done
	}
}
