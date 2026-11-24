# ADR 0004: Bramka e-mail↔SMS – wbudowany serwer SMTP i powiadomienia e-mail

- Status: proponowana (wymaga akceptacji właściciela, L3: bezpieczeństwo, nowy punkt wejścia sieciowego)
- Data: 2026-11-24
- Powiązane: PLAN §5 #5, SPEC §4 (wysyłka, idempotencja), §7 (bezpieczeństwo), ADR 0002, ADR 0003,
  `docs/email-gateway.md`, `docs/security/threat-model.md` §3a

## Kontekst

Część użytkowników nie potrafi lub nie chce wołać REST API. Mają za to narzędzia, które umieją wysłać e-mail:
monitoring (Zabbix, Nagios, UPS, NAS, kamery), skrypty z `sendmail`, klienta poczty. Stary wzorzec
„e-mail na adres `<numer>@bramka`” jest im znany. W drugą stronę właściciel karty SIM chce dostawać SMS-y
przychodzące (kody, odpowiedzi) do swojej skrzynki bez stawiania odbiornika webhooków.

Ograniczenia: tikker działa także w kontenerze na routerze (mało RAM, flash), ma być statyczną binarką bez CGO
i bez nowych zależności, jeśli nie są konieczne (ADR 0003). Każdy nowy nasłuch sieciowy to nowa powierzchnia ataku,
a bramka SMS-ów jest atrakcyjnym celem (wysyłka na koszt właściciela SIM, spam, SMS pumping).

## Decyzja

### E-mail → SMS: wbudowany, minimalny serwer SMTP submission (`internal/mailgw`)

- **Domyślnie wyłączony** (`mail_gateway.enabled=false`). Osobny nasłuch (np. `:2525`), niezależny od HTTP.
- **Tylko submission z uwierzytelnieniem.** `MAIL FROM` bez udanego `AUTH` jest odrzucane (`530 5.7.0`).
  Nie ma trybu „zaufanej sieci”, więc serwer nie może stać się otwartym przekaźnikiem.
- **Hasłem SMTP jest istniejący token API** (`tkr_…`). Weryfikacja przez tę samą funkcję co w REST API
  (`auth.Service.AuthenticateToken`): porównanie w stałym czasie, wygasłe i usunięte tokeny, wyłączony właściciel,
  `last_used_at`. Wymagany zakres `sms:send` (przecięty z rolą właściciela). Nazwa użytkownika jest ignorowana
  (zalecana: `token`). Nie wprowadzamy osobnych haseł SMTP – jeden mechanizm do odwołania (usunięcie tokenu).
- **TLS obowiązkowy** poza loopbackiem: tryb `starttls` (domyślny) albo `tls` (implicit TLS, port 465). Tryb `none`
  jest dozwolony wyłącznie przy nasłuchu na adresie loopback (walidacja konfiguracji odrzuca start, serwer dodatkowo
  odrzuca połączenia spoza loopbacku). `AUTH` jest ogłaszane i przyjmowane dopiero po TLS.
- **Adresaci**: `RCPT TO:<numer>@<domena>`. Numer normalizowany tą samą logiką co w API (`internal/sms/phone`,
  region domyślny, `allow_short_numbers`). Inna domena: `550 5.7.1` (brak przekazywania). Zły numer: `553 5.1.3`
  już na etapie `RCPT`. Limit adresatów na wiadomość (domyślnie 10).
- **Treść**: parser `net/mail` + `mime/multipart`; pierwsza część `text/plain` (bez załączników), dekodowanie
  quoted-printable i base64, zestawy znaków UTF-8, US-ASCII, ISO-8859-2, ISO-8859-1, Windows-1250 (przez
  `golang.org/x/text`, już zależność), `format=flowed`; odcięcie podpisu po linii `-- `. Opcjonalnie temat jako
  pierwsza linia. Wiadomość tylko-HTML jest odrzucana (`554 5.6.0`) – nie konwertujemy HTML na tekst.
