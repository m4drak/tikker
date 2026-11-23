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

## Żądanie

```
POST <url>
Content-Type: application/json
X-Tikker-Event: incoming_sms | message_status
X-Tikker-Delivery: <id>
X-Tikker-Signature: sha256=<hex HMAC-SHA256(sekret, treść)>
```

Treść: `{"id", "event", "occurred_at", "message": {...}}`. `id` jest w podpisanej treści i jest taki sam przy
każdym ponowieniu. Odbiorca sprawdza podpis, a potem że nagłówek `X-Tikker-Delivery` równa się `id`:

```go
env, err := webhook.VerifyRequest(secret, body, r.Header) // ErrBadSignature przy niezgodności
if err != nil { /* 401 */ }
if seen(env.ID) { /* 200: już przetworzone */ }
```

## Bezpieczeństwo

- Sekret jest szyfrowany kluczem aplikacji; zwraca go tylko odpowiedź na `POST /webhooks` (`Cache-Control: no-store`).
- Wysyłka nie łączy się z adresami loopback, prywatnymi, link-local (w tym `169.254.169.254`), CGNAT ani
  zarezerwowanymi, a nazwa jest rozwiązywana i łączona do zatwierdzonego IP (certyfikat TLS jest nadal sprawdzany
  względem nazwy z URL). Proxy ze zmiennych środowiskowych jest ignorowane, przekierowania nie są śledzone.
  Wyjątek: hosty z `TIKKER_ALLOWED_HTTP_HOSTS` (też dla `http://`).
- Zmiana `url` lub wyłączenie webhooka anuluje niedostarczone zdarzenia; ładunek jest kasowany po zakończeniu
  dostarczenia, a niedostarczone zdarzenia starsze niż `retention` są porzucane.
- Dodanie, zmiana i usunięcie webhooka trafia do dziennika audytu (`webhook.create|update|delete`, bez sekretu i ścieżki URL).
