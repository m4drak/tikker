<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Konektor `routeros-rest` (T12)

Implementacja `connector.Connector` dla REST API RouterOS 7 (SPEC §3). Tylko biblioteka standardowa.
Testy działają wyłącznie przeciw [fake RouterOS](../fake/README.md) (T13).

```go
reg := connector.NewRegistry()
err := routeros.Register(reg, routeros.Options{Logger: logger}) // bez init(), bez globalnego rejestru
conn, err := reg.New(ctx, connector.DeviceConfigFromDevice(dev, password))
```

## Klient HTTP

- Własny `http.Transport`: keep-alive, `MaxConnsPerHost=8`, timeout dial i TLS 10 s, `ResponseHeaderTimeout`
  = `SendTimeout + 5 s` (zabezpieczenie; właściwe limity dają konteksty), bez proxy ze środowiska, bez HTTP/2,
  bez podążania za przekierowaniami (nagłówek basic auth nie może trafić na inny host).
- Każde żądanie: basic auth, `Content-Type: application/json`, kontekst z timeoutem. Komendy idą jako POST z JSON
  (GET z parametrami nie działa). Odpowiedź jest ograniczona do 4 MiB (`Options.MaxResponseBytes`).
- Timeout wysyłki: `Capabilities.SendTimeout` = 65 s. Pozostałe wywołania: `DeviceConfig.Timeout` albo 15 s.
- Jedna wysyłka naraz na port (semafor o pojemności `MaxConcurrentSends`, oczekiwanie honoruje `ctx`).
  Wysyłki na różnych portach idą równolegle.
- TLS (`tls_mode`): `verify` – systemowe CA; `ca` – tylko CA z `ca_pem`; `fingerprint` – przypięcie SHA-256
  certyfikatu liścia (`InsecureSkipVerify` wyłącznie razem z własną weryfikacją w `VerifyPeerCertificate`);
  `insecure` – bez weryfikacji, z ostrzeżeniem w logu. `http://` działa bez TLS; konektor tylko to zgłasza
  (`Conn.InsecureTransport()`, `DeviceProbe.TLS == nil`), a o dopuszczeniu decyduje API.
- Usuwanie ze skrzynki: najpierw `DELETE /tool/sms/inbox/{id}`; przy 405/501, 404 bez „no such item” lub 400
  „no such command” konektor przechodzi na `POST /tool/sms/inbox/remove` i zapamiętuje wariant dla urządzenia.
  Wpis, którego już nie ma („no such item”), liczy się jako usunięty. Identyfikatory są usuwane pojedynczo.
- `FetchInbox` zwraca surowe `pdu`, surowy `timestamp` i `.id`; dekodowanie PDU i sklejanie części robią
  `internal/sms` i poller (T14/T16). Jeśli wpisy nie mają pola `port`, skrzynka należy do portu z `/tool sms`
  (inne porty dostają pustą listę).
- `Probe`: `/system/resource` (łączność, TLS, logowanie, wersja ≥ 7.16), `/system/routerboard` (opcjonalnie),
  `/tool/sms` (pakiet i uprawnienia), `/interface/lte` + `monitor once` dla każdego portu. Po błędzie TLS konektor
  osobno pobiera certyfikat (bez wysyłania danych), żeby UI mogło zaproponować odcisk do przypięcia.

## Klasyfikacja błędów

Tabela reguł jest w jednym miejscu: `statusRules` w [errors.go](errors.go). „Wysłane” oznacza, że żądanie mogło
dotrzeć do routera (uzyskano połączenie i zapis nie zawiódł); tylko wtedy błąd wysyłki jest niejednoznaczny.

