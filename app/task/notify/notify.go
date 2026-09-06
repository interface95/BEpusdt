package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cast"
	"github.com/v03413/bepusdt/app"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/notifier"
	"github.com/v03413/bepusdt/app/utils"

	"github.com/v03413/go-cache"
	"gorm.io/gorm"
)

type EpNotify struct {
	TradeId            string  `json:"trade_id"`             //  本地订单号
	OrderId            string  `json:"order_id"`             //  客户交易id
	Amount             float64 `json:"amount"`               //  订单金额 CNY
	ActualAmount       string  `json:"actual_amount"`        //  USDT 交易数额
	Token              string  `json:"token"`                //  收款钱包地址
	BlockTransactionId string  `json:"block_transaction_id"` // 区块id
	Signature          string  `json:"signature"`            // 签名
	Status             int     `json:"status"`               //  1：等待支付，2：支付成功，3：订单超时
}

func Handle(order model.Order) error {
	return handle(order, false)
}

// HandleManual preserves the admin's explicit request to resend a paid order.
// Automatic retries skip an already acknowledged order; both paths share a lease.
func HandleManual(order model.Order) error {
	return handle(order, true)
}

func handle(snapshot model.Order, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var order model.Order
	if err := model.Db.WithContext(ctx).First(&order, snapshot.ID).Error; err != nil {
		return err
	}
	if order.Status != model.OrderStatusSuccess {
		return errors.New("订单未支付 无法回调")
	}
	if !force && order.NotifyState == model.OrderNotifyStateSucc {
		return nil
	}
	if err := claimNotifyLease(model.Db.WithContext(ctx), &order); err != nil {
		return err
	}

	var err error
	if order.ApiType == model.OrderApiTypeEpay {
		err = epay(ctx, order)
	} else {
		err = epusdt(ctx, order)
	}

	if persistErr := finishNotifyLease(model.Db, order, err); persistErr != nil {
		return errors.Join(err, persistErr)
	}
	if err != nil {
		markNotifyFail(order, err.Error())
		return err
	}

	log.Info("订单回调成功：", order.TradeId)

	return nil
}

func epay(ctx context.Context, order model.Order) error {
	var notifyUrl = fmt.Sprintf("%s?%s", order.NotifyUrl, order.BuildNotifyParams())

	postReq, err2 := http.NewRequestWithContext(ctx, "GET", notifyUrl, nil)
	if err2 != nil {
		return errors.New("invalid callback URL")
	}

	postReq.Header.Set("Powered-By", "https://github.com/v03413/bepusdt")
	return deliverTerminal(postReq)
}

func epusdt(ctx context.Context, order model.Order) error {
	jsonBody, err := epusdtPayload(order, model.AuthToken())
	if err != nil {
		return err
	}
	postReq, err := http.NewRequestWithContext(ctx, "POST", order.NotifyUrl, strings.NewReader(string(jsonBody)))
	if err != nil {
		return errors.New("invalid callback URL")
	}
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("Powered-By", "https://github.com/v03413/bepusdt")
	postReq.Header.Set("User-Agent", "BEpusdt/"+app.Version)
	return deliverTerminal(postReq)
}

func deliverTerminal(req *http.Request) error {
	client := http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("callback request timed out")
		}
		// url.Error includes the entire callback URL, which can contain secrets.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return errors.New("callback transport error")
		}
		return errors.New("callback request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("callback HTTP status %d; expected 200", resp.StatusCode)
	}
	const maxACKBytes = 4096
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxACKBytes+1))
	if err != nil {
		return errors.New("callback response read failed")
	}
	ack := strings.TrimSpace(string(body))
	if len(body) > maxACKBytes || (!strings.EqualFold(ack, "ok") && !strings.EqualFold(ack, "success")) {
		return errors.New("callback response must be exactly ok or success")
	}
	return nil
}

func epusdtPayload(order model.Order, authToken string) ([]byte, error) {
	var data = make(map[string]interface{})
	var body = EpNotify{
		TradeId:            order.TradeId,
		OrderId:            order.OrderId,
		Amount:             cast.ToFloat64(order.Money),
		ActualAmount:       order.Amount,
		Token:              order.Address,
		BlockTransactionId: order.RefHash,
		Status:             order.Status,
	}
	var jsonBody, err = json.Marshal(body)
	if err != nil {
		return nil, err
	}

	if err = json.Unmarshal(jsonBody, &data); err != nil {
		return nil, err
	}

	// 签名
	body.Signature = utils.EpusdtSign(data, authToken)

	// 再次序列化
	return json.Marshal(body)
}

func Bepusdt(o model.Order) {
	if o.ApiType != model.OrderApiTypeEpusdt && o.ApiType != model.OrderApiTypeEpusdtOrder {

		return
	}

	var authToken = model.AuthToken()
	var client = &http.Client{Timeout: time.Second * 5}
	go func() {
		if err := deliverBepusdtStatusUpdate(model.Db, client, authToken, o); err != nil {
			log.Warn("notify BEpusdt Error:", err.Error())
		}
	}()
}

func deliverBepusdtStatusUpdate(db *gorm.DB, client *http.Client, authToken string, o model.Order) (deliveryErr error) {
	if o.Status != model.OrderStatusWaiting && o.Status != model.OrderStatusExpired {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: time.Second * 5}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var current model.Order
	tx := db.WithContext(ctx).Where("trade_id = ? and status = ?", o.TradeId, o.Status).Limit(1).Find(&current)
	if tx.Error != nil {
		return tx.Error
	}
	if tx.RowsAffected == 0 {
		return nil
	}
	if current.Status == model.OrderStatusWaiting && waitingDisabled(current.NotifyUrl) {
		return nil
	}

	var key = fmt.Sprintf("bepusdt_notify_%d_%s", current.Status, current.TradeId)
	if _, ok := cache.Get(key); ok {
		return nil
	}

	if err := claimNotifyLease(db.WithContext(ctx), &current); err != nil {
		return err
	}
	defer func() { deliveryErr = errors.Join(deliveryErr, releaseNotifyLease(db, current)) }()
	if _, ok := cache.Get(key); ok {
		return nil
	}
	jsonBody, err := epusdtPayload(current, authToken)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", current.NotifyUrl, strings.NewReader(string(jsonBody)))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Powered-By", "https://github.com/v03413/BEpusdt")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("resp.StatusCode != 200")
	}

	all, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	cache.Set(key, true, time.Minute)
	log.Info(fmt.Sprintf("订单回调成功[%d]：%s %s", current.Status, current.TradeId, string(all)))

	return nil
}

func waitingDisabled(notifyURL string) bool {
	var urls []string
	if err := json.Unmarshal([]byte(model.GetC(model.ConfKey("notify_waiting_disabled_urls"))), &urls); err != nil {
		return false
	}
	for _, candidate := range urls {
		if candidate == notifyURL {
			return true
		}
	}
	return false
}

func markNotifyFail(o model.Order, reason string) {
	log.Warn(fmt.Sprintf("订单回调失败(%v)：%s", o.TradeId, reason))

	notifier.NotifyFail(o, reason)
}
