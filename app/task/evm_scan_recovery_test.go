package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	applog "github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
)

func recoveryTestCreateJob(t *testing.T, db *gorm.DB, job model.EVMScanJob) model.EVMScanJob {
	t.Helper()
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return job
}

func recoveryTestJob(t *testing.T, db *gorm.DB, kind string) model.EVMScanJob {
	t.Helper()
	var job model.EVMScanJob
	if err := db.Where("network = ? AND kind = ? AND order_id = ?", "bsc", kind, 0).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	return job
}

func TestScanRecoveryPromoteSeparatesOldLiveCursor(t *testing.T) {
	db := scanTestDB(t)
	recoveryTestCreateJob(t, db, model.EVMScanJob{
		Network: "bsc", Kind: "live", NextBlock: 10, TargetBlock: 30,
		Attempts: 5, LastError: "historical outage", NextRetryAt: time.Now().Add(time.Hour),
	})
	before := time.Now()
	if err := evmTestEngine().promoteLiveWindow(db, 100); err != nil {
		t.Fatal(err)
	}
	live := recoveryTestJob(t, db, "live")
	gap := recoveryTestJob(t, db, "recovery")
	if live.NextBlock != 69 || live.TargetBlock != 100 || live.Completed {
		t.Fatalf("latest window not independently runnable: %+v", live)
	}
	if live.Attempts != 0 || live.LastError != "" || live.NextRetryAt.After(time.Now()) {
		t.Fatalf("live inherited historical backoff: %+v", live)
	}
	if gap.NextBlock != 10 || gap.TargetBlock != 68 || gap.Completed || gap.NextRetryAt.Before(before) {
		t.Fatalf("missing or prematurely completed gap: %+v", gap)
	}
	if gap.TargetBlock+1 != live.NextBlock {
		t.Fatal("handoff left an unowned block")
	}
}

func TestScanRecoveryRepeatedPromotionMergesGapWithoutResettingProgress(t *testing.T) {
	db := scanTestDB(t)
	recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 10, TargetBlock: 30})
	e := evmTestEngine()
	if err := e.promoteLiveWindow(db, 100); err != nil {
		t.Fatal(err)
	}
	gap := recoveryTestJob(t, db, "recovery")
	retryAt := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if err := db.Model(&gap).Updates(map[string]any{
		"next_block": 28, "next_retry_at": retryAt, "attempts": 7, "last_error": "archive unavailable",
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, head := range []int64{100, 100, 120, 120, 110} {
		if err := e.promoteLiveWindow(db, head); err != nil {
			t.Fatal(err)
		}
	}
	merged := recoveryTestJob(t, db, "recovery")
	live := recoveryTestJob(t, db, "live")
	if merged.ID != gap.ID || merged.NextBlock != 28 || merged.TargetBlock != 88 || merged.Completed {
		t.Fatalf("duplicate promotion lost progress or gap: %+v", merged)
	}
	if merged.Attempts != 7 || merged.LastError != "archive unavailable" || !merged.NextRetryAt.Equal(retryAt) {
		t.Fatalf("promotion reset historical retry budget: %+v", merged)
	}
	if live.NextBlock != 89 || live.TargetBlock != 120 {
		t.Fatalf("repeated or older head rewound live: %+v", live)
	}
	var count int64
	if err := db.Model(&model.EVMScanJob{}).Where("network = ?", "bsc").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("promotion created %d jobs instead of one live and one recovery", count)
	}
}

func TestScanRecoveryPromotionReopensCompletedGap(t *testing.T) {
	db := scanTestDB(t)
	recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 69, TargetBlock: 100})
	old := recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "recovery", NextBlock: 69, TargetBlock: 68, Completed: true})
	if err := evmTestEngine().promoteLiveWindow(db, 120); err != nil {
		t.Fatal(err)
	}
	gap := recoveryTestJob(t, db, "recovery")
	if gap.ID != old.ID || gap.NextBlock != 69 || gap.TargetBlock != 88 || gap.Completed {
		t.Fatalf("new gap left completed or lost: %+v", gap)
	}
}

