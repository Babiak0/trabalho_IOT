"""Servidor local acadêmico. Execute: python app.py"""
import csv
import io
import json
import logging
import os
import re
import sqlite3
import threading
import time
import uuid
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.parse import urlparse

from flask import Flask, jsonify, request, send_file
import paho.mqtt.client as mqtt
from edge_logic import EdgeFilter

BASE = Path(__file__).resolve().parent
LOG = logging.getLogger('ambiente')


class Project:
    def __init__(self, config, clock=time.time, notify=None):
        self.cfg = config
        self.clock = clock
        self.notify = notify or self.send_ntfy
        self.lock = threading.RLock()
        self.stop_event = threading.Event()
        self.threads = []
        self.client = None
        self.broker_online = False
        self.error = ''
        self.status_offline = False
        self.demo_temperature, self.demo_humidity = 24.0, 45.0
        self.demo_paused = False
        self.led = False
        self.boot = uuid.uuid4().hex
        self.seq = 0
        self.filter = EdgeFilter()
        self.streak = {'temperature': 0, 'humidity': 0}
        self.active = {'temperature': False, 'humidity': False}
        self.last_alert = {'temperature': -float('inf'), 'humidity': -float('inf')}
        db_path = config.get('database', ':memory:')
        if db_path != ':memory:':
            db_path = str(BASE / db_path)
            Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(db_path, check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.executescript('''
            PRAGMA journal_mode=WAL;
            CREATE TABLE IF NOT EXISTS readings(
                id INTEGER PRIMARY KEY, received_at REAL NOT NULL,
                device_id TEXT NOT NULL, boot_id TEXT NOT NULL, seq INTEGER NOT NULL,
                temperature REAL NOT NULL, humidity REAL NOT NULL,
                raw_temperature REAL NOT NULL, raw_humidity REAL NOT NULL,
                led INTEGER NOT NULL, UNIQUE(device_id,boot_id,seq));
            CREATE TABLE IF NOT EXISTS alerts(
                id INTEGER PRIMARY KEY, created_at REAL NOT NULL, metric TEXT NOT NULL,
                message TEXT NOT NULL, status TEXT NOT NULL, attempts INTEGER DEFAULT 0,
                next_attempt REAL DEFAULT 0, error TEXT DEFAULT '');
            CREATE TABLE IF NOT EXISTS commands(
                id TEXT PRIMARY KEY, created_at REAL NOT NULL, expires_at REAL NOT NULL,
                action TEXT NOT NULL, status TEXT NOT NULL, ack_at REAL);
            CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY,value REAL NOT NULL);
            INSERT OR IGNORE INTO settings VALUES('temperature',30);
            INSERT OR IGNORE INTO settings VALUES('humidity',70);
        ''')
        self.db.commit()
        previous = self.db.execute('SELECT led FROM readings ORDER BY id DESC LIMIT 1').fetchone()
        if previous:
            self.led = bool(previous[0])

    def thresholds(self):
        with self.lock:
            return {r['key']: r['value'] for r in self.db.execute('SELECT * FROM settings')}

    def set_thresholds(self, data):
        values = {}
        for key, lo, hi in [('temperature', -40, 80), ('humidity', 0, 100)]:
            v = data.get(key)
            if isinstance(v, bool) or not isinstance(v, (int, float)) or not lo <= v <= hi:
                raise ValueError('Limites inválidos')
            values[key] = float(v)
        with self.lock:
            for key, value in values.items():
                self.db.execute('UPDATE settings SET value=? WHERE key=?', (value, key))
            self.db.commit()
            self.streak = dict.fromkeys(self.streak, 0)
            self.active = dict.fromkeys(self.active, False)
            self.last_alert = dict.fromkeys(self.last_alert, -float('inf'))
        return values

    def ingest(self, data):
        if not isinstance(data, dict) or data.get('device_id') != self.cfg['device_id']:
            raise ValueError('Dispositivo desconhecido')
        for name, lo, hi in [('temperature', -40, 80), ('humidity', 0, 100),
                             ('raw_temperature', -40, 80), ('raw_humidity', 0, 100)]:
            v = data.get(name)
            if isinstance(v, bool) or not isinstance(v, (int, float)) or not lo <= v <= hi:
                raise ValueError('Telemetria inválida: ' + name)
        seq, boot = data.get('seq'), data.get('boot_id')
        if type(seq) is not int or seq < 0 or not isinstance(boot, str) or not 1 <= len(boot) <= 64:
            raise ValueError('Identificador de mensagem inválido')
        if type(data.get('led')) is not bool:
            raise ValueError('Estado do LED inválido')
        with self.lock:
            # Descarta duplicatas e mensagens atrasadas do mesmo boot.
            prev = self.db.execute('SELECT MAX(seq) FROM readings WHERE device_id=? AND boot_id=?',
                                   (data['device_id'], boot)).fetchone()[0]
            if prev is not None and seq <= prev:
                return False
            now = self.clock()
            last = self.db.execute('SELECT received_at FROM readings ORDER BY id DESC LIMIT 1').fetchone()
            if last and now - last[0] > self.cfg['stale_seconds']:
                self.streak = dict.fromkeys(self.streak, 0)
            self.db.execute('''INSERT INTO readings(received_at,device_id,boot_id,seq,
                temperature,humidity,raw_temperature,raw_humidity,led) VALUES(?,?,?,?,?,?,?,?,?)''',
                (now, data['device_id'], boot, seq, data['temperature'], data['humidity'],
                 data['raw_temperature'], data['raw_humidity'], int(data['led'])))
            self.led = data['led']
            self.status_offline = False
            thresholds = self.thresholds()
            for key in self.streak:
                value = data[key]
                if value > thresholds[key]:
                    self.streak[key] += 1
                    if (self.streak[key] >= self.cfg['alert_samples'] and
                            now - self.last_alert[key] >= self.cfg['alert_cooldown_seconds']):
                        label, unit = ('Temperatura', '°C') if key == 'temperature' else ('Umidade', '%')
                        message = f'{label} elevada: {value:.1f} {unit}. Limite: {thresholds[key]:.1f} {unit}.'
                        status = 'pendente' if self.cfg.get('ntfy_topic') else 'painel'
                        self.db.execute('INSERT INTO alerts(created_at,metric,message,status,next_attempt) VALUES(?,?,?,?,?)',
                                        (now, key, message, status, now))
                        self.active[key] = True
                        self.last_alert[key] = now
                else:
                    self.streak[key] = 0
                    hysteresis = 2 if key == 'temperature' else 5
                    if value <= thresholds[key] - hysteresis:
                        self.active[key] = False
            self.db.commit()
        return True

    def acknowledge(self, data):
        if not isinstance(data, dict) or data.get('device_id') != self.cfg['device_id']:
            return False
        with self.lock:
            row = self.db.execute('SELECT * FROM commands WHERE id=?', (data.get('id'),)).fetchone()
            if row is None or row['status'] not in ('pendente', 'expirado'):
                return False
            expected = row['action'] == 'on'
            if type(data.get('led')) is not bool or data['led'] != expected:
                return False
            now = self.clock()
            status = 'confirmado' if now <= row['expires_at'] else 'confirmado_tardio'
            self.db.execute('UPDATE commands SET status=?,ack_at=? WHERE id=?', (status, now, row['id']))
            self.db.commit()
            self.led = expected
            return True

    def command(self, action):
        if action not in ('on', 'off'):
            raise ValueError('Comando inválido')
        with self.lock:
            if self.cfg['mode'] == 'mqtt' and not self.broker_online:
                raise RuntimeError('Broker desconectado. Aguarde a conexão.')
            if not self.device_online():
                raise RuntimeError('Dispositivo sem leitura recente. Aguarde a conexão.')
            now = self.clock()
            command_id = uuid.uuid4().hex
            self.db.execute('INSERT INTO commands VALUES(?,?,?,?,?,NULL)',
                            (command_id, now, now + self.cfg['command_timeout_seconds'], action, 'pendente'))
            self.db.commit()
            payload = {'id': command_id, 'action': action, 'device_id': self.cfg['device_id']}
            if self.cfg['mode'] == 'demo':
                self.acknowledge({'id': command_id, 'device_id': self.cfg['device_id'], 'led': action == 'on'})
            else:
                result = self.client.publish(self.cfg['topic_root'] + '/commands', json.dumps(payload), qos=0, retain=False)
                if result.rc != mqtt.MQTT_ERR_SUCCESS:
                    self.db.execute('UPDATE commands SET status=? WHERE id=?', ('falhou', command_id))
                    self.db.commit()
                    raise RuntimeError('Falha ao publicar o comando')
            return command_id

    def device_online(self):
        with self.lock:
            row = self.db.execute('SELECT received_at FROM readings ORDER BY id DESC LIMIT 1').fetchone()
            return bool(row and not self.status_offline and self.clock() - row[0] <= self.cfg['stale_seconds'])

    def snapshot(self):
        with self.lock:
            self.db.execute("UPDATE commands SET status='expirado' WHERE status='pendente' AND expires_at<?", (self.clock(),))
            self.db.commit()
            readings = [dict(r) for r in self.db.execute('SELECT * FROM readings ORDER BY id DESC LIMIT 120')][::-1]
            latest = readings[-1] if readings else None
            return {'mode': self.cfg['mode'], 'device_online': self.device_online(),
                    'broker_online': self.broker_online, 'error': self.error,
                    'led': self.led, 'latest': latest, 'history': readings,
                    'thresholds': self.thresholds(), 'active': dict(self.active),
                    'alerts': [dict(r) for r in self.db.execute('SELECT * FROM alerts ORDER BY id DESC LIMIT 20')],
                    'commands': [dict(r) for r in self.db.execute('SELECT * FROM commands ORDER BY created_at DESC LIMIT 10')],
                    'ntfy_enabled': bool(self.cfg.get('ntfy_topic')),
                    'topic_root': self.cfg['topic_root'], 'device_id': self.cfg['device_id'],
                    'demo_paused': self.demo_paused}

    def demo_step(self):
        with self.lock:
            if self.demo_paused:
                return
            temperature, humidity = self.filter.add(self.demo_temperature, self.demo_humidity)
            self.seq += 1
            self.ingest({'device_id': self.cfg['device_id'], 'boot_id': self.boot, 'seq': self.seq,
                         'temperature': temperature, 'humidity': humidity,
                         'raw_temperature': self.demo_temperature, 'raw_humidity': self.demo_humidity,
                         'led': self.led})

    def configure_demo(self, data):
        if self.cfg['mode'] != 'demo':
            raise ValueError('Controles de simulação só estão disponíveis no modo demonstração')
        with self.lock:
            if 'paused' in data:
                if type(data['paused']) is not bool:
                    raise ValueError('Pausa inválida')
                self.demo_paused = data['paused']
            if 'temperature' in data or 'humidity' in data:
                EdgeFilter().add(data.get('temperature'), data.get('humidity'))
                self.demo_temperature = data['temperature']
                self.demo_humidity = data['humidity']

    def send_ntfy(self, message):
        topic = self.cfg['ntfy_topic']
        if not re.fullmatch(r'[-_A-Za-z0-9]{1,64}', topic):
            raise ValueError('Tópico ntfy inválido')
        url = self.cfg.get('ntfy_server', 'https://ntfy.sh').rstrip('/') + '/' + topic
        headers = {'Title': 'Ambiente Monitorado', 'Priority': 'default', 'Tags': 'warning'}
        if os.environ.get('NTFY_TOKEN'):
            headers['Authorization'] = 'Bearer ' + os.environ['NTFY_TOKEN']
        req = Request(url, data=message.encode('utf-8'), headers=headers, method='POST')
        with urlopen(req, timeout=5) as response:
            if response.status != 200:
                raise RuntimeError('ntfy recusou o envio')

    def deliver_one(self):
        with self.lock:
            row = self.db.execute("SELECT * FROM alerts WHERE status='pendente' AND next_attempt<=? ORDER BY id LIMIT 1",
                                  (self.clock(),)).fetchone()
        if row is None:
            return False
        error = ''
        try:
            self.notify(row['message'])
            status = 'enviado'
        except Exception as exc:
            error = type(exc).__name__  # Nunca gravar tokens/URLs privadas em mensagens de erro.
            status = 'falhou' if row['attempts'] + 1 >= 3 else 'pendente'
        with self.lock:
            self.db.execute('UPDATE alerts SET status=?,attempts=attempts+1,next_attempt=?,error=? WHERE id=?',
                            (status, self.clock() + 10 * (2 ** row['attempts']), error, row['id']))
            self.db.commit()
        return True

    def mqtt_message(self, client, userdata, msg):
        try:
            if len(msg.payload) > 4096 or msg.retain:
                return  # Não executar eventos antigos retidos no broker público.
            data = json.loads(msg.payload)
            suffix = msg.topic.rsplit('/', 1)[-1]
            if suffix == 'telemetry':
                self.ingest(data)
            elif suffix == 'acks':
                self.acknowledge(data)
            elif suffix == 'status' and isinstance(data, dict) and data.get('device_id') == self.cfg['device_id']:
                with self.lock:
                    self.status_offline = data.get('online') is False
        except (ValueError, TypeError, sqlite3.Error):
            LOG.warning('Mensagem inválida descartada')

    def start(self):
        def notification_loop():
            while not self.stop_event.is_set():
                self.deliver_one()
                self.stop_event.wait(1)
        def demo_loop():
            while not self.stop_event.is_set():
                self.demo_step()
                self.stop_event.wait(self.cfg['sample_seconds'])
        jobs = [notification_loop]
        if self.cfg['mode'] == 'demo':
            jobs.append(demo_loop)
        else:
            self.client = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2,
                                      client_id='backend-' + uuid.uuid4().hex[:16])
            self.client.on_message = self.mqtt_message
            def connected(client, userdata, flags, reason, properties):
                with self.lock:
                    self.broker_online = not reason.is_failure
                    self.error = '' if self.broker_online else 'Conexão MQTT recusada'
                if self.broker_online:
                    for suffix in ('telemetry', 'acks', 'status'):
                        client.subscribe(self.cfg['topic_root'] + '/' + suffix, qos=0)
            def disconnected(client, userdata, flags, reason, properties):
                with self.lock:
                    self.broker_online = False
                    self.error = 'Broker desconectado; tentando reconectar'
            self.client.on_connect = connected
            self.client.on_disconnect = disconnected
            if self.cfg.get('mqtt_username'):
                self.client.username_pw_set(self.cfg['mqtt_username'], os.environ.get('MQTT_PASSWORD', ''))
            if self.cfg.get('mqtt_tls'):
                self.client.tls_set()
            self.client.reconnect_delay_set(min_delay=1, max_delay=30)
            self.client.connect_async(self.cfg['mqtt_host'], self.cfg['mqtt_port'], keepalive=30)
            self.client.loop_start()
        for job in jobs:
            thread = threading.Thread(target=job, daemon=True)
            thread.start()
            self.threads.append(thread)

    def stop(self):
        self.stop_event.set()
        for thread in self.threads:
            thread.join(timeout=6)
        if self.client:
            self.client.disconnect()
            self.client.loop_stop()
        with self.lock:
            self.db.close()


