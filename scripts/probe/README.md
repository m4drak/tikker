# probe: narzędzie pomiarowe T00

Mały program Go (tylko biblioteka standardowa) do powtarzalnych pomiarów zachowania RouterOS przez REST API
(`/rest/...`). Zapisuje żądania i odpowiedzi z czasami jako JSON, **już zanonimizowane**.
Pełna procedura pomiarów: [docs/research/t00-checklist.md](../../docs/research/t00-checklist.md).

## Budowanie

```
go build -o probe ./scripts/probe
go vet ./scripts/probe/... && go test -race ./scripts/probe/...
```

## Konfiguracja

| Zmienna / flaga | Znaczenie |
|---|---|
| `PROBE_URL`, `-url` | adres routera, np. `https://192.168.88.1` |
| `PROBE_USER`, `-user` | użytkownik testowy |
| `PROBE_PASSWORD` | hasło, **wyłącznie ze zmiennej środowiskowej** (brak flagi) |
| `PROBE_TO`, `-to` | numer testowy odbiorcy (`+48…`) |
| `PROBE_PORT`, `-port` | port LTE, domyślnie `lte1` |
| `PROBE_FINGERPRINT`, `-fingerprint` | oczekiwany odcisk SHA-256 certyfikatu routera |
| `-insecure` | brak weryfikacji TLS (tylko testy) |
| `-ca plik.pem` | własny CA |
| `-out katalog` | katalog bazowy wyników (domyślnie `probe-results`) |
| `-label nazwa` | etykieta dopisywana do nazwy katalogu wyników |
| `-yes` | potwierdzenie wysyłki prawdziwych SMS-ów (koszty) |
| `-gap` | przerwa między wiadomościami w `send-suite` (domyślnie 10 s) |
| `-keep-inbox-bodies` | nie zasłaniaj `message` i `pdu` w skrzynce |

Flagi podaje się przed trybem: `./probe -label p1 policies`.

## Tryby

| Tryb | Co robi | Wysyła SMS? |
|---|---|---|
| `info` | system, pakiety, device-mode, `ip/service`, grupy, porty, LTE (list, monitor, info), ustawienia SMS | nie |
| `inbox` | odczyt skrzynki, liczba i lista pól | nie |
| `inbox-delete -id '*1'` | `DELETE /rest/tool/sms/inbox/{id}`, a przy błędzie `POST …/inbox/remove` | usuwa wpis, wymaga `--yes` |
| `send` | jedna wiadomość (`-message`) | tak, wymaga `--yes` |
| `send-suite` | zestaw przypadków: ASCII 160/161/320/500, polskie 70/71, GSM-7 spoza ASCII, znaki rozszerzone, emoji (`-list-cases` pokazuje je offline, `-only a,b` wybiera podzbiór) | tak, wymaga `--yes` |
| `throughput` | `-count` wiadomości, `-parallel` żądań naraz; opóźnienia i wiadomości na minutę | tak, wymaga `--yes` |
| `policies` | stały zestaw wywołań (odczyt) do porównania zestawów polityk; z `-to` i `--yes` także `send` | opcjonalnie |

## Wyniki

Każde uruchomienie tworzy katalog `probe-results/<data>-<tryb>[-etykieta]/`:

- `NNN-<etykieta>.json`: jedna wymiana (czas startu, czas trwania w ms, żądanie, odpowiedź lub błąd);
- `summary.json`: lista wymian oraz dane trybu (wersja routera, analiza przypadków `send-suite`, statystyki `throughput`, macierz `policies`).

## Anonimizacja

Maskowane jest wszystko, co trafia do plików (nie istnieje tryb zapisu surowego):

- numery telefonów (podany `-to` oraz wzorce `+…` i 9–14 cyfr), IMEI/IMSI (15 cyfr), ICCID (89…), adresy IPv4/IPv6 i MAC;
- wartości kluczy: `phone`, `phone-number`, `source`, `imei`, `imsi`, `uicc`, `serial-number`, `mac-address`, `identity`, kody komórek;
- hasło, `secret`, `sim-pin`, nagłówek `Authorization`, ciasteczka, nazwa hosta z `-url`;
- treść `message` i `pdu` odpowiedzi z `/tool/sms/inbox` (zostają tylko długości), chyba że `-keep-inbox-bodies`.

Znaczniki są spójne w obrębie uruchomienia (`<PHONE-1>` to zawsze ten sam numer). Treści testowe wysyłane przez
`send-suite` (nie zawierają długich ciągów cyfr) zostają w wynikach, bo są potrzebne do analizy.
Anonimizacja jest heurystyczna: **przejrzyj wyniki przed commitem** (zob. sekcja 8 listy kontrolnej).
Znane skutki uboczne: ciągi 9–14 cyfr (np. czas epoch) są maskowane jak numery telefonów.

## Testy

`go test -race ./scripts/probe/...` uruchamia testy anonimizacji oraz testy trybów przeciw serwerowi `httptest`
udającemu RouterOS (401, błędy polityk, wysyłka, skrzynka, usuwanie, odcisk TLS). Testy nie wymagają routera.
