<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Model zagrożeń tikker (T50)

- Data: 2026-11-23, stan kodu: `5e15938` (gałąź `task/t50-threat-model`).
- Metoda: STRIDE dla pięciu komponentów z PLAN §T50. Środki zaradcze wskazane jako `plik:linia`.
- Szczegółowe znaleziska z przeglądu kodu: [review-T50.md](review-T50.md) (identyfikatory `F-xx`).
- Uwaga: bezpieczeństwo opisuje SPEC **§7** (zadanie odwołuje się do „§8”, ale §8 to konfiguracja).

## 1. Zasoby i granice zaufania

| Zasób | Gdzie | Skutek utraty |
|---|---|---|
| Hasło routera (konto `tikker`, polityki `api,rest-api,read,write`) | `devices.password_enc` (AES-256-GCM) | rekonfiguracja routera z podsieci tikkera |
| Klucz szyfrujący | `TIKKER_SECRET_KEY` lub `/data/secret.key` | odszyfrowanie haseł routerów |
| Tokeny API, sesje | tylko SHA-256 w bazie | wysyłka SMS na koszt właściciela SIM, odczyt skrzynki |
| Treści SMS (w tym przychodzące kody 2FA/OTP na kartę SIM) | `messages.body` (retencja 90 dni) | wyciek danych osobowych (RODO), przejęcie kont powiązanych z numerem SIM |
| Dziennik audytu | `audit_log` | utrata rozliczalności |

Granice: (1) Internet/LAN → HTTP tikkera (`/api/v1`, panel); (2) tikker → REST API RouterOS (veth, LAN lub WireGuard);
(3) proces → `/data` (baza, klucz); (4) RouterOS → kontener (device-mode, mount, envs).

## 2. Podsumowanie

| # | Komponent | S | T | R | I | D | E | Ryzyko rezydualne |
|---|---|---|---|---|---|---|---|---|
| 1 | API `/api/v1` | low | low | low | medium | low | low | **low** |
| 2 | Panel WWW | medium (kreator) | low | low | low | medium | low | **medium** |
| 3 | Sekrety | — | low | — | medium | low | medium (F-02) | **medium** |
| 4 | Kanał do routera | low | low | — | medium | low | medium | **medium** |
| 5 | Kontener na routerze | low | medium | low | medium | medium | medium | **medium** |

Brak ryzyk ocenionych jako *high* po uwzględnieniu istniejących środków. Pięć pozycji *medium* wymaga poprawek lub decyzji
właściciela przed `v0.1.0` (§8).

## 3. API (`/api/v1`, Bearer `tkr_…`)

| STRIDE | Zagrożenie | Środki w kodzie | Ryzyko |
|---|---|---|---|
| S | Zgadywanie/kradzież tokenu | 32 znaki alnum ≈190 bit (`internal/auth/token.go:21-26`), próbkowanie bez obciążenia (`token.go:33-51`), w bazie tylko SHA-256 (`token.go:68`), porównanie w stałym czasie także dla nieznanego prefiksu (`internal/auth/middleware.go:44-53`) | low |
| S | Token po wyłączeniu właściciela / po terminie | odrzucenie `Expired` i `Disabled` przy każdym żądaniu (`middleware.go:55-64`) | low |
| E | Zakres tokenu ponad rolę | przy tworzeniu `scope_exceeds_role` (`internal/auth/service.go:566`), przy użyciu przecięcie zakresów z bieżącą rolą (`internal/auth/principal.go:41-46`); test degradacji roli `internal/api/accounts_test.go:274` | low |
| E | Brak kontroli zakresu na trasie | jedna tabela tras z zakresem (`internal/api/api.go:298-334`), kontrola w `route` (`api.go:361`); test macierzy `accounts_test.go:274` | low |
| T | Powtórzone żądanie / podwójna wysyłka | `Idempotency-Key` per aktor, 24 h, hash kanoniczny (`internal/api/idempotency.go:49-65`), zapis w tej samej transakcji (`internal/store/sqlite/messages.go:153-175`), 409 dla równoległych (`idempotency.go:79-88`) | low |
| T | SQL injection | wyłącznie zapytania parametryzowane (`messages.go:508-541`) | low |
| R | Kto wysłał | `created_by=token:<id>`, audyt wysyłek i anulowań (`internal/sending/sending.go:548,597`) | low |
| I | Odczyt cudzych wiadomości | **brak izolacji per token/użytkownik**: każdy `sms:read` widzi wszystkie wiadomości, także przychodzące kody OTP (decyzja: brak wielodzierżawności, SPEC §1.2) | medium (F-09) |
| I | Wyciek w błędach | błędy 5xx bez szczegółów, `request_id` (`api.go:375-390`), recover (`api.go:393-406`) | low |
| D | Zalew żądań | limit per token 120/min (`internal/api/ratelimit.go:71-104`), limit ciała 2 MiB (`api.go:76`), batch ≤100, timeouty serwera (`internal/app/app.go:68-72`) | low |

