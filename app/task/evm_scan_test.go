package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	applog "github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/task/notify"
	"gorm.io/gorm"
)

func scanTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scan.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Order{}, &model.EVMScanJob{}, &model.EVMPaymentClaim{}, &model.Conf{}, &model.Wallet{}); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	old := model.Db
	model.Db = db
	t.Cleanup(func() { model.Db = old })
	if err := db.Create(&model.Conf{K: model.PaymentMatchMode, V: string(model.Classic)}).Error; err != nil {
		t.Fatal(err)
	}
	model.RefreshC()
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func scanOrder(t *testing.T, db *gorm.DB, id string, created, expires time.Time) model.Order {
	t.Helper()
	o := model.Order{OrderId: id, TradeId: id, TradeType: model.UsdtBep20, Rate: "7", Amount: "2", Money: "14", Address: "0x1111111111111111111111111111111111111111", Status: model.OrderStatusExpired, ExpiredAt: expires, ConfirmedAt: &created, AutoTimeAt: model.AutoTimeAt{CreatedAt: (*model.Datetime)(&created), UpdatedAt: (*model.Datetime)(&created)}}
	if err := db.Create(&o).Error; err != nil {
		t.Fatal(err)
	}
	return o
}

func TestScanWindowMixedExpiredAndWaiting(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	start, end := evmOrderWindow([]model.Order{{ExpiredAt: now.Add(-time.Minute), AutoTimeAt: model.AutoTimeAt{CreatedAt: dt(now.Add(-time.Hour))}}, {ExpiredAt: now.Add(time.Minute), AutoTimeAt: model.AutoTimeAt{CreatedAt: dt(now.Add(-10 * time.Minute))}}}, now)
	if start != now.Add(-time.Hour).Unix() || end != now.Unix() {
		t.Fatalf("window shortened: %d %d", start, end)
	}
}

func dt(at time.Time) *model.Datetime { return (*model.Datetime)(&at) }

func TestScanCommitPinsOldOrderAndDeduplicatesEvent(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	created := now.Add(-5 * time.Hour)
	o := scanOrder(t, db, "one", created, created.Add(time.Minute*20))
	job := model.EVMScanJob{Network: "bsc", Kind: "order", OrderID: o.ID, NextBlock: 10, TargetBlock: 10, EndAt: o.ExpiredAt.Unix()}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	e := evm{Network: "bsc"}
	tr := transfer{Network: "bsc", TxHash: "tx", EventIndex: "log:1", BlockHash: "block", BlockNum: 10, TradeType: model.UsdtBep20, RecvAddress: o.Address, FromAddress: "sender", Amount: decimal.NewFromInt(2), Timestamp: created.Add(time.Minute)}
	if err := e.commitScanRange(db, job, evmBlock{From: 10, To: 10}, []transfer{tr}); err != nil {
		t.Fatal(err)
	}
	db.First(&o, o.ID)
	db.First(&job, job.ID)
	if o.Status != model.OrderStatusConfirming || job.NextBlock != 11 {
		t.Fatalf("not durably delivered: %d %+v", o.Status, job)
	}
	if err := o.SetSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := e.commitScanRange(db, job, evmBlock{From: 10, To: 10}, []transfer{tr}); err != nil {
		t.Fatal(err)
	}
	db.First(&o, o.ID)
	if o.Status != model.OrderStatusSuccess {
		t.Fatal("replay regressed order")
	}
	var count int64
	db.Model(&model.EVMPaymentClaim{}).Count(&count)
	if count != 1 {
		t.Fatalf("claims=%d", count)
	}
}

