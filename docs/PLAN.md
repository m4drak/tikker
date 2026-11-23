# tikker – plan realizacji MVP

Specyfikacja: [SPEC.md](SPEC.md). Architektura: [ADR 0001](adr/0001-architektura.md).

## 1. Modele i zasady pracy agentów

### 1.1 Przydział modeli

| Poziom | Główny (Claude) | Kiedy | Alternatywy |
|---|---|---|---|
| **L3** – krytyczne | Claude **Opus** | architektura, współbieżność, bezpieczeństwo, integracja z RouterOS, kontrakty | ChatGPT (model flagowy, np. GPT-5.x), Gemini Pro |
| **L2** – typowa implementacja | Claude **Sonnet** | moduły wg kontraktu, UI, testy integracyjne, wdrożenie | Gemini Pro, ChatGPT (flagowy), DeepSeek V4 Flash @ Hermes |
| **L1** – mechaniczne | Claude **Haiku** | scaffolding, konfiguracje CI, Dockerfile, dokumentacja, checklisty | DeepSeek V4 Flash @ Hermes, Gemini Flash, ChatGPT (mini) |

Konkretne wersje modeli alternatywnych należy dobrać do aktualnej oferty dostawców.
W tabeli zadań podana jest rodzina i poziom.

### 1.2 Reguły
1. **Przegląd krzyżowy:** PR napisany przez model jednego dostawcy przegląda model innego dostawcy
   (np. kod Sonnet → przegląd ChatGPT/Gemini; kod alternatywy → przegląd Claude Opus). Zadania L3 zawsze przegląda człowiek.
2. **Jeden PR = jedno zadanie.** W opisie PR podajemy ID zadania, użyty model i wynik przeglądu.
3. **Kontrakt przed kodem:** zadania implementacyjne opierają się na interfejsach z T04 i kontrakcie OpenAPI z T03. Zmiana kontraktu wymaga osobnego PR (L3).
4. **Definicja ukończenia (DoD):** obowiązkowe są wszystkie punkty:
   - `make lint test` przechodzi;
   - testy obejmują nowy kod;
   - nagłówki SPDX GPL-3.0 są obecne;
   - brak nowych zależności spoza listy dozwolonych licencji;
   - zaktualizowana dokumentacja;
   - `Signed-off-by` (DCO).
5. **Brak sprzętu nie blokuje pracy:** do czasu T00 implementacja opiera się na fake RouterOS (T13). Miejsca oznaczone w SPEC jako [WERYFIKACJA] mają być konfigurowalne lub odizolowane w konektorze.
6. Kontekst dla agenta to SPEC.md, PLAN.md (jego zadanie), ADR-y i `AGENTS.md` (powstaje w T01).

## 2. Fazy i zależności

```
Faza 0  T00 ───────────────────────────────┐ (sprzęt, równolegle do wszystkiego)
        T01 ─► T02                         │
        T03, T04 (równolegle z T01)        │
Faza 1  T10, T11, T13, T14  (po T01+T04)   │
        T12 (po T04,T13; finalizacja po T00)◄┘
        T15 (po T11,T12,T14) ─► T17
        T16 (po T11,T12)
Faza 2  T20 (po T11) ─► T21 (po T03,T15,T16,T20), T22 (po T11), T23
Faza 3  T30 (po T20) ─► T31, T32 ─► T33
Faza 4  T40 (po T02) ─► T41, T42, T43
Faza 5  T50 (po T21,T32) ; T51 (po T41,T42) ; T52
```

Maksymalna równoległość: Faza 0 (4 agentów), Faza 1 (4–5), Faza 3 i 4 równolegle.

## 3. Zadania

Format każdego zadania: **cel**, **wynik**, **kryteria akceptacji**, **zależności**, **model główny** i **alternatywa**.

### Faza 0 – Fundamenty

#### T00 – Weryfikacja zachowania RouterOS na sprzęcie
- **Cel:** potwierdzić wszystkie punkty [WERYFIKACJA] ze SPEC.
- **Wynik:** `docs/research/routeros-sms.md` z wynikami oraz zrzutami żądań i odpowiedzi
  (zanonimizowane: numery, IMEI, hasła), a także `scripts/probe/` (skrypt Go/curl do powtarzalnych pomiarów).