At-least-once (ADR 0002): duplikat SMS u odbiorcy jest **akceptowanym** ryzykiem; jest oznaczany `possible_duplicate`.
Atakujący z `sms:send` nie może wymusić duplikatów inaczej niż wysyłając ponownie (to samo, co legalne użycie).
Aliasy tokenów: w kodzie nie ma takiej funkcji; jedynym identyfikatorem dla ludzi jest `name` + publiczny prefiks.

## 4. Panel WWW (sesje, htmx, html/template)

| STRIDE | Zagrożenie | Środki w kodzie | Ryzyko |
|---|---|---|---|
| S | Brute-force logowania | GCRA per login 5 + 1/min i per IP 20 + 1/6 s, IPv6 per /64, atomowo przed weryfikacją (`internal/auth/ratelimit.go:24-25,139-150`, `internal/auth/session.go:118-121,389-399`); argon2id m=19 MiB (`internal/auth/password.go:36`) | low |
| S | Enumeracja użytkowników (timing) | hash atrapy dla nieznanego loginu i złego hasha, `disabled` sprawdzane po weryfikacji (`session.go:157-190`); test `session_test.go:143` | low |
| S | Przejęcie pustej instancji przez kreatora `/setup` | atomowa kontrola „brak użytkowników” (`service.go:346-353`), ochrona cross-origin; **brak sekretu kreatora** | **high** (F-01, na publicznie dostępnej niezainicjalizowanej instancji) |
| S | Fiksacja / kradzież sesji | 256-bit token, w bazie SHA-256 (`session.go:74-78`), rotacja przy logowaniu (`session.go:228-235`), `HttpOnly`, `SameSite=Lax`, `__Host-` przy HTTPS (`session.go:65-70`), 1 h/12 h (`service.go:47-48`), koniec sesji po zmianie hasła/roli (`service.go:496`) | low |
| T | CSRF | `http.CrossOriginProtection` + Referer (`internal/auth/csrf.go:48-70`), token HMAC z sesji w stałym czasie (`csrf.go:97-108`, `session.go:87-91`), wszystkie zmiany stanu to POST; testy `csrf_test.go:16`, `web/security_test.go:99,230` | low |
| T | XSS | `html/template` bez `template.HTML`/`JS` (brak w `internal/web/*.go`), CSP bez `unsafe-inline`/`unsafe-eval` (`internal/web/security.go:11-20`), htmx `allowEval:false, allowScriptTags:false` (`templates/layout/base.html:13`), test braku kodu inline (`security_test.go:187`) | low |
| T | Clickjacking / open redirect | `frame-ancestors 'none'`, `X-Frame-Options: DENY` (`security.go:28`), `SafeRedirect` (`middleware.go:191-200`, test `auth_test.go:143`) | low |
| E | Viewer/operator wywołuje POST admina | `protect` sprawdza rolę na każdej trasie (`internal/web/web.go:300-308`), role na trasach poprawne (np. `pages_users.go:28-34`, `pages_send.go:83-84`, `pages_messages.go:115`); test `pages_admin_test.go:73` (luki w liście tras: F-12) | low |
| R | Zaprzeczenie działań | audyt logowań, zmian użytkowników, tokenów, urządzeń, wysyłek z UI | low |
| I | Hasła/tokeny w logach | `secrets.Password` redaguje każdą ścieżkę formatowania (`internal/secrets/password.go:23-44`), token pokazany raz z `no-store` (`pages_tokens.go:167-168`); **hasło wpisane w pole loginu trafia do logu i audytu** | low (F-07) |
| D | Zalew logowań wyczerpuje argon2 / RAM | semafor 2 hashowań (`password.go:41,125-134`) | medium w trybie kontenerowym (F-05) |

## 5. Sekrety (AES-256-GCM, klucz)

