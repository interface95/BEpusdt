package task

import (
	"context"
	"errors"
	"time"

	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/notifier"
	"github.com/v03413/bepusdt/app/task/notify"
	"github.com/v03413/bepusdt/app/utils"
)

func init() {
	Register(Task{Duration: time.Second * 3, Callback: notifyRetry})
	Register(Task{Duration: time.Second * 30, Callback: notifyRoll})
}

// notifyRetry 回调失败重试
func notifyRetry(ctx context.Context) {
	tradeOrders, err := model.GetNotifyFailedTradeOrders()
	if err != nil {
		log.Task.Error("待回调订单获取失败", err)

		return
	}

	for _, order := range tradeOrders {
		if ctx.Err() != nil {
			return
		}
		if order.ConfirmedAt == nil {
			log.Task.Warnf("订单 %s 缺少确认时间，无法安排回调重试", order.TradeId)
			continue
		}
		next := utils.CalcNextNotifyTime(*order.ConfirmedAt, order.NotifyNum)
		if time.Now().Unix() >= next.Unix() {
			go deliverOrderNotification(order)
		}
	}
}

func notifyRoll(context.Context) {
	for _, o := range model.GetOrderByStatus(model.OrderStatusWaiting) {
		notify.Bepusdt(o)
	}
}

// notifyOrderSuccess 统一触发订单成功后的回调与订单通知。
func notifyOrderSuccess(order model.Order) {
	go deliverOrderNotification(order)
	go notifier.Success(order)
}

func deliverOrderNotification(order model.Order) {
	if err := notify.Handle(order); err != nil && !errors.Is(err, notify.ErrNotifyInProgress) {
		log.Task.Warnf("订单 %s 回调未完成: %v", order.TradeId, err)
	}
}