- **Ta sama ścieżka wysyłki co REST API**: `sending.Service` (walidacja, wybór kanału, segmentacja, transliteracja,
  strategia długich wiadomości). Jedna transakcja SMTP z N adresatami = N wiadomości zapisanych atomowo
  (wszystkie albo żadna), wpis audytu `message.send` z `source=smtp`.
- **Idempotencja**: `Message-ID` pełni rolę `Idempotency-Key` (w zakresie tokenu, 24 h). Ponowienie tej samej
  wiadomości przez klienta SMTP (np. po zerwanym połączeniu przed odpowiedzią `250`) nie tworzy duplikatów SMS.
- **Kody odpowiedzi**: `250` po zapisaniu w kolejce; `4xx` przy błędach przejściowych (baza, limit szybkości);
  `5xx` przy błędach walidacji, z kodem problemu API w tekście (`554 5.6.0 out_of_range: ...`).
- **Ochrona zasobów**: limit rozmiaru (domyślnie 64 KiB) liczony podczas odczytu, limit długości linii, timeouty
  na komendę i na `DATA`, limit równoczesnych połączeń, limit nieudanych `AUTH` na połączenie i na adres IP
  (IPv6 per /64), wspólny z API limit żądań na token (każdy adresat zużywa jedno żądanie). Brak `PIPELINING`;
  dane przesłane razem ze `STARTTLS` zrywają połączenie (ochrona przed wstrzyknięciem komend, CVE-2011-0411).
- Zamknięcie razem z aplikacją: nowe połączenia odrzucane, bezczynne sesje dostają `421`, trwające zapisy kończą się.

### SMS → e-mail: trwała kolejka powiadomień (`internal/mailnotify`)

- **Domyślnie wyłączone** (`mail_notify.enabled=false`). Przekaźnik SMTP (host, port, `starttls`/`tls`, `none`
  tylko dla loopbacku), opcjonalne logowanie (hasło z env, YAML lub pliku, typ `Secret`, nigdy nie logowane), `From`,
  lista `To`.
- Każdy zapisany SMS przychodzący dodaje wiersz do nowej tabeli `mail_outbox` **w tej samej transakcji** co zapis
  SMS-a (wzorzec webhooków, ADR 0002: at-least-once). Wiersz przechowuje tylko ID wiadomości: treść jest czytana
  z `messages` w chwili wysyłki, więc nie powstaje druga kopia SMS-a z osobną retencją.
- Wysyłka przez `net/smtp` z dzierżawą, licznikiem prób jako tokenem i wykładniczym backoffem (jak webhooki).
  Odpowiedzi `5xx` kończą próby od razu, `4xx` i błędy sieci są ponawiane.
- Temat zawiera zamaskowany numer (`SMS from +48600***200`), treść e-maila – pełny numer, czas, kanał i tekst.
  Logi zawierają wyłącznie zamaskowane numery.

### Dlaczego nie istniejąca tabela `webhook_deliveries`

`webhook_deliveries.webhook_id` jest kluczem obcym na `webhooks` (adres URL, zaszyfrowany sekret, wersja) i jest
widoczna w API `/api/v1/webhooks`. Wpisanie „sztucznego” webhooka dla e-maila mieszałoby dwa kanały w jednym
kontrakcie API. Osobna, mała tabela z tym samym schematem dzierżaw jest prostsza i nie zmienia `api/openapi.yaml`.

## Rozważane alternatywy