- **Zakres:**
  - minimalna wersja RouterOS;
  - minimalne polityki grupy;
  - format `/tool sms inbox`;
  - wysyłka długich SMS i UCS-2 (polskie znaki);
  - przepustowość wysyłki;
  - kody błędów (brak SIM, brak zasięgu, zły numer);
  - zachowanie przy pełnej skrzynce;
  - lista urządzeń LTE z obsługą `container` (arch., RAM, nośnik);
  - procedura `device-mode`.
- **Kryteria:** każdy punkt [WERYFIKACJA] ma odpowiedź „potwierdzone/obalone + dowód”. SPEC jest zaktualizowany.
- **Stan (2026-11-19):** część agentowa gotowa. Pomiary na sprzęcie **odłożone** decyzją właściciela. Do tego czasu kod opiera się na researchu, a założenia są oznaczone `ASSUMPTION(T00)` i [WERYFIKACJA].
- **Zależności:** brak. **Wymaga człowieka z urządzeniem** (agent przygotowuje skrypty i checklistę, człowiek uruchamia).
- **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro (research dokumentacji).

#### T01 – Szkielet repozytorium
- **Wynik:**
  - `go.mod` (Go ≥ 1.23), struktura katalogów z SPEC §9 i `cmd/tikker` z `--version`;
  - `Makefile` (`build`, `test`, `lint`, `run`), `.golangci.yml`, `.editorconfig`;
  - nagłówki SPDX, `REUSE.toml`;
  - `CONTRIBUTING.md` (DCO), `CODE_OF_CONDUCT.md`, `SECURITY.md`;
  - `AGENTS.md` (zasady z §1.2 tego planu) i `CLAUDE.md` wskazujący na `AGENTS.md`;
  - README z zastrzeżeniem o znakach towarowych.
- **Kryteria:** `make build lint test` przechodzi lokalnie. `reuse lint` jest czysty.
- **Zależności:** brak. **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

#### T02 – CI (GitHub Actions)
- **Wynik:**
  - workflow: lint, `go test -race -cover`, build matrix (`amd64`, `arm64`, `arm/v7`);
  - kontrola licencji (`go-licenses check` z allowlistą z SPEC §11);
  - `reuse lint`, `govulncheck`;
  - dependabot dla Go i Actions.
- **Kryteria:** CI zielone na PR. Dodanie zależności GPL-niezgodnej (test) łamie build.
- **Zależności:** T01. **Model:** Claude Haiku. **Alternatywa:** ChatGPT (mini).

#### T03 – Kontrakt OpenAPI 3.1
- **Wynik:**
  - `api/openapi.yaml` wg SPEC §5: schematy, błędy RFC 9457, paginacja, `Idempotency-Key`, zakresy tokenów, przykłady;
  - lint (`vacuum` lub `redocly`, licencje OK) w CI.
- **Kryteria:** kontrakt przechodzi lint. Każdy endpoint ma przykłady i kody błędów. Przegląd człowieka.
- **Zależności:** brak. **Model:** Claude Opus. **Alternatywa:** ChatGPT (flagowy).

#### T04 – Model domeny i interfejsy wewnętrzne
- **Wynik:**
  - `internal/domain` (encje, statusy, maszyna stanów z testami przejść, typy błędów `Transient`/`Permanent`/`Auth`);
  - interfejsy `Connector`, `Store` (repozytoria), `Clock`;
  - ADR 0002 (gwarancje dostarczenia, dzierżawy);
  - ADR 0003 (wybór bibliotek: router HTTP – stdlib `net/http`, `modernc.org/sqlite`, migracje `embed`, `nyaruka/phonenumbers`, htmx, Pico CSS).
- **Kryteria:** kompiluje się, maszyna stanów ma 100% pokrycia, ADR-y zatwierdzone przez człowieka.
- **Zależności:** brak (równolegle z T01, merge po T01). **Model:** Claude Opus. **Alternatywa:** Gemini Pro.

### Faza 1 – Rdzeń

#### T10 – Konfiguracja i logowanie
- **Wynik:**
  - `internal/config`: env `TIKKER_*` + YAML, walidacja, wartości domyślne z SPEC §8;
  - `log/slog` (JSON/text), maskowanie numerów (helper).
- **Kryteria:** testy tabelaryczne priorytetu env > YAML > domyślne. Błędna konfiguracja daje czytelny błąd i exit 2.
- **Zależności:** T01. **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

#### T11 – Warstwa danych (SQLite)
- **Wynik:**
  - `internal/store`: połączenie (WAL, `busy_timeout`, `synchronous` z konfiguracji);
  - migracje `embed` i implementacje repozytoriów z T04, z indeksami pod kolejkę (`status, next_attempt_at`) i dedup (`dedup_hash` UNIQUE);
  - atomowe „pobierz następną wiadomość z dzierżawą” (`UPDATE … RETURNING`);
  - zadanie retencji.
