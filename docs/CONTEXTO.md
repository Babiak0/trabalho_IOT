# Contexto do projeto: Ambiente Monitorado

Situação do projeto em 12/11/2026: o que já foi feito, como está montado e o que ainda falta.
O guia técnico de instalação e uso está no [README](../README.md). Este arquivo não traz senhas nem tokens.

## 1. A disciplina e o desafio

- **Disciplina:** Arquitetura de Sistemas IoT e Computação em Nuvem. Prof. Eduardo Tamanaha. Engenharia de Software, 2026/2.
- **Datas:** a pré-banca foi na última semana de outubro. A **banca final é na 3ª semana de novembro**.
- **Entregáveis:** relatório técnico, apresentação de 20 minutos e demonstração ao vivo de ponta a ponta.
- **Requisitos do enunciado:**

| Requisito | Onde está atendido |
|---|---|
| Hardware físico e sensores | ESP32 DevKit V1 + DHT11 + 2 LEDs (seção 4) |
| Processamento na borda | Firmware: validação, rejeição de picos, média móvel, envio por exceção, alarme local, modo de segurança |
| MQTT com broker e segurança | HiveMQ Cloud com TLS 8883, usuário e senha por dispositivo, QoS 1, mensagens retidas, Last Will |
| Comunicação bidirecional | Comandos `/ligar` e `/desligar` pelo painel e pelo Telegram → ESP32 → confirmação (ack) |
| Banco de dados | PostgreSQL (leituras, alertas, comandos, estado dos alertas, fila de notificações) |
| Dashboard | Painel web com login, gráfico, comandos, limites, alertas e indicadores de SLA |
| Notificações no Telegram | Bot com alertas, lembretes e comandos |
| Análise IaaS / PaaS / SaaS | No relatório (VPS Oracle = IaaS, HiveMQ = PaaS/SaaS, Telegram = SaaS) |
| Proposta de SLA | No relatório; medida pela rota `/api/metrics` e pelos indicadores do painel |

## 2. Arquitetura

```
DHT11 → ESP32 (borda, C++) ⇄ MQTT/TLS ⇄ HiveMQ Cloud ⇄ Back-end Go ⇄ PostgreSQL
                                                          ├→ Painel web (navegador)
                                                          └⇄ Telegram (alertas e comandos)
```

- **Tecnologias:** Go (back-end), C++/Arduino (firmware), PostgreSQL, Docker. O protótipo antigo em Python foi removido.
- **Tópicos MQTT:** `ambiente/<id>/{telemetry,status,ack}` (ESP32 → nuvem) e `ambiente/<id>/{cmd,config}` (nuvem → ESP32).
- **Dispositivo:** `esp32-sala-01`. O simulador em Go (`esp32-sim-01`) foi desativado e apagado do banco de produção.

## 3. O que já está pronto

### Back-end (Go)
- **Recebimento:** cliente MQTT (paho) com validação da telemetria.
- **Alertas:** motor que dispara após 3 leituras seguidas fora dos limites, envia lembrete a cada 5 min e usa histerese de 1 °C / 3 %. O estado dos alertas fica salvo no banco, então reiniciar o back-end não duplica alertas.
- **Telegram:** fila de envio (outbox) com até 5 tentativas.
- **Watchdog:** marca o dispositivo como offline depois de 30 s sem dados.
- **Comandos:** timeout de 15 s e confirmação (ack) pelo ESP32. Pelo Telegram, um comando que não muda o estado não é enviado.
- **API REST e painel:** o painel é embutido no binário. O login usa cookie de sessão assinado (HMAC) e limita tentativas.
- **Bot do Telegram:** `/status`, `/ligar`, `/desligar`, `/limites` e `/ajuda`, com menu de comandos.
- **SLA e testes:** a rota `/api/metrics` traz latência p50/p95, completude e sucesso de comandos. Há testes automatizados (`go test ./...`).

### Painel web
- **Leituras:** valores em tempo real, gráfico, alertas, limites editáveis e exportação em CSV.
- **Botões de ventilação:** só o botão que muda o estado fica habilitado. Os dois ficam desabilitados com o dispositivo offline ou com um comando pendente.
- **Retorno do comando:** a mensagem acompanha o resultado (confirmado, confirmado com atraso ou sem resposta).
- **Responsivo:** corrigido para celular.

### Infraestrutura / nuvem
- **Produção:** VPS Oracle Ampere A1 (sa-saopaulo-1), em `/opt/iot-aps`, com `docker-compose.prod.yml` (PostgreSQL + back-end).
- **Acesso:** Cloudflare (proxy, SSL Full) → Nginx (TLS) → app em `127.0.0.1:18080`. Domínio: **iot.bruggeongrails.com**.
- **Broker:** HiveMQ Cloud Serverless, cluster já existente (o plano gratuito será desligado em 31/12/2026).
- **Desenvolvimento local:** `docker-compose.yml` com Mosquitto, senha e ACL por dispositivo.
- **VPS compartilhada:** outros serviços rodam nela, então o projeto fica isolado na própria pasta e porta.

