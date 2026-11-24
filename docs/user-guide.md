# Przewodnik użytkownika tikker

Ten przewodnik opisuje, jak po wdrożeniu uruchomić tikkera, zalogować się do panelu, dodać router i kanały,
wysyłać SMS-y, korzystać ze skrzynki, tokenów API, użytkowników i ról, dziennika audytu oraz jak wykonać kopię
zapasową. Na końcu są przykłady integracji przez REST API (curl, Python, PHP) i najczęstsze pytania.

Wdrożenie opisują: [docs/deploy-mikrotik.md](deploy-mikrotik.md) (kontener na routerze) oraz
[docs/deploy-server.md](deploy-server.md) (Docker Compose, systemd). Wersja angielska: [user-guide.en.md](user-guide.en.md).

Adresy stron podane są względem adresu instancji, np. `https://sms.example.com/devices`.

> **Konfiguracja.** Każde ustawienie ma klucz w pliku YAML i zmienną `TIKKER_*` (np. `listen` to `TIKKER_LISTEN`,
> `setup_secret` to `TIKKER_SETUP_SECRET`). Ustawienia podaje się w środowisku procesu albo w pliku wskazanym przez
> `--config`. `tikker --check-config` pokazuje efektywne ustawienia z zamaskowanymi sekretami i kończy się kodem 2,
> gdy coś jest niepoprawne.

## 1. Pierwsze uruchomienie

Konto administratora tworzy się na jeden z dwóch sposobów. Wybierz jeden.

**A. Hasło w zmiennej `TIKKER_ADMIN_PASSWORD`.** Gdy baza jest pusta, tikker przy starcie tworzy użytkownika `admin`
z tym hasłem (min. 12 znaków). Po pierwszym logowaniu zmień hasło w panelu. Przy niepustej bazie zmienna jest ignorowana, ale nie usuwaj jej bez
zastąpienia: przy nasłuchu poza loopbackiem musi pozostać ustawione `TIKKER_ADMIN_PASSWORD` albo `TIKKER_SETUP_SECRET`
(patrz niżej).

**B. Kreator w przeglądarce (`/setup`).** Dopóki nie ma żadnego użytkownika, strona logowania przekierowuje na `/setup`.
Formularz prosi o:

- nazwę użytkownika,
- sekret kreatora (pole pojawia się tylko wtedy, gdy ustawiono `TIKKER_SETUP_SECRET`),
- hasło (min. 12 znaków) i jego potwierdzenie.

Po zatwierdzeniu konto powstaje i od razu jesteś zalogowany. Kreator znika, gdy istnieje choć jeden użytkownik.

**Sekret kreatora.** Domyślny adres nasłuchu `:8080` obejmuje wszystkie interfejsy. Jeśli nasłuch jest poza
loopbackiem, tikker nie wystartuje, dopóki nie jest ustawione `TIKKER_ADMIN_PASSWORD` albo `TIKKER_SETUP_SECRET`
(min. 12 znaków). Wymóg obowiązuje przy każdym starcie, także gdy użytkownicy już istnieją, więc **nie usuwaj
obu zmiennych naraz**. Dzięki temu nikt z sieci nie przejmie niezainicjalizowanej instancji. Zalecane: ustaw
`TIKKER_SETUP_SECRET` (wpisujesz go w kreatorze; gdy istnieje użytkownik, kreator jest wyłączony, więc sekret już
niczego nie odblokowuje) i nie ustawiaj `TIKKER_ADMIN_PASSWORD`. Alternatywa: nasłuch na `127.0.0.1` za reverse proxy,
wtedy żadna z dwóch zmiennych nie jest wymagana.

## 2. Logowanie i sesje

- Strona logowania: `/login`. Podajesz nazwę użytkownika i hasło.
- Sesja wygasa po 1 godzinie bezczynności albo 12 godzinach od logowania. Wartości zmieniają
  `TIKKER_SESSION_IDLE_TIMEOUT` i `TIKKER_SESSION_ABSOLUTE_TIMEOUT`.