- **Kryteria:**
  - testy na bazie w pamięci i w pliku;
  - test współbieżnego pobierania (brak podwójnej dzierżawy) pod `-race`;
  - zero zapisów przy pustym odczycie.
- **Zależności:** T01, T04. **Model:** Claude Sonnet. **Alternatywa:** ChatGPT (flagowy).

#### T12 – Konektor `routeros-rest`
- **Wynik:**
  - `internal/connector/routeros`: klient REST (timeouty, keep-alive, TLS: CA / odcisk / insecure);
  - `Send`, `FetchInbox`, `DeleteInbox`, `Health`, wykrywanie portów LTE;
  - mapowanie błędów na `Transient`/`Permanent`/`Auth`;
  - limit jednego równoległego żądania wysyłki na port.
- **Kryteria:**
  - testy kontraktowe przeciw fake RouterOS (T13);
  - test przypięcia odcisku certyfikatu;
  - po T00: testy przeciw nagranym odpowiedziom z realnego sprzętu (golden files).
- **Zależności:** T04, T13. Finalizacja (golden files z realnego sprzętu) po pomiarach T00, które są odłożone. **Model:** Claude Opus. **Alternatywa:** ChatGPT (flagowy).

#### T13 – Fake RouterOS
- **Wynik:**
  - `internal/connector/fake`: serwer `httptest` emulujący `/rest/tool/sms/*`, `/rest/interface/lte`, `/rest/system/resource`;
  - sterowanie scenariuszami: opóźnienia, 5xx, 401, pełna skrzynka, wstrzykiwanie SMS przychodzących, rejestr wysłanych;
  - binarka `cmd/fake-routeros` do testów E2E i lokalnego developmentu.
- **Kryteria:** API scenariuszy udokumentowane. Używany przez T12, T15, T16 i E2E.
- **Zależności:** T04. **Model:** Claude Sonnet. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

#### T14 – Kodowanie, segmentacja, numery
- **Wynik:**
  - `internal/sms`: wykrywanie GSM-7 (tablica podstawowa + rozszerzenia) vs UCS-2;
  - liczenie segmentów (160/153, 70/67), transliteracja PL→ASCII;
  - strategie `reject|split|truncate`, normalizacja E.164 (`nyaruka/phonenumbers`).
- **Kryteria:** testy tabelaryczne z przypadkami brzegowymi (znaki rozszerzone `€[]{}`, emoji – pary surogatów, dokładnie 160/161 znaków, polskie znaki); fuzz test segmentacji.
- **Zależności:** T01. **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro.

#### T15 – Kolejka i dispatcher
- **Wynik:**
  - `internal/queue`: worker per kanał, token bucket, pobieranie z dzierżawą;
  - backoff z jitterem, `max_attempts`, `expires_at`;
  - odzyskiwanie wygasłych dzierżaw po restarcie;
  - łagodne zamknięcie, wybudzanie workera po nowej wiadomości (bez busy-pollingu bazy).
- **Kryteria:**
  - testy z fałszywym zegarem;
  - test „kill w trakcie wysyłki” zgodny z ADR 0002;
  - brak wycieków goroutine (`goleak`, MIT);
  - limit szybkości jest dotrzymany.
- **Zależności:** T11, T12, T14. **Model:** Claude Opus. **Alternatywa:** ChatGPT (flagowy).

#### T16 – Poller skrzynki odbiorczej
- **Wynik:**
  - `internal/inbox`: cykl pobierz → zapisz (dedup) → usuń z routera;
  - sklejanie wieloczęściowych (wg T00), backoff przy błędach kanału.
- **Kryteria:**
  - test „awaria między zapisem a usunięciem” nie daje duplikatów w bazie;
  - test pełnej skrzynki;
  - zero zapisów do bazy przy pustej skrzynce.
- **Zależności:** T11, T12. **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro.

#### T17 – Routing i health-check kanałów
- **Wynik:**
  - wybór kanału wg SPEC §4.4;
  - `internal/health`: okresowe `Health`, przejścia `ok/degraded/down`, zapis sygnału i operatora;
  - histereza (bez „migotania” stanu).
- **Kryteria:** testy wyboru kanału (tagi, remisy, brak zdrowych) i przejść stanów.
- **Zależności:** T15. **Model:** Claude Sonnet. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

