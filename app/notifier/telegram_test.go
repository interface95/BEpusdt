package notifier

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/v03413/bepusdt/app/model"
)

func captureTelegramMessage(t *testing.T, send func(*Telegram)) url.Values {
	t.Helper()
	messages := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest/sendMessage" {
			t.Errorf("unexpected Telegram method: %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(64 << 10); err != nil {
			t.Errorf("parse Telegram message: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer r.MultipartForm.RemoveAll()
		messages <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	}))
	t.Cleanup(server.Close)
	api, err := bot.New("test", bot.WithSkipGetMe(), bot.WithServerURL(server.URL), bot.WithHTTPClient(time.Second, server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	send(&Telegram{api: api, chatID: 42, topicID: 7})
	select {
	case message := <-messages:
		if message.Get("chat_id") != "42" || message.Get("message_thread_id") != "7" {
			t.Fatalf("notification destination changed: %v", message)
		}
		return message
	default:
		t.Fatal("Telegram message was not sent")
		return nil
	}
}

func TestOrderNotificationsEscapeTextAndKeepLegacyOrders(t *testing.T) {
	for _, kind := range []string{"success", "notify_fail"} {
		t.Run(kind, func(t *testing.T) {
			confirmedAt := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			order := model.Order{
				OrderId: "order-<one>&`two`", TradeType: model.UsdtTrc20,
				Status: model.OrderStatusSuccess, Fiat: model.CNY,
				Amount: "146.05", Money: "980", Rate: "6.71", ConfirmedAt: &confirmedAt,
				AutoTimeAt: model.AutoTimeAt{CreatedAt: (*model.Datetime)(&confirmedAt), UpdatedAt: (*model.Datetime)(&confirmedAt)},
			}
			reason := "callback <bad>&`error`"
			message := captureTelegramMessage(t, func(telegram *Telegram) {
				if kind == "success" {
					telegram.Success(order)
				} else {
					telegram.NotifyFail(order, reason)
				}
			})
			if message.Get("parse_mode") != "HTML" {
				t.Fatalf("parse mode = %q, want HTML", message.Get("parse_mode"))
			}
			text := message.Get("text")
			if !strings.Contains(text, html.EscapeString(order.OrderId)) || strings.Contains(text, "<one>") {
				t.Fatalf("unsafe order reference: %s", text)
			}
			if !strings.Contains(text, "146.05") || !strings.Contains(text, "980") {
				t.Fatalf("payment amounts missing: %s", text)
			}
			if strings.Contains(text, "用户 ID") || strings.Contains(text, "用户名") || strings.Contains(text, "显示名称") {
				t.Fatalf("legacy order contains empty user labels: %s", text)
			}
			if kind == "notify_fail" && !strings.Contains(text, html.EscapeString(reason)) {
				t.Fatalf("failure reason was not escaped: %s", text)
			}
		})
	}
}

func TestOrderNotificationsIncludeUserSnapshot(t *testing.T) {
	for _, kind := range []string{"success", "notify_fail"} {
		t.Run(kind, func(t *testing.T) {
			confirmedAt := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			order := model.Order{
				OrderId: "merchant-485", TradeType: model.UsdtTrc20,
				Status: model.OrderStatusSuccess, Fiat: model.CNY,
				Amount: "146.05", Money: "980", Rate: "6.71", ConfirmedAt: &confirmedAt,
				AutoTimeAt: model.AutoTimeAt{CreatedAt: (*model.Datetime)(&confirmedAt), UpdatedAt: (*model.Datetime)(&confirmedAt)},
				UserID:     "485", Username: "test_user`<admin>&", DisplayName: "测试用户 <张三> & friends",
			}
			message := captureTelegramMessage(t, func(telegram *Telegram) {
				if kind == "success" {
					telegram.Success(order)
				} else {
					telegram.NotifyFail(order, "timeout")
				}
			})
			for _, line := range []string{
				"用户 ID：485", "用户名：" + html.EscapeString(order.Username),
				"显示名称：" + html.EscapeString(order.DisplayName),
			} {
				if !strings.Contains(message.Get("text"), line) {
					t.Errorf("missing safely encoded user snapshot %q in %s", line, message.Get("text"))
				}
			}
			if strings.Contains(message.Get("text"), "<admin>") || strings.Contains(message.Get("text"), "<张三>") {
				t.Fatalf("user text was rendered as markup: %s", message.Get("text"))
			}
			var keyboard models.InlineKeyboardMarkup
			if err := json.Unmarshal([]byte(message.Get("reply_markup")), &keyboard); err != nil {
				t.Fatal(err)
			}
			if len(keyboard.InlineKeyboard) != 1 || len(keyboard.InlineKeyboard[0]) != 1 {
				t.Fatalf("missing transaction link: %+v", keyboard)
			}
			button := keyboard.InlineKeyboard[0][0]
			if button.URL != order.GetTxUrl() || button.CallbackData != "" {
				t.Fatalf("transaction link must be a URL, not length-limited callback data: %+v", button)
			}
		})
	}
}

func TestOrderNotificationsUnsupportedTradeDoesNotLeakUser(t *testing.T) {
	for _, kind := range []string{"success", "notify_fail"} {
		t.Run(kind, func(t *testing.T) {
			order := model.Order{TradeType: "unsupported", Status: model.OrderStatusSuccess,
				UserID: "private-id", Username: "private-name", DisplayName: "private-display"}
			message := captureTelegramMessage(t, func(telegram *Telegram) {
				if kind == "success" {
					telegram.Success(order)
				} else {
					telegram.NotifyFail(order, "timeout")
				}
			})
			if !strings.Contains(message.Get("text"), "交易类型不支持") || strings.Contains(message.Get("text"), "private-") {
				t.Fatalf("unexpected unsupported-trade notice: %s", message.Get("text"))
			}
		})
	}
}

func TestNonOrderTransferTitleUsesMatchAddress(t *testing.T) {
	trans := model.TronTransfer{
		RecvAddress: "0xf9dff4e813644edc7d9b1b535100b7e92333f743",
		FromAddress: "0x6cba0ff7e76b225ee3b975c3096d828f012c8c72",
	}
	wallet := model.Wallet{
		Address:   "0xF9DFF4E813644eDc7D9b1b535100b7e92333f743",
		MatchAddr: "0xf9dff4e813644edc7d9b1b535100b7e92333f743",
	}

	if title := nonOrderTransferTitle(trans, wallet); title != "收入" {
		t.Fatalf("title = %q, want %q", title, "收入")
	}
}

func TestNonOrderTransferTitleMarksOutgoingTransfer(t *testing.T) {
	trans := model.TronTransfer{
		RecvAddress: "0xrecipient",
		FromAddress: "0xf9dff4e813644edc7d9b1b535100b7e92333f743",
	}
	wallet := model.Wallet{
		Address:   "0xF9DFF4E813644eDc7D9b1b535100b7e92333f743",
		MatchAddr: "0xf9dff4e813644edc7d9b1b535100b7e92333f743",
	}

	if title := nonOrderTransferTitle(trans, wallet); title != "支出" {
		t.Fatalf("title = %q, want %q", title, "支出")
	}
}
