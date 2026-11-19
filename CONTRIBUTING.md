# Jak współtworzyć tikker

Dziękujemy za chęć pomocy! Ten dokument opisuje zasady wkładu w projekt.
Najpierw przeczytaj [specyfikację](docs/SPEC.md) i [plan realizacji](docs/PLAN.md).

Zgłaszanie błędów i propozycji: GitHub Issues. Luki bezpieczeństwa zgłaszaj zgodnie z [SECURITY.md](SECURITY.md).
Zasady zachowania: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Licencja i DCO

Projekt jest na licencji **GNU GPL v3.0 lub nowszej** (`GPL-3.0-or-later`).
Wkład przyjmujemy na zasadzie **Developer Certificate of Origin (DCO)**, bez CLA.
Każdy commit musi zawierać linię `Signed-off-by` potwierdzającą prawo do wniesienia zmian:

```sh
git commit -s -m "feat(sms): ..."
```

Brak `Signed-off-by` blokuje scalenie. Treść DCO: <https://developercertificate.org/>.

## Nagłówki SPDX

Każdy plik źródłowy (np. `.go`) zaczyna się od nagłówka dokładnie w tej postaci:

```go
// SPDX-FileCopyrightText: 2026 The tikker Authors
// SPDX-License-Identifier: GPL-3.0-or-later
```

Pliki bez komentarzy (Markdown, YAML, TOML, `go.mod`, `Makefile`) są opisane w `REUSE.toml`.
Zgodność sprawdza `reuse lint` (`reuse` z pip). Nowe typy plików wymagają wpisu w `REUSE.toml`.

## Zależności

Dodanie zależności wymaga uzasadnienia w PR. Dozwolone licencje (SPEC §11):

- MIT, BSD (2- i 3-klauzulowa), ISC, 0BSD, Apache-2.0, MPL-2.0, LGPL;
- projekt sam jest GPL-3.0-or-later.

Zależności spoza tej listy (np. licencje z klauzulami niekompatybilnymi z GPLv3) nie są przyjmowane.
Kontrolę automatyzuje CI (`go-licenses` z allowlistą, zadanie T02).

## Commity: Conventional Commits

Format tytułu: `typ(zakres): opis w trybie rozkazującym`, np. `feat(queue): add token bucket limiter`.
Dozwolone typy: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `chore`, `style`, `revert`.
Zmiana łamiąca zgodność: `!` po typie lub stopka `BREAKING CHANGE:`.

## Pull requesty

- **Jeden PR = jedno zadanie** z `docs/PLAN.md` (podaj jego ID, np. `T01`).
- W opisie PR podaj: ID zadania, wynik przeglądu, a jeśli kod powstał przy wsparciu AI,
  **użyty model** (np. `Claude Haiku 5.5`). Kod generowany przez modele podlega tym samym zasadom
  i przeglądowi co kod pisany ręcznie.
- Zadania L3 (architektura, współbieżność, bezpieczeństwo, integracja z RouterOS, kontrakty) przegląda człowiek.
- Zmiana kontraktu (`api/openapi.yaml`, interfejsy z `internal/domain`) wymaga osobnego PR.
- Przed zgłoszeniem PR uruchom lokalnie `make build lint test`.

### Definicja ukończenia (DoD)

- [ ] `make lint test` przechodzi;
- [ ] testy obejmują nowy kod;
- [ ] nagłówki SPDX są obecne, `reuse lint` jest czysty;
- [ ] brak nowych zależności spoza dozwolonych licencji;
- [ ] dokumentacja zaktualizowana;
- [ ] commity podpisane (`Signed-off-by`).

## Lokalne środowisko

- Go 1.24 lub nowszy, `make`, [golangci-lint](https://golangci-lint.run/) (v2).
- `reuse` (opcjonalnie lokalnie, obowiązkowo w CI): `pip install reuse`.
- Sprzęt MikroTik nie jest potrzebny do pracy nad kodem: do czasu zadania T00 używamy fake RouterOS (T13).
