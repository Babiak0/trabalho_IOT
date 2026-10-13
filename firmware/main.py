"""ESP32 virtual no Wokwi. Copie também mqtt0.py e edge_logic.py."""
import network
import time
import machine
import dht
import json
import ubinascii
import os
from mqtt0 import MQTT0
from edge_logic import EdgeFilter

HOST, PORT = 'test.mosquitto.org', 1883
ROOT = 'ambiente/academico-3ac9654e2b7f4c61'  # Igual ao config.json do servidor.
DEVICE = 'esp32-deposito-01'
WIFI_NAME, WIFI_PASSWORD = 'Wokwi-GUEST', ''
sensor = dht.DHT22(machine.Pin(4))
led = machine.Pin(18, machine.Pin.OUT)
led.value(0)
filter = EdgeFilter()
boot = ubinascii.hexlify(os.urandom(8)).decode()
seq = 0
executed = []
client = None


def receive(topic, body):
    try:
        cmd = json.loads(body)
        if not isinstance(cmd, dict) or cmd.get('device_id') != DEVICE:
            return
        command_id, action = cmd.get('id'), cmd.get('action')
        if not isinstance(command_id, str) or len(command_id) != 32 or action not in ('on', 'off'):
            return
        if command_id not in executed:
            led.value(1 if action == 'on' else 0)
            executed.append(command_id)
            if len(executed) > 20:
                executed.pop(0)
        client.publish(ROOT + '/acks', json.dumps({'id': command_id, 'device_id': DEVICE, 'led': bool(led.value())}))
    except (ValueError, TypeError):
        print('Comando inválido descartado')


def wifi():
    wlan = network.WLAN(network.STA_IF)
    wlan.active(True)
    if not wlan.isconnected():
        wlan.connect(WIFI_NAME, WIFI_PASSWORD)
        started = time.ticks_ms()
        while not wlan.isconnected():
            if time.ticks_diff(time.ticks_ms(), started) > 20000:
                raise OSError('Wi-Fi indisponível')
            time.sleep_ms(100)


while True:
    try:
        wifi()
        client = MQTT0(HOST, PORT, 'edge-' + boot, receive)
        client.connect(ROOT + '/status', json.dumps({'device_id': DEVICE, 'online': False}))
        client.subscribe(ROOT + '/commands')
        client.publish(ROOT + '/status', json.dumps({'device_id': DEVICE, 'online': True}))
        print('Conectado. Ajuste o DHT22 para variar os valores.')
        last_sample = time.ticks_add(time.ticks_ms(), -5000)
        last_ping = time.ticks_ms()
        while True:
            client.check()
            now = time.ticks_ms()
            if time.ticks_diff(now, last_sample) >= 5000:
                last_sample = now
                try:
                    sensor.measure()
                    raw_t, raw_h = sensor.temperature(), sensor.humidity()
                    avg_t, avg_h = filter.add(raw_t, raw_h)
                except (OSError, ValueError):
                    print('Leitura inválida descartada')
                    continue
                seq += 1
                reading = {'device_id': DEVICE, 'boot_id': boot, 'seq': seq,
                           'temperature': avg_t, 'humidity': avg_h,
                           'raw_temperature': raw_t, 'raw_humidity': raw_h,
                           'led': bool(led.value())}
                client.publish(ROOT + '/telemetry', json.dumps(reading))
                print('Telemetria:', reading)
            if time.ticks_diff(now, last_ping) >= 15000:
                client.ping()
                last_ping = now
            time.sleep_ms(50)
    except (OSError, ValueError) as exc:
        print('Reconectando:', exc)
        if client:
            client.close()
        time.sleep(3)
