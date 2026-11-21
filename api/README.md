<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Kontrakt REST API

[`openapi.yaml`](openapi.yaml) to kontrakt OpenAPI 3.1 REST API tikkera (`/api/v1`) oraz sond
`/healthz` i `/readyz`. Jest **źródłem prawdy** dla handlerów w `internal/api`, klientów i dokumentacji
(SPEC §5).

## Kontrakt przed kodem

- Implementacja dostosowuje się do kontraktu, a nie odwrotnie. Gdy kod wymaga innego zachowania niż
  opisane w `openapi.yaml`, najpierw zmieniamy kontrakt.
- Zmiana kontraktu to **osobny PR** (poziom L3, przegląd człowieka – PLAN §1.2 p. 3). PR opisuje wpływ
  na klientów: czy zmiana jest wstecznie zgodna (nowe pole opcjonalne, nowy endpoint) czy łamiąca
  (usunięcie lub zmiana pola, nowe pole wymagane w żądaniu, zmiana kodu odpowiedzi).
- Zmiany łamiące w `/api/v1` są niedozwolone po wydaniu 1.0; wymagają `/api/v2`.
- Klienci muszą tolerować nieznane pola w odpowiedziach i nieznane wartości `errors[].code`.

## Najważniejsze konwencje

| Temat | Konwencja |
|---|---|
| Autoryzacja | `Authorization: Bearer tkr_<prefix>_<secret>`; zakresy `sms:read`, `sms:send`, `admin` w `x-required-scopes` |
| Błędy | RFC 9457 `application/problem+json`, `type` = `urn:tikker:problem:<slug>`, walidacja w `errors[]` (`field` jako wskaźnik JSON) |
| Paginacja | `?cursor=&limit=` (domyślnie 50, maks. 200), odpowiedź `{items, next_cursor}` |
| Wysyłka | `POST /messages` → `202` + `{messages: [Message, …]}` (kilka przy strategii `split`), `Location` na pierwszą; batch → `{items: [{messages: […]}, …]}`, jedna pozycja na pozycję żądania |
| Idempotencja | `Idempotency-Key` na `POST /messages` i `/messages/batch`, 24 h per token; rozpoznawana przed walidacją; inne ciało → `422`, równoległe → `409` |
| Limity | `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` w odpowiedziach na uwierzytelnione żądania (nie na `401`); `429` + `Retry-After` |
| Rozmiar ciała | ponad limit (domyślnie 2 MiB) → `413` `payload-too-large`; ponad 100 pozycji paczki → `413` `batch-too-large` |
| Nieznana ścieżka / metoda | `404` / `405` z `Allow` (`method-not-allowed`), przed uwierzytelnieniem; opis w „Odpowiedzi wspólne” |
| Częściowa aktualizacja | `PATCH` z semantyką JSON Merge Patch (RFC 7396) |
| Pola JSON | `snake_case`, czasy RFC 3339 w UTC, `id` wiadomości to ULID |

## Zmiany kontraktu

### 0.2.0 (T21b, zmiany łamiące przed 1.0)

- `POST /messages` zwraca `SendMessageResponse` (`{messages: [Message, …]}`) zamiast pojedynczego `Message`:
  przy strategii `split` jedno żądanie tworzy kilka wiadomości (SPEC §12 p. 3). Usunięty nagłówek
  `X-Tikker-Message-Ids`. `Location` wskazuje pierwszą wiadomość.
- `POST /messages/batch`: `items[i]` to `SendMessageResponse` dla `messages[i]` żądania (grupy zamiast płaskiej listy),
  także przy powtórzeniu idempotentnym (rozmiary grup są zapisane z rekordem klucza).
- Powtórzenie z `Idempotency-Key` jest rozpoznawane przed walidacją i zwraca pierwotne wiadomości w bieżącym stanie.
- `413`: nowy typ `payload-too-large` (zbyt duże ciało) na każdej operacji z ciałem; `batch-too-large` tylko dla
  ponad 100 pozycji paczki.
- `405`: nowy typ `method-not-allowed` z nagłówkiem `Allow` (wcześniej `malformed-request`); opisany w
  `components/responses/MethodNotAllowed` i w sekcji „Odpowiedzi wspólne”.
- Opis nagłówków `RateLimit-*` zgodny z implementacją: tylko odpowiedzi na uwierzytelnione żądania.
- `ChannelCreate.transliterate` bez wartości domyślnej `false`: pominięte → serwer wybiera wg możliwości konektora
  (`true` bez UCS-2, czyli dla RouterOS).
- Nowy kod walidacji `unsupported_characters` (znaki spoza GSM-7 bez UCS-2 i bez transliteracji; wcześniej `invalid_format`).
- `Message.possible_duplicate` (ADR 0002) – nowe pole odpowiedzi.
- `title` problemu jest stały dla `type` (np. `Malformed request` zamiast `Bad Request`).

## Lint

Konfiguracja lintera: [`redocly.yaml`](../redocly.yaml) (reguły `recommended-strict`, RFC 9457 dla odpowiedzi 4xx,
zgodność wszystkich przykładów ze schematami, wymagane `x-required-scopes`).
Świadome wyjątki: [`.redocly.lint-ignore.yaml`](../.redocly.lint-ignore.yaml).

```sh
# z katalogu głównego repozytorium (Node.js >= 18; Redocly CLI ma licencję MIT)
npx --yes @redocly/cli@1.34.5 lint

# podgląd dokumentacji w przeglądarce
npx --yes @redocly/cli@1.34.5 preview-docs api/openapi.yaml
```

Dodatkowa walidacja strukturalna (opcjonalnie, Python, licencja Apache-2.0):

```sh
pip install openapi-spec-validator
openapi-spec-validator api/openapi.yaml
```

W CI lint uruchamia workflow [`.github/workflows/openapi.yml`](../.github/workflows/openapi.yml).
