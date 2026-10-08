package base

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// UserMetadataText rejects invalid UTF-8 before encoding/json can replace it.
// Length, controls and normalization are checked by the order model.
type UserMetadataText string

func (s *UserMetadataText) UnmarshalJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("user metadata must be valid UTF-8")
	}
	if len(data) == 0 || data[0] != '"' {
		return errors.New("user metadata must be a string")
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("invalid user metadata string")
	}
	*s = UserMetadataText(value)
	return nil
}
