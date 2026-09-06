package model

import "time"

// One durable cursor per order pins its original payment window across restarts.
type EVMScanJob struct {
	ID          int64  `gorm:"primaryKey" json:"id"`
	Network     string `gorm:"size:32;uniqueIndex:idx_evm_scan_owner" json:"network"`
	Kind        string `gorm:"size:16;uniqueIndex:idx_evm_scan_owner" json:"kind"`
	OrderID     int64  `gorm:"uniqueIndex:idx_evm_scan_owner" json:"order_id"`
	StartAt     int64
	EndAt       int64
	NextBlock   int64
	TargetBlock int64
	Attempts    int
	NextRetryAt time.Time `gorm:"index"`
	LastError   string    `gorm:"size:512"`
	Completed   bool      `gorm:"index"`
	UpdatedAt   time.Time
}

func (EVMScanJob) TableName() string { return "bep_evm_scan_job" }

// Token log indices and native transfers occupy different identities.
type EVMPaymentClaim struct {
	Network    string `gorm:"primaryKey;size:32"`
	TxHash     string `gorm:"primaryKey;size:66"`
	EventIndex string `gorm:"primaryKey;size:32"`
	OrderID    int64  `gorm:"uniqueIndex"`
	CreatedAt  time.Time
}

func (EVMPaymentClaim) TableName() string { return "bep_evm_payment_claim" }
