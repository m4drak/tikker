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
| Idempotencja | `Idempotency-Key` na `POST /messages` i `/messages/batch`, 24 h per token; inne ciało → `422`, równoległe → `409` |
| Limity | `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`; `429` + `Retry-After` |
| Częściowa aktualizacja | `PATCH` z semantyką JSON Merge Patch (RFC 7396) |
| Pola JSON | `snake_case`, czasy RFC 3339 w UTC, `id` wiadomości to ULID |

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
