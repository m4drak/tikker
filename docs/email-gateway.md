# Bramka e-mail ↔ SMS

tikker może (opcjonalnie) przyjmować e-maile i wysyłać je jako SMS (**e-mail → SMS**) oraz przesyłać SMS-y
przychodzące na skrzynkę e-mail (**SMS → e-mail**). Obie funkcje są **domyślnie wyłączone** i niezależne od siebie.
Decyzja projektowa i alternatywy: [ADR 0004](adr/0004-bramka-email-sms.md). Zagrożenia: [model zagrożeń §3a](security/threat-model.md).

## 1. E-mail → SMS (serwer SMTP)

tikker uruchamia mały serwer SMTP (submission). E-mail wysłany na adres `<numer>@<domena>` staje się SMS-em
na ten numer. Przykład: `600100200@sms.example.net` → SMS na `+48600100200` (region domyślny `PL`).

- **Uwierzytelnienie jest obowiązkowe.** Login: dowolny (zalecany `token`), hasło: token API tikkera (`tkr_…`)
  z zakresem `sms:send`. Token tworzysz w panelu (Tokeny API) albo `tikker token create --scope sms:send`.
  Wygasły, usunięty token albo wyłączony właściciel = odmowa (`535`). Zalecenie: osobny token na każde urządzenie,
  tylko z `sms:send`, z datą wygaśnięcia.
- **Brak przekazywania (open relay).** Bez `AUTH` nie przejdzie `MAIL FROM` (`530`). Adresat spoza skonfigurowanej
  domeny: `550 5.7.1 Relaying denied`. Zły numer: `553 5.1.3 invalid_phone_number` już przy `RCPT TO`.
- **TLS.** Domyślny tryb `starttls` (port np. 587 albo 2525), `tls` = implicit TLS (port 465). `AUTH` jest dostępne
  dopiero po TLS. Tryb `none` (bez szyfrowania) jest dozwolony tylko przy nasłuchu na `127.0.0.1`/`::1` – np. gdy
  lokalny Postfix przekazuje pocztę do tikkera.
- **Treść SMS-a.** Pierwsza część `text/plain` (bez załączników); quoted-printable i base64; UTF-8, US-ASCII,
  ISO-8859-1, ISO-8859-2, Windows-1250; `format=flowed`. Podpis od linii `-- ` jest odcinany. Przy `body: subject_text`
  temat trafia do pierwszej linii SMS-a. E-mail tylko w HTML: `554 5.6.0 html_only` (wyślij tekst).
- **Ta sama ścieżka co REST API.** Walidacja, wybór kanału, transliteracja, strategia długich wiadomości
  (`TIKKER_LONG_MESSAGE_STRATEGY`) i limit 1600 znaków działają jak dla `POST /api/v1/messages`. Każdy adresat to osobna
  wiadomość; wszystkie adresaci jednego e-maila są zapisywani razem albo wcale. Każdy adresat zużywa jedno żądanie
  z limitu tokenu (`TIKKER_API_RATE_LIMIT`), wspólnego z API. W audycie: `message.send` z `source=smtp`.
- **Idempotencja.** `Message-ID` działa jak `Idempotency-Key` (per token, 24 h): klient, który ponawia ten sam e-mail
  (np. po zerwanym połączeniu), nie wyśle SMS-a drugi raz; dostaje `250 ... already queued`. Ten sam `Message-ID`
  z inną treścią: `554 5.7.0 idempotency_key_mismatch`. `Message-ID` trafia też do `client_ref` (jeśli ≤ 128 znaków).
  E-mail bez `Message-ID` jest za każdym razem nowym SMS-em.
- **Odpowiedzi.** `250 2.0.0 Ok: queued as <id>[,<id>…]` – wiadomości są w kolejce (status sprawdzisz w API/panelu).
  `4xx` – błąd chwilowy (baza, limit tokenu): klient powinien ponowić. `5xx` – błąd trwały, w treści kod jak w API,
  np. `554 5.6.0 out_of_range: At most 1600 characters.`, `554 5.6.0 no_matching_channel: ...`.

### Konfiguracja