### Faza 2 – API i bezpieczeństwo

#### T20 – Uwierzytelnianie i autoryzacja
- **Wynik:**
  - `internal/auth`: użytkownicy (argon2id), sesje (cookie, rotacja ID po logowaniu);
  - CSRF, limit prób logowania;
  - tokeny API (`tkr_<prefix>_<secret>`, hash, zakresy, wygasanie, `last_used_at`);
  - middleware ról i zakresów, bootstrap admina, dziennik audytu.
- **Kryteria:** testy negatywne (zły token, wygasły, brak zakresu, CSRF, brute-force). Porównania w stałym czasie. Przegląd człowieka.
- **Zależności:** T11. **Model:** Claude Opus. **Alternatywa:** ChatGPT (flagowy).

#### T21 – Implementacja REST API
- **Wynik:**
  - `internal/api`: handlery wg `api/openapi.yaml` na stdlib `net/http`;
  - walidacja, problem+json, paginacja kursorowa;
  - `Idempotency-Key`, batch, limit żądań per token.
- **Kryteria:** test zgodności odpowiedzi z kontraktem (walidator OpenAPI w testach) i testy E2E z fake RouterOS: wyślij → `sent`, odbierz → widoczne w `GET /messages`.
- **Zależności:** T03, T15, T16, T20. **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro.

#### T22 – Szyfrowanie sekretów
- **Wynik:** `internal/secrets`: AES-256-GCM, klucz z env lub generowany plik 0600, integracja z repozytorium urządzeń (hasło tylko do zapisu w API/UI).
- **Kryteria:** testy round-trip i manipulacji szyfrogramem. Hasło nigdy nie pojawia się w logach ani odpowiedziach.
- **Zależności:** T11. **Model:** Claude Sonnet (przegląd: Opus). **Alternatywa:** ChatGPT (flagowy).

#### T23 – Health/ready i diagnostyka
- **Wynik:**
  - `/healthz` (proces żyje) i `/readyz` (baza, klucz sekretów, dispatcher; stan kanałów nie wpływa na gotowość – zgodnie z `api/openapi.yaml`);
  - `--check-config`;
  - `HEALTHCHECK` w obrazie (T40) przez sam `tikker healthcheck`.
- **Zależności:** T11, T17. **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

### Faza 3 – Panel WWW

#### T30 – Fundament UI
- **Wynik:**
  - `internal/web`: layout (`html/template`), htmx i Pico CSS osadzone (`go:embed`, z plikami licencji);
  - i18n PL/EN, CSP bez `unsafe-inline`;
  - strony logowania i kreatora pierwszego admina, nawigacja wg roli.
- **Kryteria:** działa bez zewnętrznych CDN. Test CSP. Dostępność: etykiety formularzy, kontrast.
- **Zależności:** T20. **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro.

#### T31 – UI: pulpit, wysyłka, wiadomości
- **Wynik:**
  - pulpit (kanały ze stanem i sygnałem, długość kolejki, ostatnie błędy; odświeżanie htmx);
  - formularz wysyłki z licznikiem znaków i segmentów na żywo (endpoint podglądu korzystający z `internal/sms`);
  - lista wiadomości z filtrami i szczegóły z anulowaniem.
- **Zależności:** T30, T21. **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro.

#### T32 – UI: urządzenia, tokeny, użytkownicy, audyt
- **Wynik:**
  - urządzenia (dodaj, test połączenia z wykryciem portów, edycja kanałów: tagi, limity, transliteracja);
  - tokeny z jednorazowym podglądem sekretu;
  - użytkownicy i role;
  - podgląd dziennika audytu.
- **Zależności:** T30, T22. **Model:** Claude Sonnet. **Alternatywa:** ChatGPT (flagowy).

#### T33 – Testy E2E UI
- **Wynik:** `test/e2e`: Playwright (narzędzie dev, Apache-2.0) przeciw `tikker` + `fake-routeros`. Scenariusze: pierwsze uruchomienie, dodanie urządzenia, wysyłka, odbiór, token, role.
- **Kryteria:** uruchamiane w CI, stabilne (bez `sleep`, oczekiwanie na stan).
- **Zależności:** T31, T32. **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.
- **Stan (2026-11-23):** zrobione przez Claude Code (Sonnet) na `task/t33-e2e-ui`. 11 testów w 7 scenariuszach, zielone lokalnie (`make e2e`, `npx playwright test`). Izolacja per test: fixture startuje własny tikker + fake-routeros z osobnym SQLite na wolnych portach, bez `sleep`. Job `e2e` dodany do `.github/workflows/ci.yml` + target `make e2e`. Do przeglądu krzyżowego przez inny model.