| STRIDE | Zagrożenie | Środki w kodzie | Ryzyko |
|---|---|---|---|
| T | Podmiana/przeniesienie szyfrogramu między rekordami | AAD = nagłówek + cel + ID urządzenia (`internal/secrets/cipher.go:100-108,121`), losowy nonce 96 bit (`cipher.go:117-120`), wersja i ID klucza w nagłówku | low |
| I | Odczyt klucza z dysku | plik 0600, odmowa startu przy szerszych prawach (`internal/secrets/key.go:92-95`), atomowe tworzenie bez nadpisania (`key.go:112-152`) | low |
| I | Kradzież nośnika `/data` (USB/microSD na routerze) lub kopii zapasowej | **baza i klucz leżą obok siebie** – kopia `/data` = hasła routerów w jawnej postaci | medium (F-06) |
| I | Klucz w zmiennej środowiskowej | `TIKKER_SECRET_KEY` widoczny w `/container/envs`, eksportach i backupach RouterOS | medium (F-06) |
| E | Admin odczytuje „write-only” hasło routera | hasło nie wraca w API (`internal/api/dto.go:120`), ale **zmiana `base_url`/`tls_mode` zachowuje zapisane hasło** → wyśle je pod dowolny adres | **medium** (F-02) |
| D | Utracony/zmieniony klucz | jasny błąd przy starcie (`internal/secrets/credentials.go:64-71`), `/readyz` sprawdza klucz (`internal/app/app.go:409-422`) | low |

Rotacja kluczy: przygotowana (`Keyring` z kluczami wycofanymi, `cipher.go:62-76`), bez procedury – po MVP (SPEC §7).

## 6. Kanał do routera (REST RouterOS)

| STRIDE | Zagrożenie | Środki w kodzie | Ryzyko |
|---|---|---|---|
| S | MITM / podszycie pod router | TLS 1.2+, tryby `verify`/`ca`/`fingerprint` (`internal/connector/routeros/routeros.go:276-302`), pin SHA-256 liścia przy każdym handshake'u, brak `ClientSessionCache` (więc brak wznowień omijających `VerifyPeerCertificate`) (`routeros.go:304-313`) | low |
| S | Pin TOFU: odcisk przypięty z pierwszej odpowiedzi | `setup.rsc` wypisuje odcisk na routerze (`deploy/routeros/setup.rsc:206-207`) – porównanie zależy od człowieka | low |
| I | Basic auth po HTTP / przez proxy / po przekierowaniu | `http://` tylko dla `TIKKER_ALLOWED_HTTP_HOSTS` (`internal/admin/devices.go:76-79`), `Proxy: nil` (`routeros.go:197`), przekierowania nie są śledzone (`routeros.go:213`) | low |
| I | Treść/numery z odpowiedzi routera w logach i `last_error` | maskowanie i obcinanie do 160 znaków (`internal/connector/routeros/errors.go:117-130`), maskowany numer w logu wysyłki (`send.go:86`) | low |
| E | Skompromitowany tikker przejmuje router | konto z `write` może zmienić prawie całą konfigurację; ograniczenie `address=` do podsieci tikkera (`setup.rsc:97`) | medium (F-08, ograniczenie RouterOS) |
| D | Wolny/wrogi router | timeouty połączenia, nagłówków i wywołań, limit ciała odpowiedzi (`internal/connector/routeros/http.go:51-56,102-111`) | low |
| T | Inbox: wstrzyknięcie treści przez SMS | treść tylko renderowana przez `html/template`; dedup po hashu z prefiksem długości (ADR 0002 §4) | low |

## 7. Kontener na routerze (device-mode, `/data`)

| STRIDE | Zagrożenie | Środki | Ryzyko |
|---|---|---|---|
| E | Proces w kontenerze jako root | obraz `USER 65532` (`deploy/docker/Dockerfile:86`), ale `setup.rsc` ustawia `ctrUser "0:0"` (`setup.rsc:61,194`) | medium (F-04) |
| E | Ruch z kontenera do LAN | brak masquerade (`setup.rsc:151-152`); **brak reguły forward blokującej `br-tikker` → LAN** | medium (F-04) |
| I | Panel po HTTP w LAN (`exposePanel=yes`) | domyślnie `no`, dst-nat tylko z `panelLanList` (`setup.rsc:73,155-163`); hasła i cookie (bez `Secure`) w jawnej postaci w LAN | medium (dokumentacja) |
| I | `TIKKER_ADMIN_PASSWORD` trwale w `/container/envs` | instrukcja usunięcia (`docs/deploy-mikrotik.md:126`) | low |
| T | Podmieniony obraz (supply chain) | cosign/SBOM w wydaniu (T43), ale `setup.rsc` używa `:latest` bez digestu (`setup.rsc:53`), RouterOS nie weryfikuje podpisów | medium (F-11) |
| D | OOM kontenera przy logowaniach | `memory-max 64M` (`setup.rsc:63`) vs. szczyt argon2 2×19 MiB + RSS ≤40 MB | medium, do weryfikacji T51 (F-05) |
| D | Zużycie flash | `/data` na USB/microSD, WAL, brak zapisów przy pustym pollingu (SPEC §2.2) | low |
| — | `device-mode container=yes` osłabia ochronę routera (fizyczne potwierdzenie) | opisane w `docs/deploy-mikrotik.md:66` | akceptowane |

