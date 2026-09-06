package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
)

func setupReliableNotify(t *testing.T) *gorm.DB {
	t.Helper()
	db := newNotifyTestDB(t)
	initNotifyTestLog(t)
	if err := db.AutoMigrate(&model.Conf{}); err != nil {
		t.Fatal(err)
	}
	previous := model.Db
	model.Db = db
	if err := db.Create(&model.Conf{K: model.ConfKey("notify_waiting_disabled_urls"), V: "[]"}).Error; err != nil {
		t.Fatal(err)
	}
	model.RefreshC()
	t.Cleanup(func() { model.Db = previous })
	return db
}

func TestTerminalACKMustBeExact(t *testing.T) {
	for _, apiType := range []string{model.OrderApiTypeEpusdt, model.OrderApiTypeEpay} {
		for i, tc := range []struct {
			body     string
			status   int
			accepted bool
		}{
			{"success", 200, true}, {" \tOK\r\n", 200, true}, {"SuCcEsS", 200, true},
			{"fail", 200, false}, {"", 200, false}, {"not ok", 200, false},
			{"unsuccessful", 200, false}, {`{"success":true}`, 200, false}, {"success", 500, false},
		} {
			t.Run(fmt.Sprintf("%s/%d", apiType, i), func(t *testing.T) {
				db := setupReliableNotify(t)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				}))
				defer server.Close()
				order := newWaitingOrder(server.URL)
				order.Status, order.ApiType = model.OrderStatusSuccess, apiType
				if err := db.Create(&order).Error; err != nil {
					t.Fatal(err)
				}
				err := Handle(order)
				if (err == nil) != tc.accepted {
					t.Fatalf("ACK %q status %d: err=%v, accepted=%v", tc.body, tc.status, err, tc.accepted)
				}
				var current model.Order
				if err := db.First(&current, order.ID).Error; err != nil {
					t.Fatal(err)
				}
				if (current.NotifyState == model.OrderNotifyStateSucc) != tc.accepted || current.NotifyNum != 1 {
					t.Fatalf("unexpected callback result state=%d attempts=%d", current.NotifyState, current.NotifyNum)
				}
			})
		}
	}
}

func TestTerminalConcurrentDeliveryDoesNotHoldDBLock(t *testing.T) {
	db := setupReliableNotify(t)
	started, release := make(chan struct{}, 32), make(chan struct{})
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		started <- struct{}{}
		<-release
		fmt.Fprint(w, "success")
	}))
	defer server.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	order := newWaitingOrder(server.URL)
	order.Status = model.OrderStatusSuccess
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- Handle(order) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP never started")
	}
	query := make(chan error, 1)
	go func() {
		query <- db.Model(&model.Order{}).Where("id = ?", order.ID).Update("name", "changed during HTTP").Error
	}()
	select {
	case err := <-query:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("HTTP held database lock/connection")
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() { Handle(order) })
	}
	// A short wait lets contenders reach the DB while the first HTTP request is held.
	time.Sleep(100 * time.Millisecond)
	unblock()
	workers.Wait()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatalf("duplicate concurrent callbacks: %d", count.Load())
	}
	if err := Handle(order); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatal("stale retry repeated an acknowledged callback")
	}
	var current model.Order
	if err := db.First(&current, order.ID).Error; err != nil || current.Name != "changed during HTTP" {
		t.Fatalf("callback overwrote order snapshot: %v %+v", err, current)
	}
}

func TestWaitingDisabledOnlyForExactURL(t *testing.T) {
	db := setupReliableNotify(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "success")
	}))
	defer server.Close()
	disabledURL := server.URL + "/callback"
	encoded, err := json.Marshal([]string{disabledURL})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Conf{}).Where("k = ?", "notify_waiting_disabled_urls").Update("v", string(encoded)).Error; err != nil {
		t.Fatal(err)
	}
	model.RefreshC()
	for i, tc := range []struct {
		url    string
		status int
		want   int32
	}{
		{disabledURL, model.OrderStatusWaiting, 0},
		{disabledURL + "?different=1", model.OrderStatusWaiting, 1},
		{disabledURL + "/", model.OrderStatusWaiting, 1},
		{disabledURL, model.OrderStatusExpired, 1},
		{disabledURL, model.OrderStatusSuccess, 1},
	} {
		order := newWaitingOrder(tc.url)
		order.TradeId += fmt.Sprintf("-exact-url-%d", i)
		order.Status = tc.status
		if err := db.Create(&order).Error; err != nil {
			t.Fatal(err)
		}
		before := calls.Load()
		if tc.status == model.OrderStatusSuccess {
			err = Handle(order)
		} else {
			err = deliverBepusdtStatusUpdate(db, server.Client(), "test-token", order)
		}
		if err != nil {
			t.Fatal(err)
		}
		if delta := calls.Load() - before; delta != tc.want {
			t.Fatalf("status=%d URL=%s: requests=%d, want=%d", tc.status, tc.url, delta, tc.want)
		}
	}
}

