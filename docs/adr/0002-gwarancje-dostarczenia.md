# ADR 0002: Gwarancje dostarczenia – at-least-once, dzierżawy, idempotencja, deduplikacja

- Status: zaakceptowana (właściciel, 2026-11-19)
- Data: 2026-11-19
- Powiązane: SPEC §4.2–§4.4, ADR 0001, kod: `internal/domain` (maszyna stanów, `RetryPolicy`,
  klasyfikacja błędów), `internal/store` (kontrakt repozytoriów), `internal/connector`

## Kontekst

tikker przekazuje SMS-y między klientami REST a routerem MikroTik. Po drodze może zawieść wszystko:
proces (kill, OOM, restart kontenera), router (restart, utrata zasięgu), sieć (timeout po wysłaniu żądania)
i nośnik (zapis w bazie). RouterOS `/tool sms send` nie ma klucza idempotencji ani raportów doręczenia,
więc tikker nie może zapytać routera, czy dana wiadomość już wyszła.

Przy takim API dokładnie-raz (exactly-once) jest nieosiągalne. Trzeba wybrać, w którą stronę się mylić:

| Wariant | Co się dzieje przy awarii w trakcie wysyłki | Ryzyko |
|---|---|---|
| at-most-once | wiadomość oznaczamy jako `failed`/porzucamy | utrata wiadomości (np. kod OTP, alarm) |
| **at-least-once** | wiadomość wraca do kolejki i jest wysyłana ponownie | duplikat u odbiorcy |

Właściciel projektu zdecydował (SPEC §12): **lepszy duplikat niż utrata**.

## Decyzja

### 1. Wiadomości wychodzące: at-least-once z dzierżawą

Maszyna stanów jest czystą funkcją `domain.NextStatus` (100% pokrycia testami). Implementacje store i dispatcher
korzystają z niej obie, więc reguły są zapisane w jednym miejscu:

```
queued  --Acquire----------------------------> sending   (Attempts++)
sending --SendSucceeded----------------------> sent
sending --SendFailedTransient, att <  max----> queued    (NextAttemptAt = backoff)
sending --SendFailedTransient, att >= max----> failed
sending --SendFailedPermanent----------------> failed
sending --LeaseExpired, att <  max-----------> queued    (PossibleDuplicate)
sending --LeaseExpired, att >= max-----------> failed    (PossibleDuplicate)
queued  --Cancel-----------------------------> cancelled
queued  --Expire-----------------------------> failed    (last_error "expired…")
```

Każde inne przejście zwraca `ErrInvalidTransition`. Wiadomości przychodzące mają stały status `received`.

**Dzierżawa.** `MessageRepo.AcquireNext` w jednej atomowej operacji (`UPDATE … RETURNING` w SQLite) wybiera
wiadomość, ustawia `status=sending`, zwiększa `attempts`, wpisuje `channel_id` i `lease_until`.
Dwa równoległe wywołania nigdy nie dostaną tej samej wiadomości. Czas dzierżawy musi być dłuższy niż
timeout pojedynczej wysyłki w konektorze z zapasem (zalecane: `lease = 2 × send_timeout + 30 s`).
Według researchu T00 wysyłka RouterOS trwa do ok. 60 s, a REST zamyka żądanie po 60 s, więc
`Capabilities.SendTimeout` konektora `routeros-rest` wynosi 65 s, a dzierżawa ok. 160 s.
Port modemu jest zajęty na czas komendy, więc na kanał przypada jedna wysyłka naraz
(`Capabilities.MaxConcurrentSends = 1`).

**Token ogrodzenia (fencing).** Licznik `attempts` pełni rolę tokenu dzierżawy (`domain.Lease.Attempt`).
`MarkSent`, `MarkFailed` i `Requeue` aktualizują wiersz tylko przy zgodnym `id`, `status=sending` i `attempts`.
Jeśli worker „obudzi się” po wygaśnięciu i odzyskaniu dzierżawy, dostaje `ErrLeaseLost` i nie nadpisuje
stanu ustawionego przez nowszego właściciela.

**Kolejność zapisu.** Worker wysyła SMS dopiero po zatwierdzeniu dzierżawy w bazie. Wynik zapisuje po
odpowiedzi routera. Okno ryzyka to czas między przyjęciem przez RouterOS a zapisem `sent`.

