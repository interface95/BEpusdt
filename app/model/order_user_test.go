package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func newOrderUserDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newOrderCASDB(t)
	if err := db.AutoMigrate(&Conf{}, &Wallet{}, &Rate{}); err != nil {
		t.Fatal(err)
	}
	FillDefaultConf()
	RefreshC()
	for _, crypto := range []string{"USDT", "USDC"} {
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&Rate{Crypto: crypto, Fiat: "CNY", Rate: "7", RawRate: 7}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func userOrderParams(t *testing.T, extra string) OrderParams {
	t.Helper()
	var p OrderParams
	if err := json.Unmarshal([]byte(`{"user_id":" private-id ","username":" private-name ","display_name":" 客户显示名 "`+extra+`}`), &p); err != nil {
		t.Fatal(err)
	}
	p.OrderId, p.Money, p.Fiat, p.ApiType = t.Name(), decimal.NewFromInt(70), CNY, OrderApiTypeEpusdtOrder
	p.TradeType, p.Address = UsdtBep20, "0x1111111111111111111111111111111111111111"
	return p
}

func assertOrderUser(t *testing.T, order Order) {
	t.Helper()
	if order.UserID != "private-id" || order.Username != "private-name" || order.DisplayName != "客户显示名" {
		t.Fatalf("unexpected user snapshot: id=%q username=%q display=%q", order.UserID, order.Username, order.DisplayName)
	}
}

func TestOrderUserSnapshotReuseRebuildAndReplacement(t *testing.T) {
	for name, build := range map[string]func(OrderParams) (Order, error){"pending": BuildPendingOrder, "transaction": StartBuildOrder} {
		for _, status := range []int{OrderStatusWaiting, OrderStatusConfirming, OrderStatusSuccess, OrderStatusExpired, OrderStatusCanceled, OrderStatusFailed} {
			t.Run(fmt.Sprintf("%s/%d", name, status), func(t *testing.T) {
				db := newOrderUserDB(t)
				p := userOrderParams(t, "")
				original, err := build(p)
				if err != nil {
					t.Fatal(err)
				}
				assertOrderUser(t, original)
				if err := db.Model(&original).Update("status", status).Error; err != nil {
					t.Fatal(err)
				}
				before := readCASOrder(t, db, original.ID)
				if _, err := build(userOrderParams(t, `,"user_id":"other-user"`)); !errors.Is(err, ErrOrderUserConflict) {
					t.Fatalf("state %d bypassed binding check: %v", status, err)
				}
				if after := readCASOrder(t, db, original.ID); !reflect.DeepEqual(before, after) {
					t.Fatal("conflicting request modified original order")
				}
				// Omitted, empty and changed descriptive fields must retain the snapshot.
				for _, extra := range []string{`,"user_id":"","username":"","display_name":""`, `,"username":"renamed","display_name":"New Name"`} {
					retry := userOrderParams(t, extra)
					retry.TradeType = UsdcBep20
					order, err := build(retry)
					if err != nil {
						t.Fatal(err)
					}
					assertOrderUser(t, order)
					assertOrderUser(t, readCASOrder(t, db, order.ID))
					if (status == OrderStatusWaiting || status == OrderStatusSuccess || status == OrderStatusConfirming) && order.ID != original.ID {
						t.Fatal("active order unexpectedly replaced")
					}
				}
				conflicting := userOrderParams(t, `,"user_id":"other-user"`)
				if _, err := build(conflicting); err == nil || strings.Contains(err.Error(), "private-id") || strings.Contains(err.Error(), "other-user") {
					t.Fatalf("binding conflict accepted or leaked: %v", err)
				}
			})
		}
	}
}

func TestOrderUserRebuildSnapshotAndConflict(t *testing.T) {
	db := newOrderUserDB(t)
	p := userOrderParams(t, "")
	p.TradeType = ""
	order, err := BuildPendingOrder(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, trade := range []TradeType{UsdtBep20, UsdcBep20, UsdcBep20} {
		p = userOrderParams(t, `,"user_id":"","username":"","display_name":""`)
		p.TradeType = trade
		order, err = RebuildOrder(order, p)
		if err != nil {
			t.Fatal(err)
		}
		assertOrderUser(t, order)
		assertOrderUser(t, readCASOrder(t, db, order.ID))
		bad := userOrderParams(t, `,"user_id":"other-user"`)
		bad.TradeType = trade
		if _, err := RebuildOrder(order, bad); err == nil {
			t.Fatal("same-payment early return bypassed binding check")
		}
	}
}

func TestOrderUserLegacyBackfillDoesNotTouchPayment(t *testing.T) {
	db := newOrderUserDB(t)
	p := userOrderParams(t, `,"user_id":"","username":"","display_name":""`)
	order, err := BuildPendingOrder(p)
	if err != nil {
		t.Fatal(err)
	}
	before := readCASOrder(t, db, order.ID)
	p = userOrderParams(t, "")
	if _, err := BuildPendingOrder(p); err != nil {
		t.Fatal(err)
	}
	after := readCASOrder(t, db, order.ID)
	assertOrderUser(t, after)
	before.UserID, before.Username, before.DisplayName = after.UserID, after.Username, after.DisplayName
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("metadata backfill changed payment snapshot: before=%+v after=%+v", before, after)
	}
}

func TestOrderUserConcurrentBinding(t *testing.T) {
	for _, tc := range []struct {
		name          string
		extra         [2]string
		wantSuccesses int
	}{
		{"identical retry", [2]string{"", ""}, 2},
		{"changed name keeps winner snapshot", [2]string{"", `,"username":"renamed","display_name":"New Name"`}, 2},
		{"complementary fields", [2]string{`,"display_name":""`, `,"username":""`}, 2},
		{"different users", [2]string{`,"user_id":"first-user"`, `,"user_id":"second-user"`}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newOrderUserDB(t)
			legacy, err := BuildPendingOrder(userOrderParams(t, `,"user_id":"","username":"","display_name":""`))
			if err != nil {
				t.Fatal(err)
			}
			before := readCASOrder(t, db, legacy.ID)
			var reads atomic.Int32
			var barrier sync.WaitGroup
			barrier.Add(2)
			// Both requests must read the same empty snapshot before either writes.
			if err := db.Callback().Query().After("gorm:query").Register("test:metadata_snapshot", func(tx *gorm.DB) {
				row, ok := tx.Statement.Dest.(*Order)
				if ok && row.ID == legacy.ID && reads.Add(1) <= 2 {
					barrier.Done()
					barrier.Wait()
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Query().Remove("test:metadata_snapshot") })
			type result struct {
				order Order
				err   error
			}
			results := make(chan result, 2)
			for _, extra := range tc.extra {
				request := userOrderParams(t, extra)
				go func() {
					order, err := BuildPendingOrder(request)
					results <- result{order: order, err: err}
				}()
			}
			var successful []Order
			for range 2 {
				result := <-results
				if result.err == nil {
					successful = append(successful, result.order)
				} else if tc.wantSuccesses == 2 || !errors.Is(result.err, ErrOrderUserConflict) {
					t.Errorf("unexpected retry error: %v", result.err)
				}
			}
			if len(successful) != tc.wantSuccesses {
				t.Fatalf("successful retries = %d, want %d", len(successful), tc.wantSuccesses)
			}
			after := readCASOrder(t, db, legacy.ID)
			for _, returned := range successful {
				if returned.ID != legacy.ID || returned.TradeId != legacy.TradeId || returned.UserID != after.UserID {
					t.Fatalf("retry returned a different order or user: %+v", returned)
				}
			}
			if tc.name == "changed name keeps winner snapshot" {
				for _, returned := range successful {
					if returned.Username != after.Username || returned.DisplayName != after.DisplayName {
						t.Fatal("retry overwrote the winning snapshot")
					}
				}
			} else if tc.wantSuccesses == 2 {
				assertOrderUser(t, after)
			}
			var count int64
			if err := db.Model(&Order{}).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("retry created duplicate orders: count=%d err=%v", count, err)
			}
			after.UserID, after.Username, after.DisplayName = "", "", ""
			if !reflect.DeepEqual(before, after) {
				t.Fatal("metadata retries changed payment fields or timestamps")
			}
		})
	}
}

