# Wdrożenie na MikroTiku (kontener)

Przewodnik uruchamia tikkera jako kontener na tym samym routerze, który ma modem LTE/5G i kartę SIM
(SPEC §2.1). Jeśli router nie obsługuje kontenerów albo wolisz osobny serwer, zobacz
[deploy-server.md](deploy-server.md). Skrypt konfiguracyjny: [`deploy/routeros/setup.rsc`](../deploy/routeros/setup.rsc).

> **Status weryfikacji.** Pomiary na sprzęcie (zadanie T00) są odłożone. Ten przewodnik i skrypt powstały
> na podstawie dokumentacji MikroTik i [docs/research/routeros-sms.md](research/routeros-sms.md).
> Wszystko, co oznaczono **[niezweryfikowane na sprzęcie]**, trzeba potwierdzić na prawdziwym routerze (T51)
> i poprawić tu oraz w skrypcie. Do tego czasu traktuj instrukcję jako szkic do przejrzenia, nie gotowy przepis.

## 1. Wymagania

| Wymaganie | Wartość | Uwagi |
|---|---|---|
| RouterOS | **≥ 7.16** | robocze minimum z research (W1), m.in. `polling` w `/tool sms`; `start-on-boot` kontenera od 7.6, wiele adresów na `veth` od 7.11 |
| Pakiet | `container` | osobny pakiet, doinstalowywany ręcznie (sekcja 3) |
| Architektura | `arm`, `arm64` lub `x86` | MIPS (MIPSBE, MMIPS, SMIPS) **nie** obsługuje kontenerów |
| RAM | ≥ 256 MB | tikker potrzebuje ok. 40 MB RSS (cel SPEC §2.2), reszta to RouterOS; 128 MB to za mało |
| Nośnik | USB lub microSD (zalecany) | dokumentacja MikroTik odradza pamięć wbudowaną dla `root-dir` (zużycie flash); zob. sekcja 9 |
| Modem | LTE/5G widoczny w `/interface lte` | `/tool sms` niedostępne na smips |

### Przykładowe urządzenia

Dane z [docs/research/routeros-sms.md](research/routeros-sms.md) §3 (strony produktów, 2026-11-19).
„Architektura OK” znaczy tylko, że procesor jest na liście obsługiwanych przez `container`: MikroTik nie
gwarantuje kontenerów na konkretnym modelu, a **żadnego z nich nie uruchamiano z tikkerem** (T51).

| Model | Architektura / RAM | Nośnik zewnętrzny | Ocena |
|---|---|---|---|
| Chateau 5G ax | ARM64, 1 GB | USB-A | najlepszy kandydat |
| Chateau LTE12 | ARM, 256 MB | USB-A | architektura OK, RAM na styk |
| Audience LTE6 kit | ARM, 256 MB | brak USB | tylko wbudowana pamięć 128 MB NAND, niezalecane |
| hAP ax lite LTE6 | ARM, 256 MB | USB-C tylko zasilanie | j.w. |
| wAP ac LTE kit | ARM, 128 MB | brak | za mało RAM, użyj trybu serwerowego |
| LtAP LTE6, SXT LTE, KNOT | MMIPS / MIPSBE | – | **brak** obsługi kontenerów, tryb serwerowy |

Modeli spoza tabeli nie sprawdzano; zajrzyj na stronę produktu na mikrotik.com. Tryb serwerowy działa z każdym.

## 2. Przygotowanie routera

1. Zaktualizuj RouterOS do ≥ 7.16 (`/system package update`), upewnij się, że modem działa:
   `/interface lte print`, `/interface lte info lte1 once`.
2. Sprawdź architekturę i pamięć: `/system resource print` (`architecture-name`, `total-memory`).
3. Zrób kopię konfiguracji: `/system backup save` i `/export file=przed-tikker`. Skrypt niczego nie usuwa,
   ale zmienia usługi, firewall i użytkowników.
4. Przygotuj nośnik: włóż pendrive/microSD, sformatuj go na ext4 z poziomu RouterOS
   (`/disk print`, `/disk format-drive usb1 file-system=ext4`; nazwy dysków różnią się między modelami
   [niezweryfikowane na sprzęcie]). Skrypt zakłada katalogi pod `usb1/tikker`.

## 3. device-mode i pakiet container

Kontenery są domyślnie wyłączone w `device-mode` jako zabezpieczenie przed zdalnym włączeniem
([research W14]; niezweryfikowane na sprzęcie).

