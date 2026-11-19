# T00: procedura pomiarów na routerze (dla człowieka)

Cel: uzupełnić kolumnę „Wynik pomiaru” w [routeros-sms.md](routeros-sms.md) i dostarczyć zanonimizowane zrzuty
żądań i odpowiedzi (golden files dla T12). Szacowany czas: 2–3 godziny, koszt: kilkadziesiąt SMS-ów.

Uwagi ogólne:
- Wysyłaj tylko na własny numer testowy. Zestaw `send-suite` wysyła 14 wiadomości (część wieloczęściowa lub odrzucona), a `throughput` tyle, ile podasz w `-count`.
- Narzędzie `probe` wymaga `--yes` przy każdym trybie, który wysyła SMS lub usuwa wpis ze skrzynki.
- Wszystkie wyniki trafiają do `probe-results/<data>-<tryb>/` już zanonimizowane. Mimo to przejrzyj je przed commitem (sekcja 8).
- Zapisuj obserwacje, których narzędzie nie widzi (co faktycznie przyszło na telefon), w tabeli z sekcji 9.

## 0. Przygotowanie

1. Zbuduj narzędzie: `go build -o probe ./scripts/probe` (Go ≥ 1.24, nic więcej nie trzeba).
2. Router: zapisz wersję i model.
   ```
   /system resource print
   /system routerboard print
   /system package print
   /interface lte print
   /interface lte info lte1 once
   /port print
   ```
   Zanotuj: wersja RouterOS i kanał (stable/long-term), model, architektura, RAM, modem (model, firmware), operator.
3. Włącz REST: `/ip service set www-ssl disabled=no address=<IP komputera>/32` (lub `www` tylko w sieci zaufanej). Certyfikat self-signed: odcisk z `/certificate print detail` (pole `fingerprint`, SHA-256) albo `openssl s_client -connect <ip>:443 </dev/null | openssl x509 -noout -fingerprint -sha256`.
4. Skonfiguruj SMS:
   ```
   /tool sms set port=lte1 receive-enabled=yes secret=<dowolne-testowe> auto-erase=yes remove-sent-sms-after-send=yes
   /tool sms print
   ```
   Sprawdź, że `status=running`. Zanotuj, czy polecenia przyjmują `auto-erase` i `remove-sent-sms-after-send` w tej wersji.
5. Ręczny test z konsoli (kontrola, że SMS w ogóle działa): `/tool sms send lte1 "+48XXXXXXXXX" message="test konsola"`. Telefon musi dostać wiadomość.
6. Zmienne środowiskowe na komputerze pomiarowym (hasło tylko w env, nie we flagach):
   ```
   export PROBE_URL=https://192.168.88.1
   export PROBE_USER=probe
   export PROBE_PASSWORD='…'
   export PROBE_TO='+48XXXXXXXXX'
   export PROBE_FINGERPRINT=<odcisk sha256>   # albo używaj flagi -insecure
   ```

## 1. Minimalne polityki grupy (W2)

Powtórz dla każdego wiersza: utwórz grupę z zestawem polityk, użytkownika `probe` w tej grupie, uruchom `probe policies`.

```
/user group add name=probe-g policy=<ZESTAW>
/user add name=probe group=probe-g password=<hasło>
```

Zestawy do wypróbowania (od najuboższego; `!x` nie jest potrzebne, wystarczy brak polityki):

| # | policy= | Uruchomienie |
|---|---|---|
| 1 | `api,rest-api` | `./probe -policies "api,rest-api" -label p1 policies` |
| 2 | `api,rest-api,read` | `… -label p2` |
| 3 | `api,rest-api,read,test` | `… -label p3` |
| 4 | `api,rest-api,read,write` | `… -label p4` |
| 5 | `api,rest-api,read,write,test` | `… -label p5` (dodaj `-to "$PROBE_TO" -yes`, by sprawdzić też `send`) |
| 6 | `rest-api,read,write,test` (bez `api`) | `… -label p6` |
| 7 | `api,read,write,test` (bez `rest-api`) | `… -label p7` |
| 8 | dodatkowo `policy`, `local`, `reboot` do najuboższego zestawu, który działa | tylko jeśli wszystko wyżej zawodzi (por. forum, v7.8) |

