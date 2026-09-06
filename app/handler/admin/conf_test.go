package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	applog "github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
)

func newConfTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "conf.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Conf{}, &model.Order{}, &model.Wallet{}); err != nil {
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
	return db
}

func confRequest(t *testing.T, handler gin.HandlerFunc, body string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestConfURLValidationAndDefaults(t *testing.T) {
	newConfTestDB(t)
	for _, key := range []model.ConfKey{model.RpcEndpointBscRead, model.RpcEndpointBscReadBackups, model.RpcEndpointBscLogBackups} {
		if got := model.GetK(key); got != "" {
			t.Fatalf("%s: %q", key, got)
		}
	}
	if got := model.GetK(model.NotifyWaitingDisabledURLs); got != "[]" {
		t.Fatalf("default: %q", got)
	}
	tests := []struct {
		key, value string
		valid      bool
	}{
		{"rpc_endpoint_bsc_read", "", true},
		{"rpc_endpoint_bsc_read", "https://node.example/v1/key?token=secret", true},
		{"rpc_endpoint_bsc_read", "http://[::1]:18545", true},
		{"rpc_endpoint_bsc_read", "ftp://node.example", false},
		{"rpc_endpoint_bsc_read", "https://", false},
		{"rpc_endpoint_bsc_read", "https://node.example:99999", false},
		{"rpc_endpoint_bsc_read", "https://node.example/#secret", false},
		{"rpc_endpoint_bsc_read", "https://node.example/a b", false},
		{"rpc_endpoint_bsc_read", "https://node.example/%zz", false},
		{"rpc_endpoint_bsc_read", "https://[not-an-ip]", false},
		{"rpc_endpoint_bsc_read", "https://node..example", false},
		{"rpc_endpoint_bsc_read", "https://node.example:", false},
		{"rpc_endpoint_bsc_read", "https://node.example/" + strings.Repeat("a", 512), false},
		{"rpc_endpoint_bsc_read_backups", "https://a.example, http://b.example/rpc", true},
		{"rpc_endpoint_bsc_read_backups", "https://a.example,https://b.example,https://c.example", true},
		{"rpc_endpoint_bsc_read_backups", "https://a.example,https://b.example,https://c.example,https://d.example", false},
		{"rpc_endpoint_bsc_log_backups", "https://a.example,https://b.example,https://c.example,https://d.example", false},
		{"rpc_endpoint_bsc_log_backups", "https://a.example,", false},
		{"rpc_endpoint_bsc_log_backups", "https://a.example,ws://b.example", false},
		{"notify_waiting_disabled_urls", `[]`, true},
		{"notify_waiting_disabled_urls", `["https://merchant.example/webhook?key=A%2FB"]`, true},
		{"notify_waiting_disabled_urls", `null`, false},
		{"notify_waiting_disabled_urls", `{}`, false},
		{"notify_waiting_disabled_urls", `[null]`, false},
		{"notify_waiting_disabled_urls", `["https://ok.example",4]`, false},
		{"notify_waiting_disabled_urls", `["https://ok.example"] trailing`, false},
		{"notify_waiting_disabled_urls", `[" https://ok.example"]`, false},
		{"notify_waiting_disabled_urls", `["/relative"]`, false},
		{"rpc_endpoint_bsc", "not-a-node", false},
		{"rpc_endpoint_tron", "grpc.trongrid.io:50051", true},
		{"rate_float_USDT_CNY", "0.02", true},
		{"legacy.custom-key", "unchanged", true},
	}
	for _, tt := range tests {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			body, err := json.Marshal(confReq{Key: tt.key, Value: tt.value})
			if err != nil {
				t.Fatal(err)
			}
			result := confRequest(t, Conf{}.Set, string(body))
			if (result["code"] == float64(200)) != tt.valid {
				t.Fatalf("valid=%v result=%v", tt.valid, result)
			}
			if !tt.valid && strings.Contains(result["msg"].(string), "secret") {
				t.Fatalf("credential leaked: %v", result)
			}
		})
	}
}