**Odzysk po awarii.** `MessageRepo.RecoverExpiredLeases` uruchamiany jest przy starcie i okresowo
(np. co `lease/2`). Każda wiadomość w `sending` z `lease_until ≤ now` przechodzi zgodnie z
`NextStatus(sending, LeaseExpired, …)`:
- `attempts < max` → `queued` z `next_attempt_at = now`;
- `attempts ≥ max` → `failed`.

W obu przypadkach ustawiane jest `possible_duplicate = true`, a zdarzenie jest logowane jako
„możliwy duplikat” (z `message_id` i `channel_id`, bez treści i z maskowanym numerem).
Limit prób obowiązuje także tu, żeby wiadomość powodująca awarię procesu (poison message) nie krążyła w nieskończoność.

**Błędy niejednoznaczne.** Timeout lub zerwane połączenie *po* wysłaniu żądania do routera konektor zgłasza
jako `domain.TransientAmbiguous`. Niesklasyfikowane `context.Canceled` / `DeadlineExceeded` są traktowane tak samo
(`domain.IsAmbiguous`). Dispatcher przekazuje wtedy `PossibleDuplicate=true` do `Requeue` lub `MarkFailed`.
Błąd, który na pewno wystąpił przed wysłaniem (np. odmowa połączenia), jest zwykłym `Transient` bez flagi.

**Klasyfikacja błędów** (`domain.KindOf`, `domain.EventForSendResult`):

| Klasa | Przykłady | Skutek dla wiadomości | Skutek dla kanału |
|---|---|---|---|
| `Transient` | sieć, timeout, 5xx, modem zajęty | ponowienie z backoffem | health checker może oznaczyć `down` |
| `Permanent` | zły numer, 4xx walidacji | `failed` | brak |
| `Auth` | 401/403 | ponowienie jak `Transient` | `degraded` + alert w UI; worker przestaje pobierać |
| niesklasyfikowany | błąd programu, nieznana odpowiedź | jak `Transient` | brak |

Nieznany błąd prowadzi do ponowienia (ograniczonego `max_attempts`), a nie do cichego porzucenia. Wynika to z at-least-once.

**Ponowienia** (`domain.RetryPolicy`): wykładniczy backoff z symetrycznym jitterem ±20%.
Domyślnie `max_attempts=5`, a nominalne opóźnienia wynoszą 30 s, 2 min, 8 min i 30 min (mnożnik 4, limit 30 min).
Źródło losowości jest wstrzykiwane (`RandFunc`), więc testy są deterministyczne.

**Wygasanie.** Wiadomość z `expires_at`, która wciąż jest w `queued`, przechodzi do `failed`
z `last_error` zaczynającym się od `expired` (`MessageRepo.ExpireQueued`). `AcquireNext` nie wydaje wiadomości po terminie.
Wiadomość już w `sending` nie wygasa, bo nie wiadomo, czy wyszła.

**Łagodne zamknięcie (SIGTERM).** Worker nie pobiera nowych dzierżaw i czeka na bieżącą wysyłkę do timeoutu.
Wysyłka przerwana przez timeout zamknięcia jest niejednoznaczna: przechodzi zgodnie z `NextStatus` jak przy
wygasłej dzierżawie, czyli przy `attempts < max` wraca do kolejki (`next_attempt_at = now`) z flagą duplikatu,
a przy `attempts ≥ max` kończy jako `failed` z `possible_duplicate` (decyzja właściciela, 2026-11-20).
Jeśli proces nie zdąży zapisać wyniku, zadziała odzysk dzierżawy przy następnym starcie.

### 2. Routing w chwili pobrania (pull)

Kanał jest wybierany przy **pobraniu** wiadomości przez worker, a nie przy zapisie. Wiadomość przechowuje
ograniczenia z żądania (`Route`: jawny `channel_id` i/lub `tags`). Worker zdrowego kanału pobiera najstarszą
wiadomość, którą może obsłużyć. Kanał w stanie innym niż `ok` nie pobiera nic, więc jego awaria nie blokuje
wiadomości, które nie były do niego przypięte. Przy `Requeue` nieprzypięta wiadomość traci `channel_id`
i może trafić do innego kanału. Efekt jest równoważny regule SPEC §4.4 „najmniejsza kolejka, round-robin przy remisie”,
bo wolny worker bierze następną wiadomość. Nie trzeba przy tym przenosić wiadomości między kanałami.