Po każdej zmianie polityk **odczekaj ok. 10 minut albo zrestartuj usługę `www-ssl`**: REST buforuje poświadczenia ok. 10 minut [F4 w routeros-sms.md]. Wyniki: `matrix` w `summary.json` (status HTTP i treść błędu dla `GET /tool/sms`, `GET inbox`, `POST send` itd.).
Zapisz najmniejszy działający zestaw oraz które wywołania wymagają `test`, `write` albo `read`.

Dalsze eksperymenty używają użytkownika z najmniejszym działającym zestawem (jeśli któryś nie działa, użyj pełnego `api,rest-api,read,write,test`).

## 2. Informacje o urządzeniu

```
./probe -label baseline info
```
Zapisuje wersję, architekturę, RAM, device-mode, pakiety, `ip/service`, grupy, porty, stan LTE i ustawienia SMS. Jeśli któreś wywołanie zwróci błąd (np. brak `/rest/system/device-mode`), zapisz, które.

## 3. Eksperymenty manualne SMS

Przed każdym: `./probe -label <nazwa> inbox`, po nim dla wysyłek: przejrzyj `summary.json`, a na telefonie sprawdź, co dotarło.

### 3.1 Wysyłka podstawowa i kodowanie (W3, W4, W5)

```
./probe -list-cases              # podgląd przypadków, oczekiwane kodowanie i segmenty
./probe -yes -gap 15s -label suite send-suite
```
Przypadki: ASCII 160/161/320/500 znaków, polskie znaki 70/71, krótkie polskie, znaki GSM spoza ASCII, znaki rozszerzone GSM (`^{}\[]~|€`) w tym 80/81 sztuk, emoji, znak nowej linii z cudzysłowami. Dla każdego zapisz **na telefonie** (tabela w sekcji 9):
- czy dotarło, ile części (jedna wiadomość czy kilka), czy sklejona w całość;
- czy polskie znaki są poprawne, zamienione na ASCII, wycięte czy zniekształcone;
- co z emoji i znakami `€`, `{`, `[`;
- ile znaków dotarło przy 161/320/500 (obcięcie?), jaki błąd zwrócił router (`response` w `summary.json`).

Oczekiwanie z researchu: > 160 znaków i znaki spoza GSM-7 nie zadziałają. To właśnie sprawdzamy.

Pojedyncza wiadomość o dowolnej treści: `./probe -yes -message 'treść' send`.

### 3.2 Odbiór: skrzynka, timestamp, `pdu` (W8, W9)

1. Z telefonu wyślij na numer SIM routera: (a) krótkie `test ascii`, (b) `Zażółć gęślą jaźń` (UCS-2), (c) tekst ASCII 300 znaków (wieloczęściowy), (d) tekst polski 150 znaków (wieloczęściowy UCS-2), (e) wiadomość z emoji.
2. Odczekaj do 1 minuty (RouterOS bez `polling` odbiera przez powiadomienia modemu; spróbuj też `/tool sms set polling=yes`, jeśli wiadomości nie przychodzą, i odnotuj różnicę).
3. Pobierz skrzynkę dwa razy:
   ```
   ./probe -label inbox-redacted inbox
   ./probe -keep-inbox-bodies -label inbox-bodies inbox
   ```
   Drugi wariant zachowuje treść `message` i `pdu`, ale numer nadawcy nadal jest maskowany. **Nie commituj** katalogu z `inbox-bodies`, dopóki nie przejrzysz go ręcznie (treść cudzych wiadomości, tokeny, kody).
