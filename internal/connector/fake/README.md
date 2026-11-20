<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Fake RouterOS (T13)

Emulator REST API RouterOS 7 dla testów tikkera: konektor `routeros-rest` (T12), kolejka (T15), poller (T16) i E2E.
Tylko biblioteka standardowa. Zachowanie wynika z [docs/research/routeros-sms.md](../../../docs/research/routeros-sms.md);
miejsca niezmierzone na sprzęcie (T00) są w kodzie oznaczone `// ASSUMPTION(T00):` (lista na końcu).

## Użycie w testach Go

```go
srv := fake.New(fake.Options{User: "tikker", Password: "secret", Ports: []string{"lte1", "lte2"}})
url := srv.Start() // httptest, http://127.0.0.1:PORT
defer srv.Close()

// konektor pod test wysyła na url ...

sent := srv.Sent()                       // rejestr przyjętych wysyłek (z czasami)
srv.InjectSMS(fake.IncomingSMS{From: "+48511222333", Text: "Cześć"})
srv.FailNext(fake.EndpointSend, fake.HTTPError(503, "busy"), 2) // 2 kolejne wysyłki: 503
```

`Server` jest też zwykłym `http.Handler` (`srv.ServeHTTP`), a `StartTLS()` + `Client()` + `Fingerprint()` służą do testów TLS
i przypinania odcisku.

## Emulowane endpointy

| Endpoint | Metoda | Odpowiedź |
|---|---|---|
| `/rest/tool/sms/send` | POST (JSON) | `[]`; GET daje 500 (jak realny router), brak `Content-Type: application/json` daje 415 |
| `/rest/tool/sms/inbox` | GET | lista `{".id","phone","timestamp","type","message","pdu"}` |
| `/rest/tool/sms/inbox/{id}` | DELETE | 204; nieznany `.id` daje 404 |
| `/rest/tool/sms/inbox/remove` | POST `{".id":"*1,*2"}` | `[]`; nieznany `.id` daje 400 `no such item (4)` (całość atomowo) |
| `/rest/tool/sms` | GET | ustawienia (`port`, `receive-enabled`, `auto-erase`, ...; wartości logiczne jako napisy `"true"`/`"false"`) |
| `/rest/interface/lte` | GET | lista interfejsów (po jednym na port) |
| `/rest/interface/lte/monitor` | POST `{"numbers":"lte1","once":true}` | jednoelementowa tablica: `registration-status`, `current-operator`, `rssi`, `rsrp`, `rsrq`, `sinr`, ... Bez `once` żądanie wisi do limitu sesji (400 `Session closed`) |
| `/rest/system/resource` | GET | `version`, `board-name`, `architecture-name`, ... |
| `/rest/system/routerboard` | GET | `model`, `serial-number`, firmware |

Uwierzytelnianie: basic auth. Zły login lub brak polityki `rest-api` daje 401, brak polityki `api` daje 500 `not allowed (9)`
(`SetPolicies`). Błędy mają format RouterOS: `{"error":400,"message":"Bad Request","detail":"..."}`. Nieznana ścieżka: 400
`no such command or directory (...)`.

## Zachowanie domyślne wysyłki (Capabilities z T00)

- tylko GSM-7 i najwyżej 160 septetów (znaki rozszerzone `^{}\[]~|€` liczą się za 2); inaczej 400 z `detail`
  `failure: Body too long` lub `failure: message contains characters outside the GSM 7-bit alphabet`;
- tryb `Lenient` (`Options.Lenient`, `SetLenient`, flaga `-lenient`) przyjmuje wszystko;
- jedna wysyłka naraz na port: kolejne czekają (`SentMessage.StartedAt/CompletedAt` pozwalają to zweryfikować);
- limit sesji REST 60 s (`Options.SessionTimeout`): wysyłka dłuższa kończy się 400 `Session closed`, a SMS i tak jest zapisany
  w rejestrze jako `Ambiguous`;
- brak rejestracji w sieci (`SetLTE`) daje 500 `failure: not registered to network`.

## API sterowania (Go, bezpieczne wątkowo)

| Metoda | Działanie |
|---|---|
| `SetSendDelay(d)`, `Options.SendDelay` | jak długo wysyłka zajmuje port |
| `SetSessionTimeout(d)` | limit sesji REST (domyślnie 60 s); ustaw mały w testach |
| `FailNext(ep, fault, n)` | `n` kolejnych wywołań `ep` kończy się błędem (`n<0`: aż do `ClearFaults`); `EndpointAny` dotyczy wszystkich |
| `ClearFaults()` | usuwa kolejkę błędów |
| `InjectSMS(IncomingSMS)` | SMS przychodzący z PDU; długi tekst dzieli się na części z UDH (osobne wpisy skrzynki) |
| `InjectRaw(RawInbox)` | wpis skrzynki dosłownie (zepsute PDU, nieparsowalny timestamp) |
| `Inbox()`, `SetInboxLimit(n)` | podgląd skrzynki, pojemność; pełna skrzynka bez `auto-erase` zwraca `ErrInboxFull`, z `auto-erase` kasuje najstarsze |
| `SetLTE(port, func(*LTEState))`, `LTE(port)` | sygnał, operator, rejestracja (`home`, `roaming`, `searching`, `denied`, `not-registered`) |
| `SetSettings(func(*Settings))`, `Settings()` | `/tool sms` (`AutoErase`, `ReceiveEnabled`, ...) |
| `SetPolicies(Policies)`, `SetCredentials(u, p)` | polityki użytkownika, rotacja hasła |
| `Sent()`, `WaitSent(n, timeout)` | rejestr przyjętych wysyłek: port, numer, treść, `Septets`, `ReceivedAt/StartedAt/CompletedAt`, `Ambiguous` |
| `Requests(ep)`, `AllRequests()`, `AuthFailures()` | liczniki żądań (po uwierzytelnieniu, razem z celowo nieudanymi) |
| `Reset()` | stan początkowy (skrzynka, rejestr, błędy, liczniki, LTE, ustawienia, dane logowania) |