Do weryfikacji na sprzęcie (T00/T51): czy `/data` na nośniku FAT pozwala na prawa 0600 (inaczej `key.go:92` odmówi startu
i użytkownik przejdzie na klucz w envs), czy `user=` i `memory-max` działają, czy RouterOS weryfikuje TLS rejestru.

## 8. Otwarte ryzyka dla `v0.1.0`

| ID | Ryzyko | Ocena | Decyzja proponowana |
|---|---|---|---|
| R1 | Kreator `/setup` bez sekretu (F-01) | **high** | naprawić przed v0.1.0 |
| R2 | Hasło routera wysyłane pod nowy `base_url`/`insecure` bez ponownego podania (F-02) | medium | naprawić przed v0.1.0 |
| R3 | `X-Forwarded-For` zaufany dla całej sieci compose (F-03) | medium | naprawić przed v0.1.0 (konfiguracja) |
| R4 | Kontener jako root, brak izolacji forward (F-04) | medium | naprawić `setup.rsc` przed v0.1.0 |
| R5 | OOM przy równoległych logowaniach w trybie kontenerowym (F-05) | medium | zmierzyć w T51; do tego czasu `HashConcurrency=1` lub większy `memory-max` |
| R6 | Klucz obok bazy / w envs (F-06) | medium | zaakceptować z dokumentacją (backup = sekret) |
| R7 | Brak izolacji odczytu między tokenami (F-09) | medium | zaakceptować (MVP bez wielodzierżawności), opisać w T52 |
| R8 | Uprawnienia `write` konta routera (F-08) | medium | zaakceptować (ograniczenie RouterOS), opisać |

Kryterium PLAN §T50 („brak otwartych problemów wysokich”) **NIE jest spełnione przed v0.1.0**: F-01 (kreator `/setup` bez sekretu) ma wagę high na publicznie dostępnej, niezainicjalizowanej instancji i musi zostać naprawiony przed wydaniem. Pozostałe znaleziska (R2–R8) są medium.

## Przegląd krzyżowy

Model główny (Claude Opus) i niezależny przegląd (OpenAI Codex). Codex potwierdził F-01, F-02, F-03, F-04, F-06, F-08, F-09, F-11 i wniosek o niedoszacowanym F-01 → podniesiony do high. F-05 wymaga pomiarów w T51 (wyliczenie 38 MiB błędne — wymaga realnego pomiaru peak).

## Stan kryterium (aktualizacja po naprawie)

**F-01 naprawione** na branchu `fix/t50-f01-setup-secret`: dodano `TIKKER_SETUP_SECRET` — kreator `/setup` wymaga go, gdy nie ma `TIKKER_ADMIN_PASSWORD`, a konfiguracja z pustym sekretem przy nasłuchu poza loopbackiem jest odrzucana. 

**F-02 naprawione** na branchu `fix/t50-f02-router-pass`: zmiana hosta urządzenia (`base_url`) lub przełączenie z trybu weryfikowanego na `insecure` wymaga ponownego podania hasła routera (porównanie ze stanem zapisanym — re-submit tej samej wartości nie trigeruje). Zapobiega wysłaniu zapisanego hasła do obcego hosta lub po cleartexcie.

**F-03 naprawione** na branchu `fix/t50-f03-trusted-proxy`: compose zaufuje teraz tylko statycznemu adresowi Caddy (172.28.0.10), nie całej podsieci `172.28.0.0/24` — to wyklucza podszywanie się pod `X-Forwarded-For` przez inny kontener w sieci.

**F-04 naprawione** na branchu `fix/t50-f04-container-isolation`: `setup.rsc` dodaje teraz regułę forward blokującą ruch z sieci kontenera do LAN oraz dokumentuje tryb nie-root (`ctrUser "65532:65532"` + chown mediów) z opcją powrotu do `0:0`, gdy nośnik nie pozwala na chown.

**F-05 naprawione** na branchu `fix/t50-f05-argon2-concurrency`: dodano `TIKKER_PASSWORD_HASH_CONCURRENCY` (config `password_hash_concurrency`, walidowane ≥0, domyślnie 2); `setup.rsc` ustawia `1` w trybie kontenerowym i podnosi `memory-max` do `96M`. Usuwa ryzyko OOM przy równoległych logowaniach. Ostateczny pomiar peak RAM zostaje w T51.

**Zaakceptowane z dokumentacją:** R6 (klucz obok bazy — backup `/data` = sekret, udokumentowane w `deploy-mikrotik.md` §8 i `deploy-server.md` §7), R7 (brak izolacji odczytu — świadoma decyzja MVP, opisana w dokumentacji), R8 (polityka `write` konta routera — ograniczenie RouterOS, opisane w deploy docs).
