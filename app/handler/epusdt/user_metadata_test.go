package epusdt

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	applog "github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/utils"
	"gorm.io/gorm"
)

func metadataDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "orders.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Order{}, &model.Conf{}, &model.Wallet{}, &model.Rate{}); err != nil {
		t.Fatal(err)
	}
	previous := model.Db
	model.Db = db
	model.FillDefaultConf()
	model.RefreshC()
	if err := applog.Init(filepath.Join(t.TempDir(), "logs")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.Db = previous
		sqlDB, err := db.DB()
		if err == nil {
			sqlDB.Close()
		}
		applog.Close()
	})
	for _, crypto := range []string{"USDT", "USDC"} {
		if err := db.Create(&model.Rate{Crypto: crypto, Fiat: "CNY", Rate: "7", RawRate: 7}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, trade := range []string{"usdt.bep20", "usdc.bep20"} {
		if err := db.Create(&model.Wallet{Address: "0x1111111111111111111111111111111111111111", MatchAddr: "0x1111111111111111111111111111111111111111", TradeType: trade, Status: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func metadataRequest(t *testing.T, handler gin.HandlerFunc, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON response %s: %v", w.Body.String(), err)
	}
	return result
}

func createMetadataBody(orderID, extra string) string {
	return fmt.Sprintf(`{"order_id":%q,"amount":70,"fiat":"CNY","trade_type":"usdt.bep20","notify_url":"https://merchant.example/notify","redirect_url":"https://merchant.example/done","signature":"test"%s}`, orderID, extra)
}

func TestCreateUserMetadataStoredAndPrivate(t *testing.T) {
	db := metadataDB(t)
	for name, handler := range map[string]gin.HandlerFunc{"order": Epusdt{}.CreateOrder, "transaction": Epusdt{}.CreateTransaction} {
		t.Run(name, func(t *testing.T) {
			body := createMetadataBody(name, `,"user_id":"  private-id  ","username":" private-name ","display_name":"  客户显示名  "`)
			result := metadataRequest(t, handler, body)
			if result["status_code"] != float64(200) {
				t.Fatal(result)
			}
			var row map[string]any
			if err := db.Table("bep_order").Select("user_id, username, display_name").Where("order_id = ?", name).Take(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row["user_id"] != "private-id" || row["username"] != "private-name" || row["display_name"] != "客户显示名" {
				t.Fatalf("metadata not normalized/persisted: %v", row)
			}
			assertPrivateResponse(t, result)
			tradeID := result["data"].(map[string]any)["trade_id"].(string)
			info := metadataRequest(t, Epusdt{}.Info, fmt.Sprintf(`{"trade_id":%q}`, tradeID))
			if info["status_code"] != float64(200) {
				t.Fatal(info)
			}
			assertPrivateResponse(t, info)
		})
	}
}

func assertPrivateResponse(t *testing.T, result any) {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"user_id", "username", "display_name", "private-id", "private-name", "客户显示名"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("public response leaked %q: %s", secret, body)
		}
	}
}

func TestCreateUserMetadataValidation(t *testing.T) {
	db := metadataDB(t)
	for name, handler := range map[string]gin.HandlerFunc{"order": Epusdt{}.CreateOrder, "transaction": Epusdt{}.CreateTransaction} {
		for field, limit := range map[string]int{"user_id": 64, "username": 64, "display_name": 128} {
			for label, value := range map[string]string{
				"long": strings.Repeat("x", limit+1), "unicode-long": strings.Repeat("名", limit+1),
				"nul": "a\x00b", "newline": "\nprivate-name", "tab": "private-name\t", "delete": "a\x7fb", "c1": "a\u0085b",
			} {
				t.Run(name+"/"+field+"/"+label, func(t *testing.T) {
					encoded, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					result := metadataRequest(t, handler, createMetadataBody(t.Name(), fmt.Sprintf(`,%q:%s`, field, encoded)))
					if result["status_code"] != float64(400) {
						t.Fatalf("invalid metadata accepted: %v", result)
					}
					if strings.Contains(fmt.Sprint(result), "private-name") {
						t.Fatalf("validation error echoes private value: %v", result)
					}
				})
			}
			for _, invalid := range []string{`123`, `true`, `[]`, `{}`, `null`, "\"bad\xff\""} {
				t.Run(name+"/"+field+"/type-"+invalid, func(t *testing.T) {
					result := metadataRequest(t, handler, createMetadataBody(t.Name(), fmt.Sprintf(`,%q:%s`, field, invalid)))
					if result["status_code"] != float64(400) {
						t.Fatalf("invalid string accepted: %v", result)
					}
				})
			}
		}
	}
	var count int64
	if err := db.Model(&model.Order{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("invalid requests wrote orders: %d %v", count, err)
	}
}

func TestPublicReselectionAndCheckoutKeepUserMetadataPrivate(t *testing.T) {
	db := metadataDB(t)
	created := metadataRequest(t, Epusdt{}.CreateOrder, createMetadataBody("reselection", `,"user_id":"private-id","username":"private-name","display_name":"客户显示名"`))
	if created["status_code"] != float64(200) {
		t.Fatal(created)
	}
	tradeID := created["data"].(map[string]any)["trade_id"].(string)
	for _, currency := range []string{"USDT", "USDC", "USDC"} {
		methods := metadataRequest(t, Epusdt{}.GetMethods, fmt.Sprintf(`{"trade_id":%q}`, tradeID))
		if methods["status_code"] != float64(200) {
			t.Fatal(methods)
		}
		assertPrivateResponse(t, methods)
		// Extra untrusted fields on a public selection must never modify the snapshot.
		result := metadataRequest(t, Epusdt{}.UpdateOrder, fmt.Sprintf(`{"trade_id":%q,"currency":%q,"network":"bsc","user_id":"attacker","username":"renamed","display_name":""}`, tradeID, currency))
		if result["status_code"] != float64(200) {
			t.Fatal(result)
		}
		assertPrivateResponse(t, result)
		var order model.Order
		if err := db.Where("trade_id = ?", tradeID).First(&order).Error; err != nil {
			t.Fatal(err)
		}
		if order.UserID != "private-id" || order.Username != "private-name" || order.DisplayName != "客户显示名" {
			t.Fatalf("public reselection lost snapshot: %+v", order)
		}
		assertPrivateResponse(t, metadataRequest(t, Epusdt{}.Info, fmt.Sprintf(`{"trade_id":%q}`, tradeID)))
	}
	engine := gin.New()
	engine.SetHTMLTemplate(template.Must(template.New("official/checkout.html").Parse(`{{ . | printf "%v" }}`)))
	engine.GET("/pay/checkout/:trade_id", Epusdt{}.Checkout)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/pay/checkout/"+tradeID, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tradeID) {
		t.Fatalf("checkout failed: %s", w.Body.String())
	}
	assertPrivateResponse(t, w.Body.String())
}

func TestCreateUserMetadataSignedRequestsAndLegacyCompatibility(t *testing.T) {
	metadataDB(t)
	for name, handler := range map[string]gin.HandlerFunc{"order": Epusdt{}.CreateOrder, "transaction": Epusdt{}.CreateTransaction} {
		engine := gin.New()
		engine.POST("/", Epusdt{}.SignVerify, handler)
		for _, extra := range []string{"", `,"user_id":" private-id ","username":"private-name","display_name":"客户显示名"`} {
			var data map[string]any
			if err := json.Unmarshal([]byte(createMetadataBody(name+fmt.Sprint(len(extra)), extra)), &data); err != nil {
				t.Fatal(err)
			}
			data["signature"] = utils.EpusdtSign(data, model.AuthToken())
			body, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(w, req)
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["status_code"] != float64(200) {
				t.Fatalf("signed request rejected: %s %v", w.Body.String(), err)
			}
			assertPrivateResponse(t, result)
			if extra != "" {
				data["user_id"] = "tampered"
				tampered, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				w = httptest.NewRecorder()
				req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(tampered)))
				req.Header.Set("Content-Type", "application/json")
				engine.ServeHTTP(w, req)
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["status_code"] != float64(400) {
					t.Fatalf("unsigned metadata change accepted: %s %v", w.Body.String(), err)
				}
			}
		}
	}
}

func TestCreateUserMetadataGatewaySignatureVector(t *testing.T) {
	// Same fixed vector as NewAPI: the existing gateway trims trailing ampersands only when signing.
	for _, username := range []string{"alice&", "alice&&"} {
		t.Run(username, func(t *testing.T) {
			db := metadataDB(t)
			if err := model.SetConfs([]model.Conf{{K: model.ApiAuthToken, V: "metadata-vector-token"}}); err != nil {
				t.Fatal(err)
			}
			data := map[string]any{
				"amount": 70, "fiat": "CNY", "notify_url": "https://merchant.example/notify",
				"order_id": "metadata-signature-vector", "redirect_url": "https://merchant.example/return",
				"user_id": "485", "username": username, "display_name": "测试用户",
				"signature": "c2a44ccf737837b9bd6339637b0d7b58",
			}
			body, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			engine := gin.New()
			engine.POST("/", Epusdt{}.SignVerify, Epusdt{}.CreateOrder)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["status_code"] != float64(200) {
				t.Fatalf("NewAPI signature vector rejected: %s %v", w.Body.String(), err)
			}
			var order model.Order
			if err := db.Where("order_id = ?", "metadata-signature-vector").First(&order).Error; err != nil {
				t.Fatal(err)
			}
			if order.Username != username || order.UserID != "485" || order.DisplayName != "测试用户" {
				t.Fatalf("signed snapshot changed: %q %q %q", order.UserID, order.Username, order.DisplayName)
			}
			data["user_id"] = "486"
			tampered, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			w = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(tampered)))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(w, req)
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["status_code"] != float64(400) {
				t.Fatalf("user ID tampering bypassed signature: %s %v", w.Body.String(), err)
			}
		})
	}
}
