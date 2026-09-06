package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/handler/base"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/notifier"
	"github.com/v03413/bepusdt/app/utils"
	"gorm.io/gorm"
)

type Conf struct {
}

type confReq struct {
	Key   string `json:"key" binding:"required"`
	Value string `json:"value"`
}

type confGetsReq struct {
	Keys []string `json:"keys" binding:"required"`
}

type confSetsReq []confReq

type notifierConf struct {
	Channel string          `json:"channel" binding:"required"`
	Params  json.RawMessage `json:"params" binding:"required"`
}

func (Conf) Set(ctx *gin.Context) {
	var req confReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	values, err := validateConfValues(confSetsReq{req})
	if err != nil {
		base.BadRequest(ctx, err.Error())
		return
	}
	if err := model.SetConfs(values); err != nil {
		base.Error(ctx, errors.New("配置保存失败"))
		return
	}

	base.Ok(ctx, "配置成功")
}

func (Conf) Get(ctx *gin.Context) {
	var req confReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	base.Ok(ctx, gin.H{"key": req.Key, "value": model.GetK(model.ConfKey(req.Key))})
}

func (Conf) Del(ctx *gin.Context) {
	var req confReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	model.Db.Where("k = ?", req.Key).Delete(&model.Conf{})

	base.Ok(ctx, "删除成功")
}

func (Conf) Gets(ctx *gin.Context) {
	var req confGetsReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	var items = make([]model.Conf, 0)
	model.Db.Where("k IN ?", req.Keys).Find(&items)

	var data = gin.H{}
	for _, item := range items {
		data[string(item.K)] = item.V
	}

	base.Ok(ctx, data)
}

func (Conf) Sets(ctx *gin.Context) {
	var req confSetsReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	values, err := validateConfValues(req)
	if err != nil {
		base.BadRequest(ctx, err.Error())
		return
	}
	if err := model.SetConfs(values); err != nil {
		base.Error(ctx, errors.New("配置保存失败"))
		return
	}

	base.Ok(ctx, "配置成功")
}

func (Conf) Rpc(ctx *gin.Context) {
	var keys = []model.ConfKey{
		model.RpcEndpointPlasma,
		model.RpcEndpointBsc,
		model.RpcEndpointBscRead,
		model.RpcEndpointBscReadBackups,
		model.RpcEndpointBscLogBackups,
		model.NotifyWaitingDisabledURLs,
		model.RpcEndpointSolana,
		model.RpcEndpointXlayer,
		model.RpcEndpointPolygon,
		model.RpcEndpointArbitrum,
		model.RpcEndpointEthereum,
		model.RpcEndpointBase,
		model.RpcEndpointAptos,
		model.RpcEndpointTron,
		model.RpcEndpointTronGridApiKey,
		model.RpcGlobalConfigUrlTon,
	}

	var rpc = map[model.ConfKey]string{
		model.RpcEndpointBscRead:        "",
		model.RpcEndpointBscReadBackups: "",
		model.RpcEndpointBscLogBackups:  "",
		model.NotifyWaitingDisabledURLs: "[]",
	}
	var items = make([]model.Conf, 0)
	queryCtx, cancel := context.WithTimeout(ctx.Request.Context(), 2*time.Second)
	defer cancel()
	db := model.Db.WithContext(queryCtx)
	if err := db.Where("k IN ?", keys).Find(&items).Error; err != nil {
		base.Error(ctx, errors.New("配置读取失败"))
		return
	}
	for _, item := range items {
		rpc[item.K] = item.V
	}

	base.Ok(ctx, gin.H{
		"rpc":         rpc,
		"stats":       conf.GetStats(),
		"diagnostics": rpcDiagnostics(db),
	})
}

func validateConfValues(req confSetsReq) ([]model.Conf, error) {
	values := make([]model.Conf, 0, len(req))
	seen := make(map[string]bool, len(req))
	for _, item := range req {
		key := strings.TrimSpace(item.Key)
		value := strings.TrimSpace(item.Value)
		if key == "" {
			return nil, errors.New("配置键不能为空")
		}
		if seen[key] {
			return nil, errors.New("配置键重复")
		}
		seen[key] = true
		if err := validateRPCValue(model.ConfKey(key), value); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		values = append(values, model.Conf{K: model.ConfKey(key), V: value})
	}
	return values, nil
}