1. `/system/device-mode/update container=yes`.
2. W ciągu 5 minut (domyślnie; konfigurowalne od 10 s do doby) **potwierdź fizycznie**: naciśnij przycisk
   reset/mode na routerze albo wyłącz i włącz zasilanie, zgodnie z komunikatem w konsoli.
   Tego kroku nie da się zrobić zdalnie, nie przewidziano obejścia.
3. Router uruchomi się ponownie. Sprawdź: `/system/device-mode/print` (`container: yes`).
   Przy nieudanej próbie są maks. 3 próby do ponownego zasilenia routera.
4. Pobierz pakiet `container` dla swojej architektury i wersji RouterOS (mikrotik.com/download, „Extra packages”,
   plik `container-<wersja>-<arch>.npk`), wgraj do Files i zrestartuj router (`/system reboot`).
   Sprawdź: `/system package print` (pakiet `container` bez statusu „disabled”).

Włączenie `container` osłabia ochronę przed przejęciem routera: nie rób tego na urządzeniu z panelem
administracyjnym wystawionym do Internetu.

## 4. Uruchomienie skryptu `setup.rsc`

1. Skopiuj [`deploy/routeros/setup.rsc`](../deploy/routeros/setup.rsc) na komputer i **edytuj zmienne na górze**
   (nie commituj wersji z hasłami):

   | Zmienna | Znaczenie |
   |---|---|
   | `mode` | `"container"` |
   | `ltePort` | port LTE, zwykle `lte1` |
   | `apiUser`, `apiPassword` | konto RouterOS dla tikkera; hasło min. 12 znaków, długie i losowe (skrypt przerywa działanie przy `CHANGE-ME`) |
   | `routerAddr`, `vethGateway`, `vethAddress`, `vethNetwork` | adresacja `veth`; domyślnie brama `172.17.0.1`, kontener `172.17.0.2/24`; zmień, jeśli koliduje z istniejącą siecią |
   | `image` | obraz kontenera (nazwa `ghcr.io/m4drak/tikker` jest **placeholderem** do pierwszego wydania; patrz sekcja 5) |
   | `mediaPath` | np. `usb1/tikker`; pod nim powstaną `root`, `data`, `pull` |
   | `adminPassword` | hasło pierwszego administratora panelu (min. 12 znaków) |
   | `baseUrl` | publiczny adres panelu, jeśli go znasz |
   | `exposePanel` | `"no"` (domyślnie) lub `"yes"`: dst-nat portu 8080 z listy `panelLanList` |

2. Wgraj plik do routera (WinBox: Files, `scp`, lub `/tool fetch`) i uruchom: `/import file-name=setup.rsc`.
3. Skrypt (idempotentny, można go uruchomić ponownie):
   - tworzy grupę `tikker` (polityki `api`, `rest-api`, `read`, `write`) i użytkownika ograniczonego adresem do podsieci `veth`;
   - ustawia `/tool sms set port=… receive-enabled=yes auto-erase=yes` (oraz `remove-sent-sms-after-send=yes`, jeśli wersja go zna);
   - generuje certyfikat self-signed `tikker-api` (SAN = `routerAddr`), włącza `www-ssl` z `address=` tylko dla podsieci `veth`, wyłącza `www`;
   - dodaje regułę firewall `input` zezwalającą podsieci `veth` na port 443 (na początku łańcucha);
   - tworzy bridge, `veth`, adres, mount `/data`, zmienne środowiskowe, kontener (`start-on-boot=yes`, `logging=yes`) i go uruchamia;
   - wypisuje **odcisk SHA-256 certyfikatu**, który wpiszesz w tikkerze.

   Polityki grupy to hipoteza z dokumentacji: forum donosi, że dla komend `/tool` bywa potrzebne też `test`
   (research W2). Jeśli tikker zgłasza `insufficient_policy`, dodaj `test` do grupy i odczekaj ok. 10 minut
   (REST buforuje poświadczenia) [niezweryfikowane na sprzęcie].

## 5. Obraz kontenera

Obraz budujesz z repozytorium (`make docker-buildx` daje archiwum OCI `dist/tikker-<wersja>.oci.tar` dla
`linux/amd64`, `arm64`, `arm/v7`; [deploy/docker/README.md](../deploy/docker/README.md)). Do czasu opublikowania
obrazów w rejestrze masz dwie drogi:

- wgraj własny obraz do swojego rejestru i podaj go w `image`;
- albo użyj `file=` zamiast `remote-image=` w `/container add` z archiwum wgranym na router (wymaga zmiany skryptu;
  [niezweryfikowane na sprzęcie]).