func TestTerminalLostReplyCanBeRetried(t *testing.T) {
	db := setupReliableNotify(t)
	var calls atomic.Int32
	var firstBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload EpNotify
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Error(err)
		}
		if calls.Add(1) == 1 {
			firstBody.Store(string(body))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		if firstBody.Load() != string(body) {
			t.Errorf("retry changed signed payment payload: %s versus %s", firstBody.Load(), body)
		}
		fmt.Fprint(w, "success")
	}))
	defer server.Close()
	order := newWaitingOrder(server.URL)
	order.Status = model.OrderStatusSuccess
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := Handle(order); err == nil {
		t.Fatal("lost reply must not acknowledge delivery")
	}
	var failed model.Order
	if err := db.First(&failed, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.NotifyState != model.OrderNotifyStateFail || failed.NotifyNum != 1 {
		t.Fatalf("lost reply is not retryable: %+v", failed)
	}
	if err := Handle(order); err != nil {
		t.Fatal(err)
	}
	var current model.Order
	if err := db.First(&current, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.NotifyState != model.OrderNotifyStateSucc || current.NotifyNum != 2 || calls.Load() != 2 {
		t.Fatalf("retry failed: state=%d attempts=%d calls=%d", current.NotifyState, current.NotifyNum, calls.Load())
	}
}

func TestTerminalExpiredLeaseRecoversAndFencesLateFailure(t *testing.T) {
	db := setupReliableNotify(t)
	order := newWaitingOrder("http://127.0.0.1/callback")
	order.Status = model.OrderStatusSuccess
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	old := order
	if err := claimNotifyLease(db, &old); err != nil {
		t.Fatal(err)
	}
	contender := order
	if err := claimNotifyLease(db, &contender); !errors.Is(err, ErrNotifyInProgress) {
		t.Fatalf("unexpired lease admitted second callback: %v", err)
	}
	if err := db.Model(&model.Order{}).Where("id = ?", order.ID).UpdateColumn("notify_lease_until", time.Now().Add(-time.Second).UnixMilli()).Error; err != nil {
		t.Fatal(err)
	}
	if err := claimNotifyLease(db, &contender); err != nil {
		t.Fatalf("expired lease could not be recovered: %v", err)
	}
	if contender.NotifyLeaseToken == old.NotifyLeaseToken {
		t.Fatal("lease ownership token reused")
	}
	if err := finishNotifyLease(db, contender, nil); err != nil {
		t.Fatal(err)
	}
	if err := finishNotifyLease(db, old, errors.New("late failure")); !errors.Is(err, model.ErrOrderStateChanged) {
		t.Fatalf("late lease completion was not fenced: %v", err)
	}
	var current model.Order
	if err := db.First(&current, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.NotifyState != model.OrderNotifyStateSucc || current.NotifyError != "" || current.NotifyNum != 1 || current.NotifyLeaseToken != "" {
		t.Fatalf("stale owner changed callback result: %+v", current)
	}
}

func TestTerminalLateFailureDoesNotOverwriteOtherSuccess(t *testing.T) {
	db := setupReliableNotify(t)
	order := newWaitingOrder("http://127.0.0.1/callback")
	order.Status = model.OrderStatusSuccess
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := claimNotifyLease(db, &order); err != nil {
		t.Fatal(err)
	}
	other := order
	if err := other.SetNotifyState(model.OrderNotifyStateSucc); err != nil {
		t.Fatal(err)
	}
	if err := finishNotifyLease(db, order, errors.New("failure arrived later")); err != nil {
		t.Fatal(err)
	}
	var current model.Order
	if err := db.First(&current, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.NotifyState != model.OrderNotifyStateSucc || current.NotifyError != "" {
		t.Fatalf("late failure overwrote success: %+v", current)
	}
}

func TestTerminalPersistenceFailureLeavesDeliveryRecoverable(t *testing.T) {
	db := setupReliableNotify(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "success")
	}))
	defer server.Close()
	order := newWaitingOrder(server.URL)
	order.Status = model.OrderStatusSuccess
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	dbFailure := errors.New("simulated unavailable database after HTTP")
	if err := db.Callback().Update().Before("gorm:update").Register("lose_first_ack_write", func(tx *gorm.DB) {
		if updates, ok := tx.Statement.Dest.(map[string]any); ok {
			if _, finishing := updates["notify_num"]; finishing && failed.CompareAndSwap(false, true) {
				tx.AddError(dbFailure)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := Handle(order); !errors.Is(err, dbFailure) {
		t.Fatalf("lost DB result not reported: %v", err)
	}
	var pending model.Order
	if err := db.First(&pending, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if pending.NotifyState != model.OrderNotifyStateFail || pending.NotifyLeaseToken == "" {
		t.Fatalf("uncommitted acknowledgement marked delivered: %+v", pending)
	}
	if err := db.Model(&model.Order{}).Where("id = ?", order.ID).UpdateColumn("notify_lease_until", 0).Error; err != nil {
		t.Fatal(err)
	}
	if err := Handle(order); err != nil {
		t.Fatalf("restart recovery failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("lost result did not retry: calls=%d", calls.Load())
	}
}

func TestTerminalManualResendPreservesAcknowledgedState(t *testing.T) {
	db := setupReliableNotify(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, "ok")
		} else {
			fmt.Fprint(w, "fail")
		}
	}))
	defer server.Close()
	order := newWaitingOrder(server.URL)
	order.Status = model.OrderStatusSuccess
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := Handle(order); err != nil {
		t.Fatal(err)
	}
	if err := HandleManual(order); err == nil {
		t.Fatal("failed manual ACK not reported")
	}
	var current model.Order
	if err := db.First(&current, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || current.NotifyState != model.OrderNotifyStateSucc || current.NotifyNum != 2 {
		t.Fatalf("manual resend broke acknowledgement monotonicity: %+v calls=%d", current, calls.Load())
	}
}

func TestTerminalDiagnosticDoesNotPersistSecrets(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(transportFailure), func(t *testing.T) {
			db := setupReliableNotify(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "secret-response-body")
			}))
			defer server.Close()
			order := newWaitingOrder(server.URL + "/callback?key=secret-url-value")
			if transportFailure {
				order.NotifyUrl = "://secret-url-value"
			}
			order.Status = model.OrderStatusSuccess
			if err := db.Create(&order).Error; err != nil {
				t.Fatal(err)
			}
			if err := Handle(order); err == nil {
				t.Fatal("expected failure")
			}
			var current model.Order
			if err := db.First(&current, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.NotifyError == "" || strings.Contains(current.NotifyError, "secret-") || strings.Contains(current.NotifyError, server.URL) {
				t.Fatalf("unsafe or missing diagnostic: %q", current.NotifyError)
			}
		})
	}
}

func TestWaitingAndExpiredRetainHTTP200ProtocolAndRetryFailures(t *testing.T) {
	for _, status := range []int{model.OrderStatusWaiting, model.OrderStatusExpired} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			db := setupReliableNotify(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadGateway)
				}
			}))
			defer server.Close()
			order := newWaitingOrder(server.URL)
			order.Status = status
			if err := db.Create(&order).Error; err != nil {
				t.Fatal(err)
			}
			if err := deliverBepusdtStatusUpdate(db, server.Client(), "token", order); err == nil {
				t.Fatal("HTTP 502 not reported")
			}
			if err := deliverBepusdtStatusUpdate(db, server.Client(), "token", order); err != nil {
				t.Fatalf("HTTP200 empty ACK must remain supported: %v", err)
			}
			if calls.Load() != 2 {
				t.Fatalf("failure incorrectly cached: %d calls", calls.Load())
			}
			var current model.Order
			if err := db.First(&current, order.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.NotifyNum != 0 || current.NotifyState != 0 || current.NotifyLeaseToken != "" {
				t.Fatalf("nonterminal message changed terminal notification state: %+v", current)
			}
		})
	}
}
