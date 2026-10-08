package admin

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/handler/base"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/task/notify"
	"github.com/v03413/bepusdt/app/utils"
)

type Order struct {
}

type oListReq struct {
	base.ListRequest
	Name      string `json:"name"`
	Money     string `json:"money"`
	Amount    string `json:"amount"`
	OrderId   string `json:"order_id"`
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	TradeId   string `json:"trade_id"`
	Status    *uint8 `json:"status"`
	Address   string `json:"address"`
	TradeType string `json:"trade_type"`
	StartAt   string `json:"start_at"`
	EndAt     string `json:"end_at"`
}

type paidReq struct {
	base.IDRequest
	RefHash string `json:"ref_hash"`
}

type createReq struct {
	UserID      base.UserMetadataText `json:"user_id"`
	Username    base.UserMetadataText `json:"username"`
	DisplayName base.UserMetadataText `json:"display_name"`
	Amount      float64               `json:"amount" binding:"required"`
	OrderID     string                `json:"order_id" binding:"required"`
	Name        string                `json:"name"`
	Fiat        model.Fiat            `json:"fiat"`
	Currencies  string                `json:"currencies"`
	Timeout     int64                 `json:"timeout"`
}

func (Order) Create(ctx *gin.Context) {
	var req createReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.Response(ctx, 400, err.Error())

		return
	}

	host := utils.GetRequestHost(ctx.Request)

	// 创建待付款订单
	order, err := model.BuildPendingOrder(model.OrderParams{
		Money:         decimal.NewFromFloat(req.Amount),
		ApiType:       model.OrderApiTypeAdmin, // 使用 Admin 类型
		UserID:        string(req.UserID),
		Username:      string(req.Username),
		DisplayName:   string(req.DisplayName),
		OrderId:       req.OrderID,
		Name:          req.Name,
		Timeout:       req.Timeout,
		Fiat:          req.Fiat,
		CurrencyLimit: req.Currencies,
	})
	if err != nil {
		base.Response(ctx, 400, gin.H{
			"status":  "failed",
			"message": "failed to create order: " + err.Error(),
		})
		return
	}

	// NotifyURL RedirectURL 同为付款链接
	url := model.CheckoutUrl(host, order.TradeId)

	// Update order with generated URLs
	hostUri := model.GetK(model.ApiAppUri)
	if hostUri == "" {
		hostUri = host
	}
	order.NotifyUrl = hostUri + "/api/v1/pay/notify"
	order.ReturnUrl = url

	if err := model.Db.Model(&model.Order{}).Where("id = ?", order.ID).
		Updates(map[string]any{"notify_url": order.NotifyUrl, "return_url": order.ReturnUrl}).Error; err != nil {
		base.Response(ctx, 400, gin.H{
			"status":  "failed",
			"message": "failed to update order urls: " + err.Error(),
		})
		return
	}

	// 返回响应数据
	base.Response(ctx, 200, gin.H{
		"status":      "success",
		"message":     "order created",
		"payment_url": url,
	})
}

