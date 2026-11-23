<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Przegląd bezpieczeństwa kodu (T50)

- Data: 2026-11-23, commit `5e15938`. Model: Claude Opus 5.5. Kontekst: [threat-model.md](threat-model.md).
- Zakres: `internal/auth`, `internal/secrets`, `internal/api`, `internal/web`, `internal/connector/routeros`,
  `internal/admin`, `internal/store/sqlite`, `internal/app`, `internal/logging`, `deploy/`.
- Metoda: lektura kodu i testów, bez uruchamiania. Zachowania niepotwierdzone oznaczono **[do weryfikacji]**.
- Wynik: **0 critical, 1 high**, 6 medium, 5 low, 2 nit.

## 1. Znaleziska

### F-01 · high · auth bypass (first-run takeover)
`internal/web/pages_auth.go:120-170`, `internal/auth/service.go:316-364`.
Dopóki baza nie ma użytkowników, każdy, kto pierwszy dotrze do `POST /setup`, tworzy admina. `TIKKER_ADMIN_PASSWORD`
jest opcjonalne (`internal/app/app.go:263-271` tylko ostrzega). Z profilem `tls` w compose instancja jest publiczna od
pierwszej minuty, a nowe certyfikaty z logów CT są skanowane automatycznie.
**Poprawka:** jednorazowy sekret kreatora (32 B losowe) generowany przy starcie, wypisywany w logu (`slog.Warn`) i wymagany
w formularzu (porównanie `subtle.ConstantTimeCompare`); alternatywnie kreator tylko z adresów loopback/prywatnych.
**Test negatywny:** `TestSetupRequiresSetupToken` – `POST /setup` z poprawnym Origin i bez/ze złym `setup_token` → 403/422,
`Users().Count()==0`.

### F-02 · medium · secret exfiltration (hasło routera „write-only”)
`internal/admin/devices.go:309,317,338` (+ `internal/api/devices.go:155`).
Zmiana `base_url` lub `tls_mode` (np. na `insecure`) zachowuje zapisane hasło (`d.PasswordEnc = nil // keeps`). Admin lub
token ze scope `admin` (np. w CI) ustawia `base_url=https://attacker` i woła `POST /devices/{id}/test` – connector wysyła
Basic auth z hasłem routera. Łamie to zasadę „hasło tylko do zapisu” (SPEC §5).
**Poprawka:** w `UpdateDevice` przy zmianie hosta/portu/schematu `base_url` lub przejściu `tls_mode` na `insecure`, albo
przy zmianie `fingerprint`/`ca_pem` bez hasła → 422 `password_required`; ewentualnie dodać host do AAD szyfrogramu.
**Test negatywny:** `TestUpdateDeviceNewHostRequiresPassword` – PATCH `{"base_url":"https://other:443"}` bez `password` → 422,
fake RouterOS pod nowym adresem nie otrzymuje nagłówka `Authorization`.

### F-03 · medium · spoofing (X-Forwarded-For)
`deploy/docker/compose.yaml:36` + `internal/auth/session.go:345-364`.
Zaufany jest cały `172.28.0.0/24`, w tym brama Dockera `172.28.0.1`, spod której przychodzi ruch przez userland-proxy
(port na `127.0.0.1`, Podman/rootless) **[do weryfikacji]** – wtedy klient sam ustawia XFF, obchodzi limit per IP
(zostaje limit per login) i fałszuje IP w audycie.
**Poprawka:** stały adres Caddy (`ipv4_address: 172.28.0.10`) i `TIKKER_TRUSTED_PROXIES=172.28.0.10/32`; bez profilu `tls`
domyślnie pusta lista.

