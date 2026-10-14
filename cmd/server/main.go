// Servidor da plataforma: MQTT -> regras -> PostgreSQL -> painel/API -> Telegram.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brugge-matheus/iot-aps/internal/api"
	"github.com/brugge-matheus/iot-aps/internal/broker"
	"github.com/brugge-matheus/iot-aps/internal/config"
	"github.com/brugge-matheus/iot-aps/internal/service"
	"github.com/brugge-matheus/iot-aps/internal/store"
	"github.com/brugge-matheus/iot-aps/internal/telegram"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "consulta /healthz e sai (usado pelo Docker)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("configuração inválida", "erro", err)
		os.Exit(1)
	}
	if *healthcheck {
		os.Exit(checkHealth(cfg.HTTPAddr))
	}
	if err := run(cfg); err != nil {
		slog.Error("encerrando com erro", "erro", err)
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	bot := telegram.New(cfg.TelegramToken, cfg.TelegramChatID)
	if !bot.Enabled() {
		slog.Warn("Telegram desativado: defina TELEGRAM_BOT_TOKEN e TELEGRAM_CHAT_ID")
	}

	svc := service.New(cfg, st, bot)
	mq := broker.Connect(broker.Options{
		URL: cfg.MQTTURL, Username: cfg.MQTTUsername, Password: cfg.MQTTPassword,
		ClientID: cfg.MQTTClientID, Prefix: cfg.TopicPrefix,
	}, svc.HandleMQTT)
	defer mq.Close()
	svc.SetPublisher(mq)
	go svc.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.New(svc, cfg.DashboardUser, cfg.DashboardPassword),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("painel disponível", "endereco", cfg.HTTPAddr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("encerrando")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func checkHealth(addr string) int {
	host := addr
	if len(host) > 0 && host[0] == ':' {
		host = "127.0.0.1" + host
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", host))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
