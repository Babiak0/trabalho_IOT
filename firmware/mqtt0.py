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