### Firmware (ESP32)
- **Uma única base de código** para a placa física e para o Wokwi, escolhida na compilação:

| | Placa física (padrão) | Wokwi |
|---|---|---|
| PlatformIO | `pio run -e esp32dev -t upload` | `pio run -e wokwi` |
| Sensor | DHT11 no GPIO 4 | DHT22 no GPIO 4 |
| Ventilação (LED verde) | GPIO 26 | GPIO 18 |
| Alarme (LED vermelho) | GPIO 25 | GPIO 19 |

- **Processamento na borda:**
  - leituras impossíveis são descartadas;
  - saltos maiores que 5 °C / 15 % são rejeitados, a menos que se repitam 3 vezes;
  - média móvel de 5 amostras;
  - envio só quando muda 0,3 °C / 1 %, com batimento a cada 10 s e envio a cada 2 s em alarme;
  - alarme local;
  - modo de segurança: sem nuvem por 30 s e com calor, liga a ventilação sozinho;
  - limites salvos na flash (NVS).
- **Firmware 1.1.0:** já foi gravado na placa física pelo PlatformIO, na porta `/dev/cu.usbserial-0001` (chip CP2102). O boot e o sensor responderam sem erro.
- **Arduino IDE:** `tools/bundle-arduino.sh` monta a pasta pronta para gravar pela IDE, caso não se use o PlatformIO.
- **Wokwi:** `tools/bundle-wokwi.sh` gera o arquivo único para o simulador.
- **Limitação do Wokwi no navegador:** com a aba em segundo plano, a simulação é desacelerada e aparecem desconexões falsas. Foi aceito porque o Wokwi serve só para testes.

### Documentação
- **Relatório e slides:** `docs/relatorio-tecnico.docx` (formato ABNT) e `docs/apresentacao.pptx`.
- **Versões em Keynote:** a enxuta (12 slides, com as edições do grupo) e a exportação em PowerPoint ficam na Mesa do computador, fora do repositório.

## 4. Montagem física

| Componente | Ligação |
|---|---|
| ESP32 DevKit V1 (30 pinos) | Alimentado pela USB |
| Módulo DHT11 (3 pinos) | S → GPIO 4, + → 3V3, − → GND (o módulo já tem pull-up) |
| LED verde (ventilação) | GPIO 26 → resistor 220 Ω → LED → GND (HIGH = ligado) |
| LED vermelho (alarme) | GPIO 25 → resistor 220 Ω → LED → GND |

## 5. O que falta fazer

### Para a placa física funcionar de ponta a ponta
- [ ] **Wi-Fi:** colocar a rede **2,4 GHz** em `firmware/esp32/include/secrets.h` (ou no `secrets.h` da pasta da Arduino IDE) e gravar de novo.
- [ ] **Wokwi:** parar a simulação antes de ligar a placa, porque as duas usam o id `esp32-sala-01`.
- [ ] **Conexão:** no monitor serial (115200), confirmar `Broker conectado` e as linhas de `Telemetria #...`.
- [ ] **Painel:** verificar o dispositivo online e as leituras chegando em iot.bruggeongrails.com.
- [ ] **Comandos:** ligar e desligar a ventilação pelo painel e pelo Telegram e conferir o LED verde.
- [ ] **Alarme:** aquecer o sensor com a mão, ou baixar o limite no painel, e conferir o LED vermelho e o alerta no Telegram.

### Para a banca
- [ ] **Relatório:** trocar "simulação" por hardware real (DHT11, pinos 26/25), com foto da montagem e tabela de ligações.
- [ ] **Slides:** colocar foto da placa no slide de arquitetura ou de demonstração, e voltar a citar o SLA, se fizer falta.
- [ ] **Ensaio da demonstração:** levar um **hotspot 2,4 GHz no celular** já gravado na placa, porque o Wi-Fi da faculdade pode ser 5 GHz ou exigir login.
- [ ] **Roteiro sugerido da demonstração:**
  1. painel online;
  2. aquecer o sensor → LED vermelho + alerta no Telegram;
  3. `/ligar` no Telegram → LED verde + confirmação;
  4. desligar pelo painel;
  5. desconectar a placa → aviso de offline.

### Pendências opcionais
- [ ] Apagar a credencial `esp32-sim-01` no HiveMQ (a senha dela já apareceu em conversa).
- [ ] Gerar de novo o pacote do Wokwi (`tools/bundle-wokwi.sh`) para ter a simulação como reserva na banca.

## 6. Regras de trabalho do grupo

- **Git flow:** a branch `develop` recebe `feature/*` e `bugfix/*` com `--no-ff`. Commits em inglês, separados por assunto.
- **Segredos:** nunca versionar `.env`, `firmware/esp32/include/secrets.h`, `wokwi-bundle/` nem `arduino-bundle/`.
- **Uma instância por vez:** só um back-end pode usar o bot e o broker. A produção roda na VPS e o back-end local fica parado.
- **Deploy:** enviar os arquivos versionados com `COPYFILE_DISABLE=1 tar --no-xattrs` (veja o README) e validar o Nginx com `nginx -t` antes de recarregar.