- Limity prób logowania są stałe: dla nazwy użytkownika 5 prób, potem jedna na minutę; dla adresu IP 20 prób,
  potem jedna co 6 sekund. Po przekroczeniu logowanie zwraca błąd; trzeba poczekać.
- Wylogowanie: przycisk w panelu (`POST /logout`).
- Adres publiczny ustaw w `TIKKER_BASE_URL`. Dodatkowe źródła żądań zmieniających dane (np. inny adres panelu)
  wpisz w `TIKKER_TRUSTED_ORIGINS`.
- Za reverse proxy (np. Caddy) ustaw `TIKKER_TRUSTED_PROXIES` na adres proxy. Tikker wierzy wtedy nagłówkowi
  `X-Forwarded-For` tylko od tego proxy. Bez tego limity i audyt widzą adres proxy zamiast klienta.

Menu panelu: **Pulpit**, **Wiadomości**, **Wysyłka** (operator i admin), **Urządzenia**, **Tokeny API**,
**Użytkownicy** i **Dziennik audytu** (ostatnie cztery tylko dla admina). Każdy użytkownik ma stronę
**Mój profil i zmiana hasła**, na której zmienia hasło.

## 3. Urządzenia i kanały

Urządzenie to router MikroTik. Kanał to port LTE (modem z kartą SIM) na urządzeniu, np. `lte1`. Wiadomości wychodzą
przez kanały. Urządzenia i kanały zarządza wyłącznie administrator.

### Dodanie urządzenia

Panel: **Urządzenia → Nowe urządzenie**, albo `POST /api/v1/devices`. Są dwa konektory:

- **`routeros-rest`** (domyślny). Tikker łączy się z REST API routera.
  - `base_url`: adres bez `/rest`, np. `https://10.20.0.1`. Plain `http://` jest dozwolony tylko dla adresów z
    `TIKKER_ALLOWED_HTTP_HOSTS`.
  - `username` i `password`: konto API na routerze (patrz `deploy/routeros/setup.rsc`).
  - `tls_mode`: `verify` (certyfikat z systemowych CA, domyślnie), `ca` (własne CA w `ca_pem`), `fingerprint`
    (przypięty odcisk SHA-256, typowe dla certyfikatu self-signed z RouterOS), `insecure` (bez weryfikacji; panel
    ostrzega).
- **`agent`**. To router łączy się z tikkerem, więc działa za CGNAT. Podajesz `agent_token` (min. 32 znaki, osobny
  dla każdego urządzenia). Szczegóły: [agent.md](agent.md).

Po dodaniu uruchom **Test** (`POST /api/v1/devices/{id}/test`). Hasło i token agenta nigdy nie są zwracane przez API.

### Dodanie kanału

Na stronie urządzenia dodaj kanał: `port` (np. `lte1`), opcjonalnie nazwę, tagi, limity i przełącznik włączenia.
Domyślne wartości:

| Pole | Domyślnie | Zakres | Znaczenie |
|---|---|---|---|
| `rate_per_min` | 6 | 1–600 | Limit wysyłki kanału. Nadmiar czeka w kolejce; to nie jest błąd `429`. |
| `max_segments` | 3 | 1–10 | Maksymalna liczba segmentów jednej wiadomości. Działa razem z możliwościami konektora. |
| `transliterate` | `true` dla `routeros-rest`, `false` dla `agent` (tylko dlatego, że `TIKKER_AGENT_UCS2` domyślnie jest `true`; dla konektora bez UCS-2 serwer ustawia `true`) | – | Zamiana polskich znaków na ASCII przed wysyłką (patrz rozdz. 4). |
| `tags` | puste | do 16 tagów | Routing: żądanie z `tags` trafia na kanał, który ma **wszystkie** te tagi. |

Stan kanału to `ok`, `degraded`, `down` albo `disabled`. Zdrowie sprawdzane jest co 60 s (`TIKKER_HEALTH_INTERVAL`).
Wyłączenie przełącznikiem w panelu ma pierwszeństwo przed wynikiem sprawdzenia.

