# tikker
SMS gateway using MikroTik devices.

Status: projektowanie. Zobacz:
- [Specyfikacja](docs/SPEC.md)
- [Plan realizacji](docs/PLAN.md)
- [ADR 0001 – architektura](docs/adr/0001-architektura.md)
- [ADR 0002 – gwarancje dostarczenia](docs/adr/0002-gwarancje-dostarczenia.md)
- [ADR 0003 – biblioteki i układ pakietów](docs/adr/0003-biblioteki.md)
- [Wdrożenie na MikroTiku (kontener)](docs/deploy-mikrotik.md)
- [Wdrożenie serwerowe (Docker, systemd, WireGuard)](docs/deploy-server.md)
- [Wydawanie wersji i weryfikacja podpisów](docs/releasing.md)

Licencja: GNU GPL v3.0 – patrz [LICENSE](LICENSE).

MikroTik i RouterOS są znakami towarowymi MikroTik. Projekt nie jest powiązany z firmą MikroTik.

## Development

Wymagania: Go 1.25+, `make`, [golangci-lint](https://golangci-lint.run/) v2. Opcjonalnie `reuse` (`pip install reuse`).

```sh
make build                    # binarka bin/tikker (CGO_ENABLED=0, wersja z git describe)
make test                     # testy z -race i pokryciem
make lint                     # golangci-lint
make run ARGS="--version"     # uruchomienie z argumentami
reuse lint                    # zgodność nagłówków SPDX (REUSE)
```

### Szybki start (dev)

Bramka z emulatorem RouterOS (`fake-routeros`) zamiast prawdziwego routera, wszystko na pętli zwrotnej.

```sh
make build && go build -o bin/fake-routeros ./cmd/fake-routeros

# 1. Emulator routera (użytkownik tikker, hasło tikker-test, port lte1, API sterujące /_fake/)
bin/fake-routeros --listen 127.0.0.1:18728 --control &

# 2. Bramka: admin z TIKKER_ADMIN_PASSWORD, http:// dozwolone tylko do 127.0.0.1
export TIKKER_DATA_DIR=$PWD/data TIKKER_LISTEN=127.0.0.1:8080 TIKKER_LOG_FORMAT=text
export TIKKER_ADMIN_PASSWORD=change-me-please-123 TIKKER_ALLOWED_HTTP_HOSTS=127.0.0.1
bin/tikker serve &

# 3. Token API (panelu WWW jeszcze nie ma; token jest wypisywany tylko raz)
TOKEN=$(bin/tikker token create --name dev)

# 4. Urządzenie i kanał (id urządzenia z odpowiedzi)
curl -s -X POST localhost:8080/api/v1/devices -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"fake","base_url":"http://127.0.0.1:18728","username":"tikker","password":"tikker-test"}'
curl -s -X POST localhost:8080/api/v1/channels -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"device_id":"<id urządzenia>","port":"lte1"}'

# 5. Wysyłka: po chwili status "sent", a SMS widać w emulatorze
curl -s -X POST localhost:8080/api/v1/messages -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"to":"+48600100200","body":"Hello from tikker"}'
# odpowiedź: {"messages":[{"id":"<id wiadomości>","status":"queued",...}]}
curl -s localhost:8080/api/v1/messages/<id wiadomości> -H "Authorization: Bearer $TOKEN"
curl -s localhost:18728/_fake/sent

# 6. SMS przychodzący (widoczny po kolejnym odpytaniu skrzynki, domyślnie co 15 s)
curl -s -X POST localhost:18728/_fake/inject -d '{"port":"lte1","from":"+48511222333","text":"Odpowiedź: tak"}'
curl -s "localhost:8080/api/v1/messages?direction=in" -H "Authorization: Bearer $TOKEN"

curl -s localhost:8080/readyz
kill %2 %1   # SIGTERM: łagodne zamknięcie bramki, potem emulatora
```

Bez `TIKKER_ADMIN_PASSWORD` przy pustej bazie bramka startuje, ale nie ma użytkownika, dla którego można utworzyć token.

Zasady współtworzenia (DCO, Conventional Commits, nagłówki SPDX): [CONTRIBUTING.md](CONTRIBUTING.md).
Instrukcje dla agentów AI: [AGENTS.md](AGENTS.md). Zgłaszanie luk: [SECURITY.md](SECURITY.md).

### Development (English summary)

Requirements: Go 1.25+, `make`, golangci-lint v2. Run `make build`, `make test`, `make lint`.
Contributions use the Developer Certificate of Origin (`git commit -s`) and Conventional Commits.
See [CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) and [SECURITY.md](SECURITY.md).
