package model

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrOrderUserConflict = errors.New("order user_id is already bound to another user")

func normalizeOrderUserMetadata(p OrderParams) (OrderParams, error) {
	var err error
	if p.UserID, err = normalizeOrderUserField("user_id", p.UserID, 64); err != nil {
		return p, err
	}
	if p.Username, err = normalizeOrderUserField("username", p.Username, 64); err != nil {
		return p, err
	}
	if p.DisplayName, err = normalizeOrderUserField("display_name", p.DisplayName, 128); err != nil {
		return p, err
	}
	return p, nil
}

func normalizeOrderUserField(name, value string, limit int) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s must be valid UTF-8", name)
	}
	// Validate before trimming so leading/trailing controls are not silently accepted.
	if strings.ContainsFunc(value, unicode.IsControl) {
		return "", fmt.Errorf("%s must not contain control characters", name)
	}
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > limit {
		return "", fmt.Errorf("%s must not exceed %d characters", name, limit)
	}
	return value, nil
}

// Existing nonempty values are an order snapshot, not a mutable user profile.
func mergeOrderUserMetadata(order Order, p OrderParams) (OrderParams, error) {
	if order.UserID != "" && p.UserID != "" && order.UserID != p.UserID {
		return p, ErrOrderUserConflict
	}
	if order.UserID != "" {
		p.UserID = order.UserID
	}
	if order.Username != "" {
		p.Username = order.Username
	}
	if order.DisplayName != "" {
		p.DisplayName = order.DisplayName
	}
	return p, nil
}

func saveOrderUserMetadata(order Order, p OrderParams) (Order, error) {
	// Competing writers can fill at most three empty snapshot fields before we converge.
	for attempt := 0; attempt < 4; attempt++ {
		merged, err := mergeOrderUserMetadata(order, p)
		if err != nil {
			return order, err
		}
		if order.UserID == merged.UserID && order.Username == merged.Username && order.DisplayName == merged.DisplayName {
			return order, nil
		}
		// Only metadata is written; payment state and timestamps remain untouched.
		result := Db.Model(&Order{}).Where("id = ?", order.ID).
			Where("user_id = ? AND username = ? AND display_name = ?", order.UserID, order.Username, order.DisplayName).
			UpdateColumns(map[string]any{"user_id": merged.UserID, "username": merged.Username, "display_name": merged.DisplayName})
		if result.Error != nil {
			return order, result.Error
		}
		if result.RowsAffected > 0 {
			order.UserID, order.Username, order.DisplayName = merged.UserID, merged.Username, merged.DisplayName
			return order, nil
		}
		var current Order
		if err := Db.First(&current, order.ID).Error; err != nil {
			return order, err
		}
		order = current
	}
	return order, ErrOrderStateChanged
}