| YAML (`mail_gateway.`) | Zmienna | Domyślnie | Opis |
|---|---|---|---|
| `enabled` | `TIKKER_MAIL_GATEWAY_ENABLED` | `false` | włącza serwer SMTP |
| `listen` | `TIKKER_MAIL_GATEWAY_LISTEN` | `:2525` | adres nasłuchu |
| `domain` | `TIKKER_MAIL_GATEWAY_DOMAIN` | – | **wymagana**: domena adresatów `<numer>@domena` |
| `hostname` | `TIKKER_MAIL_GATEWAY_HOSTNAME` | = `domain` | nazwa w powitaniu `220` |
| `tls_mode` | `TIKKER_MAIL_GATEWAY_TLS_MODE` | `starttls` | `starttls`, `tls` (implicit) albo `none` (tylko loopback) |
| `tls_cert_file` / `tls_key_file` | `TIKKER_MAIL_GATEWAY_TLS_CERT_FILE` / `..._KEY_FILE` | certyfikat HTTPS (`TIKKER_TLS_*`) | certyfikat serwera SMTP (PEM) |
| `max_message_size` | `TIKKER_MAIL_GATEWAY_MAX_MESSAGE_SIZE` | `65536` | maks. rozmiar e-maila w bajtach (1 KiB–10 MiB) |
| `max_recipients` | `TIKKER_MAIL_GATEWAY_MAX_RECIPIENTS` | `10` | maks. adresatów jednego e-maila (1–100) |
| `max_connections` | `TIKKER_MAIL_GATEWAY_MAX_CONNECTIONS` | `20` | maks. równoczesnych sesji SMTP |
| `command_timeout` | `TIKKER_MAIL_GATEWAY_COMMAND_TIMEOUT` | `1m` | czas na jedną komendę |
| `data_timeout` | `TIKKER_MAIL_GATEWAY_DATA_TIMEOUT` | `5m` | czas na przesłanie treści po `DATA` |
| `body` | `TIKKER_MAIL_GATEWAY_BODY` | `text` | `text` albo `subject_text` (temat w pierwszej linii) |

Region numerów i numery krótkie: `TIKKER_DEFAULT_REGION`, `TIKKER_ALLOW_SHORT_NUMBERS` (jak w API).
Stałe limity: linia do 4096 bajtów, 3 nieudane `AUTH` na połączenie, 10 nieudanych `AUTH` na adres IP (IPv6: /64),
potem jedno co 30 s; 10 błędnych komend kończy sesję.

Przykład (`config.yaml`):

```yaml
mail_gateway:
  enabled: true
  listen: ":2525"
  domain: sms.example.net
  tls_mode: starttls
  tls_cert_file: /data/tls/smtp.crt
  tls_key_file: /data/tls/smtp.key
```

W Dockerze odkomentuj port `2525` w `deploy/docker/compose.yaml`. Na routerze MikroTik wystaw port tylko do LAN
(dst-nat z listy adresów), nigdy do Internetu bez filtrowania.

### Przykłady klientów

**curl** (lokalnie, `tls_mode: none` na `127.0.0.1`):

```sh
printf 'From: nas@example.net\r\nTo: 600100200@sms.local\r\nSubject: backup\r\nMessage-ID: <b-2026-11-24@nas>\r\n\r\nBackup OK\r\n' > mail.txt
curl --url smtp://127.0.0.1:2525 --mail-from nas@example.net --mail-rcpt 600100200@sms.local \
     --user "token:$TIKKER_TOKEN" --upload-file mail.txt
```

Z STARTTLS: `--url smtp://sms.example.net:2525 --ssl-reqd` (przy certyfikacie własnym dodaj `--cacert ca.pem`).
Implicit TLS: `--url smtps://sms.example.net:465`.

**swaks**:

```sh
swaks --server sms.example.net:2525 --tls --auth PLAIN --auth-user token --auth-password "$TIKKER_TOKEN" \
      --from monitor@example.net --to 600100200@sms.example.net --header 'Subject: alarm' --body 'UPS on battery'
```

**Thunderbird** (konto wychodzące): serwer `sms.example.net`, port `2525`, zabezpieczenie STARTTLS, uwierzytelnianie
„normalne hasło”, użytkownik `token`, hasło = token API. Wysyłaj jako „zwykły tekst” (Opcje → Format → Tylko tekst),
inaczej e-mail tylko w HTML zostanie odrzucony.

**Postfix** jako przekaźnik (np. dla urządzeń, które umieją tylko SMTP bez TLS w LAN) – mapa transportu kieruje
domenę SMS do tikkera z uwierzytelnieniem:

