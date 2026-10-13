"""Teste opcional real por MQTT; não envia avisos a aplicativos.
Execute da pasta do projeto: python tests/check_mqtt_live.py
"""
import json
import sys
import time
import uuid
from pathlib import Path
BASE = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(BASE))
sys.path.insert(0, str(BASE / 'firmware'))
from app import Project
from mqtt0 import MQTT0


def run():
    cfg = json.loads((BASE / 'config.json').read_text())
    cfg.update(mode='mqtt', database=':memory:', ntfy_topic='',
               topic_root=cfg['topic_root'] + '/teste-' + uuid.uuid4().hex[:12])
    project = Project(cfg)
    edge = None
    try:
        project.start()
        end = time.time() + 20
        while not project.broker_online and time.time() < end:
            time.sleep(.1)
        if not project.broker_online:
            raise RuntimeError('Broker público indisponível neste teste')
        def callback(topic, body):
            cmd = json.loads(body)
            edge.publish(cfg['topic_root'] + '/acks', json.dumps({
                'id': cmd['id'], 'device_id': cfg['device_id'], 'led': cmd['action'] == 'on'}))
        edge = MQTT0(cfg['mqtt_host'], cfg['mqtt_port'], 'teste-edge-' + uuid.uuid4().hex[:8], callback)
        edge.connect(cfg['topic_root'] + '/status', json.dumps({'device_id': cfg['device_id'], 'online': False}))
        edge.subscribe(cfg['topic_root'] + '/commands')
        for seq in range(1,4):
            edge.publish(cfg['topic_root'] + '/telemetry', json.dumps({
                'device_id': cfg['device_id'], 'boot_id': 'teste-rede', 'seq': seq,
                'temperature': 35, 'humidity': 45, 'raw_temperature': 35,
                'raw_humidity': 45, 'led': False}))
            end = time.time() + 10
            while len(project.snapshot()['history']) < seq and time.time() < end:
                time.sleep(.05)
        assert len(project.snapshot()['history']) == 3, 'Telemetria incompleta'
        assert len(project.snapshot()['alerts']) == 1, 'Alerta não criado'
        for action in ('on','off'):
            command_id = project.command(action)
            end = time.time() + 10
            while time.time() < end:
                edge.check()
                row = next(r for r in project.snapshot()['commands'] if r['id'] == command_id)
                if row['status'] == 'confirmado':
                    break
                time.sleep(.05)
            assert row['status'] == 'confirmado', 'Comando sem confirmação'
            assert project.led == (action == 'on'), 'LED incorreto'
        print('PASSOU: broker real, 3 telemetrias, alerta local e 2 comandos confirmados.')
        print('Mesmo cliente MQTT usado no firmware, executado aqui em Python do computador.')
        print('Não valida o sensor ou a execução do MicroPython dentro do Wokwi.')
    finally:
        if edge:
            edge.close()
        project.stop()


if __name__ == '__main__':
    run()
