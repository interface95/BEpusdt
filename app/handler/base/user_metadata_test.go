package base

import (
	"encoding/json"
	"testing"
)

func TestUserMetadataTextDecode(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		valid     bool
	}{
		{`""`, "", true},
		{`"  customer  "`, "  customer  ", true},
		{`"客户"`, "客户", true},
		{`"\ud83d\ude00"`, "\U0001f600", true},
		{`"a\nb"`, "a\nb", true},
		{`123`, "", false},
		{`null`, "", false},
		{`true`, "", false},
		{`{}`, "", false},
		{`[]`, "", false},
		{"\"bad\xff\"", "", false},
		{`"\x12"`, "", false},
		{"", "", false},
	} {
		var value UserMetadataText
		err := value.UnmarshalJSON([]byte(tc.raw))
		if (err == nil) != tc.valid || string(value) != tc.want {
			t.Fatalf("decode %q: value=%q error=%v", tc.raw, value, err)
		}
	}
	var request struct {
		UserID UserMetadataText `json:"user_id"`
	}
	if err := json.Unmarshal([]byte(`{}`), &request); err != nil || request.UserID != "" {
		t.Fatalf("omitted field is not backward compatible: %+v %v", request, err)
	}
}