4. Zanotuj: nazwy i format pól (`.id`, `source`, `phone`, `timestamp`, `type`, `message`, `pdu`), format i strefę `timestamp`, czy części wieloczęściowe są osobnymi wpisami, czy `message` dla UCS-2 jest zniekształcone, czy `pdu` jest obecne i jak wygląda (długość, prefiks SMSC; z niego da się odczytać DCS i UDH). Skopiuj do notatek (lokalnie, nie do repozytorium) kilka `pdu`, by T16 mogło napisać dekoder.
5. Z konsoli porównaj: `/tool sms inbox print detail` (pola widoczne w CLI a w REST).

### 3.3 Usuwanie wpisu (W10)

```
./probe -yes -id '*1' -label delete inbox-delete
```
Sprawdź w `summary.json`, czy zadziałał `DELETE /rest/tool/sms/inbox/*1` (`delete_rest_ok`), a jeśli nie, `POST …/inbox/remove` (`remove_post_ok`), i czy kolejne `GET` nie zawiera wpisu. Zanotuj format `.id` i ewentualny kod błędu.

### 3.4 Pełna skrzynka (W11)

1. Ustaw `auto-erase=no`: `/tool sms set auto-erase=no`.
2. Wyślij na SIM routera z telefonu kolejno wiadomości, aż przestaną być odbierane (liczba zależy od modemu i `sms-storage`; zapisz limit). Alternatywnie sprawdź `/tool sms print` po kilkunastu.
3. Zanotuj: po ilu wiadomościach odbiór się zatrzymał, czy REST/konsola pokazuje jakikolwiek błąd, czy wysyłka nadal działa.
4. Włącz `auto-erase=yes`, wyślij jeszcze kilka i sprawdź, czy najstarsze znikają. Wyczyść skrzynkę `inbox-delete` lub `/tool sms inbox remove [find]`.
5. Dla modemów MBIM sprawdź, czy po serii wysyłek (np. `throughput`) wysłane SMS-y zapełniają SIM (`/interface lte at-chat lte1 input="AT+CPMS?"`) i czy `remove-sent-sms-after-send=yes` temu zapobiega.

### 3.5 Błędy: SIM, zasięg, numer (W6)

Dla każdego scenariusza uruchom `./probe -yes -label <nazwa> -message 'blad test' send` i `./probe -label <nazwa>-lte info`. Zapisz: kod HTTP, `message`/`detail` błędu, czas odpowiedzi (`duration_ms`), stan `lte monitor` (czy status zmienił się na `no-sim`, `searching` itp.).

| Scenariusz | Jak wywołać | Obserwacja |
|---|---|---|
| Brak SIM | wyłącz router, wyjmij SIM, włącz | |
| SIM z PIN-em, brak PIN w konfiguracji | włóż SIM z PIN (bez `sim-pin`) | |
| Brak zasięgu | ekranowanie modemu (folia aluminiowa/klatka) albo wyjęcie anten | |
| Zły numer | `-to 12` ; `-to '+48'` ; `-to 'abc'` ; numer nieistniejący `+4800000000000` | |
| Numer krótki | numer krótki Twojego operatora (np. infolinia); **nie** numery alarmowe | |
| Pusta wiadomość | `-message ''` | |
| Modem zajęty | `-count 3 -parallel 3 throughput` (równoległe żądania, por. 4) | |
| Złe hasło / zły użytkownik | `PROBE_PASSWORD=zle ./probe info` | |
| Wyłączony `www-ssl` / zła `address=` | zmień ustawienia usługi | |

Numerów alarmowych (112, 997, 998, 999) nie używaj w żadnym teście.

## 4. Przepustowość (W12)

```
./probe -yes -count 10 -label tp10 throughput
./probe -yes -count 30 -label tp30 throughput
./probe -yes -count 10 -parallel 3 -label tp10p3 throughput
```
`summary.json` zawiera `ok_per_minute`, `latency_ms` (min/median/max/avg) i `status_counts`. Zanotuj: stabilność (czy opóźnienia rosną), błędy przy równoległości (zajęty port), zachowanie operatora (limity, blokada). Odpowiedz: jaka bezpieczna wartość `rate_per_min`? Po serii sprawdź skrzynkę i `/tool sms print`.

