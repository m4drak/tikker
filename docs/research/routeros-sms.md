# RouterOS i SMS: weryfikacja założeń SPEC (T00)

Dokument zbiera wszystkie punkty oznaczone **[WERYFIKACJA]** w [SPEC](../SPEC.md) oraz zakres T00 z [PLAN](../PLAN.md).
Kolumna „Wynik pomiaru” jest pusta: uzupełnia ją człowiek z routerem według
[t00-checklist.md](t00-checklist.md), używając narzędzia [`scripts/probe`](../../scripts/probe/README.md).

- Research wykonano 2026-11-19 na publicznej dokumentacji i forum MikroTik (sieć działała). Strony pobierano
  narzędziem, które streszcza treść, więc cytaty i liczby warto przy ostatecznej decyzji sprawdzić w źródle.
- Statusy:
  - **dokumentacja**: twierdzenie wynika z oficjalnej dokumentacji MikroTik (podano wersję strony, jeśli znana);
  - **forum**: zgłoszenie użytkownika lub pracownika wsparcia, bez gwarancji dla bieżącej wersji;
  - **do pomiaru**: nie znaleziono wiarygodnego źródła, trzeba zmierzyć na sprzęcie;
  - **niezweryfikowane**: wiedza własna autora, bez źródła w tej sesji.
- Dokumentacja MikroTik nie podaje wersji RouterOS przy większości opisów SMS. Wiele stron SMS wygląda na
  nieaktualne (np. „UCS2 not supported”), dlatego nawet punkty „dokumentacja” dotyczące kodowania wymagają pomiaru
  na docelowej wersji.

## 1. Tabela punktów [WERYFIKACJA]

