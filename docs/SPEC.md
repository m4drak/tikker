# tikker – specyfikacja (MVP)

Bramka SMS oparta o urządzenia MikroTik (RouterOS 7) z modemem LTE/5G i kartą SIM.
Licencja: GNU GPL v3.0. Decyzja architektoniczna: [ADR 0001](adr/0001-architektura.md).

Oznaczenie **[WERYFIKACJA]** oznacza założenie o zachowaniu RouterOS, które trzeba potwierdzić
na sprzęcie w zadaniu T00 (patrz [PLAN.md](PLAN.md)) przed implementacją zależnego kodu.

---

## 1. Cel i zakres

### 1.1 MVP
- Wysyłanie SMS przez REST API z trwałą kolejką, ponowieniami, limitami szybkości i statusami.
- Odbieranie SMS: polling skrzynki routera, zapis w bazie, usunięcie z routera.
- Obsługa wielu urządzeń i kanałów (kanał = urządzenie + port LTE).
- Panel WWW: logowanie, pulpit, wysyłka, historia wiadomości, urządzenia, tokeny API, użytkownicy.
- Dwa tryby wdrożenia tego samego obrazu: **na MikroTiku** (container) i **na serwerze**.

### 1.2 Poza MVP (backlog)
Konektor `agent` (pull, CGNAT), webhooki, PostgreSQL, bramka e-mail↔SMS, CLI, metryki Prometheus,
raporty doręczenia, wielodzierżawność, szablony wiadomości, kampanie i harmonogramy.

---

## 2. Architektura

```
            klienci (REST, Bearer token)        przeglądarka (panel WWW)
                         │                                │
                ┌────────▼────────────────────────────────▼────────┐
                │                    tikker (Go)                   │
                │  HTTP: /api/v1/*  |  /ui/*  |  /healthz          │
                │  ┌───────────┐ ┌──────────────┐ ┌─────────────┐  │
                │  │ Auth      │ │ Message svc  │ │ Device svc  │  │
                │  └───────────┘ └──────┬───────┘ └──────┬──────┘  │
                │        ┌──────────────▼─────────┐      │         │
                │        │ Queue / Dispatcher     │      │         │
                │        │ (worker per kanał)     │      │         │
                │        └──────────┬─────────────┘      │         │
                │  ┌────────────────▼───────┐ ┌──────────▼───────┐ │
                │  │ Inbox poller           │ │ Health checker   │ │
                │  └────────────┬───────────┘ └──────────┬───────┘ │
                │        ┌──────▼─────────────────────────▼──┐     │
                │        │ Connector (interfejs)             │     │
                │        │  └─ routeros-rest (MVP)           │     │
                │        │  └─ agent (po MVP)                │     │
                │        └──────────────┬────────────────────┘     │
                │  Store: SQLite (/data/tikker.db)                 │
                └─────────────────────────┼────────────────────────┘
                         HTTPS REST API   │  (veth lokalnie lub LAN/VPN)
                ┌─────────────────────────▼────────────────────────┐
                │ MikroTik RouterOS 7: /tool sms send, /tool sms inbox │
                └──────────────────────────────────────────────────┘
```

### 2.1 Tryby wdrożenia

| | Kontener na MikroTiku | Serwer (Docker/Podman/systemd) |
|---|---|---|
| Urządzenia | 1 (lokalne), opcjonalnie kolejne przez sieć | wiele |
| Połączenie z API | `https://<IP bramy veth>` (np. 172.17.0.1) | LAN / VPN (WireGuard) |
| Wymagania | RouterOS 7 z pakietem `container`, arch. ARM/ARM64/x86, `device-mode container=yes`, nośnik na `/data` (zalecany USB/microSD) | dowolny Linux amd64/arm64/armv7 |
| CGNAT | nie dotyczy | VPN (MVP) lub konektor `agent` (po MVP) |

Aplikacja nie wie, w którym trybie działa. Różni się tylko konfiguracja (adresy urządzeń, ścieżka `/data`).