## 5. Test kontenera (W14, W15, W16, T41)

Wymaga routera ARM/ARM64/x86 z pakietem `container` (por. tabela w [routeros-sms.md](routeros-sms.md)). Kopia zapasowa konfiguracji przed zmianami (`/system backup save`, `/export file=przed-kontenerem`).

1. Wersja i pakiet: `/system package print` (musi być `container`; jeśli brak, doinstaluj pakiet dodatkowy zgodny z wersją i architekturą, restart).
2. Device-mode (W14):
   ```
   /system device-mode print
   /system device-mode update container=yes
   ```
   Zanotuj dokładny komunikat i czas na potwierdzenie. W ciągu 5 minut (domyślnie) naciśnij fizycznie przycisk reset/mode albo odłącz i podłącz zasilanie (x86: zimny restart). Router się zrestartuje. Sprawdź `/system device-mode print` (`container: yes`). Zapisz: wersję RouterOS, czy wymagany był przycisk czy zasilanie, ile prób i ile czasu minęło.
3. Nośnik: podłącz USB (jeśli jest port) i sformatuj: `/disk print`, `/disk format-drive usb1 file-system=ext4` (zanotuj prędkość nośnika). Bez USB zanotuj ewentualne ostrzeżenia przy użyciu pamięci wbudowanej.
4. Sieć kontenera:
   ```
   /interface veth add name=veth1 address=172.17.0.2/24 gateway=172.17.0.1
   /interface bridge add name=containers
   /ip address add address=172.17.0.1/24 interface=containers
   /interface bridge port add bridge=containers interface=veth1
   /ip firewall nat add chain=srcnat action=masquerade src-address=172.17.0.0/24
   ```
5. Prosty obraz testowy (architektura zgodna z routerem): `/container config set registry-url=https://registry-1.docker.io tmpdir=usb1/pull ram-high=64M` (nazwy parametrów mogą się różnić między wersjami; zapisz, jak wyglądają u Ciebie). Następnie:
   ```
   /container add remote-image=library/alpine:latest interface=veth1 root-dir=usb1/alpine cmd="sleep 3600" logging=yes start-on-boot=no
   /container print
   /container start 0
   ```
   Zapisz: czas pobrania i rozpakowania, zużycie RAM (`/system resource print` przed i po), błędy (np. brak architektury w obrazie, brak miejsca w `tmpdir`).
6. Dostęp do REST przez veth: w kontenerze `alpine` uruchom powłokę (`/container shell 0`), zainstaluj `curl` (`apk add curl`) i sprawdź:
   ```
   curl -sk -u probe:<hasło> https://172.17.0.1/rest/system/resource
   curl -sk -u probe:<hasło> https://172.17.0.1/rest/tool/sms
   ```
   Warunki: `/ip service` `www-ssl` musi nasłuchiwać na adresie bramy veth (`address=` obejmuje `172.17.0.0/24`), firewall nie blokuje `input` z bridge `containers`. Zanotuj: czy działa przez HTTP i HTTPS, czy certyfikat jest akceptowany z `-k`, czas odpowiedzi.
7. Pomiar tikkera w kontenerze (po T40/T41, jeśli dostępny obraz): zużycie RAM (`/container print` i `/system resource print`), zapisy na nośnik.
8. Sprzątanie: `/container stop 0`, `/container remove 0`, usuń `veth`, bridge i NAT, jeśli nie są dalej potrzebne. Device-mode możesz zostawić, ale zapisz, że `container=yes` jest włączone (wzmocnij zabezpieczenia routera: dokumentacja ostrzega, że container można potem zarządzać zdalnie).

## 6. Opcjonalnie: eksperyment PDU przez `at-chat`

Tylko jeśli sekcja 3.1 potwierdzi brak UCS-2 i chcesz sprawdzić obejście (zgłoszenie z forum: „device inactive”). **Ryzyko:** przejmujesz port modemu, więc RouterOS i AT mogą sobie przeszkadzać; wykonuj przy braku ruchu SMS.

