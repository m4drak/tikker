# Webhooki

tikker wysyła `POST` na adres skonfigurowany przez administratora (`/api/v1/webhooks`, zakres `admin`), gdy:

- `incoming_sms`: zapisano nowy SMS przychodzący (treść i numer są w ładunku);
- `message_status`: wiadomość wychodząca przeszła do `sent` albo `failed` (także po wygaśnięciu `expires_at`
  i po odzysku wygasłej dzierżawy). Ładunek nie zawiera treści wiadomości.

## Gwarancje

Dostarczenie jest at-least-once (ADR 0002). Zdarzenie jest zapisywane w tej samej transakcji SQLite co zmiana
statusu lub zapis SMS-a, więc nie ginie przy awarii. Ponowienia (timeout, `5xx`, `408`, `429`, błąd połączenia)
idą z wykładniczym opóźnieniem (10 s, 40 s, 2 min 40 s, ... do 1 h, 7 prób). Inne `4xx`, przekierowania i adresy
wewnętrzne kończą dostarczenie błędem od razu. Odbiorca deduplikuje po `id`.

### Okno ponowień i deduplikacja

Nominalne opóźnienia to 10 s, 40 s, 160 s, 640 s, 2560 s i 3600 s (po 6 nieudanych próbach z 7), czyli
7010 s (ok. 1 h 57 min). Jitter ±20% daje do 8412 s (ok. 2 h 20 min), a siedem prób po maksymalnie 10 s
(timeout żądania) dodaje do 70 s. Zdarzenie może więc dotrzeć do odbiorcy nawet ok. 2 h 22 min po pierwszej
próbie. Po przestoju tikkera ponowienie może wyjść jeszcze później (dostarczenie czeka w kolejce do `retention`).

- Odbiorca musi przechowywać zapamiętane `id` dostarczeń co najmniej tak długo, jak trwa okno ponowień, plus
  zapas na różnicę zegarów (kilka minut). Zalecane jest 24 h; krótsza pamięć niż okno ponowień powoduje
  podwójne przetworzenie ponowionego zdarzenia.
- Odbiorca powinien odrzucać zdarzenia, których `occurred_at` jest starsze niż okno ponowień plus zapas
  na różnicę zegarów (i te z `occurred_at` odległym w przyszłość), żeby przechwycone, poprawnie podpisane żądanie
  nie mogło być odtworzone po wygaśnięciu pamięci `id`. Jeśli odbiorca ma przyjmować zdarzenia dostarczone
  po przestoju tikkera, powinien zamiast tego przechowywać `id` przez cały `retention`.

## Żądanie

```
POST <url>
Content-Type: application/json
X-Tikker-Event: incoming_sms | message_status
X-Tikker-Delivery: <id>
X-Tikker-Signature: sha256=<hex HMAC-SHA256(sekret, treść)>
```

Treść: `{"id", "event", "occurred_at", "message": {...}}`. `id` jest w podpisanej treści i jest taki sam przy
każdym ponowieniu. Odbiorca sprawdza podpis, a potem że nagłówki `X-Tikker-Delivery` i `X-Tikker-Event` równają się
podpisanym `id` i `event`:

```go
env, err := webhook.VerifyRequest(secret, body, r.Header) // ErrBadSignature przy niezgodności
if err != nil { /* 401 */ }
if seen(env.ID) { /* 200: już przetworzone */ }
```

## Bezpieczeństwo

- Sekret jest szyfrowany kluczem aplikacji; zwraca go tylko odpowiedź na `POST /webhooks` (`Cache-Control: no-store`).
- Wysyłka nie łączy się z adresami loopback, prywatnymi, link-local (w tym `169.254.169.254`), CGNAT ani
  zarezerwowanymi (także zakresami IPv6 osadzającymi IPv4: NAT64, 6to4, Teredo, IPv4-compatible), a nazwa jest rozwiązywana i łączona do zatwierdzonego IP (certyfikat TLS jest nadal sprawdzany
  względem nazwy z URL). Proxy ze zmiennych środowiskowych jest ignorowane, przekierowania nie są śledzone.
  Wyjątek: hosty z `TIKKER_ALLOWED_HTTP_HOSTS` (też dla `http://`).
- Zmiana `url` lub wyłączenie webhooka anuluje niedostarczone zdarzenia; ładunek jest kasowany po zakończeniu
  dostarczenia, a niedostarczone zdarzenia starsze niż `retention` są porzucane.
- Dodanie, zmiana i usunięcie webhooka trafia do dziennika audytu (`webhook.create|update|delete`, bez sekretu i ścieżki URL).
