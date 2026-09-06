package model

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func newOrderCASDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "orders.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(1000)"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&Order{}); err != nil {
		t.Fatal(err)
	}
	previous := Db
	Db = db
	t.Cleanup(func() {
		Db = previous
		sqlDB.Close()
	})
	return db
}

func seedCASOrder(t *testing.T, db *gorm.DB, status int) Order {
	t.Helper()
	now := time.Now()
	order := Order{
		OrderId: t.Name(), TradeId: t.Name(), TradeType: UsdtBep20,
		Rate: "7", Amount: "10", Money: "70", Address: "recipient",
		Status: status, RefHash: "tx-current", ConfirmedAt: &now,
		ExpiredAt:  now.Add(time.Hour),
		AutoTimeAt: AutoTimeAt{CreatedAt: (*Datetime)(&now), UpdatedAt: (*Datetime)(&now)},
	}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	return order
}

func readCASOrder(t *testing.T, db *gorm.DB, id int64) Order {
	t.Helper()
	var order Order
	if err := db.First(&order, id).Error; err != nil {
		t.Fatal(err)
	}
	return order
}

func TestOrderStaleSnapshotsCannotRollbackSuccess(t *testing.T) {
	for _, operation := range []string{"expired", "failed", "canceled", "confirming"} {
		t.Run(operation, func(t *testing.T) {
			db := newOrderCASDB(t)
			order := seedCASOrder(t, db, OrderStatusWaiting)
			if operation == "failed" {
				order.Status = OrderStatusConfirming
			}
			if err := db.Model(&Order{}).Where("id = ?", order.ID).Updates(map[string]any{
				"status": OrderStatusSuccess, "amount": "25", "notify_state": OrderNotifyStateSucc,
			}).Error; err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "expired":
				order.SetExpired()
			case "failed":
				order.SetFailed()
			case "canceled":
				order.SetCanceled()
			case "confirming":
				order.MarkConfirming(42, "sender", "other-tx", time.Now(), decimal.NewFromInt(3))
			}
			current := readCASOrder(t, db, order.ID)
			if current.Status != OrderStatusSuccess || current.Amount != "25" || current.NotifyState != OrderNotifyStateSucc {
				t.Fatalf("stale %s overwrote paid order: %+v", operation, current)
			}
		})
	}
}

func TestOrderSuccessRequiresCurrentConfirmationHash(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusConfirming)
	if err := db.Model(&Order{}).Where("id = ?", order.ID).Update("ref_hash", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	order.SetSuccess()
	current := readCASOrder(t, db, order.ID)
	if current.Status != OrderStatusConfirming || current.RefHash != "replacement" {
		t.Fatalf("stale confirmation changed order: %+v", current)
	}
}

func TestOrderConcurrentMarkConfirmingHasOneWinner(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusWaiting)
	var winners atomic.Int32
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			snapshot := order
			if snapshot.MarkConfirming(i, "sender", fmt.Sprintf("tx-%d", i), time.Now(), decimal.NewFromInt(10)) == nil {
				winners.Add(1)
			}
		}()
	}
	workers.Wait()
	if winners.Load() != 1 {
		t.Fatalf("confirmation winners = %d, want 1", winners.Load())
	}
}

func TestOrderNotifyUpdateDoesNotWriteSnapshot(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusSuccess)
	if err := db.Model(&Order{}).Where("id = ?", order.ID).Updates(map[string]any{
		"amount": "20", "notify_num": 7,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := order.SetNotifyState(OrderNotifyStateSucc); err != nil {
		t.Fatal(err)
	}
	current := readCASOrder(t, db, order.ID)
	if current.Amount != "20" || current.NotifyNum != 8 || current.Status != OrderStatusSuccess {
		t.Fatalf("notification overwrote unrelated fields or counter: %+v", current)
	}
}

func TestOrderCASReportsChangedStateWithoutMutatingReceiver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		change func(*Order) error
	}{
		{"success", OrderStatusConfirming, (*Order).SetSuccess},
		{"failed", OrderStatusConfirming, (*Order).SetFailed},
		{"expired", OrderStatusWaiting, (*Order).SetExpired},
		{"canceled", OrderStatusWaiting, (*Order).SetCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newOrderCASDB(t)
			order := seedCASOrder(t, db, tc.status)
			if err := db.Model(&Order{}).Where("id = ?", order.ID).Update("ref_hash", "different").Error; err != nil {
				t.Fatal(err)
			}
			if err := tc.change(&order); !errors.Is(err, ErrOrderStateChanged) {
				t.Fatalf("got %v, want ErrOrderStateChanged", err)
			}
			if order.Status != tc.status {
				t.Fatalf("rejected CAS mutated receiver: %+v", order)
			}
			order = readCASOrder(t, db, order.ID)
			if err := tc.change(&order); err != nil {
				t.Fatalf("valid CAS failed: %v", err)
			}
		})
	}
}