### F-04 · medium · privilege / isolation (kontener RouterOS)
`deploy/routeros/setup.rsc:61,194` (`user=0:0`), `setup.rsc:140-152` (brak reguły forward).
Proces działa jako root (obraz deklaruje `USER 65532`), a podsieć veth może routować do LAN – przejęty tikker staje się
przyczółkiem w sieci.
**Poprawka:** `user=65532:65532` i `chown` katalogu na nośniku (lub dokumentacja, jeśli RouterOS nie pozwala) **[do weryfikacji T00]**;
reguła `chain=forward src-address=$vethNetwork connection-state=new action=drop` (wyjątki tylko jawne).

### F-05 · medium · DoS (pamięć w trybie kontenerowym)
`internal/auth/password.go:36,41` vs `deploy/routeros/setup.rsc:63`, `deploy/docker/Dockerfile:84`.
Dwa równoległe hashowania = ~38 MiB poza `GOMEMLIMIT=24MiB`; przy RSS ok. 30–40 MB przekracza `memory-max 64M`. Dwa adresy IP
mieszczące się w limitach mogą utrzymywać OOM-kill kontenera **[do weryfikacji pomiarem w T51]**.
**Poprawka:** `TIKKER_HASH_CONCURRENCY` (domyślnie 1 w obrazie), `memory-max` ≥ 96M w `setup.rsc`; testy obciążeniowe w T51.

### F-06 · medium · secret exposure (klucz obok danych)
`internal/secrets/key.go:46-77`, `setup.rsc:170-173`.
`secret.key` leży w tym samym `/data` co baza; nośnik USB/microSD lub backup `/data` zawiera wszystko do odszyfrowania haseł.
Alternatywa (`TIKKER_SECRET_KEY`) trafia do `/container/envs` i eksportów RouterOS. Brak lepszego magazynu na routerze.
Dodatkowo na FAT prawa 0600 są niemożliwe, więc `key.go:92` odmówi startu **[do weryfikacji]**.
**Poprawka:** dokumentacja (backup = sekret, szyfrowanie kopii), opcja `TIKKER_SECRET_KEY_FILE` (np. osobny mount tylko do
odczytu), czytelny komunikat dla FAT. Akceptowalne dla v0.1.0 po opisaniu.

### F-07 · low · secret leak (hasło w polu loginu)
`internal/auth/session.go:119,128-132`, `internal/auth/service.go:382-387`.
Przy `unknown_user` login spełniający `[A-Za-z0-9._-]{3,64}` trafia do logu i do audytu – częsty przypadek to hasło wpisane
w pole loginu.
**Poprawka:** przy `unknown_user` nie zapisywać nazwy (albo zapisać skrót HMAC); nazwę logować tylko dla istniejącego konta.

### F-08 · low · least privilege (konto RouterOS)
`deploy/routeros/setup.rsc:91-95`. Polityka `write` daje zmianę niemal całej konfiguracji. RouterOS nie ma ACL per ścieżka.
**Poprawka:** udokumentować; po T00 sprawdzić, czy wystarcza mniejszy zestaw (W2); monitorować logi `system,info` routera.

### F-09 · low · information disclosure (brak izolacji odczytu)
`internal/api/messages.go:237`, `internal/api/api.go:306-308`, `internal/web/pages_messages.go:113-115`.
Każdy `sms:read`/viewer widzi wszystkie wiadomości, w tym przychodzące kody OTP; `DELETE /messages/{id}` anuluje cudze
wiadomości. Zgodne ze SPEC (brak wielodzierżawności), ale nieopisane dla użytkowników.
**Poprawka:** opis w T52; po MVP opcjonalny filtr `created_by` dla tokenów (`store.MessageFilter.CreatedBy` już istnieje).

### F-10 · low · session/token lifecycle
`internal/auth/service.go:496` kończy sesje po zmianie hasła, ale **tokeny API pozostają ważne**; ponowne włączenie użytkownika
reaktywuje jego tokeny (`middleware.go:60-64`); `CreateToken` przyjmuje wyłączonego właściciela (`service.go:546-552`).
**Poprawka:** przy zmianie hasła w UI pytanie „unieważnić też tokeny?”; odmowa tworzenia tokenu dla `Disabled`.