func TestScanRecoveryHandoffRollsBackWhenLiveUpdateFails(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_gap_%t", existing), func(t *testing.T) {
			db := scanTestDB(t)
			recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 40, TargetBlock: 50})
			if existing {
				recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "recovery", NextBlock: 10, TargetBlock: 39})
			}
			injected := errors.New("injected live cursor write failure")
			if err := db.Callback().Update().Before("gorm:update").Register("recovery_fail_live_update", func(tx *gorm.DB) {
				updates, ok := tx.Statement.Dest.(map[string]any)
				if tx.Statement.Table == "bep_evm_scan_job" && ok {
					if _, movingLive := updates["next_block"]; movingLive {
						tx.AddError(injected)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := evmTestEngine().promoteLiveWindow(db, 100); !errors.Is(err, injected) {
				t.Fatalf("expected handoff rollback, got %v", err)
			}
			live := recoveryTestJob(t, db, "live")
			if live.NextBlock != 40 || live.TargetBlock != 50 {
				t.Fatalf("failed handoff advanced live: %+v", live)
			}
			var gaps []model.EVMScanJob
			if err := db.Where("kind = ?", "recovery").Find(&gaps).Error; err != nil {
				t.Fatal(err)
			}
			if existing {
				if len(gaps) != 1 || gaps[0].NextBlock != 10 || gaps[0].TargetBlock != 39 {
					t.Fatalf("gap extension escaped rollback: %+v", gaps)
				}
			} else if len(gaps) != 0 {
				t.Fatalf("gap creation escaped rollback: %+v", gaps)
			}
		})
	}
}

func TestScanRecoveryStaleCompletionCannotHideExtendedTarget(t *testing.T) {
	db := scanTestDB(t)
	recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 69, TargetBlock: 100})
	stale := recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "recovery", NextBlock: 69, TargetBlock: 68})
	e := evmTestEngine()
	if err := e.promoteLiveWindow(db, 120); err != nil {
		t.Fatal(err)
	}
	if err := e.processScanJob(context.Background(), &stale); err != nil {
		t.Fatal(err)
	}
	gap := recoveryTestJob(t, db, "recovery")
	if gap.Completed || gap.TargetBlock != 88 || gap.NextBlock != 69 {
		t.Fatalf("stale completion discarded extended work: %+v", gap)
	}
	if err := db.Model(&gap).Update("next_block", 89).Error; err != nil {
		t.Fatal(err)
	}
	finished := recoveryTestJob(t, db, "recovery")
	if err := e.processScanJob(context.Background(), &finished); err != nil {
		t.Fatal(err)
	}
	if !recoveryTestJob(t, db, "recovery").Completed {
		t.Fatal("current completion did not finish the fully scanned target")
	}
}

func TestScanRecoveryStaleCompletionCannotOverrideChangedNextBlock(t *testing.T) {
	db := scanTestDB(t)
	stale := recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "recovery", NextBlock: 69, TargetBlock: 68})
	if err := db.Model(&model.EVMScanJob{}).Where("id = ?", stale.ID).Update("next_block", 50).Error; err != nil {
		t.Fatal(err)
	}
	if err := evmTestEngine().processScanJob(context.Background(), &stale); err != nil {
		t.Fatal(err)
	}
	gap := recoveryTestJob(t, db, "recovery")
	if gap.Completed || gap.NextBlock != 50 || gap.TargetBlock != 68 {
		t.Fatalf("stale completion discarded rewound work: %+v", gap)
	}
}

func TestScanRecoveryInFlightBatchCommitPreservesExtendedTarget(t *testing.T) {
	db := scanTestDB(t)
	recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 69, TargetBlock: 100})
	stale := recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "recovery", NextBlock: 60, TargetBlock: 68})
	e := evmTestEngine()
	if err := e.promoteLiveWindow(db, 120); err != nil {
		t.Fatal(err)
	}
	if err := e.commitScanRange(db, stale, evmBlock{From: 60, To: 68}, nil); err != nil {
		t.Fatal(err)
	}
	gap := recoveryTestJob(t, db, "recovery")
	if gap.NextBlock != 69 || gap.TargetBlock != 88 || gap.Completed {
		t.Fatalf("old batch commit overwrote extended target: %+v", gap)
	}
}

func TestScanRecoveryStaleFailureCannotDelayPromotedLive(t *testing.T) {
	db := scanTestDB(t)
	if err := applog.Init(filepath.Join(t.TempDir(), "logs")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(applog.Close)
	stale := recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 10, TargetBlock: 30, Attempts: 7})
	e := evmTestEngine()
	if err := e.promoteLiveWindow(db, 100); err != nil {
		t.Fatal(err)
	}
	before := recoveryTestJob(t, db, "live")
	e.deferScanJob(stale, &EVMRPCError{Kind: "rate_limited", RetryAfter: time.Hour})
	after := recoveryTestJob(t, db, "live")
	if after.NextBlock != before.NextBlock || after.TargetBlock != before.TargetBlock ||
		after.Attempts != before.Attempts || after.LastError != before.LastError || !after.NextRetryAt.Equal(before.NextRetryAt) {
		t.Fatalf("stale history failure delayed fresh live: before=%+v after=%+v", before, after)
	}
}