func TestOrderMarkConfirmingTxSharesCommitAndRollback(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusExpired)
	order.AddressLocked = true
	if err := db.Model(&Order{}).Where("id = ?", order.ID).Update("address_locked", true).Error; err != nil {
		t.Fatal(err)
	}
	abort := errors.New("rollback scanner event")
	for _, rollback := range []bool{true, false} {
		snapshot := order
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := snapshot.MarkConfirmingTx(tx, 99, "payer", "real-tx", time.Now(), decimal.RequireFromString("1.25")); err != nil {
				return err
			}
			if err := tx.Model(&Order{}).Where("id = ?", order.ID).Updates(map[string]any{
				"ref_event_index": "7", "ref_block_hash": "block-proof",
			}).Error; err != nil {
				return err
			}
			if rollback {
				return abort
			}
			return nil
		})
		if rollback && !errors.Is(err, abort) || !rollback && err != nil {
			t.Fatal(err)
		}
		current := readCASOrder(t, db, order.ID)
		if rollback {
			if current.Status != OrderStatusExpired || current.RefEventIndex != "" || current.Amount != "10" {
				t.Fatalf("rollback partially committed order/event: %+v", current)
			}
		} else if current.Status != OrderStatusConfirming || current.RefEventIndex != "7" || current.RefBlockHash != "block-proof" || current.RefAmount != "1.25" || current.Amount != "1.25" || current.Money != "8.75" {
			t.Fatalf("transaction lost confirmation evidence: %+v", current)
		}
	}
}

func TestOrderMarkConfirmingRejectsAllNonReceivableStates(t *testing.T) {
	for _, status := range []int{OrderStatusConfirming, OrderStatusCanceled, OrderStatusFailed, OrderStatusSuccess} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			db := newOrderCASDB(t)
			order := seedCASOrder(t, db, status)
			stale := order
			stale.Status = OrderStatusWaiting
			for _, snapshot := range []Order{order, stale} {
				if err := snapshot.MarkConfirming(50, "wrong", "wrong", time.Now(), decimal.NewFromInt(10)); !errors.Is(err, ErrOrderStateChanged) {
					t.Fatalf("status %d accepted replacement: %v", status, err)
				}
			}
		})
	}
}

func TestOrderNotifySuccessIsMonotonicWithAtomicAttemptCount(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusSuccess)
	successful := order
	if err := successful.SetNotifyState(OrderNotifyStateSucc); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			stale := order
			if err := stale.SetNotifyState(OrderNotifyStateFail); err != nil {
				t.Error(err)
			}
			if stale.NotifyState != OrderNotifyStateSucc {
				t.Errorf("receiver was not refreshed after monotonic update: %+v", stale)
			}
		})
	}
	workers.Wait()
	current := readCASOrder(t, db, order.ID)
	if current.NotifyState != OrderNotifyStateSucc || current.NotifyNum != 17 {
		t.Fatalf("late failure downgraded ACK or lost attempts: %+v", current)
	}
}

func TestOrderManualPaidUsesCASAndPreservesOverrideSemantics(t *testing.T) {
	for _, status := range []int{OrderStatusWaiting, OrderStatusExpired, OrderStatusFailed, OrderStatusConfirming, OrderStatusCanceled, OrderStatusSuccess} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			db := newOrderCASDB(t)
			order := seedCASOrder(t, db, status)
			err := order.MarkPaid("manual-reference", time.Now())
			if status == OrderStatusCanceled || status == OrderStatusSuccess {
				if !errors.Is(err, ErrOrderStateChanged) {
					t.Fatalf("terminal manual overwrite: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			current := readCASOrder(t, db, order.ID)
			if current.Status != OrderStatusSuccess || current.RefHash != "manual-reference" {
				t.Fatalf("manual override not applied: %+v", current)
			}
		})
	}
}