> **Uwaga o RouterOS.** Konektor `routeros-rest` zakłada: tylko GSM-7, jeden segment, brak konkatenacji. Te
> założenia pochodzą z researchu (`ASSUMPTION(T00)`) i nie zostały jeszcze potwierdzone na sprzęcie. Sprawdź je
> na swoim modelu przed uruchomieniem produkcyjnym.

## 4. Wysyłka SMS

### Panel

**Wysyłka** pokazuje formularz. Przed wysyłką możesz kliknąć podgląd (`POST /send/preview`), który pokaże liczbę części
i segmentów, a także znaki, które zostaną zamienione przy transliteracji.

### Numer odbiorcy

`to` jest normalizowany do E.164 z regionem domyślnym `TIKKER_DEFAULT_REGION` (domyślnie `PL`). Wszystkie poniższe
zapisy to ten sam numer: `+48600100200`, `+48 600 100 200`, `600100200`. Numery krótkie i alfanumeryczne działają
tylko po ustawieniu `TIKKER_ALLOW_SHORT_NUMBERS=true`. Niepoprawny numer daje `422` (`invalid_phone_number`).

### Treść, kodowanie i segmenty

Treść to tekst UTF-8 o długości 1–1600 znaków. Tikker sam wybiera kodowanie:

| | GSM-7 | UCS-2 |
|---|---|---|
| Kiedy | tekst zawiera wyłącznie znaki alfabetu GSM 03.38 (np. `@`, `€`, `é`, `ü`, litery bez polskich ogonków) | wystarczy jeden znak spoza GSM-7, np. `ą ć ę ł ń ó ś ź ż` |
| Jeden SMS | 160 znaków | 70 znaków |
| Każdy segment, gdy jest ich więcej | 153 znaki | 67 znaków |

Znaki rozszerzone GSM-7 (`^ { } \ [ ] ~ | €`) zajmują w SMS-ie **dwie** pozycje, więc liczą się podwójnie.
Jeden polski znak zmienia cały SMS na UCS-2, dlatego długość limitu spada z 160 do 70 znaków.

Przykład: `Zażółć gęślą jaźń` ma 17 znaków, ale wymaga UCS-2, więc mieści się w jednym SMS-ie (70 znaków).

### Transliteracja

Gdy kanał ma `transliterate=true`, tikker zamienia znaki spoza GSM-7 przed wyliczeniem segmentów:
`ą→a, ć→c, ę→e, ł→l, ń→n, ó→o, ś→s, ź→z, ż→z`. Znaki bez odpowiednika (np. emoji, znaki CJK) stają się `?`. Podgląd
w panelu pokazuje, które znaki zostaną zamienione. Dla powyższego przykładu transliteracja daje `Zazolc gesla jazn`,
czyli tekst GSM-7 w jednym SMS-ie (160 znaków).

Gdy transliteracja jest włączona, zapisana treść (pole `body` w historii i w odpowiedzi API) jest już wersją po zamianie,
a `encoding` i `segments` dotyczą właśnie tej wersji.

Dla kanału bez transliteracji i bez obsługi UCS-2 tekst z polskimi znakami kończy się `422`
(`unsupported_characters`). Kanał `routeros-rest` nie obsługuje UCS-2, więc domyślnie ma transliterację włączoną.

### Długie wiadomości

Liczba segmentów nie może przekroczyć efektywnego limitu, czyli mniejszej z wartości `max_segments` kanału i
możliwości konektora. Zachowanie przy przekroczeniu określa `TIKKER_LONG_MESSAGE_STRATEGY` (ustawienie globalne,
nie per kanał):

- `reject` (domyślnie): `422` (`too_many_segments`), wiadomość nie trafia do kolejki;
- `split`: tekst dzielony jest na kilka niezależnych wiadomości z prefiksem `(1/N) `, każda z własnym statusem.
  Suma segmentów wszystkich części nadal nie może przekroczyć `max_segments` kanału; powyżej tego limitu
  też dostajesz `422` (`too_many_segments`);