### 3. Idempotencja API

`Idempotency-Key` jest unikalny w obrębie tokenu (aktora `created_by`) przez 24 h i dotyczy całego żądania
(pojedynczego lub paczki). Store trzyma rekord `(created_by, key) → (request_hash, message_ids)`
i obsługuje go w tej samej transakcji, w której zapisuje wiadomości (`MessageRepo.Enqueue`):
- ten sam klucz i ten sam hash żądania: zwracamy istniejące wiadomości (`Replayed`, nagłówek `Idempotent-Replayed: true`);
- ten sam klucz i inny hash: `ErrIdempotencyMismatch` (API: 422);
- paczka jest zapisywana atomowo: wszystkie wiadomości albo żadna.

Klient, który nie dostał odpowiedzi (timeout), może bezpiecznie powtórzyć żądanie z tym samym kluczem.
Tak domyka się at-least-once po stronie klienta bez duplikatów w kolejce.

### 4. Wiadomości przychodzące: zapis przed usunięciem, deduplikacja

Cykl pollera: `FetchInbox` → zapis wszystkich wiadomości w **jednej transakcji** (`InsertInbound`) →
**po commicie** `DeleteInbox`. Awaria między zapisem a usunięciem zostawia SMS na routerze.
Następny cykl pobierze go ponownie, a `InsertInbound` rozpozna duplikat po `dedup_hash` (UNIQUE) i zwróci `created=false`.
To nie jest błąd.

`dedup_hash = domain.InboundDedupHash(device_id, port, phone, raw_timestamp, body)` to SHA-256 pól
z prefiksem długości, żeby uniknąć kolizji przez separator. Używamy **surowego** tekstu znacznika czasu z RouterOS,
bo parsowanie formatu [WERYFIKACJA] mogłoby zmienić hash między wersjami. `.id` z RouterOS (`*1A`) nie nadaje się
do deduplikacji, bo router może go użyć ponownie.

RouterOS nie skleja SMS-ów wieloczęściowych i dekoduje wszystko jako GSM-7 (research T00), dlatego
`connector.InboundSMS` niesie też surowe pole `pdu`. tikker sam dekoduje UCS-2 i składa części po UDH.
Hash liczymy dla każdej części osobno, przed sklejaniem, z surowych danych routera
(`body` = `pdu`, jeśli jest dostępne, w przeciwnym razie `message`). Sposób przechowywania części
oczekujących na komplet ustali T16 po pomiarach T00.

Efekt: at-least-once między routerem a bazą i brak duplikatów w bazie.

## Konsekwencje

- (+) Żadna wiadomość przyjęta przez API (202) nie ginie w wyniku awarii procesu, routera ani sieci.
  Zostaje wysłana albo kończy jako `failed` z czytelnym `last_error`.
- (+) Duplikaty są ograniczone do okna między przyjęciem przez RouterOS a zapisem wyniku i zawsze są oznaczone
  (`possible_duplicate`, log), więc da się je wyjaśnić.
- (+) Przewidywalne reguły w czystych funkcjach (`NextStatus`, `RetryPolicy`) dają się w pełni przetestować.
  Test „kill w trakcie wysyłki” (T15) sprawdza zachowanie z tego ADR.
- (−) Odbiorca może dostać ten sam SMS dwa razy. Klienci wysyłający treści wrażliwe na duplikaty (np. jednorazowe kody)
  muszą to tolerować. Opisze to dokumentacja użytkownika (T52).
- (−) `Auth` zużywa próbę wiadomości (jedną na kanał, bo kanał przechodzi w `degraded`).
  Przy długiej awarii uprawnień wiadomość może skończyć jako `failed`.
- (−) Dwa różne SMS-y od tego samego nadawcy, z identyczną treścią i znacznikiem czasu (rozdzielczość sekundy)
  na tym samym porcie, zostaną zapisane jako jeden. Ryzyko akceptujemy.
- (−) Idempotencja wymaga dodatkowej tabeli rekordów kluczy (sprzątanie po 24 h w zadaniu retencji).
- Do potwierdzenia w T00: czy RouterOS zwraca błąd synchronicznie (wpływa na liczbę błędów `Ambiguous`)
  i czy wieloczęściowe SMS-y przychodzą jako osobne wpisy (wpływa na sklejanie przed deduplikacją).