### F-11 · low · supply chain
`deploy/routeros/setup.rsc:53`, `deploy/docker/compose.yaml:23`: `:latest` bez digestu; RouterOS nie weryfikuje cosign.
**Poprawka:** przypięte tagi wersji (najlepiej `@sha256:`) w przykładach i w README wydania.

### F-12 · nit · test coverage
`internal/web/pages_admin_test.go:85-90`: lista POST nie obejmuje `/users/{id}/status` ani `/users/{id}/password`.
**Poprawka:** test generowany z `s.routes` – każda trasa z rolą ≠ `""` musi dać 403 dla roli niższej (z poprawnym CSRF).

### F-13 · nit · hygiene
`internal/store/sqlite/sqlite.go:101` – plik bazy z domyślnym umask (0644 w obrazie; `secret.key` ma 0600), retencja
nie czyści `audit_log` z pełnymi IP (RODO); `phoneLike` (`routeros/errors.go:118`) nie łapie numerów z myślnikami.
**Poprawka:** `os.Chmod(path, 0o600)` po otwarciu, retencja audytu konfigurowalna, regex `[\d -]`.

## 2. Zweryfikowane jako OK

- Tokeny: format, entropia, stały czas, atrapa dla nieznanego prefiksu, odrzucenie po terminie i dla wyłączonego właściciela
  (`token.go`, `middleware.go:38-77`).
- CSRF: `CrossOriginProtection` + Referer + token HMAC sesji; brak tras GET zmieniających stan; logout i zmiana języka to POST.
- XSS: brak `template.HTML`/`JS`/`URL` w `internal/web`, CSP bez `unsafe-inline`, htmx bez eval, vendorowany htmx z sumą SHA-256.
- Sesje: rotacja, `__Host-`, timeouty, odczyt roli przy każdym żądaniu, ostatni admin chroniony w store
  (`internal/store/sqlite/users.go:123-131`).
- Argon2id zgodnie z SPEC §7, limit długości hasła, równa praca dla wszystkich porażek.
- AES-GCM z AAD, wersjonowaniem i ID klucza; `secrets.Password` redaguje `%v`, `%#v`, slog, JSON, tekst.
- Connector: brak proxy, brak przekierowań, pin liścia, limity ciała i czasu, maskowanie numerów w błędach.
- SQL wyłącznie parametryzowany; idempotencja per aktor w tej samej transakcji co zapis.
- Systemd: `DynamicUser`, `ProtectSystem=strict`, `UMask=0077`, pusty `CapabilityBoundingSet`.

## 3. Porównanie z SPEC §7

| Cel SPEC §7 | Stan | Dowód |
|---|---|---|
| argon2id m=19 MiB, t=2, p=1, ≤2 równoległe, rehash, min. 12 znaków | spełniony | `password.go:36,41,47`, `session.go:186-188` |
| Limity logowania 5/min per login, 20 + 1/6 s per IP, /64, 429 + `Retry-After` | spełniony | `ratelimit.go:24-25`, `session.go:389-399`, `pages_auth.go:86-90` |
| Brak różnicy czasowej brak użytkownika / złe hasło | spełniony | `session.go:157-185`, test `session_test.go:143` |
| Sesje w bazie jako hash, 1 h / 12 h, `__Host-`, koniec po zmianie hasła/roli/wyłączeniu | spełniony | `session.go:28-31,74-78,306`, `service.go:496` |
| CSRF: token HMAC + `CrossOriginProtection`; Bearer bez CSRF | spełniony | `csrf.go:86-112`, `app.go:400-405` |
| Tokeny `tkr_<8>_<32>`, SHA-256, stały czas | spełniony | `token.go:21-26`, `middleware.go:45,51` |
| AES-256-GCM, `secret.key` 0600 generowany przy starcie | spełniony (ryzyko F-06) | `cipher.go`, `key.go:92-95,124` |
| CSP bez `unsafe-inline`, XFO, Referrer-Policy | spełniony | `security.go:11-35` |
| Audyt logowań, urządzeń, tokenów, użytkowników, wysyłek z UI | spełniony | `session.go:137,145`, `service.go`, `sending.go:548,597` |
| Brak treści SMS w logach, numery maskowane | spełniony | test `api/e2e_test.go:124`, `send.go:86`, `worker.go:205` |
| Retencja treści 90 dni | spełniony dla treści; audyt bez retencji (F-13) | `store/sqlite/retention.go` |
| Hasło routera tylko do zapisu | **częściowo** (F-02) | `dto.go:120`, `admin/devices.go:338` |