- `truncate`: tekst jest obcinany do limitu.

### Cykl życia wiadomości

`queued` → `sending` → `sent` albo `failed`. Wiadomość w `queued` można anulować (`queued` → `cancelled`), w panelu
albo przez `DELETE /api/v1/messages/{id}`; dla innych stanów zwracane jest `409`. `sent` oznacza, że router przyjął
SMS-a. Tikker nie dostaje raportów doręczenia, więc nie ma stanu „doręczono”.

## 5. Wiadomości i skrzynka

**Wiadomości** pokazuje wychodzące i przychodzące SMS-y od najnowszych. Filtry: kierunek (`in`/`out`), status,
numer, kanał i zakres dat. Ta sama lista jest dostępna w API (`GET /api/v1/messages`).

**Skrzynka.** Przychodzące SMS-y mają kierunek `in` i status `received`. Tikker odpytuje skrzynkę każdego kanału co
15 s (`TIKKER_INBOX_POLL_INTERVAL`), więc nowy SMS pojawia się najpóźniej po tym czasie.

**Retencja.** Treści wiadomości są usuwane po `TIKKER_RETENTION` (domyślnie `2160h`, czyli 90 dni). Rekord zostaje,
a pole `body` ma wtedy wartość `null`.

## 6. Tokeny API

Token to dostęp do API. Tworzy się go w panelu (**Tokeny API**, tylko admin) albo w linii poleceń na tej samej instancji
(ten sam `TIKKER_DATA_DIR`):

```sh
tikker token create --user admin --name ci --scope sms:send --scope sms:read --ttl 720h
```

Domyślnie: właściciel `admin`, nazwa `cli`, zakres `admin`, bez wygaśnięcia (`--ttl 0`). Token w postaci
`tkr_<prefix>_<secret>` pokazywany jest **tylko raz**. W kontenerze: `docker compose exec tikker /tikker token create ...`. Na kontenerze MikroTika (obraz `scratch`, bez
powłoki) CLI jest niedostępne: utwórz token w panelu albo przez `POST /api/v1/tokens`.

| Zakres | Uprawnia do |
|---|---|
| `sms:read` | odczytu wiadomości i kanałów |
| `sms:send` | wysyłki i anulowania wiadomości |
| `admin` | zarządzania urządzeniami, kanałami, tokenami i użytkownikami; zawiera `sms:read` i `sms:send` |

Efektywne uprawnienia tokenu to część wspólna jego zakresów i roli właściciela. Token użytkownika z rolą `viewer` nie
wyśle SMS-a, nawet z zakresem `sms:send`. Token wyłączonego użytkownika oraz token wygasły lub unieważniony są odrzucane z `401`,
a brak zakresu daje `403`.

Dobra praktyka: osobny token na każdą integrację, z najwęższym zakresem (np. system zgłoszeń dostaje tylko `sms:send`),
z datą wygaśnięcia. Unieważnienie: usuń token w panelu albo `DELETE /api/v1/tokens/{id}`.

## 7. Użytkownicy i role

Użytkowników zarządza admin (**Użytkownicy**). Każdy użytkownik ma jedną z ról:

| Rola | Może |
|---|---|
| `viewer` | przeglądać pulpit i wiadomości |
| `operator` | jak `viewer`, plus wysyłać SMS-y, anulować wiadomości w kolejce |
| `admin` | wszystko: urządzenia, kanały, tokeny, użytkownicy, webhooki, audyt |

Użytkownika można wyłączyć (jego tokeny przestają działać). Hasło zmienia się na stronie **Mój profil i zmiana hasła**. Zalecane jest
konto administratora dla każdej osoby, zamiast współdzielonego loginu.

## 8. Dziennik audytu

**Dziennik audytu** (tylko admin) pokazuje zdarzenia z filtrem po akcji i aktorze. Rejestrowane są m.in.: udane i nieudane
logowania, wylogowania, tworzenie i zmiany użytkowników, tworzenie i usuwanie tokenów, urządzeń i kanałów, wysyłka
i anulowanie wiadomości, zmiany webhooków. Dziennik jest dostępny tylko w panelu, nie w API.