### 2.2 Wymagania niefunkcjonalne
- Obraz `FROM scratch` lub distroless static. Rozmiar ≤ 25 MB (cel), proces działa jako użytkownik nie-root.
- Bez CGO: SQLite przez `modernc.org/sqlite`. Kompilacja skrośna na `amd64`, `arm64`, `arm/v7`.
- RSS w spoczynku ≤ 40 MB, przy 10 kanałach ≤ 64 MB.
- Mało zapisów na flash: WAL, batching, retencja i konfigurowalny `synchronous`.
  Brak zapisów przy pustym pollingu. Logi idą na stdout, nie do pliku.
- Rootfs tylko do odczytu, jedyny zapisywalny katalog to `/data`.
- Łagodne zamknięcie (SIGTERM): dokończenie bieżących wysyłek (timeout), zwolnienie dzierżaw.

---

## 3. Integracja z RouterOS

### 3.1 Wymagania po stronie routera
- RouterOS ≥ 7.x [WERYFIKACJA: minimalna wersja z poprawnym `/rest/tool/sms/*`].
- Włączona usługa `www-ssl` (zalecana) albo `www`, z ograniczeniem `address=` do IP tikkera / podsieci veth.
- `/tool sms set port=lte1 receive-enabled=yes`.
- Dedykowany użytkownik i grupa z minimalnymi politykami: `read`, `write`, `rest-api`
  [WERYFIKACJA: czy `send` wymaga dodatkowo `test`/`policy`].

Projekt dostarczy skrypt `deploy/routeros/setup.rsc`, który konfiguruje użytkownika, grupę, SMS, usługę oraz,
w trybie kontenerowym, `veth`, bridge, mount i kontener.

### 3.2 Wywołania API (konektor `routeros-rest`)
| Operacja | Wywołanie | Uwagi |
|---|---|---|
| Wysyłka | `POST /rest/tool/sms/send` `{"port":"lte1","phone-number":"+48…","message":"…"}` | [WERYFIKACJA: długie SMS, UCS-2/polskie znaki, kody błędów, czas odpowiedzi] |
| Odczyt skrzynki | `GET /rest/tool/sms/inbox` | pola: `.id`, `phone`, `timestamp`, `message`, `type` [WERYFIKACJA: format timestamp, sklejanie wieloczęściowych] |
| Usunięcie | `DELETE /rest/tool/sms/inbox/{.id}` lub `POST /rest/tool/sms/inbox/remove` | |
| Zdrowie | `GET /rest/system/resource`, `GET /rest/interface/lte` (+ `monitor` jednorazowo) | sygnał, operator, stan rejestracji |

Interfejs konektora (Go, szkic):
```go
type Connector interface {
    Send(ctx context.Context, ch ChannelRef, to string, body string) (SendResult, error)
    FetchInbox(ctx context.Context, ch ChannelRef) ([]InboundSMS, error)
    DeleteInbox(ctx context.Context, ch ChannelRef, ids []string) error
    Health(ctx context.Context, ch ChannelRef) (ChannelHealth, error)
}
```
Błędy klasyfikujemy jako `Transient` (sieć, timeout, 5xx, modem zajęty) → ponowienie,
`Permanent` (zły numer, 4xx walidacji) → `failed`, `Auth` (401/403) → kanał `degraded` i alert w UI.

### 3.3 TLS do routera
Domyślnie weryfikacja certyfikatu. Opcje per urządzenie: własny CA (PEM), przypięcie odcisku SHA-256
(przydatne dla certyfikatu self-signed z RouterOS) albo jawne `insecure_skip_verify` z ostrzeżeniem w UI.
W trybie kontenerowym dopuszczalne jest HTTP wyłącznie na adresie veth (oznaczone w UI).

---

## 4. Model domeny i dane

### 4.1 Encje
- **User**: `id`, `username`, `password_hash` (argon2id), `role` (`admin`|`operator`|`viewer`), `created_at`, `disabled`.
- **APIToken**: `id`, `user_id`, `name`, `prefix`, `hash` (SHA-256 z sekretu), `scopes` (`sms:send`, `sms:read`, `admin`), `expires_at`, `last_used_at`.
  Token pokazywany jest tylko raz, w formacie `tkr_<prefix>_<secret>`.
- **Device**: `id`, `name`, `base_url`, `username`, `password_enc` (AES-256-GCM), `tls_mode`, `ca_pem`, `fingerprint`, `enabled`, `created_at`.
- **Channel**: `id`, `device_id`, `port` (np. `lte1`), `name`, `tags[]`, `rate_per_min`, `max_segments`, `enabled`,
  `state` (`ok`|`degraded`|`down`|`disabled`), `last_health_at`, `signal`, `operator`.