```
/interface lte at-chat lte1 input="AT+CMGF=0"
/interface lte at-chat lte1 input="AT+CMGS=<długość>"
```
a następnie PDU zakończone Ctrl+Z (`\1A`). Zanotuj dokładne odpowiedzi modemu. Jeśli działa, dodaj do raportu przykład PDU UCS-2 i UDH, żeby ocenić konektor `at-chat` w backlogu.

## 7. Wyniki do raportu

1. Uzupełnij kolumnę „Wynik pomiaru” w [routeros-sms.md](routeros-sms.md) i status (`potwierdzone`/`obalone` + nazwa pliku z dowodem).
2. Skopiuj zanonimizowane katalogi wyników do `docs/research/results/<model>-<wersja>/` (tylko wybrane: `info`, `policies`, `send-suite`, `throughput`, `inbox` z zasłoniętą treścią).
3. Wypisz zmiany do SPEC (sekcja 2 w routeros-sms.md) i otwórz zadanie na ich naniesienie.

## 8. Anonimizacja przed commitem

`probe` maskuje automatycznie: numery telefonów (w tym wskazany `-to`), IMEI, IMSI, ICCID, adresy IP (IPv4 i IPv6), adresy MAC, numery seryjne, nazwę hosta routera, hasło, nagłówek `Authorization` i ciasteczka, kody komórek (cell-id, TAC), pole `identity`. Treść `message` i `pdu` ze skrzynki jest zastępowana opisem długości, chyba że użyto `-keep-inbox-bodies`.

Ręcznie, zawsze:
1. `grep -rEn '\+?[0-9]{9,}' probe-results/` (nie powinno nic zwrócić; długie liczby to potencjalne numery; wyjątek: epoch w nazwach katalogów).
2. `grep -rEn '([0-9]{1,3}\.){3}[0-9]{1,3}' probe-results/` (adresy IP powinny być `<IP-n>`).
3. Szukaj nazw własnych: SSID, nazwa operatora w połączeniu z lokalizacją, `identity`, `comment`, nazwy użytkowników, nazwy hostów (`grep -rEin 'ssid|comment|identity|dns-name|hostname' probe-results/`).
4. Odpowiedzi `/system/package`, `/ip/service`, `/user/group`, `/port` mogą zawierać nazwy użytkowników, `comment` i adresy w polach `address`; po zamaskowaniu IP sprawdź je wzrokiem.
5. Treść wiadomości z `-keep-inbox-bodies`: usuń kody weryfikacyjne i dane osobowe cudzych wiadomości (lepiej użyć własnego telefonu).
6. Numer seryjny i ID modemu pojawiają się w `/system/routerboard` i `/interface/lte/info`: sprawdź, że są `<SERIAL-n>`, `<IMEI-n>`.
7. Certyfikaty, klucze i hasła z `/certificate` lub eksportów konfiguracji: **nie dołączać**; narzędzie ich nie zbiera.
8. Na koniec: `git diff --stat` i przegląd wszystkich dodawanych plików. Nie commituj surowych zrzutów z konsoli ani zrzutów ekranu bez przejrzenia (IP, SSID, identity widoczne w Winbox).

Po testach usuń użytkownika testowego, wyłącz `www`/`www-ssl` lub ogranicz `address=`, zmień hasło `secret` SMS.

## 9. Arkusz obserwacji ręcznych (do wklejenia w raport)

| Przypadek | Router: HTTP / błąd | Telefon: dotarło? | Części | Znaki poprawne? | Uwagi |
|---|---|---|---|---|---|
| ascii-160 | | | | | |
| ascii-161 | | | | | |
| ascii-320 | | | | | |
| ascii-500 | | | | | |
| pl-short | | | | | |
| pl-70 | | | | | |
| pl-71 | | | | | |
| gsm7-accents | | | | | |
| gsm-ext-short | | | | | |
| gsm-ext-80 | | | | | |
| gsm-ext-81 | | | | | |
| emoji | | | | | |
| newline | | | | | |