```
# /etc/postfix/main.cf
transport_maps = hash:/etc/postfix/transport
smtp_sasl_auth_enable = yes
smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd
smtp_sasl_security_options = noanonymous
smtp_tls_security_level = encrypt

# /etc/postfix/transport
sms.example.net   smtp:[tikker.lan]:2525

# /etc/postfix/sasl_passwd   (chmod 600; postmap sasl_passwd)
[tikker.lan]:2525   token:tkr_xxxxxxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

Postfix ponawia wiadomości po odpowiedziach `4xx` i zwraca nadawcy bounce po `5xx`.

**Bez serwera SMTP**: e-mail można też przekazać potokiem do CLI (`tikker sms send`, rozdz. 11 przewodnika),
np. z `procmail`/Postfix `pipe` – wtedy bramka SMTP nie jest potrzebna.

## 2. SMS → e-mail (powiadomienia)

Każdy zapisany SMS przychodzący jest wysyłany e-mailem na skonfigurowane adresy przez Twój przekaźnik SMTP
(np. konto pocztowe u dostawcy, Postfix w LAN).

- Powiadomienie jest zapisywane w bazie w tej samej transakcji co SMS (tabela `mail_outbox`), więc nie ginie przy
  restarcie. Odpowiedź `4xx` przekaźnika i błędy sieci: ponowienia (30 s, 2 min, 8 min, 32 min, potem co godzinę;
  8 prób). `5xx` (np. zły login, odrzucony adresat): koniec prób, błąd w logu.
- Temat: `SMS from +48600***200` (zamaskowany numer – temat bywa widoczny na ekranie blokady). Treść e-maila: pełny
  numer nadawcy, czas odbioru (UTC), kanał, ID wiadomości i tekst SMS-a. Logi zawierają tylko zamaskowane numery.
- Treść jest czytana z bazy w chwili wysyłki: po upływie retencji (`TIKKER_RETENTION`) niewysłane powiadomienie
  kończy się błędem zamiast wysyłać pustą wiadomość.
- **Uwaga:** e-mail przenosi treść SMS-ów (także kody jednorazowe OTP/2FA) do przekaźnika i skrzynki. Włączaj funkcję
  świadomie i tylko z TLS do przekaźnika.

| YAML (`mail_notify.`) | Zmienna | Domyślnie | Opis |
|---|---|---|---|
| `enabled` | `TIKKER_MAIL_NOTIFY_ENABLED` | `false` | włącza powiadomienia |
| `smtp_host` | `TIKKER_MAIL_NOTIFY_SMTP_HOST` | – | **wymagany**: host przekaźnika |
| `smtp_port` | `TIKKER_MAIL_NOTIFY_SMTP_PORT` | `587` | port przekaźnika |
| `tls_mode` | `TIKKER_MAIL_NOTIFY_TLS_MODE` | `starttls` | `starttls`, `tls` (port 465) albo `none` (tylko przekaźnik na loopbacku) |
| `username` | `TIKKER_MAIL_NOTIFY_USERNAME` | – | login (AUTH PLAIN); puste = bez logowania |
| `password` | `TIKKER_MAIL_NOTIFY_PASSWORD` | – | hasło (sekret, redagowane w logach i `--check-config`) |
| `password_file` | `TIKKER_MAIL_NOTIFY_PASSWORD_FILE` | – | plik z hasłem (zamiast `password`; końcowy znak nowej linii jest pomijany) |
| `from` | `TIKKER_MAIL_NOTIFY_FROM` | – | **wymagany**: nadawca, np. `tikker <sms@example.net>` |
| `to` | `TIKKER_MAIL_NOTIFY_TO` | – | **wymagani**: adresaci, po przecinku (maks. 20) |
| `timeout` | `TIKKER_MAIL_NOTIFY_TIMEOUT` | `30s` | limit jednej wysyłki (1 s–10 min) |

```yaml
mail_notify:
  enabled: true
  smtp_host: smtp.example.net
  smtp_port: 587
  tls_mode: starttls
  username: sms@example.net
  password_file: /run/secrets/smtp_password
  from: "tikker <sms@example.net>"
  to: [owner@example.net]
```

Certyfikat przekaźnika jest weryfikowany systemowymi CA (obraz zawiera `ca-certificates`); nazwa hosta w `smtp_host`
musi zgadzać się z certyfikatem.

## 3. Bezpieczeństwo – podsumowanie

- Serwer SMTP nie jest przekaźnikiem: wymaga tokenu z `sms:send` i przyjmuje wyłącznie skonfigurowaną domenę.
- Token nie przechodzi otwartym tekstem poza loopbackiem; konfiguracja z `tls_mode: none` na adresie innym niż
  loopback jest odrzucana przy starcie.
- Nie wystawiaj portu SMTP do Internetu bez potrzeby; jeśli musisz – firewall/lista adresów. Limit połączeń jest
  globalny, nie per adres IP.
- Odwołanie dostępu = usunięcie tokenu (działa natychmiast, także dla SMTP).
- Treść e-maili i SMS-ów nie trafia do logów; numery w logach są maskowane.