Rejestr domyślny to `https://registry-1.docker.io` (`/container config registry-url`); dla innego rejestru zmień
polecenie w skrypcie (dane logowania do rejestru: RouterOS ≥ 7.8).

Proces w obrazie działa jako `65532:65532`, a katalog zamontowany przez RouterOS należy do roota, więc skrypt
ustawia `user=0:0`. Jeśli router odrzuca opcję `user` albo w logu pojawia się błąd zapisu do `/data`, to jest
znane ryzyko do sprawdzenia w T51 [niezweryfikowane na sprzęcie].

## 6. Pierwsze logowanie, token i połączenie z routerem

W obrazie `scratch` nie ma powłoki, więc `/container/shell` nie zadziała, a polecenia `tikker token create`
nie uruchomisz. Zamiast tego:

1. Wystaw panel tak, aby był osiągalny z Twojej sieci. Bezpieczniejsze opcje to port forwarding przez SSH
   (`ssh -L 8080:172.17.0.2:8080 admin@<router>` [niezweryfikowane na sprzęcie]) lub `exposePanel="yes"` tylko
   dla zaufanej listy LAN. **Nigdy nie wystawiaj panelu do WAN** (sekcja 10).
2. Zaloguj się na `http://<adres>:8080` jako `admin` z hasłem `adminPassword` (utworzonym tylko przy pustej bazie).
   Po pierwszym logowaniu zmień hasło, a z konfiguracji kontenera usuń je:
   `/container/envs remove [find name=tikker key=TIKKER_ADMIN_PASSWORD]` i uruchom ponownie kontener.
3. Token API utwórz w panelu (zakładka tokeny; pokazywany tylko raz) albo przez API
   (`POST /api/v1/tokens`) z sesji administratora.
4. Dodaj urządzenie (panel lub `POST /api/v1/devices`):
   - `base_url`: `https://172.17.0.1` (adres bramy `veth`);
   - `username` / `password`: dane z `apiUser` i `apiPassword`;
   - `tls_mode`: `fingerprint` i pole `fingerprint` z odciskiem SHA-256 certyfikatu.

   Odcisk odczytasz na routerze (skrypt też go wypisał):

   ```
   /certificate get tikker-api fingerprint
   ```

   Certyfikat jest self-signed, więc użyj przypięcia odcisku, nie `insecure`. Odcisk zmieni się po wygenerowaniu
   nowego certyfikatu, wtedy trzeba go zaktualizować w tikkerze.
5. Uruchom test urządzenia (`POST /devices/{id}/test` lub przycisk w panelu) i dodaj kanał dla portu `lte1`.
   `TIKKER_ALLOWED_HTTP_HOSTS` nie jest potrzebne, bo połączenie idzie przez HTTPS. Plain HTTP do `172.17.0.1`
   byłoby dopuszczalne tylko po wpisaniu podsieci `veth` w tej zmiennej (SPEC §3.3); odradzamy.

## 7. Aktualizacja obrazu

RouterOS nie aktualizuje obrazu kontenera w miejscu [niezweryfikowane na sprzęcie]:

1. Zrób kopię `/data` (sekcja 8).
2. `/container stop [find comment=tikker]`, potem `/container remove [find comment=tikker]`.
   Mount i zmienne środowiskowe zostają, dane też (są na `mediaPath/data`).
3. Zmień `image` na nowy tag (zalecane przypięte wersje zamiast `latest`) i ponownie uruchom `setup.rsc`.
   Skrypt doda kontener od nowa i go wystartuje.
4. Sprawdź log: `/log print where topics~"container"`, a w panelu stan kanałów.

Migracje bazy uruchamiają się przy starcie. Przy cofnięciu wersji przywróć kopię danych z sekcji 8.

## 8. Kopia zapasowa `/data`

W `/data` są baza `tikker.db` (+ pliki WAL) i **`secret.key`**, klucz szyfrujący hasła urządzeń (AES-256-GCM).
Bez `secret.key` kopia bazy jest bezużyteczna: hasła routerów nie da się odszyfrować, a utracony klucz
oznacza ponowne wpisanie haseł. Kopiuj oba pliki razem, trzymaj je w tym samym, chronionym miejscu.

- Zatrzymaj kontener (`/container stop`), skopiuj katalog `mediaPath/data` (`/file print`, pobranie z Files lub
  `scp` z routera), uruchom kontener. Spójna kopia bez zatrzymania wymaga mechanizmów SQLite
  ([niezweryfikowane na sprzęcie]).