func TestScanRecoveryStaleRangeResolutionCannotRewindPromotedLive(t *testing.T) {
	db := scanTestDB(t)
	stale := recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 31, TargetBlock: 30})
	e := evmTestEngine()
	promoted := make(chan error, 1)
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_blockNumber":
			promoted <- e.promoteLiveWindow(db, 100)
			return "0x64", nil
		default:
			t.Errorf("stale range continued into RPC %s after losing ownership", call.Method)
			return nil, map[string]any{"code": -32601, "message": "unsupported"}
		}
	})
	if err := model.SetConfs([]model.Conf{
		{K: model.RpcEndpointBsc, V: server.URL}, {K: "rpc_endpoint_bsc_read", V: server.URL},
		{K: "rpc_endpoint_bsc_read_backups", V: ""}, {K: "rpc_endpoint_bsc_log_backups", V: ""},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.processScanJob(ctx, &stale); err == nil {
		t.Fatal("stale range resolution succeeded after promotion changed ownership")
	}
	select {
	case err := <-promoted:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("test did not interleave promotion with range resolution")
	}
	live := recoveryTestJob(t, db, "live")
	gap := recoveryTestJob(t, db, "recovery")
	if live.NextBlock != 69 || live.TargetBlock != 100 || gap.NextBlock != 31 || gap.TargetBlock != 68 {
		t.Fatalf("range resolution overwrote handoff: live=%+v gap=%+v", live, gap)
	}
	if stale.NextBlock != 31 || stale.TargetBlock != 30 {
		t.Fatalf("uncommitted range mutated caller snapshot: %+v", stale)
	}
}

func TestScanRecoveryResolutionFailureRetainsDurableRetrySnapshot(t *testing.T) {
	db := scanTestDB(t)
	if err := applog.Init(filepath.Join(t.TempDir(), "logs")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(applog.Close)
	now := time.Now().Truncate(time.Second)
	created := now.Add(-time.Minute)
	order := scanOrder(t, db, "range-resolution-failure", created, now.Add(time.Minute))
	job := recoveryTestCreateJob(t, db, model.EVMScanJob{
		Network: "bsc", Kind: "order", OrderID: order.ID, StartAt: created.Unix(),
	})
	var blockReads atomic.Int32
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_blockNumber":
			return "0x64", nil
		case "eth_getBlockByNumber":
			// Tip and start resolve successfully; fail only when resolving the end.
			if blockReads.Add(1) >= 3 {
				return nil, map[string]any{"code": -32000, "message": "historical data unavailable"}
			}
			header := evmTestHeader()
			header["timestamp"] = fmt.Sprintf("0x%x", created.Unix())
			return header, nil
		default:
			t.Errorf("unexpected method %s", call.Method)
			return nil, map[string]any{"code": -32601, "message": "unsupported"}
		}
	})
	if err := model.SetConfs([]model.Conf{
		{K: model.RpcEndpointBsc, V: server.URL}, {K: "rpc_endpoint_bsc_read", V: server.URL},
		{K: "rpc_endpoint_bsc_read_backups", V: ""}, {K: "rpc_endpoint_bsc_log_backups", V: ""},
	}); err != nil {
		t.Fatal(err)
	}
	e := evmTestEngine()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := e.processScanJob(ctx, &job)
	if err == nil || blockReads.Load() < 3 {
		t.Fatalf("test did not fail after resolving start: reads=%d error=%v", blockReads.Load(), err)
	}
	if job.NextBlock != 0 || job.TargetBlock != 0 || job.EndAt != 0 {
		t.Fatalf("failed resolution mutated retry snapshot: %+v", job)
	}
	before := time.Now()
	e.deferScanJob(job, err)
	var persisted model.EVMScanJob
	if err := db.First(&persisted, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.NextBlock != 0 || persisted.TargetBlock != 0 || persisted.EndAt != 0 ||
		persisted.Attempts != 1 || persisted.LastError == "" || !persisted.NextRetryAt.After(before) {
		t.Fatalf("failed resolution could not persist backoff: %+v", persisted)
	}
}

