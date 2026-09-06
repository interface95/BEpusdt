package task

import (
	"errors"
	"time"

	"github.com/v03413/bepusdt/app/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const evmLiveWindowBlocks int64 = 32

// Move stale live work to a durable recovery cursor before serving recent blocks.
// Re-scanning overlapping completed blocks is harmless because payment claims are unique.
func (e *evm) promoteLiveWindow(db *gorm.DB, head int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var live model.EVMScanJob
		if err := tx.Where("network = ? AND kind = ?", e.Network, "live").First(&live).Error; err != nil {
			return err
		}
		next := head - evmLiveWindowBlocks + 1
		if live.NextBlock <= 0 || live.NextBlock >= next {
			return nil
		}
		gap := model.EVMScanJob{Network: e.Network, Kind: "recovery", NextBlock: live.NextBlock, TargetBlock: next - 1, NextRetryAt: time.Now()}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&gap)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			if err := tx.Model(&model.EVMScanJob{}).Where("network = ? AND kind = ?", e.Network, "recovery").Updates(map[string]any{
				"target_block": gorm.Expr("CASE WHEN target_block < ? THEN ? ELSE target_block END", next-1, next-1),
				"completed":    false,
			}).Error; err != nil {
				return err
			}
		}
		res = tx.Model(&model.EVMScanJob{}).Where("id = ? AND next_block = ?", live.ID, live.NextBlock).Updates(map[string]any{"next_block": next, "target_block": head, "next_retry_at": time.Now(), "attempts": 0, "last_error": ""})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("live scan cursor changed during recovery handoff")
		}
		return nil
	})
}