func validateRPCValue(key model.ConfKey, value string) error {
	var urls []string
	switch key {
	case model.RpcEndpointBscRead:
		if value != "" {
			urls = []string{value}
		}
	case model.RpcEndpointBscReadBackups, model.RpcEndpointBscLogBackups:
		if value != "" {
			urls = strings.Split(value, ",")
			if len(urls) > 3 {
				return errors.New("备用 RPC 最多配置 3 个")
			}
			for i := range urls {
				urls[i] = strings.TrimSpace(urls[i])
			}
		}
	case model.NotifyWaitingDisabledURLs:
		if err := json.Unmarshal([]byte(value), &urls); err != nil || urls == nil {
			return errors.New("必须是 URL 字符串的 JSON 数组")
		}
	case model.RpcEndpointBsc, model.RpcEndpointSolana, model.RpcEndpointXlayer,
		model.RpcEndpointPolygon, model.RpcEndpointArbitrum, model.RpcEndpointEthereum,
		model.RpcEndpointBase, model.RpcEndpointAptos, model.RpcEndpointPlasma, model.RpcGlobalConfigUrlTon:
		urls = []string{value}
	default:
		return nil // Tron uses gRPC host:port; other settings retain their existing contracts.
	}
	if utf8.RuneCountInString(value) > 512 {
		return errors.New("配置不能超过 512 个字符")
	}
	for i, raw := range urls {
		if !validHTTPURL(raw) {
			return fmt.Errorf("第 %d 项必须是完整的 HTTP(S) URL，不能含空白或片段", i+1)
		}
	}
	return nil
}