## 9. Webhooki, metryki i konektor agent

- **Webhooki** (`/api/v1/webhooks`, zakres `admin`): tikker wysyła `POST` przy SMS-ie przychodzącym (`incoming_sms`)
  i przy zmianie statusu wychodzącej wiadomości na `sent` lub `failed` (`message_status`). Szczegóły i podpis HMAC:
  [webhooks.md](webhooks.md).
- **Metryki Prometheus**: `GET /metrics`, bez tokenu. Endpoint nie wymaga uwierzytelnienia, więc na publicznym
  adresie ogranicz go na reverse proxy do sieci monitoringu. Zob. [metrics.md](metrics.md).
- **Konektor `agent`**: router odpytuje tikkera przez HTTPS, bez VPN i bez przekierowań portów: [agent.md](agent.md).
- **Model zagrożeń**: [security/threat-model.md](security/threat-model.md).

## 10. Kopia zapasowa `/data`

W katalogu danych (`/data` w kontenerze, `/var/lib/tikker` w wdrożeniu serwerowym) są:

- `tikker.db` wraz z plikami WAL (`tikker.db-wal`, `tikker.db-shm`): cała baza, czyli urządzenia, wiadomości, tokeny,
  użytkownicy i audyt;
- `secret.key`: klucz AES-256-GCM, którym zaszyfrowane są hasła routerów i tokeny agentów.

Kopiuj **oba** pliki razem i chroń je jak sekrety. Sama baza nie pozwoli odszyfrować haseł routerów, a kopia
`secret.key` jest niezbędna do odtworzenia instancji. Klucz podany przez `TIKKER_SECRET_KEY` (32 bajty w base64)
działa tak samo jak plik. Jeśli go zmienisz albo zgubisz `secret.key`, a w bazie są urządzenia, start kończy się
błędem o niezgodnym kluczu i panel jest niedostępny. Przywróć oryginalny klucz z kopii (albo usuń urządzenia z bazy).
Przy bazie bez urządzeń brakujący klucz jest po cichu generowany od nowa.