## 4. Pokrycie zagrożeń testami

| Zagrożenie | Test | Ocena |
|---|---|---|
| CSRF (cross-site, same-site, brak tokenu, htmx) | `auth/csrf_test.go:16,97`, `web/security_test.go:99,230` | dobre |
| Role w panelu | `web/pages_admin_test.go:73`, `auth_test.go:336`, `pages_test.go:289`, `pages_messages_test.go:270` | dobre, luka F-12 |
| Zakresy tokenów i degradacja roli | `api/accounts_test.go:274,337` | dobre |
| Kod inline / CSP | `web/security_test.go:44,187` | dobre |
| Brute-force, równość pracy, XFF | `auth/session_test.go:143,159,493`, `ratelimit_test.go` | dobre |
| Szyfrowanie (podmiana, inny rekord, inny klucz, nonce) | `secrets/cipher_test.go:62,74,117`, `password_test.go:20,144` | dobre |
| Wycieki sekretów do logów | `api/e2e_test.go:124` | dobre; brak przypadku F-07 |
| Escaping treści SMS/nazw w HTML | `pages_messages_test.go`, `pages_admin_test.go`, `security_test.go` (ładunki `<script>`) | dobre |
| Kreator po utworzeniu admina | `web/auth_test.go:237,324` | brak testu F-01 |
| Zmiana `base_url` a hasło | brak | brak (F-02) |

## 5. Lista poprawek (do zamiany na issues)

### Wymagane przed v0.1.0

| # | Tytuł issue | Waga | Priorytet | Ref. |
|---|---|---|---|---|
| 1 | fix(auth): jednorazowy sekret kreatora `/setup` | **high** | P1 | F-01 |
| 2 | fix(admin): wymagaj hasła przy zmianie hosta `base_url` / `tls_mode=insecure` / pinu | medium | P1 | F-02 |
| 3 | fix(deploy): zaufany proxy tylko adres Caddy w compose | medium | P1 | F-03 |
| 4 | fix(deploy): `setup.rsc` – użytkownik nie-root i reguła forward dla veth | medium | P1 | F-04 |
| 5 | fix(auth): konfigurowalna współbieżność argon2, 1 w obrazie; `memory-max` ≥ 96M | medium | P1 | F-05 |
| 6 | docs(security): klucz i backup `/data`, izolacja odczytu, polityka `write` (T52) | medium/low | P1 | F-06, F-08, F-09 |

### Backlog

| # | Tytuł issue | Waga | Priorytet | Ref. |
|---|---|---|---|---|
| 7 | fix(auth): nie zapisuj nieznanych loginów w logu i audycie | low | P2 | F-07 |
| 8 | feat(auth): unieważnianie tokenów przy zmianie hasła; brak tokenów dla wyłączonych | low | P2 | F-10 |
| 9 | chore(deploy): przypięte wersje/digesty obrazu w przykładach | low | P2 | F-11 |
| 10 | test(web): generowany test ról dla wszystkich tras POST | nit | P3 | F-12 |
| 11 | chore(store): prawa 0600 bazy, retencja audytu, regex maskowania | nit | P3 | F-13 |
| 12 | feat(api): opcjonalna izolacja `created_by` dla tokenów (po MVP) | low | P3 | F-09 |