func TestScanCommitFailureDoesNotAdvanceOrMatch(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	o := scanOrder(t, db, "rollback", now.Add(-time.Minute), now.Add(time.Minute))
	job := model.EVMScanJob{Network: "bsc", Kind: "order", OrderID: o.ID, NextBlock: 10, TargetBlock: 10}
	db.Create(&job)
	if err := db.Callback().Update().Before("gorm:update").Register("fail_scan_commit", func(tx *gorm.DB) {
		if tx.Statement.Table == "bep_evm_scan_job" {
			tx.AddError(errors.New("disk failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	e := evm{Network: "bsc"}
	tr := transfer{Network: "bsc", TxHash: "tx", EventIndex: "log:0", BlockNum: 10, TradeType: model.UsdtBep20, RecvAddress: o.Address, Amount: decimal.NewFromInt(2), Timestamp: now}
	if err := e.commitScanRange(db, job, evmBlock{From: 10, To: 10}, []transfer{tr}); err == nil {
		t.Fatal("expected database failure")
	}
	db.First(&job, job.ID)
	db.First(&o, o.ID)
	if job.NextBlock != 10 || o.Status != model.OrderStatusExpired {
		t.Fatalf("partial commit: %+v %d", job, o.Status)
	}
}

func TestScanDiscoveryAndRestartKeepCapturedOrder(t *testing.T) {
	db := scanTestDB(t)
	if err := db.Create(&model.Conf{K: model.PaymentLookbackHour, V: "3"}).Error; err != nil {
		t.Fatal(err)
	}
	model.RefreshC()
	now := time.Now()
	o := scanOrder(t, db, "restart", now.Add(-time.Hour), now.Add(-30*time.Minute))
	e := evm{Network: "bsc"}
	e.discoverScanOrders(context.Background())
	e.discoverScanOrders(context.Background())
	var jobs []model.EVMScanJob
	if err := db.Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("duplicate jobs: %d", len(jobs))
	}
	// Change wall-clock eligibility without deleting the durable association.
	if err := db.Model(&model.Order{}).Where("id = ?", o.ID).Update("expired_at", now.Add(-4*time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	var restored model.EVMScanJob
	if err := db.First(&restored, jobs[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if restored.OrderID != o.ID || restored.Completed {
		t.Fatal("restart lost pending order")
	}
}

func TestScanRejectsLateTransfer(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	o := scanOrder(t, db, "late", now.Add(-time.Hour), now.Add(-time.Minute))
	tr := transfer{TradeType: o.TradeType, RecvAddress: o.Address, Timestamp: now, Amount: decimal.NewFromInt(2)}
	if evmScanMatch(o, tr) {
		t.Fatal("late payment matched outside original window")
	}
}

func TestScanDustDoesNotConsumeExclusiveOrder(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	o := scanOrder(t, db, "dust", now.Add(-time.Minute), now.Add(time.Minute))
	if err := db.Model(&o).Update("address_locked", true).Error; err != nil {
		t.Fatal(err)
	}
	job := model.EVMScanJob{Network: "bsc", Kind: "order", OrderID: o.ID, NextBlock: 10, TargetBlock: 11}
	db.Create(&job)
	e := evm{Network: "bsc"}
	tr := transfer{Network: "bsc", TxHash: "dust", EventIndex: "log:1", BlockNum: 10, TradeType: model.UsdtBep20, RecvAddress: o.Address, Timestamp: now, Amount: decimal.New(1, -18)}
	if err := e.commitScanRange(db, job, evmBlock{From: 10, To: 10}, []transfer{tr}); err != nil {
		t.Fatal(err)
	}
	db.First(&o, o.ID)
	var claims int64
	db.Model(&model.EVMPaymentClaim{}).Count(&claims)
	if o.Status != model.OrderStatusExpired || claims != 0 {
		t.Fatal("dust consumed exclusive invoice")
	}
	db.First(&job, job.ID)
	tr.TxHash = "payment"
	tr.BlockNum = 11
	tr.Amount = decimal.NewFromInt(2)
	if err := e.commitScanRange(db, job, evmBlock{From: 11, To: 11}, []transfer{tr}); err != nil {
		t.Fatal(err)
	}
	db.First(&o, o.ID)
	if o.Status != model.OrderStatusConfirming {
		t.Fatal("real payment not matched after dust")
	}
}

func TestScanHeadRefreshesOldConfirmingOrderAfterRestart(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	o := scanOrder(t, db, "old-confirming", now.Add(-5*time.Hour), now.Add(-4*time.Hour))
	db.Model(&o).Update("status", model.OrderStatusConfirming)
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return "0x38", nil
		}
		return "0x64", nil
	})
	if err := model.SetConfs([]model.Conf{{K: model.RpcEndpointBsc, V: server.URL}, {K: "rpc_endpoint_bsc_read", V: server.URL}, {K: "rpc_endpoint_bsc_read_backups", V: ""}, {K: model.PaymentLookbackHour, V: "3"}}); err != nil {
		t.Fatal(err)
	}
	chainBlockNum.Delete("bsc")
	defer chainBlockNum.Delete("bsc")
	evmTestEngine().syncDurableHead(context.Background())
	if head, ok := chainBlockNum.Load("bsc"); !ok || head.(int64) != 100 {
		t.Fatal("old confirmation lost its head after restart")
	}
}

func TestScanPendingPaymentsGetIndependentLiveCursor(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	o := scanOrder(t, db, "live", now.Add(-time.Minute), now.Add(time.Minute))
	db.Model(&o).Update("status", model.OrderStatusWaiting)
	server := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		if call.Method == "eth_chainId" {
			return "0x38", nil
		}
		return "0x64", nil
	})
	if err := model.SetConfs([]model.Conf{{K: model.RpcEndpointBsc, V: server.URL}, {K: "rpc_endpoint_bsc_read", V: server.URL}, {K: "rpc_endpoint_bsc_read_backups", V: ""}}); err != nil {
		t.Fatal(err)
	}
	evmTestEngine().syncDurableHead(context.Background())
	var count int64
	db.Model(&model.EVMScanJob{}).Where("kind = ?", "live").Count(&count)
	if count != 1 {
		t.Fatal("ordinary payment has no independent live scanner")
	}
	var watching int64
	db.Model(&model.Wallet{}).Where("other_notify = ?", model.WaOtherEnable).Count(&watching)
	if watching != 0 {
		t.Fatal("enabled non-order notifications unexpectedly")
	}
}

func TestScanCandidatePaginationDoesNotSkipLaterMatch(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	for i := 0; i < 130; i++ {
		o := scanOrder(t, db, fmt.Sprint("page-", i), now.Add(-time.Minute), now.Add(time.Minute))
		db.Model(&o).Update("amount", "1")
	}
	o := scanOrder(t, db, "last-page", now.Add(-time.Minute), now.Add(time.Minute))
	job := model.EVMScanJob{Network: "bsc", Kind: "live", NextBlock: 10, TargetBlock: 10}
	db.Create(&job)
	e := evm{Network: "bsc"}
	tr := transfer{Network: "bsc", TxHash: "tx", EventIndex: "log:1", BlockNum: 10, TradeType: o.TradeType, RecvAddress: o.Address, Amount: decimal.NewFromInt(2), Timestamp: now}
	if err := e.commitScanRange(db, job, evmBlock{From: 10, To: 10}, []transfer{tr}); err != nil {
		t.Fatal(err)
	}
	db.First(&o, o.ID)
	if o.Status != model.OrderStatusConfirming {
		t.Fatal("later candidate page skipped")
	}
}

func TestScanAmbiguousLegacyEventDoesNotAdvance(t *testing.T) {
	db := scanTestDB(t)
	now := time.Now()
	old := scanOrder(t, db, "legacy", now.Add(-time.Minute), now.Add(time.Minute))
	db.Model(&old).Updates(map[string]any{"status": model.OrderStatusSuccess, "ref_hash": "tx"})
	o := scanOrder(t, db, "second-event", now.Add(-time.Minute), now.Add(time.Minute))
	job := model.EVMScanJob{Network: "bsc", Kind: "order", OrderID: o.ID, NextBlock: 10, TargetBlock: 10}
	db.Create(&job)
	e := evm{Network: "bsc"}
	tr := transfer{Network: "bsc", TxHash: "tx", EventIndex: "log:2", BlockNum: 10, TradeType: o.TradeType, RecvAddress: o.Address, Amount: decimal.NewFromInt(2), Timestamp: now}
	if err := e.commitScanRange(db, job, evmBlock{From: 10, To: 10}, []transfer{tr}); err == nil {
		t.Fatal("ambiguous legacy allocation silently skipped")
	}
	db.First(&job, job.ID)
	if job.NextBlock != 10 {
		t.Fatal("ambiguous event advanced scan")
	}
}

func TestScanToMerchantLostACKDoesNotDuplicateCredit(t *testing.T) {
	db := scanTestDB(t)
	if err := applog.Init(filepath.Join(t.TempDir(), "logs")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(applog.Close)
	var calls, credits int
	var merchantOrderID string
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload notify.EpNotify
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if payload.Status != model.OrderStatusSuccess || payload.Signature == "" {
			t.Error("invalid terminal callback")
			w.WriteHeader(400)
			return
		}
		calls++
		if merchantOrderID == "" {
			merchantOrderID = payload.OrderId
			credits++
		} else if merchantOrderID != payload.OrderId {
			t.Error("retry changed merchant order identity")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("success"))
	}))
	defer merchant.Close()
	rpc := evmTestServer(t, func(call evmTestCall) (any, map[string]any) {
		switch call.Method {
		case "eth_chainId":
			return "0x38", nil
		case "eth_getBlockByNumber":
			return evmTestHeader(), nil
		case "eth_getLogs":
			return nil, map[string]any{"code": -32602, "message": "Archive requests require a personal token"}
		case "eth_getBlockReceipts":
			return []any{evmTestReceipt()}, nil
		case "eth_getTransactionReceipt":
			return evmTestReceipt(), nil
		default:
			t.Errorf("unexpected method %s", call.Method)
			return nil, nil
		}
	})
	for _, key := range []model.ConfKey{model.RpcEndpointBsc, "rpc_endpoint_bsc_read", "rpc_endpoint_bsc_log_backups", "rpc_endpoint_bsc_read_backups"} {
		value := ""
		if key == model.RpcEndpointBsc || key == "rpc_endpoint_bsc_read" {
			value = rpc.URL
		}
		if err := db.Create(&model.Conf{K: key, V: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model.RefreshC()
	created := time.Unix(1699999900, 0)
	o := scanOrder(t, db, "lost-ack", created, created.Add(10*time.Minute))
	if err := db.Model(&o).Updates(map[string]any{"amount": "1", "address": evmTestTo, "notify_url": merchant.URL}).Error; err != nil {
		t.Fatal(err)
	}
	job := model.EVMScanJob{Network: "bsc", Kind: "order", OrderID: o.ID, NextBlock: 100, TargetBlock: 100}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	e := evmTestEngine()
	ctx := context.Background()
	transfers, err := e.fetchEVMRange(ctx, evmBlock{From: 100, To: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.commitScanRange(db, job, evmBlock{From: 100, To: 100}, transfers); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&o, o.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.verifyEVMOrder(ctx, o); err != nil {
		t.Fatal(err)
	}
	if err := o.SetSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := notify.Handle(o); err == nil {
		t.Fatal("missing ACK accepted")
	}
	if err := notify.Handle(o); err != nil {
		t.Fatal(err)
	}
	if err := notify.Handle(o); err != nil {
		t.Fatal(err)
	}
	db.First(&o, o.ID)
	if credits != 1 || calls != 2 || o.Status != model.OrderStatusSuccess || o.NotifyState != model.OrderNotifyStateSucc {
		t.Fatalf("credits=%d calls=%d status=%d notify=%d", credits, calls, o.Status, o.NotifyState)
	}
}

// Opt-in deployment probe only reads public chain data, never writes a production order.
func TestBSCReadOnlyProductionProbe(t *testing.T) {
	if os.Getenv("BEPUSDT_READ_ONLY_PROBE") != "1" {
		t.Skip("deployment-only public RPC check")
	}
	db := scanTestDB(t)
	for key, value := range map[model.ConfKey]string{model.RpcEndpointBsc: "https://bsc-rpc.publicnode.com", model.ConfKey("rpc_endpoint_bsc_read"): "https://bsc-dataseed.bnbchain.org", model.ConfKey("rpc_endpoint_bsc_read_backups"): "https://bsc-dataseed-public.bnbchain.org,https://bsc-dataseed.defibit.io"} {
		if err := db.Create(&model.Conf{K: key, V: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	model.RefreshC()
	e := evm{Network: "bsc", Client: &http.Client{Timeout: 15 * time.Second}, Native: evmNative{Parse: true, Decimal: -18, TradeType: model.BscBnb}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	head, err := e.evmHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transfers, err := e.fetchEVMRange(ctx, evmBlock{From: 120236484, To: 120236484})
	if err != nil {
		t.Fatal(err)
	}
	const hash = "0x26da39e910be109bbf3325d18a8fdce858ff9f4c5a9ef1d7067d631e1864bcab"
	var found *transfer
	for i := range transfers {
		if transfers[i].TxHash == hash && transfers[i].TradeType == model.UsdtBep20 {
			found = &transfers[i]
			break
		}
	}
	if found == nil || !found.Amount.Equal(decimal.RequireFromString("424.74")) {
		t.Fatal("historical payment missing")
	}
	created, err := time.Parse(time.RFC3339, "2026-09-06T12:09:08+08:00")
	if err != nil {
		t.Fatal(err)
	}
	o := model.Order{TradeType: model.UsdtBep20, Address: "0xeb1f8e1f4e484b94fbd8209cd57ad34313c5597c", FromAddress: found.FromAddress, Amount: "424.74", RefHash: hash, RefBlockNum: 120236484, RefBlockHash: found.BlockHash, RefEventIndex: found.EventIndex, RefAmount: found.Amount.String(), ConfirmedAt: &found.Timestamp, ExpiredAt: created.Add(20 * time.Minute), AutoTimeAt: model.AutoTimeAt{CreatedAt: dt(created)}}
	if err := e.verifyEVMOrder(ctx, o); err != nil {
		t.Fatal(err)
	}
	t.Logf("read-only verification passed head=%d block=%d amount=%s event=%s", head, found.BlockNum, found.Amount, found.EventIndex)
}