Bezpieczna kopia: zatrzymaj tikkera, skopiuj katalog danych, uruchom ponownie. Przykład dla Docker Compose jest
w [deploy-server.md](deploy-server.md#7-kopie-zapasowe), a dla kontenera na MikroTiku w
[deploy-mikrotik.md](deploy-mikrotik.md#8-kopia-zapasowa-data). Przywracanie: zatrzymaj usługę, podmień pliki i uruchom
ją ponownie, zachowując właściciela plików (w kontenerze `65532:65532`).

## 11. REST API: przykłady

Adres bazowy: `https://<instancja>/api/v1`. Każde żądanie wymaga nagłówka `Authorization: Bearer tkr_…`.
Pełny kontrakt: [api/openapi.yaml](../api/openapi.yaml).

| Cel | Metoda i ścieżka | Zakres |
|---|---|---|
| Wysyłka SMS | `POST /api/v1/messages` (nagłówek `Idempotency-Key` opcjonalny, zalecany) | `sms:send` |
| Status wiadomości | `GET /api/v1/messages/{id}` | `sms:read` |
| Przychodzące SMS-y | `GET /api/v1/messages?direction=in` | `sms:read` |
| Anulowanie | `DELETE /api/v1/messages/{id}` (tylko stan `queued`) | `sms:send` |

**Wysyłka.** Body (`application/json`): `to` (wymagane), `body` (wymagane), opcjonalnie `client_ref` (do 128 znaków,
zwracany w odpowiedzi), `tags`, `channel_id`, `expires_at` (RFC 3339, w przyszłości). Odpowiedź to `202 Accepted`,
nagłówek `Location` wskazuje pierwszą wiadomość, a `messages[]` zawiera utworzone wiadomości w stanie `queued`.
Zwykle jest jedna; przy `split` jest ich więcej. Wiele wiadomości naraz (do 100, atomowo) wysyła
`POST /api/v1/messages/batch`; `Idempotency-Key` dotyczy wtedy całej paczki (szczegóły w `api/openapi.yaml`).

**Idempotencja.** `Idempotency-Key` (1–255 drukowalnych znaków ASCII, bez spacji, np. UUID v4) jest ważny 24 h w obrębie tokenu:

- ten sam klucz i to samo ciało: ponowna odpowiedź `202` z tymi samymi wiadomościami i nagłówkiem
  `Idempotent-Replayed: true`, bez nowej wiadomości;
- ten sam klucz z innym ciałem: `422` (`idempotency-key-mismatch`);
- ten sam klucz, gdy poprzednie żądanie wciąż trwa: `409` (`idempotency-key-in-use`).

Gdy nie znasz wyniku (timeout, błąd sieci), powtórz żądanie z **tym samym** kluczem. Nie generuj nowego.

**Błędy.** Odpowiedzi błędów to `application/problem+json` (RFC 9457). Pole `type` ma postać
`urn:tikker:problem:<kod>`, a `errors[]` przy `422` podaje pole i kod, np. `too_many_segments`.

**Limity.** Każde żądanie z poprawnym tokenem zużywa limit tokenu: domyślnie 120 żądań na 60 s (`TIKKER_API_RATE_LIMIT`,
`0` wyłącza limit). Nagłówki `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` pokazują stan; po wyczerpaniu
przychodzi `429` z `Retry-After`.

### curl

```sh
TIKKER_URL=https://sms.example.com/api/v1
TOKEN='tkr_xxxxxxxx_yyyyyyyy'   # token z sekcji 6, zakres sms:send i sms:read

# 1. Wysyłka z kluczem idempotencji
KEY=$(openssl rand -hex 16)
curl -sS -X POST "$TIKKER_URL/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"to":"+48600100200","body":"Twój kod weryfikacyjny: 482913","client_ref":"otp-7f3a91"}'
# -> {"messages":[{"id":"01M89...","status":"queued",...}]}

# 2. Status wiadomości (id z odpowiedzi)
MSG_ID=01M89304ZQ61F6CVNX486V10VC
curl -sS "$TIKKER_URL/messages/$MSG_ID" -H "Authorization: Bearer $TOKEN"

# 3. Przychodzące SMS-y (najnowsze pierwsze, 50 na stronę)
curl -sS "$TIKKER_URL/messages?direction=in&limit=50" -H "Authorization: Bearer $TOKEN"
# następna strona: dołącz &cursor=<next_cursor>, dopóki next_cursor nie jest null
```

### Python (stdlib, bez zależności)

```python
import json
import os
import urllib.error
import urllib.parse
import urllib.request
import uuid

BASE = os.environ.get("TIKKER_URL", "https://sms.example.com/api/v1")
TOKEN = os.environ["TIKKER_TOKEN"]


def call(method, path, body=None, headers=None):
    data = None
    req_headers = {"Authorization": "Bearer " + TOKEN, "Accept": "application/json"}
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        req_headers["Content-Type"] = "application/json"
    req_headers.update(headers or {})
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=req_headers)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read() or b"null")
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"HTTP {e.code}: {e.read().decode('utf-8', 'replace')}") from e


# 1. Wysyłka; klucz zapisz razem z żądaniem, żeby móc go powtórzyć po błędzie sieci
key = str(uuid.uuid4())
sent = call(
    "POST",
    "/messages",
    {"to": "+48600100200", "body": "Twój kod weryfikacyjny: 482913", "client_ref": "otp-7f3a91"},
    {"Idempotency-Key": key},
)
msg_id = sent["messages"][0]["id"]

# 2. Status
msg = call("GET", f"/messages/{msg_id}")
print(msg["status"], msg["last_error"])

# 3. Przychodzące, wszystkie strony
cursor = None
while True:
    query = {"direction": "in", "limit": 50}
    if cursor:
        query["cursor"] = cursor
    page = call("GET", "/messages?" + urllib.parse.urlencode(query))
    for m in page["items"]:
        print(m["created_at"], m["peer"], m["body"])
    cursor = page["next_cursor"]
    if cursor is None:
        break
```

Uruchomienie: `export TIKKER_TOKEN="$(cat ~/.config/tikker/token)"` (albo pobierz token z menedżera sekretów, albo wpisz
`read -rs TIKKER_TOKEN && export TIKKER_TOKEN`), a potem `python3 sms.py`. Nie wpisuj tokenu w linii poleceń, bo trafi do
historii powłoki.

### PHP (rozszerzenie curl, PHP 8.0+)

```php
<?php

declare(strict_types=1);

function tikker(string $base, string $token, string $method, string $path, ?array $body = null, array $headers = []): array
{
    $ch = curl_init($base . $path);
    $hdr = ['Authorization: Bearer ' . $token, 'Accept: application/json'];
    if ($body !== null) {
        $hdr[] = 'Content-Type: application/json';
        curl_setopt($ch, CURLOPT_POSTFIELDS, json_encode($body, JSON_THROW_ON_ERROR));
    }
    foreach ($headers as $name => $value) {
        $hdr[] = $name . ': ' . $value;
    }
    curl_setopt_array($ch, [
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_HTTPHEADER => $hdr,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_TIMEOUT => 10,
    ]);
    $raw = curl_exec($ch);
    if ($raw === false) {
        $err = curl_error($ch);
        curl_close($ch);
        throw new RuntimeException('curl: ' . $err);
    }
    $code = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    curl_close($ch);
    if ($code >= 400) {
        throw new RuntimeException("HTTP $code: $raw");
    }
    return json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
}

$base  = getenv('TIKKER_URL') ?: 'https://sms.example.com/api/v1';
$token = getenv('TIKKER_TOKEN') ?: throw new RuntimeException('Brak TIKKER_TOKEN');

// 1. Wysyłka; klucz zapisz razem z żądaniem, żeby móc go powtórzyć po błędzie sieci
$key  = bin2hex(random_bytes(16));
$sent = tikker($base, $token, 'POST', '/messages',
    ['to' => '+48600100200', 'body' => 'Twój kod weryfikacyjny: 482913', 'client_ref' => 'otp-7f3a91'],
    ['Idempotency-Key' => $key]);
$msgId = $sent['messages'][0]['id'];

// 2. Status
$msg = tikker($base, $token, 'GET', '/messages/' . $msgId);
echo $msg['status'], ' ', $msg['last_error'] ?? '-', PHP_EOL;

// 3. Przychodzące, wszystkie strony
$cursor = null;
do {
    $query = http_build_query(array_filter(
        ['direction' => 'in', 'limit' => 50, 'cursor' => $cursor],
        static fn ($v) => $v !== null,
    ));
    $page = tikker($base, $token, 'GET', '/messages?' . $query);
    foreach ($page['items'] as $m) {
        echo $m['created_at'], ' ', $m['peer'], ' ', $m['body'], PHP_EOL;
    }
    $cursor = $page['next_cursor'];
} while ($cursor !== null);
```

Uruchomienie: ustaw `TIKKER_TOKEN` jak wyżej (z pliku, menedżera sekretów albo `read -rs`), a potem `php sms.php`.

## 12. Najczęstsze pytania

**Router jest za CGNAT (brak publicznego IP). Co wybrać?**
Masz dwie drogi, obie bez przekierowań portów na routerze.
1. **WireGuard**: router i serwer tikkera łączy tunel VPN, a tikker łączy się z REST API routera przez adres tunelu.
   Konfiguracja w [deploy-server.md, sekcja 6](deploy-server.md#6-routery-za-cgnat-wireguard).
2. **Konektor `agent`**: to router odpytuje tikkera przez HTTPS, więc wystarczy, że serwer tikkera ma publiczny adres.
   Nie potrzeba VPN ani publicznego IP routera. Patrz [agent.md](agent.md).

**Dlaczego polskie znaki zmniejszają długość SMS-a?**
Polskie litery nie należą do GSM-7, więc SMS przechodzi na UCS-2: 70 znaków na jeden SMS i 67 na każdy segment długiej
wiadomości, zamiast 160 i 153. Jeśli treść może być bez ogonków, włącz transliterację albo wpisz tekst bez nich.

**Dlaczego dostaję `422` `too_many_segments`?**
Tekst nie mieści się w efektywnym limicie segmentów (patrz rozdz. 4). Skróć treść, albo ustaw globalnie
`TIKKER_LONG_MESSAGE_STRATEGY=split` (wiele SMS-ów, ale suma segmentów nadal ograniczona przez `max_segments` kanału) lub
`truncate` (obcięcie). Na konektorze `routeros-rest` limit wynosi
jeden segment, bo RouterOS nie łączy SMS-ów (założenie `ASSUMPTION(T00)`).

**Dlaczego dostaję `422` `unsupported_characters`?**
Tekst zawiera znaki spoza GSM-7, a kanał nie obsługuje UCS-2 i nie ma transliteracji. Włącz `transliterate` na kanale
albo usuń takie znaki.

**Co oznacza status `sent`? Czy SMS dotarł?**
`sent` znaczy, że router przyjął SMS-a do wysyłki. Modem nie zwraca raportów doręczenia, więc tikker nie potwierdza
dotarcia SMS-a do telefonu.

**Router jest offline. Co dzieje się z wiadomościami?**
- Po nieudanych sprawdzeniach kanał przechodzi w `degraded` albo `down`. Taki kanał nie pobiera nowych wiadomości:
  wiadomości bez przypisanego kanału wysyła inny zdrowy kanał, a wiadomości przypięte do kanału (`channel_id`) czekają.
- Błąd przejściowy (brak sieci, timeout) ponawia wysyłkę do 5 prób, z opóźnieniami ok. 30 s, 2 min, 8 min, 30 min
  (±20% jittera). Po wyczerpaniu prób wiadomość kończy się `failed`.
- Wiadomość z `expires_at` przechodzi w `failed` z `last_error` zaczynającym się od `expired`, jeśli nie została
  wysłana przed tym czasem.
- Gwarancja jest **at-least-once** ([ADR 0002](adr/0002-gwarancje-dostarczenia.md)). Tikker nie zgubi wiadomości, ale
  w rzadkich przypadkach (przerwana wysyłka, wygasła dzierżawa) SMS może wyjść dwa razy. Wtedy pole
  `possible_duplicate` ma wartość `true`.

**Ile wiadomości mogę wysłać?**
- Limit API: domyślnie 120 żądań na minutę na token (`TIKKER_API_RATE_LIMIT`). Przekroczenie to `429`.
- Limit kanału: domyślnie 6 SMS-ów na minutę (`rate_per_min`). To nie jest błąd: nadmiar czeka w kolejce, czyli przy
  domyślnym ustawieniu około jednego SMS-a na 10 s. Większy limit ustawiasz na kanale, do 600 na minutę.

**Jak sprawdzić, czy tikker działa?**
`GET /healthz` (żyje), `GET /readyz` (gotowy). W kontenerze `tikker healthcheck` zwraca kod 0, gdy instancja jest gotowa;
`tikker healthcheck --live` sprawdza samą żywotność.

**Czy mogę używać panelu po HTTP?**
Tylko w zaufanej sieci lokalnej i tylko do testów. W Internecie stawiaj TLS (Caddy w `deploy/docker`, albo własny
reverse proxy). Hasła i cookie sesji przechodzące po HTTP są widoczne w sieci.

**Zgubiłem `secret.key`. Co teraz?**
Bez oryginalnego klucza zapisane hasła routerów są nieodczytywalne, a tikker nie wystartuje z tą bazą, jeśli są w niej
urządzenia (panel będzie niedostępny). Przywróć `secret.key` albo `TIKKER_SECRET_KEY` z kopii; ewentualnie usuń
urządzenia z bazy i dodaj je od nowa. Przy bazie bez urządzeń klucz jest po cichu generowany od nowa. Dlatego
rób kopię `secret.key` razem z bazą.
