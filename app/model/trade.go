package model

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/spf13/cast"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/utils"
)

type OrderParams struct {
	Money             decimal.Decimal `json:"money"`        // 交易金额 (单位：法币)s
	ApiType           string          `json:"api_type"`     // 支付 API 类型
	Address           string          `json:"address"`      // 收款地址
	OrderId           string          `json:"order_id"`     // 商户订单 ID
	TradeType         TradeType       `json:"trade_type"`   // 交易类型
	RedirectUrl       string          `json:"redirect_url"` // 成功跳转地址
	NotifyUrl         string          `json:"notify_url"`   // 异步通知地址
	Name              string          `json:"name"`         // 商品名称
	UserID            string          `json:"user_id"`
	Username          string          `json:"username"`
	DisplayName       string          `json:"display_name"`
	Timeout           int64           `json:"timeout"`            // 订单超时时间（秒）
	Rate              string          `json:"rate"`               // 强制指定汇率
	Fiat              Fiat            `json:"fiat"`               // 法币类型
	CurrencyLimit     string          `json:"currency_limit"`     // 限定币种
	AddressLocked     bool            `json:"address_locked"`     // 地址独占锁定
	ClientFingerprint string          `json:"client_fingerprint"` //  客户端指纹
}

type Addr struct {
	Address   string // 对外前端收款地址
	MatchAddr string // 对内订单校验地址
}

type Trade struct {
	Wallet Wallet
	Crypto Crypto
	Rate   decimal.Decimal
	Amount string
}

var buildMutex sync.Mutex

func StartBuildOrder(p OrderParams) (Order, error) {
	var order Order
	p, err := normalizeOrderUserMetadata(p)
	if err != nil {
		return order, err
	}

	if p.Address != "" {
		if !utils.IsValidTronAddress(p.Address) &&
			!utils.IsValidEvmAddress(p.Address) &&
			!utils.IsValidSolanaAddress(p.Address) &&
			!utils.IsValidAptosAddress(p.Address) &&
			!utils.IsValidTonAddress(p.Address) {

			return order, fmt.Errorf("钱包地址格式错误：%s", p.Address)
		}
	}
	if _, ok := registry[p.TradeType]; !ok {
		return order, fmt.Errorf("不支持的交易类型：%s", p.TradeType)
	}
	if _, ok := supportFiat[p.Fiat]; !ok {
		return order, fmt.Errorf("不支持的法币类型：%s", p.Fiat)
	}
	maxAmount := decimal.NewFromFloat(cast.ToFloat64(GetC(PaymentMaxAmount)))
	minAmount := decimal.NewFromFloat(cast.ToFloat64(GetC(PaymentMinAmount)))
	if !p.AddressLocked && (p.Money.GreaterThan(maxAmount) || p.Money.LessThan(minAmount)) {
		return order, fmt.Errorf("交易金额必须在 %s - %s 之间", minAmount.String(), maxAmount.String())
	}

	if err := Db.Where("order_id = ?", p.OrderId).Order("id desc").Limit(1).Find(&order).Error; err != nil {
		return order, err
	}
	if order.Status == OrderStatusSuccess || order.Status == OrderStatusConfirming {
		return saveOrderUserMetadata(order, p)
	}

	buildMutex.Lock()
	defer buildMutex.Unlock()

	if err := Db.Where("order_id = ?", p.OrderId).Order("id desc").Limit(1).Find(&order).Error; err != nil {
		return order, err
	}
	if order.Status == OrderStatusSuccess || order.Status == OrderStatusConfirming {
		return saveOrderUserMetadata(order, p)
	}
	if order.Status == OrderStatusWaiting {
		return RebuildOrder(order, p)
	}
	p, err = mergeOrderUserMetadata(order, p)
	if err != nil {
		return order, err
	}

	data, err := BuildTrade(p)
	if err != nil {
		return order, err
	}

	return BuildOrder(p, data)
}

func BuildOrder(p OrderParams, trade Trade) (Order, error) {
	p, err := normalizeOrderUserMetadata(p)
	if err != nil {
		return Order{}, err
	}
	tradeId, err := utils.GenerateTradeId()
	if err != nil {
		return Order{}, err
	}

	zero := time.Unix(0, 0)
	tradeOrder := Order{
		OrderId:       p.OrderId,
		TradeId:       tradeId,
		RefHash:       tradeId,
		TradeType:     p.TradeType,
		Rate:          fmt.Sprintf("%v", trade.Rate),
		Amount:        trade.Amount,
		Money:         p.Money.String(),
		Address:       trade.Wallet.GetPaymentAddr(),
		MatchAddress:  trade.Wallet.GetMatchAddr(),
		AddressLocked: p.Money.IsZero(), // 零值订单，地址锁定 独占
		Status:        OrderStatusWaiting,
		Name:          p.Name,
		UserID:        p.UserID,
		Username:      p.Username,
		DisplayName:   p.DisplayName,
		ApiType:       p.ApiType,
		ReturnUrl:     p.RedirectUrl,
		NotifyUrl:     p.NotifyUrl,
		NotifyNum:     0,
		NotifyState:   OrderNotifyStateFail,
		ExpiredAt:     CalcTradeExpiredAt(p.Timeout),
		Fiat:          p.Fiat,
		Crypto:        trade.Crypto,
		CurrencyLimit: p.CurrencyLimit,
		ConfirmedAt:   &zero, // 默认填充一个0值时间，尽量避免数据库出现允许 NULL 值存在
	}

	if tradeOrder.Name == "" {
		tradeOrder.Name = tradeOrder.OrderId
	}
	if err = Db.Create(&tradeOrder).Error; err != nil {
		log.Error("订单创建失败：", err.Error())
		return Order{}, err
	}

	return tradeOrder, nil
}