### Faza 4 – Pakowanie i wdrożenie

#### T40 – Obraz kontenera
- **Wynik:**
  - `deploy/docker/Dockerfile`: multi-stage, `CGO_ENABLED=0`, `FROM scratch` + certyfikaty CA + tzdata;
  - użytkownik nie-root, `VOLUME /data`, etykiety OCI (`source`, `revision`, `licenses=GPL-3.0`);
  - `docker buildx` dla `amd64`, `arm64`, `arm/v7`.
- **Kryteria:** obraz ≤ 25 MB. Uruchamia się z rootfs read-only. Pomiar RSS w spoczynku ≤ 40 MB.
- **Zależności:** T02 (finalnie po T21). **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

#### T41 – Wdrożenie na MikroTiku (kontener)
- **Wynik:**
  - `deploy/routeros/setup.rsc` (parametryzowany): grupa i użytkownik z minimalnymi politykami, `/tool sms`;
  - usługa `www-ssl` z certyfikatem i `address=` ograniczonym do podsieci veth;
  - `veth`, bridge, NAT/firewall dla panelu, `/container/mounts`, `envs`, pull obrazu;
  - `docs/deploy-mikrotik.md`: wymagania (arch., RAM, nośnik), `device-mode`, aktualizacja obrazu, kopia `/data`.
- **Kryteria:** procedura przetestowana na urządzeniu z T00 (przez człowieka). Skrypt jest idempotentny.
- **Zależności:** T40, T00. **Model:** Claude Sonnet. **Alternatywa:** Gemini Pro.

#### T42 – Wdrożenie serwerowe
- **Wynik:**
  - `deploy/docker/compose.yaml` (tikker + opcjonalnie Caddy jako reverse proxy TLS);
  - unit systemd dla binarki;
  - `docs/deploy-server.md` z sekcją WireGuard do routerów za CGNAT i konfiguracją routera (`setup.rsc` w trybie „bez kontenera”).
- **Zależności:** T40. **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

#### T43 – Pipeline wydań
- **Wynik:**
  - GoReleaser (MIT): binarki, obrazy GHCR multi-arch, checksumy;
  - SBOM (SPDX), podpis cosign, `THIRD_PARTY_NOTICES`;
  - changelog z Conventional Commits, archiwum źródeł przy wydaniu (GPL §6).
- **Kryteria:** wydanie testowe `v0.1.0-rc1` z tagu przechodzi end-to-end.
- **Zależności:** T40, T02. **Model:** Claude Sonnet. **Alternatywa:** ChatGPT (flagowy).

### Faza 5 – Jakość i wydanie

#### T50 – Model zagrożeń i przegląd bezpieczeństwa
- **Wynik:**
  - `docs/security/threat-model.md` (STRIDE: API, UI, sekrety, kanał do routera, kontener na routerze);
  - przegląd kodu `auth`, `secrets`, `api`, `web`, `connector`;
  - lista poprawek jako issues.