func TestOrderNoCASUpsertWhenRowWasDeleted(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusConfirming)
	if err := db.Delete(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := order.SetSuccess(); !errors.Is(err, ErrOrderStateChanged) {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Order{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("deleted order resurrected: %d %v", count, err)
	}
}

func TestOrderNotifyStateUpdateTouchesOnlyNotificationColumns(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusSuccess)
	before := readCASOrder(t, db, order.ID)
	if err := order.SetNotifyState(OrderNotifyStateSucc); err != nil {
		t.Fatal(err)
	}
	after := readCASOrder(t, db, order.ID)
	before.NotifyNum, before.NotifyState, before.NotifyError = after.NotifyNum, after.NotifyState, after.NotifyError
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("notification update changed other columns: before=%+v after=%+v", before, after)
	}
}

func TestOrderMarkConfirmingRejectsReselectedPaymentSnapshot(t *testing.T) {
	for field, value := range map[string]any{
		"trade_type": UsdcBep20, "address": "new-recipient", "match_address": "new-match",
		"address_locked": true, "amount": "11", "rate": "8", "expired_at": time.Now().Add(2 * time.Hour), "ref_hash": "new-reference",
	} {
		t.Run(field, func(t *testing.T) {
			db := newOrderCASDB(t)
			order := seedCASOrder(t, db, OrderStatusWaiting)
			if err := db.Model(&Order{}).Where("id = ?", order.ID).UpdateColumn(field, value).Error; err != nil {
				t.Fatal(err)
			}
			if err := order.MarkConfirming(100, "stale-payer", "stale-tx", time.Now(), decimal.NewFromInt(10)); !errors.Is(err, ErrOrderStateChanged) {
				t.Fatalf("reselected %s accepted old transfer: %v", field, err)
			}
			current := readCASOrder(t, db, order.ID)
			if current.Status != OrderStatusWaiting || current.FromAddress == "stale-payer" || current.RefHash == "stale-tx" {
				t.Fatalf("reselection overwritten: %+v", current)
			}
		})
	}
}

func TestOrderCASPropagatesDatabaseError(t *testing.T) {
	for _, operation := range []string{"success", "confirming", "paid", "notify"} {
		t.Run(operation, func(t *testing.T) {
			db := newOrderCASDB(t)
			order := seedCASOrder(t, db, OrderStatusWaiting)
			if operation == "success" {
				order.Status = OrderStatusConfirming
			} else if operation == "notify" {
				order.Status = OrderStatusSuccess
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlDB.Close(); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "success":
				err = order.SetSuccess()
			case "confirming":
				err = order.MarkConfirming(9, "from", "ref", time.Now(), decimal.NewFromInt(1))
			case "paid":
				err = order.MarkPaid("manual", time.Now())
			case "notify":
				err = order.SetNotifyState(OrderNotifyStateSucc)
			}
			if err == nil || errors.Is(err, ErrOrderStateChanged) {
				t.Fatalf("database error lost or misclassified: %v", err)
			}
		})
	}
}

func TestOrderManualPaidCannotWinAfterCancellation(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusWaiting)
	stale := order
	if err := order.SetCanceled(); err != nil {
		t.Fatal(err)
	}
	if err := stale.MarkPaid("manual", time.Now()); !errors.Is(err, ErrOrderStateChanged) {
		t.Fatalf("concurrent cancellation bypassed: %v", err)
	}
}

func TestOrderNotifyRejectsInvalidAndDeletedOrder(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusSuccess)
	if err := order.SetNotifyState(99); err == nil {
		t.Fatal("invalid notify state accepted")
	}
	if err := db.Delete(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := order.SetNotifyState(OrderNotifyStateSucc); !errors.Is(err, ErrOrderStateChanged) {
		t.Fatalf("notification resurrected deleted order: %v", err)
	}
	var zero Order
	if err := zero.SetNotifyState(OrderNotifyStateSucc); !errors.Is(err, ErrOrderStateChanged) {
		t.Fatalf("zero order accepted: %v", err)
	}
}
