import json
import math
import sys
import unittest
from unittest.mock import patch, MagicMock
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from app import Project, create_app
from edge_logic import EdgeFilter


class ProjectTests(unittest.TestCase):
    def setUp(self):
        self.now = 1000.0
        self.sent = []
        config = json.loads((Path(__file__).resolve().parents[1] / 'config.json').read_text())
        config['database'] = ':memory:'
        self.project = Project(config, clock=lambda: self.now, notify=self.sent.append)
        self.http = create_app(self.project).test_client()
        self.seq = 0

    def tearDown(self):
        self.project.stop()

    def reading(self, temperature=24, humidity=45, **changes):
        self.seq += 1
        data = {'device_id': self.project.cfg['device_id'], 'boot_id': 'test-boot',
                'seq': self.seq, 'temperature': temperature, 'humidity': humidity,
                'raw_temperature': temperature, 'raw_humidity': humidity, 'led': False}
        data.update(changes)
        self.project.ingest(data)
        return data

    def test_filter_and_invalid_values(self):
        f = EdgeFilter()
        self.assertEqual(f.add(24, 40), (24, 40))
        self.assertEqual(f.add(30, 60), (27, 50))
        self.assertEqual(f.add(36, 80), (30, 60))
        self.assertEqual(f.add(42, 100), (36, 80))
        for value in (math.nan, math.inf, True, '30', 81):
            with self.assertRaises(ValueError):
                f.add(value, 50)

    def test_duplicate_and_out_of_order(self):
        data = self.reading()
        self.assertFalse(self.project.ingest(data))
        data['seq'] = 0
        self.assertFalse(self.project.ingest(data))
        self.assertEqual(len(self.project.snapshot()['history']), 1)

    def test_invalid_device_and_numeric_payload(self):
        for changes in ({'temperature': math.nan}, {'humidity': 101}, {'led': 1},
                        {'device_id': 'outro'}, {'seq': True}, {'boot_id': ''}):
            with self.assertRaises(ValueError):
                self.reading(**changes)
        self.assertEqual(self.project.snapshot()['history'], [])

    def test_three_consecutive_and_cooldown(self):
        self.reading(31)
        self.reading(31)
        self.assertEqual(self.project.snapshot()['alerts'], [])
        self.reading(31)
        self.assertEqual(len(self.project.snapshot()['alerts']), 1)
        self.reading(31)
        self.assertEqual(len(self.project.snapshot()['alerts']), 1)
        self.now += 120
        # Uma interrupção longa reinicia a contagem.
        for _ in range(3):
            self.reading(31)
        self.assertEqual(len(self.project.snapshot()['alerts']), 2)

    def test_normal_reading_resets_streak_and_hysteresis(self):
        self.reading(31)
        self.reading(30)
        self.reading(31)
        self.reading(31)
        self.assertEqual(len(self.project.snapshot()['alerts']), 0)
        self.reading(31)
        self.assertTrue(self.project.snapshot()['active']['temperature'])
        self.reading(29)
        self.assertTrue(self.project.snapshot()['active']['temperature'])
        self.reading(28)
        self.assertFalse(self.project.snapshot()['active']['temperature'])

    def test_staleness_and_pause(self):
        self.project.demo_step()
        self.assertTrue(self.project.device_online())
        self.project.configure_demo({'paused': True})
        self.project.demo_step()
        self.now += 21
        self.assertFalse(self.project.device_online())
        self.assertEqual(len(self.project.snapshot()['history']), 1)
        self.project.configure_demo({'paused': False})
        self.project.demo_step()
        self.assertTrue(self.project.device_online())

    def test_command_demo_confirms(self):
        self.reading()
        response = self.http.post('/api/command', json={'action': 'on'})
        self.assertEqual(response.status_code, 200)
        s = self.project.snapshot()
        self.assertTrue(s['led'])
        self.assertEqual(s['commands'][0]['status'], 'confirmado')
        self.assertEqual(self.http.post('/api/command', json={'action': 'toggle'}).status_code, 400)

    def test_disconnected_command(self):
        self.assertEqual(self.http.post('/api/command', json={'action': 'on'}).status_code, 503)
        self.reading()
        self.project.cfg['mode'] = 'mqtt'
        self.assertEqual(self.http.post('/api/command', json={'action': 'on'}).status_code, 503)

    def test_expiry_and_ack_matching(self):
        self.project.db.execute('INSERT INTO commands VALUES(?,?,?,?,?,NULL)', ('cmd', self.now, self.now+15, 'on', 'pendente'))
        self.project.db.commit()
        self.assertFalse(self.project.acknowledge({'device_id': self.project.cfg['device_id'], 'id': 'cmd', 'led': False}))
        self.now += 16
        self.assertEqual(self.project.snapshot()['commands'][0]['status'], 'expirado')
        self.assertTrue(self.project.acknowledge({'device_id': self.project.cfg['device_id'], 'id': 'cmd', 'led': True}))
        self.assertEqual(self.project.snapshot()['commands'][0]['status'], 'confirmado_tardio')

    def test_threshold_validation_and_persistence(self):
        self.assertEqual(self.http.post('/api/thresholds', json={'temperature': 35, 'humidity': 75}).status_code, 200)
        self.assertEqual(self.project.thresholds(), {'temperature': 35, 'humidity': 75})
        self.assertEqual(self.http.post('/api/thresholds', json={'temperature': '35', 'humidity': 75}).status_code, 400)

    def test_ntfy_mock_and_retry(self):
        self.project.cfg['ntfy_topic'] = 'apenas-teste-mock'
        for _ in range(3):
            self.reading(35)
        self.assertTrue(self.project.deliver_one())
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.project.snapshot()['alerts'][0]['status'], 'enviado')
        self.now += 120
        for _ in range(3):
            self.reading(35)
        def fail(message):
            raise OSError('indisponível')
        self.project.notify = fail
        for delta in (0, 10, 20):
            self.now += delta
            self.project.deliver_one()
        alert = self.project.snapshot()['alerts'][0]
        self.assertEqual(alert['status'], 'falhou')
        self.assertEqual(alert['attempts'], 3)

    def test_http_export_and_cross_origin(self):
        self.reading(25)
        page = self.http.get('/')
        self.assertEqual(page.status_code, 200)
        page.close()
        self.assertEqual(self.http.get('/api/state').status_code, 200)
        csv = self.http.get('/api/export.csv')
        self.assertIn('temperatura_media', csv.data.decode('utf-8'))
        self.assertIn('25.0', csv.data.decode('utf-8'))
        self.assertEqual(self.http.post('/api/demo', json={'paused': True}, headers={'Origin': 'https://outro.example'}).status_code, 403)
        self.assertEqual(self.http.post('/api/demo', json=[]).status_code, 400)
        self.assertEqual(self.http.post('/api/demo', data='nada').status_code, 415)

    def test_ntfy_http_request_without_external_send(self):
        self.project.cfg['ntfy_topic'] = 'teste-local-apenas'
        response = MagicMock()
        response.__enter__.return_value.status = 200
        with patch('app.urlopen', return_value=response) as opened:
            self.project.send_ntfy('Temperatura elevada: 35 °C')
        req = opened.call_args.args[0]
        self.assertEqual(req.full_url, 'https://ntfy.sh/teste-local-apenas')
        self.assertEqual(req.get_method(), 'POST')
        self.assertEqual(req.data.decode('utf-8'), 'Temperatura elevada: 35 °C')
        self.assertEqual(req.get_header('Title'), 'Ambiente Monitorado')
        self.assertEqual(opened.call_args.kwargs['timeout'], 5)


if __name__ == '__main__':
    unittest.main(verbosity=2)