### Błędy (`Fault`)

| Konstruktor | Skutek |
|---|---|
| `HTTPError(status, detail)` | JSON błędu RouterOS (np. 500, 400 z `detail`) |
| `Unauthorized()` | 401 |
| `NotAllowed()` | 500 `not allowed (9)` |
| `SessionClosed()` | 400 `Session closed` |
| `DropConnection()` | zerwanie połączenia bez odpowiedzi |
| `Hang()` | brak odpowiedzi do limitu sesji, potem 400 `Session closed` (lub aż klient się podda) |
| `.AfterAccept()` | (tylko `send`) router najpierw przyjmuje SMS (wpis w `Sent()` z `Ambiguous=true`), dopiero potem błąd: scenariusz „timeout po przyjęciu” |
| `.After(d)` | opóźnia wystąpienie błędu |

Przykład „niejednoznaczny timeout”:

```go
srv.FailNext(fake.EndpointSend, fake.Hang().AfterAccept(), 1)
// klient z timeoutem 1 s dostaje błąd sieciowy, a srv.Sent()[0].Ambiguous == true
```

### Budowanie PDU

`BuildDeliverPDU(PDUParams, text)` buduje szesnastkowy SMS-DELIVER (z polem SMSC) dla GSM-7 i UCS-2, także z UDH
(`Concat`, 8- i 16-bitowy numer referencyjny); `BuildDeliverPDUs` dzieli długi tekst na części (153/152 septety lub 67/66
jednostek UTF-16, bez rozcinania znaków rozszerzonych i par zastępczych). Nadawca może być numerem (`+48...`, `511...`) lub
alfanumeryczny. Układ wg 3GPP TS 23.040 §9.2.2.1, pakowanie wg TS 23.038 §6.1.2.1. Pole `message` wpisu skrzynki ma
GSM-7 dla tekstu 7-bitowego, a dla UCS-2 „krzaki” (RouterOS dekoduje wszystko jako GSM-7), więc klient musi czytać `pdu`.

## Binarka `cmd/fake-routeros`

```sh
go run ./cmd/fake-routeros -listen 127.0.0.1:8080 -user tikker -password secret -ports lte1,lte2 -control
go run ./cmd/fake-routeros -tls -tls-hosts router.local   # wypisuje odcisk SHA-256 certyfikatu
```

Flagi: `-listen`, `-user`, `-password`, `-ports`, `-tls`, `-tls-hosts`, `-lenient`, `-control`, `-send-delay`,
`-session-timeout`, `-inbox-limit`, `-auto-erase`. Certyfikat TLS jest samopodpisany (ECDSA P-256); w logu pojawia się
odcisk w postaci `AB:CD:...` i zwykłym hexem, do wpisania w konfiguracji urządzenia (`tls_mode=fingerprint`).

### Endpoint sterujący `/_fake/` (tylko z `-control`)

Bez uwierzytelniania: używaj na loopbacku lub w sieci testowej. Bez flagi ścieżka `/_fake/...` trafia do emulacji RouterOS
(401 bez danych logowania).

```sh
# SMS przychodzący (długi tekst zostanie podzielony z UDH; encoding: auto|gsm7|ucs2)
curl -X POST localhost:8080/_fake/inject -d '{"from":"+48511222333","text":"Cześć!","port":"lte1"}'
# wpis surowy
curl -X POST localhost:8080/_fake/inject -d '{"raw":{"phone":"+48511222333","message":"x","pdu":"00","timestamp_text":"???"}}'
# 2 kolejne wysyłki kończą się 503; kind: http|drop|hang; accepted:true = po przyjęciu SMS
curl -X POST localhost:8080/_fake/fail -d '{"endpoint":"send","status":503,"detail":"busy","count":2}'
curl -X DELETE localhost:8080/_fake/fail                       # wyczyść błędy
curl -X POST localhost:8080/_fake/config -d '{"send_delay_ms":2000,"lenient":true,"inbox_limit":3,"auto_erase":false}'
curl -X POST localhost:8080/_fake/lte -d '{"port":"lte1","registration":"searching","no_signal":true}'
curl localhost:8080/_fake/sent       # rejestr wysłanych
curl localhost:8080/_fake/inbox      # skrzynka
curl localhost:8080/_fake/requests   # liczniki per endpoint
curl -X POST localhost:8080/_fake/reset
```

## Założenia do poprawy po T00

Szukaj `ASSUMPTION(T00)` w kodzie. Najważniejsze: format `timestamp` skrzynki (domyślnie `2026-11-19 12:00:00`, dostępny
też `TimestampLegacy`), obecność SMSC w `pdu`, brak pola `port` we wpisach skrzynki, kody i teksty błędów dla zbyt długiej
wiadomości, znaków spoza GSM-7, braku rejestracji, braku argumentów i nieznanego `.id` (DELETE 404, remove 400), odpowiedź
`[]` na `send`, nazwy pól i wartości `monitor`, zachowanie `monitor` bez `once` oraz to, czy SMS wychodzi po `Session closed`.