func TestOrderUserRetryDoesNotRecreateDeletedOrder(t *testing.T) {
	db := newOrderUserDB(t)
	legacy, err := BuildPendingOrder(userOrderParams(t, `,"user_id":"","username":"","display_name":""`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := saveOrderUserMetadata(legacy, userOrderParams(t, "")); err == nil {
		t.Fatal("deleted order accepted a metadata update")
	}
	var count int64
	if err := db.Model(&Order{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("retry recreated deleted order: count=%d err=%v", count, err)
	}
}

func TestOrderUserJSONAndEpayPrivacy(t *testing.T) {
	newOrderUserDB(t)
	order, err := BuildPendingOrder(userOrderParams(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	order.ApiType, order.Status, order.ReturnUrl = OrderApiTypeEpay, OrderStatusSuccess, "https://merchant.example/done"
	for _, payload := range []string{string(body), order.BuildNotifyParams(), order.RedirectUrl()} {
		for _, secret := range []string{"user_id", "username", "display_name", "private-id", "private-name", "客户显示名"} {
			if strings.Contains(payload, secret) {
				t.Fatalf("public payload leaked %q: %s", secret, payload)
			}
		}
	}
}

func TestOrderUserAutoMigratePreservesLegacyRows(t *testing.T) {
	db := newOrderCASDB(t)
	order := seedCASOrder(t, db, OrderStatusSuccess)
	before := readCASOrder(t, db, order.ID)
	if err := db.Migrator().DropIndex(&Order{}, "idx_bep_order_user_id"); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"user_id", "username", "display_name"} {
		if err := db.Migrator().DropColumn(&Order{}, column); err != nil {
			t.Fatal(err)
		}
		if db.Migrator().HasColumn(&Order{}, column) {
			t.Fatalf("legacy fixture still has %s", column)
		}
	}
	for range 2 {
		if err := AutoMigrate(); err != nil {
			t.Fatal(err)
		}
		after := readCASOrder(t, db, order.ID)
		if !reflect.DeepEqual(before, after) || after.UserID != "" || after.Username != "" || after.DisplayName != "" {
			t.Fatalf("migration altered legacy order: before=%+v after=%+v", before, after)
		}
		var missing int64
		if err := db.Model(&Order{}).Where("user_id IS NULL OR username IS NULL OR display_name IS NULL").Count(&missing).Error; err != nil || missing != 0 {
			t.Fatalf("metadata defaults are not empty strings: %d %v", missing, err)
		}
	}
}

func TestOrderUserValidationBoundaries(t *testing.T) {
	for field, limit := range map[string]int{"user_id": 64, "username": 64, "display_name": 128} {
		for _, value := range []string{"", "   ", "  name with spaces  ", strings.Repeat("名", limit), strings.Repeat("\U0001f600", limit), "\u3000name\u00a0"} {
			got, err := normalizeOrderUserField(field, value, limit)
			if err != nil || got != strings.TrimSpace(value) {
				t.Fatalf("valid %s rejected: %q %v", field, got, err)
			}
		}
		for _, value := range []string{strings.Repeat("x", limit+1), strings.Repeat("名", limit+1), "bad\xff", "\tname", "name\n", "a\x00b", "a\x7fb", "a\u009fb"} {
			if _, err := normalizeOrderUserField(field, value, limit); err == nil {
				t.Fatalf("invalid %s accepted", field)
			}
		}
	}
	for _, build := range []func(OrderParams) (Order, error){
		StartBuildOrder, BuildPendingOrder,
		func(p OrderParams) (Order, error) { return BuildOrder(p, Trade{}) },
		func(p OrderParams) (Order, error) { return RebuildOrder(Order{}, p) },
	} {
		for _, p := range []OrderParams{{UserID: "bad\xff"}, {Username: "bad\xff"}, {DisplayName: "bad\xff"}} {
			if _, err := build(p); err == nil {
				t.Fatal("order builder accepted invalid UTF-8")
			}
		}
	}
}

func TestOrderUserStaleRebuildCannotOverwriteBinding(t *testing.T) {
	db := newOrderUserDB(t)
	p := userOrderParams(t, `,"user_id":"","username":"","display_name":""`)
	stale, err := BuildPendingOrder(p)
	if err != nil {
		t.Fatal(err)
	}
	current, err := BuildPendingOrder(userOrderParams(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	before := readCASOrder(t, db, current.ID)
	for _, trade := range []TradeType{stale.TradeType, UsdcBep20} {
		request := userOrderParams(t, `,"user_id":"other-user"`)
		request.TradeType = trade
		if _, err := RebuildOrder(stale, request); err == nil {
			t.Fatal("stale rebuild overwrote bound user")
		}
		if after := readCASOrder(t, db, current.ID); !reflect.DeepEqual(before, after) {
			t.Fatal("rejected stale rebuild changed the order")
		}
	}
}