- **Message**: `id` (ULID), `direction` (`out`|`in`), `channel_id`, `peer` (E.164), `body`, `encoding` (`gsm7`|`ucs2`),
  `segments`, `status`, `attempts`, `next_attempt_at`, `lease_until`, `last_error`, `idempotency_key`, `client_ref`,
  `created_by` (user/token), `created_at`, `sent_at`, `received_at`, `dedup_hash` (dla `in`).
- **AuditLog**: `id`, `actor`, `action`, `target`, `meta` (JSON), `at`.

### 4.2 Statusy wiadomości wychodzących
```
queued ──(dispatcher bierze dzierżawę)──► sending ──ok──► sent
   ▲                                         │
   └──── błąd Transient, attempts < max ─────┤
                                             ├─ błąd Permanent / attempts ≥ max ──► failed
queued ──(DELETE przez API/UI)──► cancelled
```
- `sent` oznacza, że RouterOS przyjął wiadomość do wysłania. MVP nie ma raportów doręczenia.
- Ponowienia: wykładniczy backoff z jitterem (np. 30 s, 2 min, 10 min, 30 min), domyślnie `max_attempts=5`.
- Dzierżawa (`lease_until`) chroni przed podwójną wysyłką po awarii. Wygasła dzierżawa w stanie `sending`
  wraca do `queued` tylko wtedy, gdy nie ma pewności wysyłki. Taka sytuacja jest logowana jako „możliwy duplikat”.
  Gwarancja jest at-least-once; wybór jest udokumentowany.
- Idempotencja: nagłówek `Idempotency-Key` (unikalny per token przez 24 h) zwraca istniejącą wiadomość.

### 4.3 Wiadomości przychodzące
- Poller per kanał co `inbox_poll_interval` (domyślnie 15 s).
- Algorytm: pobierz skrzynkę → zapisz w bazie w transakcji (dedup po `dedup_hash = sha256(device|port|phone|timestamp|message)`)
  → dopiero po commicie usuń z routera. Gwarancja at-least-once bez duplikatów w bazie.
- Sklejanie SMS wieloczęściowych, jeśli RouterOS zwraca części osobno [WERYFIKACJA].

### 4.4 Routing (wybór kanału)
Kolejność:
1. `channel_id` jawnie w żądaniu.
2. `tags` (kanały mające wszystkie tagi).
3. Domyślnie wszystkie aktywne kanały.

Spośród kandydatów w stanie `ok` wybieramy kanał z najmniejszą kolejką (round-robin przy remisie).
Brak zdrowego kanału zostawia wiadomość w `queued` do skutku lub do `expires_at`, jeśli podano.

### 4.5 Limity
Każdy kanał ma token bucket `rate_per_min` (domyślnie 6/min) [WERYFIKACJA realnej przepustowości modemu],
pojedynczy worker i serializację wysyłek. Globalnie obowiązuje limit żądań API per token.

### 4.6 Numery i kodowanie
- Normalizacja do E.164 przez `github.com/nyaruka/phonenumbers` (MIT). `default_region` konfigurowalny (domyślnie `PL`).
  Krótkie numery i numery alfanumeryczne są dopuszczone, jeśli włączy je flaga `allow_short_numbers`.
- Licznik segmentów: GSM-7 (160/153, z rozszerzeniami liczonymi podwójnie) oraz UCS-2 (70/67).
- Opcja `transliterate` (per kanał): zamiana polskich znaków na ASCII, gdy RouterOS/modem nie obsługuje UCS-2
  [WERYFIKACJA]. `max_segments` (domyślnie 3) to limit, powyżej którego zwracamy 422.
- Jeśli RouterOS nie wspiera wysyłki wieloczęściowej [WERYFIKACJA], strategia jest konfigurowalna:
  `reject`, `split` (osobne SMS z prefiksem `(1/3)`) lub `truncate`.

---

## 5. REST API (`/api/v1`)

