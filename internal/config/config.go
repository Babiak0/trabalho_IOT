// Package config carrega a configuração do servidor a partir de variáveis de ambiente.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	DashboardUser     string
	DashboardPassword string
	SessionSecret     string // assina os cookies de sessão; vazio = sessões perdidas ao reiniciar

	MQTTURL      string
	MQTTUsername string
	MQTTPassword string
	MQTTClientID string
	TopicPrefix  string

	TelegramToken  string
	TelegramChatID int64

	StaleAfter       time.Duration // sem telemetria por esse tempo => dispositivo offline
	CommandTimeout   time.Duration
	AlertSamples     int           // leituras consecutivas fora do limite antes do alerta
	AlertReminder    time.Duration // intervalo mínimo entre lembretes do mesmo alerta
	HistoryRetention time.Duration
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:          env("HTTP_ADDR", ":8080"),
		DatabaseURL:       env("DATABASE_URL", "postgres://iot:iot@localhost:5432/iot?sslmode=disable"),
		DashboardUser:     env("DASHBOARD_USER", ""),
		DashboardPassword: env("DASHBOARD_PASSWORD", ""),
		SessionSecret:     env("SESSION_SECRET", ""),
		MQTTURL:           env("MQTT_URL", "tcp://localhost:1883"),
		MQTTUsername:      env("MQTT_USERNAME", ""),
		MQTTPassword:      env("MQTT_PASSWORD", ""),
		MQTTClientID:      env("MQTT_CLIENT_ID", "backend"),
		TopicPrefix:       env("MQTT_TOPIC_PREFIX", "ambiente"),
		TelegramToken:     env("TELEGRAM_BOT_TOKEN", ""),
	}
	var err error
	if c.TelegramChatID, err = envInt64("TELEGRAM_CHAT_ID", 0); err != nil {
		return c, err
	}
	if c.StaleAfter, err = envDuration("DEVICE_STALE_AFTER", 30*time.Second); err != nil {
		return c, err
	}
	if c.CommandTimeout, err = envDuration("COMMAND_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}
	if c.AlertReminder, err = envDuration("ALERT_REMINDER", 5*time.Minute); err != nil {
		return c, err
	}
	if c.HistoryRetention, err = envDuration("HISTORY_RETENTION", 30*24*time.Hour); err != nil {
		return c, err
	}
	samples, err := envInt64("ALERT_SAMPLES", 3)
	if err != nil {
		return c, err
	}
	c.AlertSamples = int(samples)
	if c.AlertSamples < 1 {
		return c, fmt.Errorf("ALERT_SAMPLES deve ser >= 1")
	}
	if (c.DashboardUser == "") != (c.DashboardPassword == "") {
		return c, fmt.Errorf("defina DASHBOARD_USER e DASHBOARD_PASSWORD juntos")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt64(key string, fallback int64) (int64, error) {
	v := env(key, "")
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s inválido: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := env(key, "")
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s inválido (use 30s, 5m...): %w", key, err)
	}
	return d, nil
}
