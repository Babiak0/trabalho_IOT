#!/bin/sh
# Gera o arquivo de senhas a partir das variáveis de ambiente a cada inicialização.
set -eu
PW=/mosquitto/data/passwd
rm -f "$PW"
mosquitto_passwd -c -b "$PW" backend "$MQTT_BACKEND_PASSWORD"
for pair in $(echo "${MQTT_DEVICES:-}" | tr ',' ' '); do
  mosquitto_passwd -b "$PW" "${pair%%:*}" "${pair#*:}"
done
chown mosquitto:mosquitto "$PW"
chmod 0700 "$PW"
exec mosquitto -c /mosquitto/config/mosquitto.conf