| ID | Punkt (źródło) | Wynik researchu | Status | Wynik pomiaru |
|---|---|---|---|---|
| W1 | Minimalna wersja RouterOS z poprawnym `/rest/tool/sms/*` (SPEC §3.1) | REST API istnieje od v7.1beta4 [D1]. Usługa `www` (HTTP) działa dopiero od v7.9 [D1], `www-ssl` wcześniej. `/rest/tool/sms` (GET ustawień) działa w v7.x [F1]. Poprawne `POST /rest/tool/sms/send` zgłaszano w 2022 i w listopadzie 2026 [F1], bez podanej wersji. Parametr `polling` w `/tool sms` dodano w v7.16 [D3]. Propozycja robocza dla SPEC: **≥ 7.16** (stabilna) do czasu pomiaru dolnej granicy. | REST: dokumentacja. SMS przez REST: forum. Dolna granica: do pomiaru | |
| W2 | Minimalne polityki grupy dla REST + SMS; czy `send` wymaga `test`/`policy` (SPEC §3.1) | Dokumentacja REST: grupa potrzebuje `rest-api` **i** `api`; `read` do odczytu, `write` do zmian, `test` dla `ping`, `sensitive` do haseł [D2]. Brak `api` daje 500 `not allowed (9)`, brak `rest-api` daje 401 [D2]. Forum: na v7.11 wystarczał `api` bez `rest-api` [F4]; na v7.8 użytkownik z samym `api,rest-api` dostawał „not enough permissions”, pomogło dodanie `local,reboot,read,write,policy,test` [F5]. Dokumentacja SMS nie podaje polityk dla `/tool sms send` ani `inbox` [D3,D4]. Hipoteza: potrzebne `api,rest-api,read,write,test` (w menu `/tool` komendy zwykle wymagają `test`). | dokumentacja (REST), reszta do pomiaru | |
| W3 | Parametry `POST /rest/tool/sms/send` (SPEC §3.2) | Argumenty `/tool sms send`: `port`, `channel`, `phone-number`, `smsc`, `message`, `type` (`class-0`, `class-1`, `ussd`), `status-report-request` (domyślnie `yes`) [D3,D4]. `phone-number`: cyfry, `*`, `#`, litery `abc`; wiodący `+` oznacza typ międzynarodowy [D3]. Działający JSON: `{"port":"lte1","phone-number":"+48…","message":"…"}`; GET z parametrami w URL daje 500 [F1]. Poprawka SPEC: tylko `POST` z `Content-Type: application/json` (bez niego 415 [D2]). Dochodzą opcjonalne `status-report-request`, `smsc`, `channel`. | dokumentacja (parametry), forum (JSON) | |
| W4 | Długie SMS (>160 znaków), wysyłka wieloczęściowa (SPEC §3.2, §4.6) | Dokumentacja: `message` ma limit 160 znaków, brak opisu wysyłki wieloczęściowej [D3]. Forum (grudzień 2025, modem FG621-EA): tryb tekstowy zwraca „Body too long” dla długich wiadomości [F3]. Wniosek roboczy: RouterOS **nie wysyła** SMS wieloczęściowych, strategia `reject`/`split`/`truncate` z SPEC §4.6 jest obowiązkowa, nie opcjonalna. | dokumentacja + forum; do pomiaru (161/320/500) | |
| W5 | Polskie znaki, UCS-2, `transliterate` (SPEC §4.6) | Dokumentacja: wysyłka tylko GSM-7, „UCS2 not supported”; znaki rozszerzone `^{}\[]~` zajmują 2 miejsca [D3]. Forum: na v7.1.3 „Hellö!” dotarło jako „Hell!”, wsparcie MikroTik: „only ASCII” [F2]; w 2025 tekst Unicode „stripped or corrupted” [F3]. Wniosek roboczy: wysyłka polskich znaków jako UCS-2 **nie jest wspierana**, `transliterate` powinno być domyślnie włączone dla `default_region=PL`, a znaki spoza GSM-7 trzeba wycinać lub zastępować. Pytanie otwarte: co robi RouterOS ze znakami GSM-7 spoza ASCII (np. `é`, `Ø`, `€`)? | dokumentacja + forum; do pomiaru na bieżącej wersji | |
| W6 | Kody błędów (SPEC §3.2: Transient/Permanent/Auth) | Format błędu REST: HTTP ≥ 400 i JSON `{"error":<kod>,"message":"…","detail":"…"}` [D2]. Znane: 401 (brak `rest-api` lub zły login), 500 + `not allowed (9)` / `not enough permissions (9)` (polityki), 415 (brak Content-Type), 400 `Failed to parse json` [D2]. Dla braku SIM, braku zasięgu, złego numeru, zajętego modemu: brak źródeł (500 bez szczegółów w [F1]). | do pomiaru | |
| W7 | Czas odpowiedzi wysyłki, limity czasu (SPEC §3.2, §4.5) | Dokumentacja: wysyłka ma limit 1 minuty, początkowa wymiana AT 2 s [D3]. REST przerywa każde żądanie po 60 s z `400 Session closed`; żądania jednego użytkownika kolejkują się za długą komendą [D2]. Port modemu jest zajęty na czas komendy i nie może być używany równolegle przez inny komponent [D3]. Wnioski: timeout klienta ≥ 65 s, jedna wysyłka naraz na kanał, osobny użytkownik RouterOS na instancję tikkera. Typowy czas odpowiedzi: brak danych. | dokumentacja (limity), do pomiaru (typowy czas) | |
| W8 | Format `GET /rest/tool/sms/inbox`, timestamp (SPEC §3.2) | Dokumentacja 7.25: pola tylko do odczytu `source`, `phone`, `type` (`class-0`), `timestamp`, `message`, `pdu` [D4]. `timestamp` to czas nadania przez operatora, nie czas routera [D3]. Format tekstowy znacznika, strefa i typ `type` dla zwykłych wiadomości: brak źródeł. **Nowość względem SPEC:** istnieje pole `pdu`, które może pozwolić poprawnie zdekodować UCS-2 i złożyć SMS wieloczęściowe po nagłówku UDH. | dokumentacja (lista pól), do pomiaru (formaty, zawartość `pdu`) | |
| W9 | Sklejanie wieloczęściowych SMS przychodzących (SPEC §4.3) | Dokumentacja: SMS łączone nie są wspierane, każda część to osobna wiadomość; 16-bitowy Unicode nie jest wspierany, dekodowanie jak GSM-7 [D3]. Forum: polskie znaki w skrzynce są zniekształcone [F3]. Wniosek roboczy: RouterOS zwraca części osobno i psuje UCS-2 w polu `message`. Sklejanie i dekodowanie trzeba robić w tikkerze, najpewniej z pola `pdu` (parsowanie PDU: UDH 0x00/0x08, DCS). Zależy od pomiaru, co zawiera `pdu`. | dokumentacja; do pomiaru (`pdu`) | |
| W10 | Usuwanie wpisu ze skrzynki: `DELETE …/inbox/{id}` czy `POST …/inbox/remove` (SPEC §3.2) | Dokumentacja REST: DELETE odpowiada komendzie `remove` rekordu, POST wywołuje dowolną komendę konsoli [D2]. Strona `inbox` nie wymienia komend [D4]. Oba warianty powinny działać, do potwierdzenia którą wersją i jakim `.id` (format `*1`). | do pomiaru | |
| W11 | Zachowanie przy pełnej skrzynce (PLAN T00) | Ustawienia `/tool sms`: `auto-erase` (od 6.44.6; usuwa najstarsze SMS-y, gdy brak miejsca; zastąpiło `keep-max-sms`), `sms-storage` (`modem` lub `sim`, tylko przy AT), `remove-sent-sms-after-send` [D3,D4]. Forum: przy modemach MBIM wysłane SMS-y zapełniają SIM i w końcu blokują wysyłkę [F6]. Zalecenie robocze dla `setup.rsc`: `receive-enabled=yes`, `auto-erase=yes`, `remove-sent-sms-after-send=yes`. | dokumentacja (ustawienia), forum (SIM), do pomiaru (zachowanie) | |
| W12 | Realna przepustowość wysyłki, domyślny `rate_per_min=6` (SPEC §4.5, §12 pkt 3) | Brak danych. Ograniczenia znane z dokumentacji: jedna wysyłka naraz na port, wysyłka do 60 s [D2,D3]. | do pomiaru | |
| W13 | Lista urządzeń LTE z `container` (arch., RAM, nośnik) (PLAN T00) | Patrz sekcja 3. Container wspiera tylko `arm`, `arm64`, `x86` [D5]. Z modeli LTE sprawdzonych na stronach produktów: Chateau LTE12 (ARM, 256 MB), Chateau 5G ax (ARM64, 1 GB), Audience LTE6 (ARM, 256 MB), hAP ax lite LTE6 (ARM, 256 MB), wAP ac LTE kit (ARM, 128 MB) [P1–P5]. Modele MIPS (LtAP LTE6, SXT LTE, KNOT) nie obsługują container [P6–P8]. | dokumentacja (architektury, produkty) | |
| W14 | Procedura `device-mode` (PLAN T00, T41) | `/system/device-mode/update container=yes`, potwierdzenie fizyczne (przycisk reset/mode albo zimny restart), domyślnie 5 min na potwierdzenie (10 s do 1 doby), maks. 3 próby do ponownego zasilenia, po potwierdzeniu restart [D6]. Container wyłączony w trybach advanced, home, basic, włączony w rose [D6]. Wymaga pakietu `container` [D5]. Od 7.17 device-mode jest fabrycznie na urządzeniach [D6]. | dokumentacja; do pomiaru na urządzeniu docelowym | |
| W15 | Minimalna wersja dla `container` i funkcji używanych w T41 (PLAN T41) | Przykład Pi-hole wymaga v7.4beta lub nowszej; `start-on-boot` od v7.6beta6; dane logowania do rejestru od v7.8; wiele adresów na `veth` od v7.11beta5 [D5]. Limity pamięci: `/container/config memory-high`, per kontener `memory-high`/`memory-max` [D5]. Zalecany zewnętrzny nośnik (USB/SATA/NVMe) dla `root-dir` i woluminów, ≥ 100 MB/s i 10 tys. IOPS [D5]. | dokumentacja | |
| W16 | Dostęp z kontenera do REST routera przez `veth` (SPEC §2.1, T41) | Dokumentacja podaje wzór `veth` + bridge + NAT [D5], ale nie podaje testu REST z kontenera. Założenie SPEC: `https://172.17.0.1`. Trzeba sprawdzić `ip/service address=`, certyfikat dla IP bramy i brak pętli przez firewall. | do pomiaru | |
| W17 | Dodatkowe ryzyka znalezione w researchu (bez numeru w SPEC) | REST buforuje poświadczenia ok. 10 minut: zmiana lub wyłączenie użytkownika nie działa od razu na aktywne sesje [F4]. `/tool sms` niedostępne na smips [D4]. Do odbioru wymagany jest `secret` i `receive-enabled=yes` [D3]; wartość tego, jak `secret` wpływa na wysyłkę przez REST, jest niejasna (post w [F1] wspomina parametr `password`). | do pomiaru | |

