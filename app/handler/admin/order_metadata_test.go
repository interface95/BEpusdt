package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/task/notify"
	"github.com/v03413/bepusdt/app/utils"
)

func TestAdminCreateAndQueryUserMetadata(t *testing.T) {
	db := newConfTestDB(t)
	for i, item := range []struct{ id, username, display string }{
		{"user-1", "alpha", "Alpha Person"},
		{"user-10", "beta", "Display Match"},
		{"user-2", "literal%_!\\name", "Gamma"},
	} {
		body, err := json.Marshal(map[string]any{"order_id": fmt.Sprint(i), "amount": 70, "fiat": "CNY", "user_id": " " + item.id + " ", "username": item.username, "display_name": item.display})
		if err != nil {
			t.Fatal(err)
		}
		if result := confRequest(t, Order{}.Create, string(body)); result["code"] != float64(200) {
			t.Fatal(result)
		}
	}
	for _, tc := range []struct {
		filter string
		ids    []string
	}{
		{``, []string{"user-1", "user-10", "user-2"}},
		{`,"user_id":"user-1"`, []string{"user-1"}},
		{`,"user_id":" user-1 "`, []string{"user-1"}},
		{`,"username":"alpha"`, []string{"user-1"}},
		{`,"username":"Display Match"`, []string{"user-10"}},
		{`,"username":"%_!\\"`, []string{"user-2"}},
		{`,"username":"' OR 1=1 --"`, nil},
		{`,"user_id":"user-1' OR 1=1 --"`, nil},
		{`,"user_id":"user-1","username":"Display Match"`, nil},
	} {
		result := confRequest(t, Order{}.List, `{"page":1,"size":10,"sort":"asc"`+tc.filter+`}`)
		if result["code"] != float64(200) {
			t.Fatal(result)
		}
		rows := result["data"].([]any)
		if len(rows) != len(tc.ids) || (len(tc.ids) > 0 && result["total"] != float64(len(tc.ids))) {
			t.Fatalf("filter %s returned %v", tc.filter, result)
		}
		for i, value := range rows {
			row := value.(map[string]any)
			if row["user_id"] != tc.ids[i] || row["username"] == nil || row["display_name"] == nil || row["wallet"] == nil {
				t.Fatalf("missing admin fields: %v", row)
			}
			detail := confRequest(t, Order{}.Detail, fmt.Sprintf(`{"id":%.0f}`, row["id"]))
			data := detail["data"].(map[string]any)
			if data["user_id"] != row["user_id"] || data["username"] != row["username"] || data["display_name"] != row["display_name"] || data["tx_url"] == nil {
				t.Fatalf("missing detail metadata: %v", detail)
			}
		}
	}
	var stored map[string]any
	if err := db.Table("bep_order").Select("user_id, username, display_name").Where("order_id = ?", "0").Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored["user_id"] != "user-1" || stored["username"] != "alpha" || stored["display_name"] != "Alpha Person" {
		t.Fatal(stored)
	}
}

func TestAdminLegacyOrderMetadataIsEmpty(t *testing.T) {
	newConfTestDB(t)
	result := confRequest(t, Order{}.Create, `{"order_id":"legacy","amount":70}`)
	if result["code"] != float64(200) {
		t.Fatal(result)
	}
	list := confRequest(t, Order{}.List, `{"page":1,"size":10,"sort":"desc"}`)
	row := list["data"].([]any)[0].(map[string]any)
	detail := confRequest(t, Order{}.Detail, fmt.Sprintf(`{"id":%.0f}`, row["id"]))
	for _, data := range []map[string]any{row, detail["data"].(map[string]any)} {
		for _, key := range []string{"user_id", "username", "display_name"} {
			if data[key] != "" {
				t.Fatalf("legacy order has inferred metadata: %v", data)
			}
		}
	}
}

func TestOrderUserMetadataNotInMerchantCallback(t *testing.T) {
	db := newConfTestDB(t)
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read failed", 500)
			return
		}
		received <- body
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	body := `{"order_id":"callback","amount":70,"user_id":"private-id","username":"private-name","display_name":"private-display"}`
	if result := confRequest(t, Order{}.Create, body); result["code"] != float64(200) {
		t.Fatal(result)
	}
	var order model.Order
	if err := db.Where("order_id = ?", "callback").First(&order).Error; err != nil {
		t.Fatal(err)
	}
	if order.UserID == "" || order.Username == "" || order.DisplayName == "" {
		t.Fatal("test requires persisted metadata")
	}
	if err := db.Model(&order).Updates(map[string]any{"notify_url": server.URL, "status": model.OrderStatusSuccess, "api_type": model.OrderApiTypeEpusdtOrder}).Error; err != nil {
		t.Fatal(err)
	}
	if err := notify.Handle(order); err != nil {
		t.Fatal(err)
	}
	callbackBody := <-received
	for _, secret := range []string{"user_id", "username", "display_name", "private-id", "private-name", "private-display"} {
		if strings.Contains(string(callbackBody), secret) {
			t.Fatalf("callback leaked %s: %s", secret, callbackBody)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(callbackBody, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 8 || payload["signature"] != utils.EpusdtSign(payload, model.AuthToken()) {
		t.Fatalf("callback contract/signature changed: %v", payload)
	}
}

func TestAdminCreateRejectsInvalidUserMetadata(t *testing.T) {
	db := newConfTestDB(t)
	for field, limit := range map[string]int{"user_id": 64, "username": 64, "display_name": 128} {
		for _, value := range []string{strings.Repeat("a", limit+1), "\nprivate-value", "private-value\x7f"} {
			body, err := json.Marshal(map[string]any{"order_id": "invalid", "amount": 70, field: value})
			if err != nil {
				t.Fatal(err)
			}
			result := confRequest(t, Order{}.Create, string(body))
			if result["code"] != float64(400) || strings.Contains(fmt.Sprint(result), "private-value") {
				t.Fatalf("invalid metadata accepted or leaked: %v", result)
			}
		}
		result := confRequest(t, Order{}.Create, fmt.Sprintf(`{"order_id":"bad-utf8","amount":70,%q:"bad`+"\xff"+`"}`, field))
		if result["code"] != float64(400) {
			t.Fatal(result)
		}
	}
	var count int64
	if err := db.Model(&model.Order{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("invalid requests wrote orders: %d %v", count, err)
	}
}
