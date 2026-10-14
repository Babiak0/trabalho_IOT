// Package telegram envia alertas e recebe comandos pela Bot API do Telegram (HTTPS, sem dependências).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Bot struct {
	token  string
	chatID int64
	http   *http.Client
	api    string
}

func New(token string, chatID int64) *Bot {
	return &Bot{token: token, chatID: chatID, http: &http.Client{Timeout: 40 * time.Second}, api: "https://api.telegram.org"}
}

func (b *Bot) Enabled() bool { return b != nil && b.token != "" && b.chatID != 0 }

type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	Result      json.RawMessage `json:"result"`
}

// call faz uma requisição à Bot API. Erros nunca incluem a URL (que contém o token).
func (b *Bot) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api+"/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("requisição inválida")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: resposta inválida (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// Send envia uma mensagem para o chat configurado.
func (b *Bot) Send(ctx context.Context, text string) error {
	return b.SendTo(ctx, b.chatID, text)
}

func (b *Bot) SendTo(ctx context.Context, chatID int64, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil)
}

// Handler responde a um comando recebido no chat. args são as palavras após o comando.
type Handler func(ctx context.Context, command string, args []string) string

// Command aparece no menu "/" do Telegram.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetCommands registra o menu de comandos do bot.
func (b *Bot) SetCommands(ctx context.Context, cmds []Command) error {
	return b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// Listen faz long polling de mensagens e responde somente ao chat autorizado.
func (b *Bot) Listen(ctx context.Context, handle Handler) {
	var offset int64
	for ctx.Err() == nil {
		var updates []struct {
			UpdateID int64 `json:"update_id"`
			Message  *struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		}
		err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 30, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("telegram indisponível", "erro", err)
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || !strings.HasPrefix(u.Message.Text, "/") {
				continue
			}
			chat := u.Message.Chat.ID
			if chat != b.chatID {
				slog.Warn("comando de chat não autorizado ignorado", "chat_id", strconv.FormatInt(chat, 10))
				_ = b.SendTo(ctx, chat, "Chat não autorizado.")
				continue
			}
			fields := strings.Fields(u.Message.Text)
			cmd := strings.ToLower(strings.SplitN(fields[0], "@", 2)[0])
			reply := handle(ctx, cmd, fields[1:])
			if reply != "" {
				if err := b.SendTo(ctx, chat, reply); err != nil {
					slog.Warn("falha ao responder no telegram", "erro", err)
				}
			}
		}
	}
}