Źródłem prawdy jest kontrakt OpenAPI 3.1 w `api/openapi.yaml`. Format JSON, błędy w RFC 9457 (`application/problem+json`).
Autoryzacja: `Authorization: Bearer tkr_…`. Paginacja kursorowa (`?cursor=&limit=`).

| Metoda i ścieżka | Zakres | Opis |
|---|---|---|
| `POST /messages` | `sms:send` | wyślij: `{to, body, channel_id?, tags?, client_ref?, expires_at?}` → 202 + obiekt |
| `POST /messages/batch` | `sms:send` | do 100 wiadomości |
| `GET /messages` | `sms:read` | filtry: `direction`, `status`, `peer`, `channel_id`, `since`, `until` |
| `GET /messages/{id}` | `sms:read` | szczegóły |
| `DELETE /messages/{id}` | `sms:send` | anuluj (tylko `queued`) |
| `GET /channels` | `sms:read` | lista kanałów ze stanem |
| `GET/POST/PATCH/DELETE /devices[/{id}]` | `admin` | zarządzanie urządzeniami (hasło tylko do zapisu) |
| `POST /devices/{id}/test` | `admin` | test połączenia, wykrycie portów LTE |
| `GET/POST/DELETE /tokens[/{id}]` | `admin` | tokeny |
| `GET/POST/PATCH /users[/{id}]` | `admin` | użytkownicy |
| `GET /healthz`, `GET /readyz` | – | bez autoryzacji, bez szczegółów wrażliwych |

---

## 6. Panel WWW

- Go `html/template` + **htmx** (0BSD) + **Pico CSS** (MIT), osadzone przez `go:embed`. Bez kroku budowania JS.
- Strony:
  - logowanie;
  - pulpit (kanały, sygnał, kolejka, ostatnie błędy);
  - wysyłka (licznik znaków i segmentów na żywo);
  - wiadomości (filtry, szczegóły, anulowanie);
  - urządzenia (dodaj / testuj / włącz / wyłącz, kanały);
  - tokeny (tworzenie z jednorazowym podglądem);
  - użytkownicy (admin);
  - dziennik audytu.
- Sesje: cookie `HttpOnly`, `Secure` (jeśli HTTPS), `SameSite=Lax`, ochrona CSRF (token w formularzach i nagłówkach htmx).
- Role: `viewer` (podgląd), `operator` (wysyłka i anulowanie), `admin` (wszystko).
- Język interfejsu: PL i EN (pliki tłumaczeń, wybór z `Accept-Language` lub ustawień użytkownika).
- Pierwsze uruchomienie: jeśli brak użytkowników, kreator tworzy admina (lub zmienna `TIKKER_ADMIN_PASSWORD`).

---

## 7. Bezpieczeństwo

- Hasła: argon2id (`golang.org/x/crypto`). Logowanie ma limit prób per IP i per login.
- Hasła do routerów szyfrowane AES-256-GCM kluczem z `TIKKER_SECRET_KEY` lub pliku `/data/secret.key`
  (generowanego przy pierwszym starcie z uprawnieniami 0600). Rotacja kluczy: po MVP.
- Tokeny API przechowywane wyłącznie jako hash. Porównania w stałym czasie.
- Nagłówki bezpieczeństwa (CSP bez `unsafe-inline`, `X-Frame-Options`, `Referrer-Policy`).
- Dziennik audytu obejmuje logowania, zmiany urządzeń, tokenów i użytkowników oraz wysyłki z UI.
- Brak treści SMS w logach aplikacji (domyślnie). Numery w logach są maskowane.
- Retencja treści wiadomości jest konfigurowalna (domyślnie 90 dni), co wspiera zgodność z RODO.
- Wbudowany serwer HTTPS jest opcjonalny (certyfikat z pliku). Zalecany reverse proxy.
- Model zagrożeń i przegląd bezpieczeństwa: zadanie T50.

---

## 8. Konfiguracja

Zmienne środowiskowe `TIKKER_*` i opcjonalny `/data/config.yaml` (env ma pierwszeństwo).
Urządzenia i kanały są przechowywane w bazie (zarządzane z UI/API), opcjonalnie zasiewane z YAML.