func (Order) List(ctx *gin.Context) {
	var req oListReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.Response(ctx, 400, err.Error())

		return
	}

	type order struct {
		model.Order
		Wallet model.Wallet `gorm:"foreignKey:MatchAddr,TradeType;references:Address,TradeType" json:"wallet"`
	}

	var data []order
	var db = model.Db

	if req.Name != "" {
		db = db.Where("name LIKE ?", "%"+req.Name+"%")
	}
	if req.Money != "" {
		db = db.Where("money = ?", req.Money)
	}
	if req.Amount != "" {
		db = db.Where("amount = ?", req.Amount)
	}
	if req.OrderId != "" {
		db = db.Where("order_id like ?", "%"+req.OrderId+"%")
	}
	if userID := strings.TrimSpace(req.UserID); userID != "" {
		db = db.Where("user_id = ?", userID)
	}
	if username := strings.TrimSpace(req.Username); username != "" {
		keyword := "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(username) + "%"
		db = db.Where("(username LIKE ? ESCAPE '!' OR display_name LIKE ? ESCAPE '!')", keyword, keyword)
	}
	if req.TradeId != "" {
		db = db.Where("trade_id like ?", "%"+req.TradeId+"%")
	}
	if req.Status != nil {
		db = db.Where("status = ?", *req.Status)
	}
	if req.Address != "" {
		db = db.Where("address like ?", "%"+req.Address+"%")
	}
	if req.TradeType != "" {
		db = db.Where("trade_type = ?", req.TradeType)
	}
	if req.StartAt != "" {
		db = db.Where("created_at >= ?", req.StartAt)
	}
	if req.EndAt != "" {
		db = db.Where("created_at <= ?", req.EndAt)
	}

	var total int64

	if err := db.Model(&model.Order{}).Count(&total).Error; err != nil {
		base.Error(ctx, err)
		return
	}

	err := db.Preload("Wallet").Limit(req.Size).Offset((req.Page - 1) * req.Size).Order("id " + req.Sort).Find(&data).Error
	if err != nil {
		base.Response(ctx, 400, err.Error())

		return
	}

	type item struct {
		order
		UserID      string `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}
	items := make([]item, 0, len(data))
	for _, o := range data {
		items = append(items, item{order: o, UserID: o.UserID, Username: o.Username, DisplayName: o.DisplayName})
	}
	base.Response(ctx, 200, items, total)
}

func (Order) Detail(ctx *gin.Context) {
	var req base.IDRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	type detail struct {
		model.Order
		TxUrl       string `gorm:"-" json:"tx_url"`
		UserID      string `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
	}

	var o model.Order
	model.Db.Where("id = ?", req.ID).Find(&o)
	if o.ID == 0 {
		base.BadRequest(ctx, "订单不存在")

		return
	}

	base.Ok(ctx, detail{
		Order:       o,
		TxUrl:       o.GetTxUrl(),
		UserID:      o.UserID,
		Username:    o.Username,
		DisplayName: o.DisplayName,
	})
}

func (Order) Paid(ctx *gin.Context) {
	var req paidReq
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	var order model.Order
	model.Db.Where("id = ?", req.ID).Find(&order)
	if order.ID == 0 {
		base.BadRequest(ctx, "订单不存在")

		return
	}

	if order.Status == model.OrderStatusCanceled {
		base.BadRequest(ctx, "交易已取消，无法补单")

		return
	}
	if order.Status == model.OrderStatusSuccess {
		base.Ok(ctx, "订单已支付，无需重复补单")
		return
	}

	confirmedAt := time.Now()
	if err := order.MarkPaid(req.RefHash, confirmedAt); err != nil {
		base.Error(ctx, err)

		return
	}

	go notify.Handle(order)

	base.Ok(ctx, "操作成功")
}

func (Order) ManualNotify(ctx *gin.Context) {
	var req base.IDRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	var order model.Order
	model.Db.Where("id = ?", req.ID).Find(&order)
	if order.ID == 0 {
		base.BadRequest(ctx, "订单不存在")

		return
	}

	if order.Status != model.OrderStatusSuccess {
		base.BadRequest(ctx, "订单状态不是交易成功,无法手动回调")

		return
	}

	if err := notify.HandleManual(order); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	base.Ok(ctx, "订单回调成功！")
}

func (Order) Cancel(ctx *gin.Context) {
	var req base.IDRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	var order model.Order
	model.Db.Where("id = ?", req.ID).Find(&order)
	if order.ID == 0 {
		base.BadRequest(ctx, "订单不存在")

		return
	}

	if order.Status != model.OrderStatusWaiting {
		base.BadRequest(ctx, "仅允许取消未支付订单")

		return
	}

	if err := order.SetCanceled(); err != nil {
		base.Error(ctx, err)

		return
	}

	base.Ok(ctx, "订单取消成功")
}

func (Order) Del(ctx *gin.Context) {
	var req base.IDListRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		base.BadRequest(ctx, err.Error())

		return
	}

	err := model.Db.Delete(&model.Order{}, req.IDList).Error
	if err != nil {
		base.Error(ctx, err)

		return
	}

	base.Ok(ctx, "删除成功")
}