func TestConfSetsAreAtomicAndPreserveUnsubmittedValues(t *testing.T) {
	db := newConfTestDB(t)
	model.SetK(model.RpcEndpointBsc, "https://original.example")
	model.SetK(model.RpcEndpointBscRead, "https://old-read.example")
	bad := `[{"key":"rpc_endpoint_bsc_read","value":"https://new.example"},{"key":"rpc_endpoint_bsc_log_backups","value":"invalid"}]`
	if r := confRequest(t, Conf{}.Sets, bad); r["code"] != float64(400) {
		t.Fatal(r)
	}
	if got := model.GetK(model.RpcEndpointBscRead); got != "https://old-read.example" {
		t.Fatalf("partial validation write: %s", got)
	}
	if err := db.Exec(`CREATE TRIGGER fail_conf BEFORE INSERT ON bep_conf WHEN NEW.k = 'rpc_endpoint_bsc_log_backups' BEGIN SELECT RAISE(ABORT, 'test rollback'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	valid := `[{"key":"rpc_endpoint_bsc_read","value":"https://new.example"},{"key":"rpc_endpoint_bsc_log_backups","value":"https://backup.example"}]`
	if r := confRequest(t, Conf{}.Sets, valid); r["code"] != float64(500) {
		t.Fatal(r)
	}
	if got := model.GetK(model.RpcEndpointBscRead); got != "https://old-read.example" {
		t.Fatalf("partial transaction write: %s", got)
	}
	if got := model.GetC(model.RpcEndpointBscRead); got != "https://old-read.example" {
		t.Fatalf("uncommitted cache: %s", got)
	}
	if err := db.Exec("DROP TRIGGER fail_conf").Error; err != nil {
		t.Fatal(err)
	}
	if r := confRequest(t, Conf{}.Sets, valid); r["code"] != float64(200) {
		t.Fatal(r)
	}
	if got := model.GetC(model.RpcEndpointBscRead); got != "https://new.example" {
		t.Fatalf("cache not refreshed: %s", got)
	}
	if got := model.GetK(model.RpcEndpointBsc); got != "https://original.example" {
		t.Fatalf("unsubmitted config overwritten: %s", got)
	}
	if r := confRequest(t, Conf{}.Sets, `[{"key":"rpc_endpoint_bsc_read","value":""}]`); r["code"] != float64(200) {
		t.Fatal(r)
	}
	if got := model.GetK(model.RpcEndpointBscRead); got != "" {
		t.Fatalf("cannot clear optional config: %s", got)
	}
	if r := confRequest(t, Conf{}.Sets, `[{"key":"rpc_endpoint_bsc_read","value":""},{"key":"rpc_endpoint_bsc_read","value":"https://a.example"}]`); r["code"] != float64(400) {
		t.Fatal(r)
	}
	if r := confRequest(t, Conf{}.Sets, `[{"key":"rate_float_USDT_CNY","value":"1.02"},{"key":"rpc_endpoint_bsc_read","value":""}]`); r["code"] != float64(200) {
		t.Fatal(r)
	}
	if got := model.GetK(model.ConfKey("rate_float_USDT_CNY")); got != "1.02" {
		t.Fatalf("dynamic rate config rejected: %q", got)
	}
}

func TestRpcDiagnostics(t *testing.T) {
	db := newConfTestDB(t)
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	missing := rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if missing.Available || missing.Idle != nil {
		t.Fatalf("missing table presented as healthy: %+v", missing)
	}
	if err := db.AutoMigrate(&model.EVMScanJob{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	jobs := []map[string]any{
		{"network": "bsc", "kind": "live", "order_id": 0, "next_block": int64(106), "target_block": int64(110), "completed": false, "next_retry_at": now.Add(time.Minute), "last_error": "timeout https://user:password@rpc.example/api-secret?token=hidden", "updated_at": now.Add(time.Second)},
		{"network": "bsc", "kind": "order", "order_id": 1, "next_block": int64(2001), "target_block": int64(2000), "completed": true, "last_error": "", "updated_at": now},
	}
	for _, job := range jobs {
		if err := db.Table("bep_evm_scan_job").Create(job).Error; err != nil {
			t.Fatal(err)
		}
	}
	created := now.Add(-time.Hour)
	order := model.Order{TradeId: "confirming-test", TradeType: model.UsdtBep20, Status: model.OrderStatusConfirming, ConfirmedAt: &created, ExpiredAt: now.Add(-time.Minute), AutoTimeAt: model.AutoTimeAt{CreatedAt: (*model.Datetime)(&created), UpdatedAt: (*model.Datetime)(&now)}}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	d := rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if !d.Available || d.BacklogJobs != 1 || d.Idle == nil || *d.Idle || d.NextRetry == nil || !d.NextRetry.Equal(now.Add(time.Minute)) {
		t.Fatalf("diagnostics: %+v", d)
	}
	if d.CompletedLiveHeight == nil || *d.CompletedLiveHeight != 105 {
		t.Fatalf("live height must exclude historical range: %+v", d)
	}
	if d.OldestConfirming == nil || !d.OldestConfirming.Equal(created) {
		t.Fatalf("oldest confirming: %+v", d)
	}
	for _, secret := range []string{"password", "api-secret", "hidden", "rpc.example"} {
		if strings.Contains(d.LastError, secret) {
			t.Fatalf("error leaks credentials: %s", d.LastError)
		}
	}
	if d.LastError == "" {
		t.Fatal("missing error classification")
	}
	if err := db.Table("bep_evm_scan_job").Where("completed = ?", false).Updates(map[string]any{"completed": true, "next_block": 111}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Order{}).Where("trade_id = ?", order.TradeId).Update("status", model.OrderStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	d = rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if d.Idle == nil || !*d.Idle || d.BacklogJobs != 0 || d.NextRetry != nil || d.OldestConfirming != nil || *d.CompletedLiveHeight != 110 {
		t.Fatalf("idle diagnostics: %+v", d)
	}
	if err := db.Where("kind = ?", "live").Delete(&model.EVMScanJob{}).Error; err != nil {
		t.Fatal(err)
	}
	d = rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if d.CompletedLiveHeight != nil {
		t.Fatalf("order-only scans cannot be a live watermark: %+v", d)
	}
	if err := db.Model(&model.Order{}).Where("trade_id = ?", order.TradeId).Update("status", model.OrderStatusConfirming).Error; err != nil {
		t.Fatal(err)
	}
	d = rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if d.Idle == nil || *d.Idle {
		t.Fatalf("confirming orders are not idle: %+v", d)
	}
	if err := db.Model(&model.Order{}).Where("trade_id = ?", order.TradeId).Update("status", model.OrderStatusSuccess).Error; err != nil {
		t.Fatal(err)
	}
	model.SetK(model.MqttNetworks, "bsc")
	d = rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if d.Idle == nil || *d.Idle {
		t.Fatalf("continuous monitoring is not idle: %+v", d)
	}
	model.SetK(model.MqttNetworks, "")
	if err := db.Migrator().DropTable(&model.Order{}); err != nil {
		t.Fatal(err)
	}
	d = rpcDiagnostics(db.WithContext(ctx))["bsc"]
	if d.Available || d.Idle != nil {
		t.Fatalf("query failure cannot imply idle: %+v", d)
	}
}

func TestConfRPCResponseAndReadFailure(t *testing.T) {
	db := newConfTestDB(t)
	model.SetK(model.RpcEndpointBsc, "https://existing.example")
	result := confRequest(t, Conf{}.Rpc, "")
	if result["code"] != float64(200) {
		t.Fatal(result)
	}
	data := result["data"].(map[string]any)
	rpc := data["rpc"].(map[string]any)
	if rpc["rpc_endpoint_bsc"] != "https://existing.example" || rpc["notify_waiting_disabled_urls"] != "[]" || rpc["rpc_endpoint_bsc_read"] != "" {
		t.Fatalf("RPC defaults overwrote existing values: %v", rpc)
	}
	diagnostics := data["diagnostics"].(map[string]any)
	if len(diagnostics) != 1 || diagnostics["bsc"].(map[string]any)["available"] != false {
		t.Fatalf("unexpected diagnostics: %v", diagnostics)
	}
	if err := db.Migrator().DropTable(&model.Conf{}); err != nil {
		t.Fatal(err)
	}
	if result := confRequest(t, Conf{}.Rpc, ""); result["code"] != float64(500) {
		t.Fatal(result)
	}
}

func TestConfBadInputAndSingleWriteFailure(t *testing.T) {
	db := newConfTestDB(t)
	for _, handler := range []gin.HandlerFunc{Conf{}.Set, Conf{}.Sets} {
		if result := confRequest(t, handler, "{"); result["code"] != float64(400) {
			t.Fatal(result)
		}
	}
	if result := confRequest(t, Conf{}.Set, `{"key":"   ","value":"value"}`); result["code"] != float64(400) {
		t.Fatal(result)
	}
	if err := db.Exec(`CREATE TRIGGER fail_single_conf BEFORE INSERT ON bep_conf BEGIN SELECT RAISE(ABORT, 'do not expose secret'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	result := confRequest(t, Conf{}.Set, `{"key":"rpc_endpoint_bsc_read","value":"https://valid.example"}`)
	if result["code"] != float64(500) || strings.Contains(result["msg"].(string), "secret") {
		t.Fatal(result)
	}
}

func TestDiagnosticErrorNeverEchoesUpstreamContent(t *testing.T) {
	cases := []struct{ input, want string }{
		{"429 too many requests at https://secret.example", "RPC 请求限流"},
		{"archive requires personal secret token", "RPC 历史查询受限"},
		{"deadline expired secret", "RPC 请求超时"},
		{"403 forbidden secret", "RPC 认证或访问被拒绝"},
		{"dial tcp secret", "RPC 网络连接失败"},
		{"invalid response secret", "RPC 响应校验失败"},
		{"secret-with-no-recognizable-format", "RPC 扫描失败"},
	}
	for _, tt := range cases {
		if got := diagnosticError(tt.input); got != tt.want || strings.Contains(got, "secret") {
			t.Fatalf("error classification: %q", got)
		}
	}
}