| Wariant | Za | Przeciw | Decyzja |
|---|---|---|---|
| **Odpytywanie IMAP** (tikker loguje się do skrzynki i czyta nowe listy) | brak nasłuchu, działa za NAT | biblioteka IMAP (zależność) albo duży własny kod, hasło do skrzynki w tikkerze, opóźnienie odpytywania, trudna idempotencja (flagi `\Seen`), zapis na flash przy każdym cyklu | odrzucone |
| **Webhooki dostawców poczty** (Mailgun, SendGrid Inbound Parse, Postmark) | dostawca robi SMTP i antyspam | uzależnienie od chmury i jej formatu, treść SMS-ów u strony trzeciej, wymaga publicznego HTTPS, każdy dostawca inny | odrzucone (można dobudować później jako osobny endpoint) |
| **Zewnętrzny MTA + potok** (Postfix `pipe` → `tikker sms send`) | dojrzały SMTP, filtry antyspam | nie działa w kontenerze na routerze, każdy użytkownik konfiguruje MTA sam; i tak jest możliwy przez CLI (`tikker sms send`) | pozostaje jako opcja w dokumentacji, nie zamiast |
| **Biblioteka SMTP** (`emersion/go-smtp`, MIT) | kompletna implementacja | nowa zależność i większa powierzchnia (PIPELINING, LMTP, CHUNKING), których nie potrzebujemy; ADR 0003: najpierw stdlib | odrzucone; minimalny serwer (~1500 linii razem z parserem MIME) na `bufio`, `net/mail`, `mime`, `crypto/tls` |
| **Uwierzytelnianie po IP / bez AUTH** | prosta konfiguracja urządzeń | otwarty przekaźnik przy błędzie konfiguracji, brak rozliczalności | odrzucone |
| **Osobne hasła SMTP** | dowolne hasło w urządzeniu | drugi mechanizm uwierzytelniania, zarządzania i audytu | odrzucone: token API z zakresem `sms:send` |

## Konsekwencje bezpieczeństwa

- (+) Brak otwartego przekaźnika: bez udanego `AUTH` nie da się wykonać `MAIL FROM`, a `RCPT TO` przyjmuje tylko
  skonfigurowaną domenę.
- (+) Te same kontrole co w API (token, zakres, rola, limit na token, walidacja numeru, audyt) – jeden model uprawnień.
- (+) Token nie przechodzi otwartym tekstem poza loopbackiem (TLS wymagany konfiguracją i w czasie działania).
- (−) Nowy nasłuch TCP: ryzyko DoS (połączenia, wolni klienci). Ograniczone limitem połączeń (łącznie i na adres IP),
  czasem i liczbą komend przed `AUTH`, timeoutami i rozmiarem; przy wystawieniu do Internetu zalecamy mimo to firewall
  lub listę adresów (wiele adresów może zająć wszystkie sesje).
- (+) Po przeglądzie bezpieczeństwa (2026-11-24): token jest sprawdzany ponownie przy `MAIL FROM` i przed zapisem
  wiadomości, a sesja ma limit czasu i liczby e-maili, więc odwołanie tokenu, jego wygaśnięcie, wyłączenie lub
  degradacja właściciela działają także na otwarte sesje.
- (−) Token API zapisany w konfiguracji urządzeń wysyłających e-mail (drukarki, NAS) jest łatwiejszy do wycieku niż
  w skrypcie. Zalecenie: osobny token z samym `sms:send`, z datą wygaśnięcia, na każde urządzenie.
- (−) SMTP nie ma odpowiedzi per adresat po `DATA`: wiadomość do wielu adresatów jest przyjmowana albo odrzucana
  w całości (walidacja numerów odbywa się wcześniej, przy `RCPT`).
- (−) Bez nagłówka `Message-ID` ponowienie przez klienta może utworzyć duplikat SMS (jak POST bez `Idempotency-Key`).
- (−) Powiadomienia e-mail przenoszą treść SMS-ów (także kody OTP) poza tikkera: do przekaźnika SMTP i skrzynki.
  To świadoma decyzja właściciela włączającego funkcję; TLS do przekaźnika jest wymagany poza loopbackiem.
- Weryfikacja w testach: `internal/mailgw` (protokół, TLS, AUTH, open relay, limity, MIME), `internal/mailnotify`
  (ponowienia), `internal/app` (end-to-end z fake RouterOS).