- **Kryteria:** brak otwartych problemów o wadze wysokiej przed `v0.1.0`.
- **Zależności:** T21, T32. **Model:** Claude Opus. **Alternatywa:** ChatGPT (flagowy). Zalecane uruchomienie obu dla niezależności.
- **Stan (2026-11-23):** model zagrożeń (Opus) i przegląd kodu gotowe w `docs/security/threat-model.md` + `docs/security/review-T50.md`. Przegląd krzyżowy (Codex) potwierdził większość findings i podniósł **F-01 (kreator `/setup` bez sekretu) do high** → kryterium „brak wysokich przed v0.1.0” **NIE jest spełnione**, naprawa to osobny issue (#1, P1). Pozostałe R2–R8 medium. F-05 do pomiaru w T51.

#### T51 – Testy akceptacyjne na sprzęcie
- **Wynik:** `docs/testing/hardware-checklist.md` oraz raport z przebiegu w obu trybach.
  - Wysyłka: PL znaki, długi SMS.
  - Odbiór, w tym wieloczęściowy.
  - Restart routera i kontenera w trakcie kolejki.
  - Utrata zasięgu, wyjęcie SIM.
  - Zmiana hasła na routerze, czyli stan `Auth`.
  - 24 h pracy z pomiarem RAM i zapisów na nośnik.
- **Zależności:** T41, T42. **Wymaga człowieka.** **Model:** Claude Haiku (checklista, raport). **Alternatywa:** Gemini Flash.

#### T52 – Dokumentacja użytkownika
- **Wynik:** README (szybki start dla obu trybów), `docs/user-guide.md` (PL + EN), przykłady API (curl, Python, PHP), FAQ (CGNAT, UCS-2, limity).
- **Zależności:** T21, T41, T42. **Model:** Claude Haiku. **Alternatywa:** DeepSeek V4 Flash @ Hermes.

## 4. Podsumowanie przydziału

| ID | Zadanie | Główny | Alternatywa |
|---|---|---|---|
| T00 | Weryfikacja RouterOS (+człowiek) | Sonnet | Gemini Pro |
| T01 | Szkielet repo | Haiku | DeepSeek V4 Flash @ Hermes |
| T02 | CI | Haiku | ChatGPT (mini) |
| T03 | OpenAPI | Opus | ChatGPT (flagowy) |
| T04 | Domena, interfejsy, ADR | Opus | Gemini Pro |
| T10 | Konfiguracja, logi | Haiku | DeepSeek V4 Flash @ Hermes |
| T11 | SQLite store | Sonnet | ChatGPT (flagowy) |
| T12 | Konektor RouterOS REST | Opus | ChatGPT (flagowy) |
| T13 | Fake RouterOS | Sonnet | DeepSeek V4 Flash @ Hermes |
| T14 | Kodowanie, segmentacja, numery | Sonnet | Gemini Pro |
| T15 | Kolejka i dispatcher | Opus | ChatGPT (flagowy) |
| T16 | Poller skrzynki | Sonnet | Gemini Pro |
| T17 | Routing, health | Sonnet | DeepSeek V4 Flash @ Hermes |
| T20 | Auth | Opus | ChatGPT (flagowy) |
| T21 | REST API | Sonnet | Gemini Pro |
| T22 | Szyfrowanie sekretów | Sonnet (+Opus review) | ChatGPT (flagowy) |
| T23 | Health/ready | Haiku | DeepSeek V4 Flash @ Hermes |
| T30 | Fundament UI | Sonnet | Gemini Pro |
| T31 | UI: pulpit, wysyłka, wiadomości | Sonnet | Gemini Pro |
| T32 | UI: urządzenia, tokeny, użytkownicy | Sonnet | ChatGPT (flagowy) |
| T33 | E2E UI | Haiku | DeepSeek V4 Flash @ Hermes |
| T40 | Obraz kontenera | Haiku | DeepSeek V4 Flash @ Hermes |
| T41 | Wdrożenie na MikroTiku (+człowiek) | Sonnet | Gemini Pro |
| T42 | Wdrożenie serwerowe | Haiku | DeepSeek V4 Flash @ Hermes |
| T43 | Pipeline wydań | Sonnet | ChatGPT (flagowy) |
| T50 | Bezpieczeństwo | Opus | ChatGPT (flagowy) |
| T51 | Testy sprzętowe (+człowiek) | Haiku | Gemini Flash |
| T52 | Dokumentacja | Haiku | DeepSeek V4 Flash @ Hermes |

Rozkład: Opus 6, Sonnet 13, Haiku 9. Przeglądy krzyżowe są dodatkowe (§1.2).

## 5. Po MVP (backlog, kolejność proponowana)
1. Konektor `agent` (skrypt RouterOS pull przez HTTPS, działa za CGNAT bez VPN) – Opus / ChatGPT (flagowy).
2. Webhooki (przychodzące SMS, zmiany statusu; podpis HMAC, ponowienia) – Sonnet / Gemini Pro. **Zrobione (2026-11-23):** `POST` na `incoming_sms` i `message_status`, HMAC-SHA256 + signed delivery id, trwała kolejka SQLite z retry, API `/api/v1/webhooks`, SSRF-guard; `docs/webhooks.md`.
3. Metryki Prometheus – Haiku / DeepSeek V4 Flash @ Hermes. **Zrobione (2026-11-23):** `GET /metrics` (text exposition, bez zewnętrznej zależności), metryki kolejki + zdrowia kanałów + startu; `docs/metrics.md`.
4. PostgreSQL jako alternatywny store – Sonnet / ChatGPT (flagowy).
5. Bramka e-mail↔SMS, CLI – Sonnet / Gemini Pro.
6. Raporty doręczenia (jeśli RouterOS/modem udostępni) – Opus / ChatGPT (flagowy).
