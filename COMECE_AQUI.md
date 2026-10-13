# Comece aqui - Ambiente Monitorado

Este pacote contém somente o código e este guia curto. Não inclui relatório ou slides.
Não há integração com Telegram. Os alertas aparecem no painel; ntfy é opcional.

## 1. Rodar a demonstração no computador

1. Extraia o ZIP.
2. Tenha Python 3.10 ou superior instalado com o Python Launcher.
3. Abra iniciar.bat na pasta AmbienteMonitorado. A primeira instalação precisa de internet.
4. Mantenha a janela aberta e acesse http://127.0.0.1:5000 no navegador.
5. Em Simular condições, altere temperatura e umidade. Use os botões para ligar/desligar o LED.

O modo inicial é demo: simulação interna para começar a desenvolver. Não usa MQTT.
O banco data/ambiente.db é criado automaticamente. Encerre o servidor com Ctrl+C.

## 2. Integrar com ESP32 virtual e MQTT

1. Pare o servidor e altere "mode": "demo" para "mode": "mqtt" no config.json.
2. Execute iniciar.bat novamente.
3. Abra https://wokwi.com/projects/new/micropython-esp32
4. Copie TODO o conteúdo de firmware/wokwi_main.py para o main.py do Wokwi.
5. Copie TODO o firmware/diagram.json para o diagram.json do Wokwi.
6. Inicie a simulação. Aguarde dispositivo e broker conectados no painel.
7. Altere o DHT22 no Wokwi e acione o LED pelo painel.

Para começar, use o arquivo único wokwi_main.py. Os arquivos main.py e mqtt0.py
são a variante modular equivalente, incluída para facilitar a divisão de trabalho.
Não é necessário copiá-los se usar a variante de arquivo único.

O tópico ROOT e o DEVICE do firmware precisam coincidir com topic_root e device_id
do config.json. Para outra cópia independente, use um novo tópico nos dois lugares.
O broker é público e a configuração acadêmica usa TCP sem TLS: somente dados fictícios.
Internet é necessária no modo MQTT. Não há hardware físico neste pacote.

## 3. Onde cada parte está

- app.py: servidor, banco SQLite, MQTT, comandos, limites e alertas.
- edge_logic.py: validação e média móvel.
- static/index.html: painel, gráficos e controles.
- firmware/wokwi_main.py: código completo para o ESP32 virtual.
- firmware/diagram.json: circuito virtual.
- config.json: configurações e modo de execução.
- requirements.txt: bibliotecas Python.
- tests/: testes automatizados e teste MQTT opcional.

## 4. Alertas

Os limites iniciais são 30 °C e 70%, apenas para demonstração. Eleve os valores
para 40 °C e 85% e aguarde cerca de 30 segundos. Há média móvel e uma sequência
de três leituras acima do limite antes do alerta. Os lembretes respeitam 120 segundos.

Para avisos externos pelo ntfy, preencha ntfy_topic no config.json com um nome
aleatório exclusivo e assine o mesmo tópico em https://ntfy.sh ou no aplicativo.
Reinicie o servidor. Deixe vazio para manter somente os alertas no painel.
A recepção no aplicativo precisa ser verificada por vocês; não foi testada aqui.

## 5. Testar o código

Dentro da pasta do projeto:

    .venv\Scripts\python.exe -m unittest discover -s tests -p test_project.py -v

Teste opcional de MQTT real, com internet e sem avisos externos:

    .venv\Scripts\python.exe tests/check_mqtt_live.py

Foram verificados 13 testes locais e o fluxo integrado no Wokwi: coleta,
telemetria, painel, alertas locais e comandos de ligar/desligar.

Se a porta 5000 estiver ocupada, altere port no config.json e abra o novo endereço.