## 2. Wpływ na SPEC (propozycje zmian po T00)

Zmiany do naniesienia po zebraniu pomiarów. Te z oznaczeniem (pewne) wynikają wprost z dokumentacji.

1. §3.2 Wysyłka: wyłącznie `POST` z `Content-Type: application/json` (pewne). Timeout HTTP klienta 65–70 s (pewne).
2. §4.6 Kodowanie: RouterOS najpewniej wysyła tylko GSM-7 i tylko do 160 znaków (dokumentacja + forum).
   - `max_segments` dla konektora `routeros-rest` ma domyślnie wartość 1.
   - `transliterate` ma być domyślnie włączone dla regionu PL.
   - Strategia dla tekstu > 160 znaków (`reject`/`split`/`truncate`) jest wymagana, nie warunkowa.
   - Licznik segmentów w UI nadal jest użyteczny jako informacja, ale nie jako obietnica wysyłki.
3. §4.3 Odbiór: dodać dekodowanie z pola `pdu` (UCS-2, UDH i składanie części), jeśli pomiar potwierdzi jego zawartość; inaczej UCS-2 przychodzące jest nieczytelne i wymaga oznaczenia w UI.
4. §3.1 Wymagania: ustawić `receive-enabled=yes`, `auto-erase=yes`, `remove-sent-sms-after-send=yes`; polityki grupy wg W2; wersja ≥ 7.16 (wstępnie).
5. §3.2 Błędy: dopisać odwzorowanie 401, 500 `not allowed`, 500 `not enough permissions`, 400 `Session closed`, po pomiarze także braku SIM i zasięgu.
6. §4.5 Limity: jedna wysyłka naraz na port (potwierdzone), osobny użytkownik RouterOS na instancję tikkera.
7. §2.1 Tryby wdrożenia: tabela modeli (sekcja 3) i uwaga o nośniku zewnętrznym dla `container`.
8. Opcjonalny backlog: konektor oparty o `/interface/lte/at-chat` (PDU, UCS-2, UDH) jako obejście ograniczeń `/tool sms send`. Forum zgłasza „device inactive” przy próbie PDU [F3], więc to tylko eksperyment z listy kontrolnej, nie plan.

