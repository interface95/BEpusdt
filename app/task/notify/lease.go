package notify

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
)

var ErrNotifyInProgress = errors.New("order notification is already in progress or its state changed")

// A persistent lease survives a process restart and coordinates multiple instances.
// HTTP has a ten-second deadline, shorter than this lease; no transaction spans I/O.
const notifyLeaseDuration = 30 * time.Second

func claimNotifyLease(db *gorm.DB, order *model.Order) error {
	now := time.Now()
	token := rand.Text()
	until := now.Add(notifyLeaseDuration).UnixMilli()
	result := db.Model(&model.Order{}).
		Where("id = ? AND status = ? AND ref_hash = ? AND notify_num = ? AND notify_state = ?", order.ID, order.Status, order.RefHash, order.NotifyNum, order.NotifyState).
		Where("notify_lease_until <= ?", now.UnixMilli()).
		UpdateColumns(map[string]any{"notify_lease_token": token, "notify_lease_until": until})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotifyInProgress
	}
	order.NotifyLeaseToken, order.NotifyLeaseUntil = token, until
	return nil
}

func releaseNotifyLease(db *gorm.DB, order model.Order) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.WithContext(ctx).Model(&model.Order{}).
		Where("id = ? AND notify_lease_token = ?", order.ID, order.NotifyLeaseToken).
		UpdateColumns(map[string]any{"notify_lease_token": "", "notify_lease_until": 0}).Error
}

func finishNotifyLease(db *gorm.DB, order model.Order, deliveryErr error) error {
	state, reason := model.OrderNotifyStateSucc, ""
	if deliveryErr != nil {
		state, reason = model.OrderNotifyStateFail, deliveryErr.Error()
	}
	if len(reason) > 512 {
		reason = reason[:512]
	}
	// Persist the result even if the HTTP deadline expired. A failed DB write leaves
	// the lease to expire, so the scheduler can retry after a crash or lost reply.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := db.WithContext(ctx).Model(&model.Order{}).
		Where("id = ? AND status = ? AND ref_hash = ? AND notify_lease_token = ?", order.ID, model.OrderStatusSuccess, order.RefHash, order.NotifyLeaseToken).
		UpdateColumns(map[string]any{
			"notify_num":         gorm.Expr("notify_num + 1"),
			"notify_state":       gorm.Expr("CASE WHEN notify_state = ? THEN ? ELSE ? END", model.OrderNotifyStateSucc, model.OrderNotifyStateSucc, state),
			"notify_error":       gorm.Expr("CASE WHEN notify_state = ? THEN '' ELSE ? END", model.OrderNotifyStateSucc, reason),
			"notify_lease_token": "", "notify_lease_until": 0,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return model.ErrOrderStateChanged
	}
	return nil
}
