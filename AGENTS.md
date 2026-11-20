# AGENTS.md – instrukcje dla agentów AI

Ten plik czytają agenci kodujący (Claude Code, Codex, Gemini CLI i inne). Ludzie też mogą z niego korzystać.

## Kontekst projektu

**tikker** to bramka SMS dla urządzeń MikroTik z RouterOS 7 (modem LTE/5G, karta SIM).
Aplikacja w Go wystawia REST API i panel WWW, kolejkuje wiadomości w SQLite i łączy się z routerem
przez REST API RouterOS. Działa w dwóch trybach: jako kontener na MikroTiku oraz na zwykłym serwerze.
Licencja: GPL-3.0-or-later.

Najpierw przeczytaj dokumenty źródłowe:

- [docs/SPEC.md](docs/SPEC.md): specyfikacja MVP (architektura, model danych, API, bezpieczeństwo, struktura repo w §9);
- [docs/PLAN.md](docs/PLAN.md): plan realizacji, zadania (T00–T52), zależności i reguły pracy (§1.2);
- [docs/adr/0001-architektura.md](docs/adr/0001-architektura.md): decyzja o architekturze;
- [docs/adr/0002-gwarancje-dostarczenia.md](docs/adr/0002-gwarancje-dostarczenia.md): at-least-once, dzierżawy, idempotencja, deduplikacja;
- [docs/adr/0003-biblioteki.md](docs/adr/0003-biblioteki.md): biblioteki i układ pakietów (`domain`, `store`, `connector`, `clock`);
- [CONTRIBUTING.md](CONTRIBUTING.md): DCO, Conventional Commits, nagłówki SPDX, dozwolone licencje.

Oznaczenie **[WERYFIKACJA]** w SPEC oznacza założenie o RouterOS, którego jeszcze nie potwierdzono
na sprzęcie (zadanie T00). Nie implementuj na jego podstawie zachowania bez konfigurowalności lub izolacji w konektorze.

## Reguły pracy (PLAN §1.2)

1. **Jeden PR = jedno zadanie** z PLAN. W opisie PR podaj ID zadania, użyty model i wynik przeglądu.
2. **Kontrakt przed kodem:** implementacja opiera się na interfejsach (`internal/domain`, `internal/connector`)
   i kontrakcie `api/openapi.yaml`. Zmiana kontraktu to osobny PR (L3, wymaga człowieka).
3. **Definicja ukończenia:** `make lint test` przechodzi; testy obejmują nowy kod; nagłówki SPDX są obecne;
   brak nowych zależności spoza dozwolonych licencji; dokumentacja zaktualizowana; commit z `Signed-off-by`.
4. **Przegląd krzyżowy:** kod pisany przez model jednego dostawcy powinien być przeglądany przez inny.
   Zadania L3 (architektura, współbieżność, bezpieczeństwo, integracja z RouterOS, kontrakty) zawsze przegląda człowiek.
5. **Brak sprzętu nie blokuje pracy:** do czasu T00 używaj fake RouterOS (`internal/connector/fake`, zadanie T13),
   a nie prawdziwego routera.

## Konwencje kodu

- Język: kod, identyfikatory i komentarze w kodzie po angielsku. Dokumentacja projektu (docs, README, CONTRIBUTING) po polsku.
- Każdy plik `.go` zaczyna się od nagłówka:
  ```go
  // SPDX-FileCopyrightText: 2026 The tikker Authors
  // SPDX-License-Identifier: GPL-3.0-or-later
  ```
- HTTP wyłącznie na stdlib `net/http` (router Go 1.22+ z wzorcami metod). Bez frameworków webowych.
- Logowanie wyłącznie przez `log/slog`. Nie loguj treści SMS, haseł, tokenów ani pełnych numerów (maskuj).
- **Bez CGO**: `CGO_ENABLED=0` dla buildów. SQLite przez `modernc.org/sqlite`.
- Błędy owijaj `fmt.Errorf("kontekst: %w", err)`, żeby zachować `errors.Is/As`. Klasy błędów konektora:
  `Transient`, `Permanent`, `Auth` (z `internal/domain`).
- Testy: tabelaryczne (`tests := []struct{...}`, `t.Run`), z `-race`. Czas w testach przez wstrzykiwany `Clock`, nie `time.Now()`.
- Kontekst (`context.Context`) przekazuj jako pierwszy argument w funkcjach I/O i długich operacjach.
- Nie dodawaj zależności bez uzasadnienia i sprawdzenia licencji (SPEC §11).
- Nie twórz plików dokumentacji (`*.md`) bez wyraźnej potrzeby zadania.

## Polecenia

```sh
make build   # binarka w bin/tikker (CGO_ENABLED=0, wersja z git describe)
make test    # go test -race -cover ./...
make lint    # golangci-lint (v2): errcheck, govet, staticcheck, gosec, revive, gofmt, goimports, ...
make run ARGS="version"
make clean
reuse lint   # sprawdzenie nagłówków SPDX i REUSE.toml (zainstaluj: pip install reuse)
```

Przed zakończeniem zadania uruchom `make build lint test` i, jeśli dostępne, `reuse lint`.

## Git

- Commity w Conventional Commits, zawsze z `git commit -s` (DCO).
- Nie pushuj i nie otwieraj PR bez wyraźnego polecenia człowieka.
- Nie commituj sekretów (`.env`, klucze, `secret.key`, prawdziwe `config.yaml`, zrzuty z routerów z prawdziwymi numerami).
- Zrzuty i logi z urządzeń zanonimizuj (numery, IMEI, hasła) przed dodaniem do repo.