## 3. Popularne urządzenia LTE MikroTik

Dane z oficjalnych stron produktów (pobrane 2026-11-19), chyba że zaznaczono inaczej. „Container” = architektura jest na liście wspieranych (`arm`, `arm64`, `x86`) [D5], co **nie** oznacza, że producent testował container na tym modelu. Pamięć flash jest mała, więc dla `root-dir` potrzebny jest nośnik zewnętrzny albo użycie NAND z zastrzeżeniami (dokumentacja odradza pamięć wbudowaną [D5]).

| Model | CPU / architektura | RAM | Flash | USB | Modem | Container? | Uwagi |
|---|---|---|---|---|---|---|---|
| Chateau LTE12 | IPQ-4019, ARM 32-bit [P1] | 256 MB | 16 MB | 1× USB-A | LTE Cat 12 | architektura tak | USB pozwala dodać pendrive/SSD |
| Chateau 5G ax | IPQ-6010, ARM64 [P2] | 1 GB | 128 MB NAND | 1× USB-A | 5G NSA/SA, Cat 20 | tak, najlepszy kandydat | |
| Audience LTE6 kit | IPQ-4019, ARM 32-bit [P3] | 256 MB | 128 MB NAND | brak w opisie | LTE Cat 6 | architektura tak | brak USB: nośnik tylko wbudowany |
| hAP ax lite LTE6 | IPQ-5010, ARM 32-bit [P4] | 256 MB | 128 MB NAND | tylko zasilanie USB-C | LTE Cat 6 | architektura tak | brak portu danych USB |
| wAP ac LTE kit | IPQ-4018, ARM 32-bit [P5] | 128 MB | 16 MB | brak w opisie | R11e-LTE (wg kodu produktu, niepotwierdzone) | architektura tak, ale mało RAM i flash | praktycznie tryb serwerowy |
| LtAP LTE6 kit | MT7621A, MMIPS [P6] | 128 MB | 16 MB | 1× USB-A | LTE Cat 6 w miniPCIe | **nie** (MMIPS) | tryb serwerowy |
| LtAP (strona `ltap`) | MT7621A, MMIPS [P7] | 128 MB | 16 MB | 1× USB-A | slot(y) miniPCIe, bez modemu w zestawie | **nie** | |
| SXT LTE kit | QCA9531, MIPSBE [P8] | 64 MB | 16 MB | brak | LTE Cat 4 | **nie** | tryb serwerowy |
| KNOT | QCA9531, MIPSBE [P9] | 64 MB | 128 MB NAND | brak | NB-IoT/Cat-M | **nie**; NB-IoT/Cat-M raczej bez SMS (niezweryfikowane) | |
| Chateau LTE18 ax, Chateau LTE6, wAP LTE kit, LHG LTE, R11e-LTE6 w RB-ach | niesprawdzone w tej sesji | | | | | niezweryfikowane | sprawdzić na mikrotik.com przed rekomendacją |