| Sytuacja | Klasa | `Ambiguous` | Kod (`DeviceTestResult`) |
|---|---|---|---|
| walidacja lokalna: kodowanie spoza `Capabilities`, > 1 segmentu, > 160 septetów, znak spoza GSM-7, brak numeru/portu | Permanent (bez wywołania routera) | nie | – |
| odmowa połączenia, DNS, zerwanie przed zapisem | Transient | nie | `unreachable` |
| błąd TLS (CA, nazwa hosta, odcisk, HTTP zamiast HTTPS) | Transient | nie | `tls_error` |
| timeout przed uzyskaniem połączenia / czekanie na port | Transient | nie | `timeout` |
| timeout lub zerwanie po wysłaniu żądania `send` | Transient | **tak** | `timeout` / `unreachable` |
| anulowanie `ctx` po wysłaniu żądania `send` | Transient | **tak** | – |
| 401 | Auth | nie | `auth_failed` |
| 403 | Auth | nie | `insufficient_policy` |
| 500 `not allowed` / `not enough permissions` | Auth | nie | `insufficient_policy` |
| 400 `Session closed` (limit 60 s REST) | Transient | **tak** (dla `send`) | `timeout` |
| 504 | Transient | **tak** (dla `send`) | `timeout` |
| `no such command` | Transient | nie | `unsupported_version` |
| `not registered`, `no sim`, `busy`, `inactive`, `timeout`, `not ready`, `modem` | Transient | nie | – |
| 400 dla `send`: `invalid phone`, `phone-number`, `too long`, `outside the GSM`, `7-bit` | Permanent | nie | – |
| inny 5xx | Transient | nie | – |
| 3xx, inny 4xx (np. 400 nieznany port, 415), odpowiedź za duża lub nie-JSON | Transient | nie | `unexpected_response` |
| `Probe`: wersja < 7.16 / nieczytelna | Permanent | nie | `unsupported_version` / `unexpected_response` |

Odpowiedź 2xx na `send` oznacza przyjęcie, nawet jeśli treści odpowiedzi nie da się odczytać. Teksty błędów
z routera są obcinane do 160 znaków, a ciągi cyfr maskowane (`logging.MaskPhone`). W logach nie ma treści SMS
ani hasła; numery są maskowane.

## Założenia `ASSUMPTION(T00)`

Wszystkie są oznaczone w kodzie komentarzem `// ASSUMPTION(T00):`.

1. `Capabilities`: tylko GSM-7, 1 segment (160 septetów, znaki rozszerzone liczą się podwójnie), bez sklejania,
   1 wysyłka naraz na port, 65 s, `pdu` dostępne w skrzynce.
2. Wszystkie teksty błędów w `statusRules` poza 401, 500 `not allowed (9)` i 400 `Session closed`
   (dokumentacja): brak rejestracji, zajęty modem, zły numer, zbyt długa treść, znaki spoza GSM-7, `no such command`,
   403 i 504.
3. `Session closed` przy wysyłce: SMS może mimo to wyjść, więc błąd jest niejednoznaczny.
4. Odpowiedź `send` to `[]` (konektor akceptuje dowolne 2xx); brak argumentów `smsc`, `channel`,
   `status-report-request`.
5. Usuwanie: DELETE zwraca 204, nieznany `.id` daje 404/400 „no such item”; brak obsługi ścieżki rozpoznajemy po
   405/501, 404 bez „no such item” lub 400 „no such command”.
6. Skrzynka: kolejność „najstarsze pierwsze”, brak pola `port` (wtedy port z `/tool sms`), format `timestamp`
   `2006-01-02 15:04:05` lub `jan/02/2006 15:04:05`, strefa czasowa `Options.Location` (domyślnie UTC).
7. `monitor`: argument `"once": true`, pola `registration-status`, `current-operator`, `access-technology`,
   `rssi`, `rsrp`, `rsrq`, `sinr` (liczby, ewentualnie z jednostką), wartości `home`/`roaming`/`registered`
   = zarejestrowany; na zatrzymanym interfejsie `monitor` nie jest wywoływany.
8. Minimalna wersja RouterOS 7.16 (`MinVersion`).
9. RouterOS kolejkuje żądania jednego użytkownika, więc `Health` lub `FetchInbox` w trakcie wysyłki może czekać
   do jej końca (15 s timeoutu). Health checker (T17) powinien to tolerować (histereza).

## Do sprawdzenia po pomiarach T00

- Nagrać odpowiedzi (`scripts/probe`) i dodać testy na golden files (PLAN T12, kryteria).
- Poprawić `statusRules` o realne teksty błędów: brak SIM, brak zasięgu, zły numer, za długa treść, zajęty port,
  brak polityki `test`/`write`; sprawdzić, czy 400 `Session closed` przy wysyłce oznacza wysłany SMS.
- Potwierdzić format `timestamp`, strefę czasową i zawartość `pdu` (z SMSC czy bez).
- Sprawdzić, który wariant usuwania działa i jakie kody zwraca dla nieistniejącego `.id`.
- Sprawdzić nazwy i wartości pól `monitor`, w tym zachowanie `once` (`true` vs `""`).
- Ustalić, czy wpisy skrzynki mają pole portu przy kilku modemach.
- Zmierzyć, czy `Health` blokuje się za trwającą wysyłką; jeśli tak, rozważyć pomijanie `monitor` na porcie
  w trakcie wysyłki.
- Zweryfikować minimalną wersję RouterOS i limit 160 znaków (także dla znaków spoza ASCII w GSM-7).