func validHTTPURL(raw string) bool {
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 || strings.ContainsAny(raw, "\\#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return false
	}
	if strings.Contains(u.Hostname(), ":") && net.ParseIP(u.Hostname()) == nil {
		return false
	}
	if strings.HasPrefix(u.Host, "[") && net.ParseIP(u.Hostname()) == nil {
		return false
	}
	if net.ParseIP(u.Hostname()) == nil {
		for _, label := range strings.Split(strings.TrimSuffix(u.Hostname(), "."), ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return false
			}
			if strings.IndexFunc(label, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' }) >= 0 {
				return false
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return false
	}
	return true
}

type rpcDiagnostic struct {
	Available           bool       `json:"available"`
	BacklogJobs         int64      `json:"backlog_jobs"`
	NextRetry           *time.Time `json:"next_retry"`
	LastError           string     `json:"last_error"`
	LastErrorAt         *time.Time `json:"last_error_at"`
	CompletedLiveHeight *int64     `json:"completed_live_height"`
	OldestConfirming    *time.Time `json:"oldest_confirming"`
	Idle                *bool      `json:"idle"`
}

// Only BSC uses the durable scheduler. Legacy-chain counters are not durable watermarks.
func rpcDiagnostics(db *gorm.DB) map[string]rpcDiagnostic {
	d := rpcDiagnostic{}
	result := map[string]rpcDiagnostic{conf.Bsc: d}
	if !db.Migrator().HasTable(&model.EVMScanJob{}) {
		return result
	}
	jobs := func() *gorm.DB { return db.Model(&model.EVMScanJob{}).Where("network = ?", conf.Bsc) }
	if err := jobs().Where("completed = ?", false).Count(&d.BacklogJobs).Error; err != nil {
		return result
	}
	var retry []model.EVMScanJob
	if err := jobs().Select("next_retry_at").Where("completed = ? AND next_retry_at > ?", false, time.Time{}).
		Order("next_retry_at asc").Limit(1).Find(&retry).Error; err != nil {
		return result
	}
	if len(retry) > 0 {
		d.NextRetry = &retry[0].NextRetryAt
	}
	var failed []model.EVMScanJob
	if err := jobs().Select("last_error", "updated_at").Where("last_error <> ?", "").Order("updated_at desc").Limit(1).Find(&failed).Error; err != nil {
		return result
	}
	if len(failed) > 0 {
		d.LastError = diagnosticError(failed[0].LastError)
		d.LastErrorAt = &failed[0].UpdatedAt
	}
	var live []model.EVMScanJob
	if err := jobs().Select("next_block").Where("(kind = ? OR (kind = ? AND completed = ?)) AND next_block > ?", "live", "recovery", false, 0).
		Order("next_block asc").Limit(1).Find(&live).Error; err != nil {
		return result
	}
	if len(live) > 0 {
		height := live[0].NextBlock - 1
		d.CompletedLiveHeight = &height
	}
	trades := model.GetNetworkTrades(model.Network(conf.Bsc))
	var orders []model.Order
	if err := db.Select("created_at").Where("trade_type IN ? AND status = ?", trades, model.OrderStatusConfirming).
		Order("created_at asc").Limit(1).Find(&orders).Error; err != nil {
		return result
	}
	if len(orders) > 0 && orders[0].CreatedAt != nil {
		created := orders[0].CreatedAt.Time()
		d.OldestConfirming = &created
	}
	var waiting, monitoring int64
	if err := db.Model(&model.Order{}).Where("trade_type IN ? AND (status = ? OR (status IN ? AND expired_at > ?))",
		trades, model.OrderStatusConfirming, []int{model.OrderStatusWaiting, model.OrderStatusExpired}, time.Now().Add(model.GetLookbackHour())).
		Count(&waiting).Error; err != nil {
		return result
	}
	if err := db.Model(&model.Wallet{}).Where("trade_type IN ? AND other_notify = ?", trades, model.WaOtherEnable).
		Count(&monitoring).Error; err != nil {
		return result
	}
	idle := d.BacklogJobs == 0 && waiting == 0 && monitoring == 0
	for _, network := range strings.Split(model.GetC(model.MqttNetworks), ",") {
		if strings.TrimSpace(network) == conf.Bsc {
			idle = false
		}
	}
	d.Idle = &idle
	d.Available = true
	result[conf.Bsc] = d
	return result
}

// Raw node errors may contain credentials in URLs, headers or arbitrary upstream text.
// Expose only a bounded classification, never interpolate any part of that text.
func diagnosticError(message string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "429"), strings.Contains(message, "rate limit"), strings.Contains(message, "too many requests"):
		return "RPC 请求限流"
	case strings.Contains(message, "archive"), strings.Contains(message, "historical"):
		return "RPC 历史查询受限"
	case strings.Contains(message, "timeout"), strings.Contains(message, "timed out"), strings.Contains(message, "deadline"):
		return "RPC 请求超时"
	case strings.Contains(message, "401"), strings.Contains(message, "403"), strings.Contains(message, "unauthorized"), strings.Contains(message, "forbidden"):
		return "RPC 认证或访问被拒绝"
	case strings.Contains(message, "connection"), strings.Contains(message, "dial"), strings.Contains(message, "no such host"), strings.Contains(message, "tls"):
		return "RPC 网络连接失败"
	case strings.Contains(message, "invalid"), strings.Contains(message, "mismatch"), strings.Contains(message, "malformed"):
		return "RPC 响应校验失败"
	default:
		return "RPC 扫描失败"
	}
}

func (Conf) Notifier(ctx *gin.Context) {
	var req notifierConf
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	var keys = []string{string(model.NotifierChannel), string(model.NotifierParams)}
	model.Db.Where("k IN ?", keys).Delete(&model.Conf{})
	model.Db.Create(&[]model.Conf{
		{K: model.NotifierChannel, V: req.Channel},
		{K: model.NotifierParams, V: string(req.Params)},
	})

	base.Ok(ctx, "配置成功")
}

func (Conf) NotifierTest(ctx *gin.Context) {
	err := notifier.Test()
	if err != nil {
		base.Ok(ctx, "发送测试失败："+err.Error())

		return
	}

	base.Ok(ctx, "发送测试成功")
}

func (Conf) CheckoutList(ctx *gin.Context) {
	base.Ok(ctx, model.CheckoutList())
}

func (Conf) ResetApiAuthToken(ctx *gin.Context) {
	model.SetK(model.ApiAuthToken, strings.ToUpper(utils.Md5String(utils.StrSha256(time.Now().String()))))

	base.Ok(ctx, "重置成功")
}