- Kopia konfiguracji routera zawiera hasła zmiennych środowiskowych (`/container/envs`): przechowuj `/export`
  i `.backup` jako sekret albo użyj `hide-sensitive`.

## 9. Pamięć i zużycie flash

- Obraz ustawia `GOGC=30` i `GOMEMLIMIT=24MiB` (RSS w spoczynku ok. 40 MB). Limit jest miękki. Jeśli kontener
  jest zabijany przez OOM, podnieś `memory-max` kontenera (zmienna `ctrMemoryMax` w skrypcie) lub nadpisz
  `GOMEMLIMIT` w `/container/envs`, zachowując go poniżej limitu kontenera.
- `/container config` ma również limit globalny (`memory-high`); przy małym RAM warto go ustawić ręcznie.
- Zużycie flash: tikker używa SQLite w trybie WAL i nie zapisuje nic przy pustym pollingu; logi idą na stdout
  (do pamięci routera, nie na dysk). Mimo to trzymaj `root-dir`, `tmpdir` i mount na zewnętrznym nośniku, a nie na
  wbudowanej NAND. Retencja treści wiadomości (`TIKKER_RETENTION`, domyślnie 90 dni) ogranicza rozmiar bazy.
- Nie ustawiaj zbyt krótkiego `TIKKER_INBOX_POLL_INTERVAL`: każde odpytanie obciąża modem i router.

## 10. Bezpieczeństwo

- **Nie wystawiaj panelu do WAN.** Port 8080 to HTTP bez TLS i panel administracyjny. Przed Internetem postaw
  reverse proxy z TLS na osobnym serwerze (tryb serwerowy, [deploy-server.md](deploy-server.md)) albo użyj VPN.
- `exposePanel="yes"` ogranicza dst-nat do listy interfejsów `panelLanList` (domyślnie `LAN`). Sprawdź, że lista
  nie zawiera interfejsu WAN i że domyślna reguła `forward` nie przepuszcza ruchu z WAN.
- Konto `tikker` ma `address=` ograniczone do podsieci `veth`, a `www-ssl` także. Nie rozszerzaj tego bez potrzeby.
- Grupa `tikker` nie ma polityk `ssh`, `ftp`, `policy`, `reboot`, `sensitive`: kompromitacja tikkera nie daje
  administracji routerem. Hasło REST przechowuje tikker zaszyfrowane, ale w `/container/envs` leży
  `TIKKER_ADMIN_PASSWORD` (usuń po pierwszym logowaniu).
- Trzymaj RouterOS aktualny i rozważ wyłączenie niepotrzebnych usług (`/ip service`).

## 11. Rozwiązywanie problemów

| Objaw | Co sprawdzić |
|---|---|
| `setup.rsc` przerywa z komunikatem o `container` | `device-mode` (`/system/device-mode/print`) i pakiet `container` (sekcja 3) |
| Kontener ma status `error` / nie startuje | `/log print where topics~"container"`; pamięć; czy nośnik jest zamontowany (`/disk print`) |
| Błąd zapisu do `/data` | uprawnienia mounta (opcja `user=0:0`), wolne miejsce na nośniku |
| Nie ma ściągania obrazu | `/container config print`, dostęp routera do rejestru (DNS, firewall `output`), `tmpdir` na nośniku |
| Test urządzenia: `unreachable` / `tls_error` | adres `172.17.0.1`, `www-ssl` (`/ip service print`), reguła firewall „tikker: REST API”, odcisk certyfikatu |
| `auth_failed` / `insufficient_policy` | hasło, `address=` użytkownika, polityki grupy (dodaj `test`); odczekaj ok. 10 min na odświeżenie cache REST |
| SMS w statusie `queued`, kanał `degraded` | `/interface lte monitor lte1 once` (rejestracja w sieci), SIM, `/tool sms print` (`status=running`) |
| SMS nie przychodzą | `receive-enabled=yes`, ewentualnie `polling=yes`, `secret`; skrzynka nie jest pełna |
| Brak zapisów po restarcie | czy mount wskazuje trwały nośnik, a nie tmp; `start-on-boot=yes` |

W razie wątpliwości porównaj zachowanie z [docs/research/t00-checklist.md](research/t00-checklist.md) i narzędziem
`scripts/probe`. Wyniki pomiarów z prawdziwego routera zgłoś jako poprawki do tego przewodnika.