func BuildTrade(p OrderParams) (Trade, error) {
	// 获取代币类型
	crypto, err := GetCrypto(p.TradeType)
	if err != nil {
		return Trade{}, fmt.Errorf("代币类型(%s)不支持：%v", p.TradeType, err)
	}

	// 获取订单汇率
	rate, err := GetOrderRate(crypto, p.Fiat, p.Rate)
	if err != nil {
		return Trade{}, err
	}
	if rate.LessThanOrEqual(decimal.Zero) {
		return Trade{}, fmt.Errorf("%s %s 汇率异常", crypto, p.Fiat)
	}

	var wallets = GetAvailableWallets(p.TradeType)
	if p.Address != "" { // 指定地址
		w, err := NewWallet(p.Address, p.TradeType)
		if err != nil {
			return Trade{}, err
		}

		wallets = []Wallet{w}
	}

	if len(wallets) == 0 {
		return Trade{}, fmt.Errorf("%s 未检测到可用钱包地址", p.TradeType)
	}

	// 计算交易金额
	wallet, amount, err := CalcTradeAmount(wallets, rate, p)
	if err != nil {

		return Trade{}, err
	}

	return Trade{
		Crypto: crypto,
		Rate:   rate,
		Wallet: wallet,
		Amount: amount,
	}, nil
}

func RebuildOrder(t Order, p OrderParams) (Order, error) {
	p, err := normalizeOrderUserMetadata(p)
	if err != nil {
		return t, err
	}
	p, err = mergeOrderUserMetadata(t, p)
	if err != nil {
		return t, err
	}
	if p.OrderId == t.OrderId && p.TradeType == t.TradeType && p.Money.String() == t.Money && p.Fiat == t.Fiat {
		return saveOrderUserMetadata(t, p)
	}

	data, err := BuildTrade(p)
	if err != nil {
		return t, err
	}

	if t.TradeType == "" && p.ClientFingerprint != "" {
		t.ClientFingerprint = p.ClientFingerprint
	}

	t.Fiat = p.Fiat
	t.Address = data.Wallet.GetPaymentAddr()
	t.MatchAddress = data.Wallet.GetMatchAddr()
	t.Crypto = data.Crypto
	t.Amount = data.Amount
	t.Money = p.Money.String()
	t.TradeType = p.TradeType
	t.Rate = fmt.Sprintf("%v", data.Rate)
	t.ExpiredAt = CalcTradeExpiredAt(p.Timeout)

	query := Db.Model(&Order{}).Where("id = ?", t.ID).
		Where("user_id = ? AND username = ? AND display_name = ?", t.UserID, t.Username, t.DisplayName)
	if p.ClientFingerprint != "" {
		query = query.Where("(client_fingerprint = '' OR client_fingerprint = ?)", p.ClientFingerprint)
	}

	res := query.Updates(map[string]any{
		"fiat":               t.Fiat,
		"address":            t.Address,
		"match_address":      t.MatchAddress,
		"crypto":             t.Crypto,
		"amount":             t.Amount,
		"money":              t.Money,
		"trade_type":         t.TradeType,
		"rate":               t.Rate,
		"expired_at":         t.ExpiredAt,
		"client_fingerprint": t.ClientFingerprint,
		"user_id":            p.UserID,
		"username":           p.Username,
		"display_name":       p.DisplayName,
	})
	if res.Error != nil {
		return t, res.Error
	}
	if res.RowsAffected == 0 {
		return t, fmt.Errorf("order metadata or client fingerprint changed")
	}
	t.UserID, t.Username, t.DisplayName = p.UserID, p.Username, p.DisplayName

	return t, nil
}

// BuildPendingOrder 创建待支付订单（不锁定地址和汇率）
func BuildPendingOrder(p OrderParams) (Order, error) {
	var order Order
	p, err := normalizeOrderUserMetadata(p)
	if err != nil {
		return order, err
	}

	maxAmount := decimal.NewFromFloat(cast.ToFloat64(GetC(PaymentMaxAmount)))
	minAmount := decimal.NewFromFloat(cast.ToFloat64(GetC(PaymentMinAmount)))
	if !p.Money.IsZero() && (p.Money.GreaterThan(maxAmount) || p.Money.LessThan(minAmount)) {
		return order, fmt.Errorf("交易金额必须在 %s - %s 之间", minAmount.String(), maxAmount.String())
	}

	if err := Db.Where("order_id = ?", p.OrderId).Order("id desc").Limit(1).Find(&order).Error; err != nil {
		return order, err
	}
	if order.Status == OrderStatusSuccess || order.Status == OrderStatusConfirming || order.Status == OrderStatusWaiting {
		return saveOrderUserMetadata(order, p)
	}
	p, err = mergeOrderUserMetadata(order, p)
	if err != nil {
		return order, err
	}

	// 默认使用 USDT
	crypto := USDT
	if p.TradeType != "" {
		c, err := GetCrypto(p.TradeType)
		if err == nil {
			crypto = c
		}
	} else if p.CurrencyLimit != "" { // 如果指定了唯一的币种限制，且不是黑名单模式，则使用该币种
		cur := strings.ToUpper(p.CurrencyLimit)
		if !strings.Contains(cur, ",") && !strings.HasPrefix(cur, "-") {
			crypto = Crypto(cur)
		}
	}

	return BuildOrder(p, Trade{
		Crypto: crypto,
		Rate:   decimal.Zero,
		Amount: "0",
		Wallet: Wallet{},
	})
}
