# Arquivo único para colar no main.py do Wokwi.
"""Cliente MQTT 3.1.1 mínimo, original: QoS 0 e uma assinatura.
Somente para este protótipo; não substitui um cliente de produção.
"""
import socket
import struct
import select


def string(value):
    if isinstance(value, str):
        value = value.encode()
    return struct.pack('!H', len(value)) + value


def packet(header, payload):
    size = len(payload)
    remaining = bytearray()
    while True:
        digit = size % 128
        size //= 128
        remaining.append(digit | (128 if size else 0))
        if not size:
            break
    return bytes([header]) + bytes(remaining) + payload


class MQTT0:
    def __init__(self, host, port, client_id, callback):
        self.host, self.port = host, port
        self.client_id, self.callback = client_id, callback
        self.sock = None

    def _write(self, data):
        while data:
            written = self.sock.send(data)
            if not written:
                raise OSError('Conexão encerrada')
            data = data[written:]

    def _exact(self, size):
        data = b''
        while len(data) < size:
            chunk = self.sock.recv(size - len(data))
            if not chunk:
                raise OSError('Conexão encerrada')
            data += chunk
        return data

    def _read(self):
        first = self._exact(1)[0]
        size, multiplier = 0, 1
        for _ in range(4):
            digit = self._exact(1)[0]
            size += (digit & 127) * multiplier
            if not digit & 128:
                break
            multiplier *= 128
        else:
            raise ValueError('Pacote MQTT malformado')
        if size > 4096:
            raise ValueError('Pacote MQTT muito grande')
        return first, self._exact(size)

    def connect(self, will_topic, will_message):
        addr = socket.getaddrinfo(self.host, self.port)[0][-1]
        self.sock = socket.socket()
        self.sock.settimeout(5)
        self.sock.connect(addr)
        # Clean session + Last Will QoS 0, sem retenção.
        payload = string('MQTT') + bytes([4, 6]) + struct.pack('!H', 30)
        payload += string(self.client_id) + string(will_topic) + string(will_message)
        self._write(packet(0x10, payload))
        first, body = self._read()
        if first != 0x20 or len(body) != 2 or body[1] != 0:
            raise OSError('Broker recusou a conexão')

    def subscribe(self, topic):
        self._write(packet(0x82, b'\x00\x01' + string(topic) + b'\x00'))
        first, body = self._read()
        if first != 0x90 or body != b'\x00\x01\x00':
            raise OSError('Broker recusou a assinatura')

    def publish(self, topic, message):
        self._write(packet(0x30, string(topic) + message.encode()))

    def check(self):
        if hasattr(select, 'poll'):
            poll = select.poll()
            poll.register(self.sock, select.POLLIN)
            ready = poll.poll(0)
        else:  # Permite testar o mesmo cliente no Python do Windows.
            ready = select.select([self.sock], [], [], 0)[0]
        if not ready:
            return
        first, body = self._read()
        if first >> 4 == 3 and not first & 1:  # Rejeita mensagens retidas.
            if (first >> 1) & 3:
                raise ValueError('QoS não suportado por este cliente')
            if len(body) < 2:
                raise ValueError('PUBLISH inválido')
            n = struct.unpack('!H', body[:2])[0]
            if len(body) < n + 2:
                raise ValueError('Tópico MQTT incompleto')
            self.callback(body[2:2+n], body[2+n:])

    def ping(self):
        self._write(b'\xc0\x00')

    def close(self):
        if self.sock:
            try:
                self.sock.close()
            except OSError:
                pass
            self.sock = None

"""Mesma filtragem no ESP32 virtual e na demonstração Python."""
class EdgeFilter:
    def __init__(self, size=3):
        self.size = size
        self.values = []

    def add(self, temperature, humidity):
        # Comparações rejeitam também NaN e infinitos.
        if (isinstance(temperature, bool) or isinstance(humidity, bool)
                or not isinstance(temperature, (int, float))
                or not isinstance(humidity, (int, float))
                or not -40 <= temperature <= 80 or not 0 <= humidity <= 100):
            raise ValueError('Leitura do sensor inválida')
        self.values.append((temperature, humidity))
        self.values = self.values[-self.size:]
        n = len(self.values)
        return (round(sum(x[0] for x in self.values) / n, 2),
                round(sum(x[1] for x in self.values) / n, 2))

"""ESP32 virtual no Wokwi. Copie também mqtt0.py e edge_logic.py."""
import network
import time
import machine
import dht
import json
import ubinascii
import os



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