def create_app(project):
    app = Flask(__name__)
    app.config['MAX_CONTENT_LENGTH'] = 4096

    @app.before_request
    def local_controls():
        # Aplicação demonstrativa exposta só em loopback. Bloqueia POST de outros sites.
        if request.method == 'POST':
            origin = request.headers.get('Origin')
            if origin and urlparse(origin).netloc != request.host:
                return jsonify(error='Origem não permitida'), 403
            if not request.is_json:
                return jsonify(error='Envie JSON'), 415
            if not isinstance(request.get_json(silent=True), dict):
                return jsonify(error='Objeto JSON inválido'), 400

    @app.errorhandler(ValueError)
    def invalid(exc):
        return jsonify(error=str(exc)), 400

    @app.errorhandler(RuntimeError)
    def unavailable(exc):
        return jsonify(error=str(exc)), 503

    @app.get('/')
    def index():
        return send_file(BASE / 'static' / 'index.html')

    @app.get('/api/state')
    def state():
        return jsonify(project.snapshot())

    @app.post('/api/command')
    def command():
        return jsonify(id=project.command(request.json.get('action')))

    @app.post('/api/thresholds')
    def thresholds():
        return jsonify(project.set_thresholds(request.json))

    @app.post('/api/demo')
    def demo():
        project.configure_demo(request.json)
        return jsonify(ok=True)

    @app.get('/api/export.csv')
    def export():
        output = io.StringIO()
        writer = csv.writer(output)
        writer.writerow(['id', 'recebido_utc_epoch', 'dispositivo', 'boot', 'sequencia',
                         'temperatura_media', 'umidade_media', 'temperatura_bruta', 'umidade_bruta', 'led'])
        with project.lock:
            writer.writerows(tuple(r) for r in project.db.execute('SELECT * FROM readings ORDER BY id'))
        return app.response_class('\ufeff' + output.getvalue(), mimetype='text/csv',
                                  headers={'Content-Disposition': 'attachment; filename=leituras.csv'})
    return app


if __name__ == '__main__':
    logging.basicConfig(level=logging.INFO)
    cfg = json.loads((BASE / 'config.json').read_text(encoding='utf-8'))
    if cfg['mode'] not in ('demo', 'mqtt'):
        raise SystemExit('mode deve ser demo ou mqtt')
    project = Project(cfg)
    project.start()
    print(f"Painel: http://127.0.0.1:{cfg['port']} | Modo: {cfg['mode']}")
    try:
        create_app(project).run(host='127.0.0.1', port=cfg['port'], debug=False, use_reloader=False)
    finally:
        project.stop()
