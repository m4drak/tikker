<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Konektor `agent` (pull za CGNAT)

Konektor `agent` pozwala tikkerowi wysyłać i odbierać SMS-y przez router, który
**nie** jest wywoływany z tikkera — to skrypt na routerze łączy się do tikkera
przez HTTPS i odpytuje oraz raportuje. Dzięki temu działa za CGNAT, bez port
forwarding i bez VPN (PLAN §5 #1).

## Dlaczego pull?

Konektor `routeros-rest` (MVP) wymaga, by tikker mógł się **połączyć do**
routera (nasłuchujący HTTPS, port 443 w internecie). Za CGNAT to niemożliwe.
Konektor `agent` odwraca role:

```
MikroTik RouterOS (za CGNAT, tylko NAT out)          tikker (serwer publiczny)
  deploy/routeros/agent.rsc  ===HTTPS polling===>    GET /agent/v1/...
      (cyklicznie)                              (autoryzacja Bearer token)
```

Skrypt na routerze:
1. co kilka sekund robi `/tool fetch` na endpointy tikkera;
2. pobiera oczekujące komendy wysyłki (`POST /agent/v1/commands`);
3. wykonuje SMS-y na modemach przez `/tool sms send` (GSM-7) albo przez
   at-chat/PDU (UCS-2 i wielosegmentowe);
4. raportuje wynik (`POST /agent/v1/commands/{id}/result`);
5. wypycha odebrane SMS-y (`POST /agent/v1/inbox`) i sprząta je na routerze
   dopiero gdy tikker je zapisze (`inbox_delete` → `/inbox/ack`);
6. raportuje stan modemów (`POST /agent/v1/checkin`) — z tego tikker czerpie
   zdrowie kanałów.

## Zapytania i deduplikacja

- Komenda wysyłki ma `message_id`. Agent **deduplikuje** po nim: jeśli przyjdzie
  drugi raz (tikker nie dostał wyniku i ponawia), agent nie wysyła drugi raz,
  tylko powtarza ostatni wynik.
- Wynik `accepted`/`permanent` jest **zapamiętywany** przez tikker na
  `ResultTTL` (24 h): redelivery danej komendy kończy się bez kontaktu z agentem.
- Brak potwierdzenia w czasie `SendTimeout` → `TransientAmbiguous` (router mógł
  wysłać); komunikat wraca do kolejki jako możliwy duplikat (ADR 0002).
- Komenda wydana agentowi jest **ukryta** przed kolejnymi zapytaniami przez
  `SendTimeout/2` (dzierżawa redelivery), więc dwa nakładające się zapytania
  nie dostaną jej obu. Po tym czasie jest wydawana ponownie (zgubiona
  odpowiedź, restart skryptu). Wynik komendy wydanej więcej niż raz jest
  zawsze niejednoznaczny: `accepted` i `transient` → `TransientAmbiguous`,
  `permanent` → błąd trwały z flagą możliwego duplikatu. Przy `accepted`
  wiadomość wraca do kolejki z `possible_duplicate`, a ponowienie kończy się
  od razu jako `sent` z zapamiętanego wyniku (bez kontaktu z agentem).
  Ograniczenie: jeśli to była ostatnia dozwolona próba (`max_attempts`),
  wiadomość kończy jako `failed` z `possible_duplicate`, choć wyszła —
  interfejs konektora nie pozwala zgłosić „wysłano, ale możliwy duplikat”.

## Usuwanie odebranych SMS-ów

RouterOS używa ponownie `.id` po usunięciu wpisu, więc samo `remote_id` nie
identyfikuje wiadomości. tikker rozróżnia wpisy po porcie, `remote_id` i
odcisku treści. Każda pozycja `inbox_delete` zawiera, oprócz `port` i
`remote_id`, także `phone` i `timestamp` w postaci wypchniętej przez agenta:

```json
{"port": "lte1", "remote_id": "*1", "phone": "+48511222333", "timestamp": "2026-11-23 10:00:00"}
```

Agent usuwa wpis z routera **tylko wtedy**, gdy pod tym `.id` nadal jest wpis
o tym samym numerze i czasie. Potwierdza (`/inbox/ack`) każdą pozycję, także
taką, której wpis już zniknął lub zawiera inną wiadomość, odsyłając ją bez
zmian (z `phone` i `timestamp`). Potwierdzenie bez `phone`/`timestamp`
(starsze skrypty) usuwa wszystkie oczekujące usunięcia pod tym `remote_id`.

## Health

Stan kanału agenta pochodzi z ostatniego `checkin`:
- `Health`/`Probe` zwracają to, co agent zaraportował (wersja RouterOS, board,
  porty, rejestracja, RSSI);
- agent starszy niż `stale_after` (domyślnie 90 s) = kanał down (transient).

## Autoryzacja

Każde urządzenie `agent` ma `agent_token` (32–256 znaków, przechowywane
zaszyfrowane jak hasło routera). Skrypt musi go wysyłać jako
`Authorization: Bearer <token>`. Porównanie jest w stałym czasie; token
współdzielony przez dwa urządzenia jest odrzucany.

## Endpointy (prefix `/agent/v1`)

| Metoda + ścieżka | Rola |
|---|---|
| `POST /agent/v1/checkin` | raport stanu modemów + wersji (health) |
| `POST /agent/v1/commands` | pobranie oczekujących komend wysyłki (long-poll) |
| `POST /agent/v1/commands/{message_id}/result` | wynik wysyłki (`accepted`/`transient`/`ambiguous`/`permanent`) |
| `POST /agent/v1/inbox` | wypchnięcie odebranych SMS-ów; odpowiedź `inbox_delete` |
| `POST /agent/v1/inbox/ack` | potwierdzenie, że router usunął wpisy |
| `GET  /agent/v1/status` | stan sesji (diagnostyka) |

Zwykłe błędne Body są `application/problem+json` (RFC 9457), jak reszta API.

## Konfiguracja serwera

Sekcja `agent` w `tikker.yaml` (lub zmienne środowiskowe):

| Klucz | Zmienna | Domyślnie | Znaczenie |
|---|---|---|---|
| `send_timeout` | `TIKKER_AGENT_SEND_TIMEOUT` | `30s` | limit jednej wysyłki (agent musi odpytać, wysłać i zaraportować); z niego dispatcher liczy dzierżawę |
| `stale_after` | `TIKKER_AGENT_STALE_AFTER` | `90s` | wiek ostatniego `checkin`, po którym kanał jest `down` |
| `max_segments` | `TIKKER_AGENT_MAX_SEGMENTS` | `10` | najdłuższa wiadomość (w segmentach); `1` wyłącza łączenie |
| `ucs2` | `TIKKER_AGENT_UCS2` | `true` | czy wysyłać UCS-2; `false` = tylko GSM-7 |

Pozostałe limity są stałe w kodzie (`internal/agent`, `Options`) i **nie** są
konfigurowalne: jedna wysyłka naraz na port, do 500 buforowanych SMS-ów
przychodzących na port, wyniki końcowe pamiętane przez 24 h.

## Weryfikacja

- **Działa i testowane z fake-agentem**: `internal/agent` (prawie 94% testów)
  + `TestAgentEndToEnd` w `internal/app` (fakeagent przez prawdziwe HTTP:
  checkin, komenda → `sent`, inbound → odebrane, skrzynka opróżniona, brak
  tokenu/treści w logach).
- **[WERYFIKACJA: wymaga T00 / sprzętu]** konkretne `:local`, certyfikaty CA,
  pobieranie na prawdziwym RouterOS 7 i wysyłka przez at-chat/PDU — patrz
  `deploy/routeros/agent.rsc`. Test na sprzęcie to zadanie T51.