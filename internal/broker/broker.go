// Package broker conecta o back-end ao Message Broker MQTT.
//
// Tópicos (<p> = prefixo, <d> = id do dispositivo):
//
//	<p>/<d>/telemetry  ESP32 -> nuvem  leituras filtradas na borda           QoS 0
//	<p>/<d>/status     ESP32 -> nuvem  online/offline (retido, Last Will)     QoS 1
//	<p>/<d>/ack        ESP32 -> nuvem  confirmação de comando                 QoS 0
//	<p>/<d>/cmd        nuvem -> ESP32  comando para o atuador                 QoS 1
//	<p>/<d>/config     nuvem -> ESP32  limites para decisões na borda (retido) QoS 1
package broker

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const (
	KindTelemetry = "telemetry"
	KindStatus    = "status"
	KindAck       = "ack"
	KindCommand   = "cmd"
	KindConfig    = "config"
)

type Options struct {
	URL      string
	Username string
	Password string
	ClientID string
	Prefix   string
}

// Handler recebe mensagens já separadas por dispositivo e tipo.
type Handler func(deviceID, kind string, payload []byte, retained bool)

type Client struct {
	c         paho.Client
	prefix    string
	connected atomic.Bool
	lastError atomic.Value
}

func Connect(o Options, handle Handler) *Client {
	cl := &Client{prefix: strings.TrimSuffix(o.Prefix, "/")}
	cl.lastError.Store("conectando")
	opts := paho.NewClientOptions().
		AddBroker(o.URL).
		SetClientID(fmt.Sprintf("%s-%d", o.ClientID, time.Now().UnixNano()%1_000_000)).
		SetUsername(o.Username).
		SetPassword(o.Password).
		SetKeepAlive(30 * time.Second).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(3 * time.Second).
		SetMaxReconnectInterval(30 * time.Second).
		SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})

	opts.SetOnConnectHandler(func(c paho.Client) {
		// Reassina a cada (re)conexão porque a sessão é limpa.
		filters := map[string]byte{}
		for _, kind := range []string{KindTelemetry, KindStatus, KindAck} {
			filters[cl.prefix+"/+/"+kind] = 1
		}
		tok := c.SubscribeMultiple(filters, func(_ paho.Client, m paho.Message) {
			parts := strings.Split(m.Topic(), "/")
			if len(parts) < 3 || len(m.Payload()) > 4096 {
				return
			}
			handle(parts[len(parts)-2], parts[len(parts)-1], m.Payload(), m.Retained())
		})
		if tok.WaitTimeout(10*time.Second) && tok.Error() == nil {
			cl.connected.Store(true)
			cl.lastError.Store("")
			slog.Info("broker MQTT conectado", "url", o.URL)
		} else {
			cl.lastError.Store("falha ao assinar tópicos")
			slog.Error("falha ao assinar tópicos", "erro", tok.Error())
		}
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		cl.connected.Store(false)
		cl.lastError.Store("conexão perdida; reconectando")
		slog.Warn("conexão MQTT perdida", "erro", err)
	})
	opts.SetReconnectingHandler(func(_ paho.Client, _ *paho.ClientOptions) {
		slog.Info("reconectando ao broker MQTT")
	})
	cl.c = paho.NewClient(opts)
	cl.c.Connect() // assíncrono por causa do SetConnectRetry
	return cl
}

func (c *Client) Connected() bool { return c.connected.Load() && c.c.IsConnectionOpen() }

func (c *Client) LastError() string { return c.lastError.Load().(string) }

// Publish envia JSON para <prefixo>/<dispositivo>/<kind> com QoS 1 e aguarda o PUBACK.
func (c *Client) Publish(deviceID, kind string, payload any, retained bool) error {
	if !c.Connected() {
		return errors.New("broker desconectado")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tok := c.c.Publish(c.prefix+"/"+deviceID+"/"+kind, 1, retained, body)
	if !tok.WaitTimeout(5 * time.Second) {
		return errors.New("tempo esgotado ao publicar")
	}
	return tok.Error()
}

func (c *Client) Close() { c.c.Disconnect(500) }