Uwagi:
- Tryb serwerowy (tikker poza routerem, łączący się przez LAN/VPN) działa z **każdym** modelem, także MIPS. Container to wygoda, nie wymóg.
- hEX Refresh (EN7562CT) obsługuje tylko obrazy arm32v5 [D5]. To nie jest urządzenie LTE, ale jeśli ktoś użyje go jako hosta kontenera z zewnętrznym modemem, trzeba zbudować `GOARM=5`.
- `/tool sms` nie jest dostępne na smips [D4]. Czy modele MIPSBE/MMIPS mają inne ograniczenia SMS: do pomiaru, jeśli ktoś takie ma.
- RAM 128 MB razem z RouterOS to mało na container (cel SPEC: RSS ≤ 40 MB). Dla 256 MB i więcej powinno wystarczyć, do potwierdzenia pomiarem.

## 4. Źródła

Dokumentacja oficjalna (strony „frozen” help.mikrotik.com przekierowują do manual.mikrotik.com):

- [D1] REST API, help.mikrotik.com: https://help.mikrotik.com/docs/spaces/ROS/pages/47579162/REST+API (wersja 7.1beta4, usługa `www` od 7.9, timeout 60 s, mapowanie metod HTTP).
- [D2] REST API, manual 7.25: http://manual.mikrotik.com/docs/7.25/developer-guides/rest-api/ (polityki `rest-api` i `api`, format błędów, Content-Type, kolejkowanie żądań).
- [D3] SMS, manual 7.25: http://manual.mikrotik.com/docs/7.25/mobile-networking/sms/ (parametry, GSM-7, 160 znaków, brak SMS łączonych, ustawienia, `polling` od 7.16, `auto-erase`).
- [D4] CLI `/tool sms`, manual 7.25: http://manual.mikrotik.com/docs/7.25/cli-reference/tool/sms/ oraz `…/sms/send/` i `…/sms/inbox/` (listy argumentów i pól, bez opisów; warunek `!smips`).
- [D5] Container: https://help.mikrotik.com/docs/spaces/ROS/pages/84901929/Container
- [D6] Device-mode: https://help.mikrotik.com/docs/spaces/ROS/pages/93749258/Device-mode

Forum MikroTik (zgłoszenia użytkowników, bez gwarancji):

- [F1] REST API - Send SMS: https://forum.mikrotik.com/t/rest-api-send-sms/156178 (GET daje 500, poprawny jest JSON POST).
- [F2] Sending SMS with GSM7 characters: https://forum.mikrotik.com/t/sending-sms-with-gsm7-characters/156020 (RBSXTR, v7.1.3, znaki spoza ASCII gubione).
- [F3] SMS Unicode/UCS-2 i długie SMS w trybie PDU: https://forum.mikrotik.com/t/how-to-send-receive-sms-with-unicode-utf-8-ucs-2-and-long-sms-160-chars-on-mikrotik-pdu-mode-gives-device-inactive/266951 (FG621-EA, grudzień 2025, wątek bez odpowiedzi).
- [F4] rest-api policy: https://forum.mikrotik.com/t/rest-api-policy/169084 (v7.11, buforowanie poświadczeń ok. 10 min).
- [F5] Dedicated user can't access API: https://forum.mikrotik.com/t/dedicated-user-cant-access-api/166242 (v7.8; informacja z fragmentu wyników wyszukiwania, strony nie pobrano w całości).
- [F6] How to send SMS via AT-CHAT: https://forum.mikrotik.com/t/how-to-send-sms-via-at-chat/183872 (SMS-y zapełniają SIM w MBIM; informacja z fragmentu wyników wyszukiwania).

Strony produktów mikrotik.com:

- [P1] https://mikrotik.com/product/chateau_lte12
- [P2] https://mikrotik.com/product/chateau_5g_ax
- [P3] https://mikrotik.com/product/audience_lte6_kit
- [P4] https://mikrotik.com/product/hap_ax_lite_lte6
- [P5] https://mikrotik.com/product/wap_ac_lte_kit
- [P6] https://mikrotik.com/product/ltap_lte6_kit
- [P7] https://mikrotik.com/product/ltap
- [P8] https://mikrotik.com/product/sxt_lte_kit
- [P9] https://mikrotik.com/product/knot

Nie sprawdzono (poza zasięgiem tej sesji): changelogi RouterOS pod kątem poprawek SMS (jedyny trafiony wpis dotyczył
usunięcia SMS na smips w 7.15beta), sam `help.mikrotik.com` dla strony SMS (zwróciła inną stronę), wątki forum poza wymienionymi.
