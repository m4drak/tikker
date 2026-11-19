# ADR 0003: Wybór bibliotek i układ pakietów

- Status: proponowana (wymaga zatwierdzenia przez człowieka, PLAN T04)
- Data: 2026-11-19
- Powiązane: SPEC §2.2, §6, §7, §9, §11; ADR 0001; ADR 0002

## Kontekst

tikker ma działać w kontenerze na routerze (mało RAM, flash, ARM) i na serwerze. Ma to być statyczna binarka
bez CGO, w obrazie ≤ 25 MB, na licencji **GPL-3.0-or-later**. Każda zależność:
- musi mieć licencję zgodną z GPLv3: MIT, BSD, ISC, 0BSD, Apache-2.0, MPL-2.0 lub LGPL (SPEC §11).
  CI sprawdza to narzędziem `go-licenses`;
- zwiększa rozmiar binarki i powierzchnię ataku, więc wymaga uzasadnienia.

Zasada ogólna: **najpierw biblioteka standardowa**. Zależność zewnętrzną dodajemy wtedy, gdy własna implementacja
byłaby ryzykowna (kryptografia, numeracja telefoniczna, silnik SQL) albo nieproporcjonalnie duża.

## Decyzja

### Biblioteki

| Obszar | Wybór | Licencja | Uzasadnienie |
|---|---|---|---|
| HTTP, routing | stdlib `net/http` (`ServeMux` z wzorcami metod i ścieżek, Go 1.22+) | BSD-3-Clause | wzorce `GET /api/v1/messages/{id}` wystarczają. Frameworki (chi, gin, echo) nic istotnego nie dodają, a zwiększają zależności |
| Logowanie | stdlib `log/slog` | BSD-3-Clause | strukturalne logi JSON/text bez zależności. `LogValuer` pozwala redagować sekrety (np. `connector.DeviceConfig`) |
| Baza danych | `modernc.org/sqlite` | BSD-3-Clause (sam SQLite: domena publiczna) | czysty Go, bez CGO, więc działa kompilacja skrośna na `arm/v7`/`arm64`. Kosztem jest większa binarka (~+6 MB) i nieco wolniejsze działanie niż `mattn/go-sqlite3`, co przy naszej skali nie ma znaczenia. Zależności przechodnie (`modernc.org/libc`, `mathutil`, `memory`, `github.com/dustin/go-humanize`, `github.com/google/uuid`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`, `golang.org/x/sys`) są na licencjach MIT/BSD |
| Migracje | **własny runner** + `embed` (`internal/store/sqlite/migrations/NNNN_opis.sql`) | – | ok. 60 linii: tabela `schema_migrations`, pliki w kolejności, każda migracja w osobnej transakcji, tylko w przód. `goose` (MIT) ma więcej zależności i funkcji, których nie potrzebujemy (migracje w Go, wiele dialektów, CLI). Decyzję zrewidujemy przy dodawaniu PostgreSQL |
| Numery telefonów | `github.com/nyaruka/phonenumbers` | MIT (zależność `google.golang.org/protobuf`: BSD-3-Clause) | port `libphonenumber`. Poprawnej walidacji E.164 i regionów nie warto pisać samemu. Metadane zwiększają binarkę o kilka MB, co jest akceptowalne |
| Hasła | `golang.org/x/crypto/argon2` (argon2id) | BSD-3-Clause | rekomendacja OWASP. Biblioteka utrzymywana przez zespół Go. Parametry przechowywane w formacie PHC w `password_hash` |
| Szyfrowanie sekretów | stdlib `crypto/aes` + `crypto/cipher` (GCM) | BSD-3-Clause | AES-256-GCM jest w stdlib. Dodatkowa biblioteka nie jest potrzebna |
| Identyfikatory | `github.com/oklog/ulid/v2` | Apache-2.0 | ULID (SPEC §4.1) z monotonicznością w obrębie milisekundy. Mała biblioteka bez zależności runtime (`pborman/getopt` w `go.mod` dotyczy tylko jej narzędzia CLI). Własna implementacja byłaby krótka, ale łatwo o błąd w kodowaniu Crockford base32 i w monotoniczności |
| UI – interakcje | htmx 2.x (plik JS osadzony przez `go:embed`) | 0BSD | brak kroku budowania JS. Działa z `html/template` i CSP bez `unsafe-inline` (przy `htmx.config.includeIndicatorStyles=false`) |
| UI – style | Pico CSS 2.x (plik CSS osadzony) | MIT | semantyczny HTML bez klas, mały rozmiar, tryb ciemny |
| Testy – wycieki goroutine | `go.uber.org/goleak` (tylko testy) | MIT | wymóg T15. Nie trafia do binarki |
| Testy – asercje | stdlib `testing` (bez testify) | – | testy tabelaryczne nie potrzebują biblioteki asercji. Mniej zależności |

Pliki htmx i Pico CSS trafiają do repozytorium razem z plikami licencji (`internal/web/static/LICENSES/`)
i są wymienione w `REUSE.toml` oraz `THIRD_PARTY_NOTICES`. Nie korzystamy z CDN (SPEC §6, CSP).

Poza zakresem T04, rekomendacja dla T10: parser YAML `go.yaml.in/yaml/v3` (MIT i Apache-2.0)
albo `github.com/goccy/go-yaml` (MIT).

### Zgodność licencyjna z GPL-3.0-or-later

- MIT, BSD-2/3-Clause, 0BSD i ISC są permisywne i zgodne z GPLv3. Wystarczy zachować informacje o prawach autorskich
  (w `THIRD_PARTY_NOTICES`).
- Apache-2.0 jest zgodna z GPLv3 (ale nie z GPLv2-only). Projekt jest „GPL-3.0-or-later”, więc ta niezgodność nas nie dotyczy.
  Trzeba zachować plik `NOTICE`, jeśli zależność go ma (`oklog/ulid` go nie ma).
- Domena publiczna (SQLite) nie nakłada wymagań.

### Układ pakietów i kierunek zależności

```
internal/clock        Clock (Now, After), System(), Fake – liść, tylko stdlib
internal/domain       encje, enumy, maszyna stanów, RetryPolicy, błędy, ID – importuje tylko clock + ulid
internal/connector    interfejs Connector, typy, Registry – importuje domain
internal/store        interfejsy repozytoriów, filtry, Page, WithTx – importuje domain
internal/store/sqlite implementacja (T11) – importuje store, domain
internal/connector/routeros, /fake – implementacje (T12, T13)
internal/queue, inbox, health, api, web – używają interfejsów, nie implementacji
cmd/tikker            jedyne miejsce, które składa implementacje (wiring)
```

- **Interfejsy repozytoriów są w `internal/store`, a nie w `internal/domain`.** Domena zostaje wolna
  od pojęć utrwalania (kursory, transakcje, dzierżawy jako operacje). Kontrakt store'u, czyli atomowość `AcquireNext`,
  token ogrodzenia i „odczyt nie pisze”, ma jedno miejsce z dokumentacją. Implementacja SQLite trafia do
  `internal/store/sqlite`. Konsumenci (`queue`, `api`) importują lekkie `internal/store`, nie ciągną sterownika
  i mogą w testach używać atrap. Przyszły PostgreSQL to `internal/store/postgres` z tym samym kontraktem.
  Cykli nie ma, bo `domain` nie importuje żadnego innego pakietu wewnętrznego.
- **`Clock` jest w osobnym pakiecie `internal/clock`** razem z `Fake` (sterowanym ręcznie, z `BlockUntil`).
  Testy kolejki (T15) i health (T17) obejdą się wtedy bez `time.Sleep`.
- **Encje domeny nie mają tagów JSON.** Część z nich zawiera sekrety (`PasswordHash`, `Hash`, `PasswordEnc`).
  Warstwa API mapuje je na jawne DTO zgodne z `api/openapi.yaml`. Enumy implementują `encoding.TextMarshaler`,
  więc DTO mogą ich używać bezpośrednio.
- **Konektor jest wiązany z urządzeniem i tworzony przez `connector.Registry`** według `Device.Connector`
  (`routeros-rest` w MVP, `agent` po MVP). Nie ma globalnego rejestru ani rejestracji w `init()`.
  Sposób podłączenia konektora `agent` opisuje dokumentacja pakietu `internal/connector`.

## Konsekwencje

- (+) Mała liczba zależności runtime: `modernc.org/sqlite` (z przechodnimi), `nyaruka/phonenumbers`, `x/crypto`, `oklog/ulid`.
  Wszystkie mają licencje z listy dozwolonych.
- (+) Brak CGO i kroku budowania frontendu. `go build` wystarcza na wszystkie architektury.
- (+) Interfejsy w osobnych, lekkich pakietach pozwalają równolegle pracować nad T11, T12, T13, T15, T16 i T17.
- (−) Własny runner migracji trzeba utrzymywać (tylko w przód, bez rollbacku). Przy PostgreSQL rozważymy `goose`.
- (−) `modernc.org/sqlite` i metadane `phonenumbers` zwiększają binarkę. Cel ≤ 25 MB obrazu trzeba zweryfikować w T40.
- (−) Odejście od SPEC §9: `internal/store/` dzieli się na interfejsy (`store`) i implementację (`store/sqlite`).
  Dochodzi też pakiet `internal/clock`. SPEC §9 wymaga aktualizacji.