| Klucz | Domyślnie | Opis |
|---|---|---|
| `TIKKER_LISTEN` | `:8080` | adres HTTP |
| `TIKKER_DATA_DIR` | `/data` | baza, klucz |
| `TIKKER_SECRET_KEY` | – | klucz szyfrujący (base64, 32 B) |
| `TIKKER_BASE_URL` | – | publiczny URL (cookies, linki) |
| `TIKKER_DEFAULT_REGION` | `PL` | region numerów |
| `TIKKER_INBOX_POLL_INTERVAL` | `15s` | |
| `TIKKER_HEALTH_INTERVAL` | `60s` | |
| `TIKKER_RETENTION` | `2160h` | retencja treści |
| `TIKKER_LOG_LEVEL` / `TIKKER_LOG_FORMAT` | `info` / `json` | `log/slog` |
| `TIKKER_SQLITE_SYNCHRONOUS` | `NORMAL` | kompromis zużycie flash / trwałość |

---

## 9. Struktura repozytorium

```
cmd/tikker/               main
internal/config/          konfiguracja
internal/store/           SQLite, migracje (embed), repozytoria
internal/domain/          encje, statusy, błędy
internal/connector/       interfejs + routeros/ (REST klient), fake/ (testy)
internal/sms/             kodowanie GSM-7/UCS-2, segmentacja, transliteracja, numery
internal/queue/           dispatcher, workery, rate limit, retry
internal/inbox/           poller
internal/health/          health checker kanałów
internal/auth/            użytkownicy, sesje, tokeny, CSRF
internal/api/             handlery REST (/api/v1)
internal/web/             panel WWW (templates/, static/, i18n/)
internal/secrets/         AES-GCM
api/openapi.yaml          kontrakt
deploy/docker/            Dockerfile, compose
deploy/routeros/          setup.rsc, instrukcje kontenera
docs/                     SPEC, PLAN, ADR, przewodniki
test/e2e/                 testy end-to-end (fake RouterOS)
```

---

## 10. Testy

- Jednostkowe: segmentacja, numery, maszyna stanów, backoff, routing, auth (≥ 80% pokrycia w `internal/sms`, `internal/queue`).
- Integracyjne: **fake RouterOS** (`internal/connector/fake`, `httptest`) symulujący wysyłkę, skrzynkę, błędy, opóźnienia, 401.
- Wyścigi: `go test -race` w CI.
- E2E: uruchomiony binarny `tikker` + fake RouterOS + testy API i UI (Playwright, tylko jako narzędzie deweloperskie).
- Sprzętowe: checklista akceptacyjna na realnym urządzeniu w obu trybach wdrożenia (T51).

---

## 11. Open source i licencje

- Licencja projektu: **GNU GPL v3.0**. W każdym pliku źródłowym nagłówek SPDX
  (`SPDX-License-Identifier: GPL-3.0-or-later` lub `GPL-3.0-only`; decyzja właściciela, patrz pytanie otwarte 1).
- Zależności tylko na licencjach zgodnych z GPLv3 (MIT, BSD, ISC, 0BSD, Apache-2.0, MPL-2.0, LGPL).
  CI sprawdza je narzędziem `go-licenses` z listą dozwolonych licencji, a `reuse lint` sprawdza nagłówki.
- `THIRD_PARTY_NOTICES` generowany w release, do tego SBOM (SPDX) dołączany do wydań.
- Obrazy kontenera muszą umożliwiać dostęp do kodu źródłowego (GPL §6). Etykieta OCI
  `org.opencontainers.image.source` + tag wersji wskazuje dokładny commit.
- Wkład przez **DCO** (`Signed-off-by`), bez CLA. `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`.
- Znaki towarowe: „MikroTik” i „RouterOS” są znakami MikroTik. README zawiera zastrzeżenie o braku afiliacji.
- Kod generowany przez modele AI podlega tej samej licencji i przeglądowi. PR oznacza użyty model w opisie (przejrzystość).

---

## 12. Pytania otwarte

1. `GPL-3.0-only` czy `GPL-3.0-or-later`? Rekomendacja: `-or-later` (zgodność z przyszłymi wersjami GPL).
2. Gwarancja at-least-once przy awarii w trakcie wysyłki (możliwy duplikat) czy at-most-once (możliwa utrata)?
   Rekomendacja: at-least-once, z przełącznikiem per kanał.
3. Domyślny limit wysyłki per kanał, do ustalenia po T00.