func TestScanRecoveryHistoricalFailuresDoNotBlockNewPayment(t *testing.T) {
	db := scanTestDB(t)
	if err := applog.Init(filepath.Join(t.TempDir(), "logs")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(applog.Close)
	now := time.Now().Truncate(time.Second)
	order := scanOrder(t, db, "latest-during-history-outage", now.Add(-time.Minute), now.Add(time.Minute))
	if err := db.Model(&order).Updates(map[string]any{
		"amount": "1", "money": "7", "address": evmTestTo, "status": model.OrderStatusWaiting,
	}).Error; err != nil {
		t.Fatal(err)
	}
	var historicalCalls atomic.Int32
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_blockNumber":
			return "0x64", nil
		case "eth_getBlockByNumber":
			var encoded string
			if len(call.Params) == 0 || json.Unmarshal(call.Params[0], &encoded) != nil {
				t.Error("missing block number")
				return nil, map[string]any{"code": -32602, "message": "invalid block"}
			}
			height, err := strconv.ParseInt(encoded, 0, 64)
			if err != nil {
				t.Error(err)
				return nil, map[string]any{"code": -32602, "message": "invalid block"}
			}
			if height < 69 {
				historicalCalls.Add(1)
				return nil, map[string]any{"code": -32000, "message": "historical data unavailable"}
			}
			return recoveryTestHeader(height, now), nil
		case "eth_getLogs":
			var filter struct {
				From string `json:"fromBlock"`
				To   string `json:"toBlock"`
			}
			if len(call.Params) == 0 || json.Unmarshal(call.Params[0], &filter) != nil {
				t.Error("invalid log filter")
				return nil, map[string]any{"code": -32602, "message": "invalid filter"}
			}
			from, fromErr := strconv.ParseInt(filter.From, 0, 64)
			to, toErr := strconv.ParseInt(filter.To, 0, 64)
			if fromErr != nil || toErr != nil {
				t.Errorf("invalid range %s..%s", filter.From, filter.To)
				return nil, map[string]any{"code": -32602, "message": "invalid range"}
			}
			if from <= 69 && to >= 69 {
				return []any{recoveryTestLog()}, nil
			}
			return []any{}, nil
		case "eth_getTransactionReceipt":
			receipt := evmTestReceipt()
			receipt["blockNumber"] = "0x45"
			receipt["blockHash"] = fmt.Sprintf("0x%064x", 69)
			receipt["logs"] = []any{recoveryTestLog()}
			return receipt, nil
		default:
			t.Errorf("unexpected method %s", call.Method)
			return nil, map[string]any{"code": -32601, "message": "unsupported"}
		}
	})
	if err := model.SetConfs([]model.Conf{
		{K: model.RpcEndpointBsc, V: server.URL}, {K: "rpc_endpoint_bsc_read", V: server.URL},
		{K: "rpc_endpoint_bsc_read_backups", V: ""}, {K: "rpc_endpoint_bsc_log_backups", V: ""},
		{K: model.BlockOffsetConfirm, V: "1"}, {K: model.PaymentLookbackHour, V: "3"},
	}); err != nil {
		t.Fatal(err)
	}
	e := evmTestEngine()
	e.Block.ConfirmedOffset = 15
	recoveryTestCreateJob(t, db, model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 10, TargetBlock: 30})
	e.syncDurableHead(context.Background())
	gap := recoveryTestJob(t, db, "recovery")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	done := make(chan struct{}, 2)
	for _, kind := range []string{"order", "live"} {
		go func(kind string) {
			e.scanWorker(ctx, kind)
			done <- struct{}{}
		}(kind)
	}
	defer func() {
		cancel()
		for range 2 {
			<-done
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	// Run the actual workers: recovery must be dispatched by the order worker,
	// while recent payment matching succeeds before the historical outage ends.
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("latest payment starved: status=%d historical attempts=%d", order.Status, gap.Attempts)
		case <-ticker.C:
			if err := db.First(&order, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			gap = recoveryTestJob(t, db, "recovery")
			if order.Status == model.OrderStatusConfirming && gap.Attempts >= 2 {
				goto confirmed
			}
		}
	}

confirmed:
	if historicalCalls.Load() == 0 || gap.NextBlock != 10 || gap.TargetBlock != 68 || gap.Completed || gap.LastError == "" {
		t.Fatalf("historical failure lost durable gap: calls=%d job=%+v", historicalCalls.Load(), gap)
	}
	if err := e.verifyEVMOrder(ctx, order); err != nil {
		t.Fatalf("latest payment cannot confirm during outage: %v", err)
	}
	if err := order.SetSuccess(); err != nil {
		t.Fatal(err)
	}
	var claims int64
	if err := db.Model(&model.EVMPaymentClaim{}).Where("order_id = ?", order.ID).Count(&claims).Error; err != nil {
		t.Fatal(err)
	}
	if claims != 1 || order.Status != model.OrderStatusSuccess {
		t.Fatalf("payment not confirmed exactly once: claims=%d status=%d", claims, order.Status)
	}
}

func recoveryTestLog() map[string]any {
	event := evmTestLog()
	event["blockNumber"] = "0x45"
	event["blockHash"] = fmt.Sprintf("0x%064x", 69)
	return event
}

func recoveryTestHeader(height int64, now time.Time) map[string]any {
	transactions := []any{}
	if height == 69 {
		tx := evmTestTransaction()
		tx["blockNumber"] = "0x45"
		tx["blockHash"] = fmt.Sprintf("0x%064x", 69)
		transactions = append(transactions, tx)
	}
	return map[string]any{
		"number": fmt.Sprintf("0x%x", height), "hash": fmt.Sprintf("0x%064x", height),
		"parentHash": fmt.Sprintf("0x%064x", height-1),
		"timestamp":  fmt.Sprintf("0x%x", now.Unix()-(100-height)), "transactions": transactions,
	}
}
