<!--
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
-->

# Metryki Prometheus

tikker wystawia metryki w formacie tekstowym Prometheus na **`GET /metrics`**
(bez uwierzytelnienia, tylko do odczytu; endpoint nie wymaga tokenu API).

Format odpowiedzi: `text/plain; version=0.0.4`, nagłówek `Cache-Control: no-store`.
Metriki są renderowane na żywo ze snapshotu kolejki, kontekstu zdrowia i licznika
startu — bez zewnętrznej zależności (czysty expositer text format, zgodnie z
zasadą „brak zależności bez potrzeby", SPEC §11).

## Konwencja nazw

Wszystkie metryki mają prefix `tikker_`.

## Liczniki kolejki (monotoniczne; `_total`)

| Metryka | Opis |
|---|---|
| `tikker_start_time_seconds` | początek procesu (unix) |
| `tikker_queue_sent_total` | wiadomości wysłane |
| `tikker_queue_failed_total` | wiadomości zakończone `failed` |
| `tikker_queue_retried_total` | wiadomości zwrócone do kolejki po błędzie |
| `tikker_queue_possible_duplicates_total` | wyniki z flagą możliwego duplikatu (ADR 0002) |
| `tikker_queue_lease_lost_total` | wyniki odrzucone po utracie dzierżawy |
| `tikker_queue_store_errors_total` | błędy store (acquire, next due, zapis wyniku) |
| `tikker_queue_auth_errors_total` | błędy Auth (kanał zdegradowany) |
| `tikker_queue_recovered_leases_total` | wiadomości odzyskane po wygasłej dzierżawie |
| `tikker_queue_expired_total` | wiadomości zakończone przez `ExpireQueued` |

## Poziomy / stany

| Metryka | Typ | Opis |
|---|---|---|
| `tikker_queue_configured_channels` | gauge | liczba skonfigurowanych kanałów |
| `tikker_channel_sent_total` itd. | counter | jak wyżej, per `channel_id` |
| `tikker_channel_active` | gauge | 1 gdy kanał może brać dzierżawy |
| `tikker_channel_sending` | gauge | 1 gdy wysyłka w toku |
| `tikker_channel_health_severity` | gauge | 0=nieznany, 1=ok, 2=degraded, 3=down; etykiety `channel_id`,`port` |
| `tikker_channel_checked` | gauge | 1 po pierwszym checku |
| `tikker_channel_auth_pending` | gauge | 1 gdy Auth oczekuje na powodzenie |
| `tikker_channel_signal_rssi_dbm` | gauge | ostatnie RSSI modemu (dBm), brak gdy nieznane |

## Przykład scrape

```sh
curl -s http://127.0.0.1:8080/metrics
# TYPE tikker_start_time_seconds gauge
# TYPE tikker_queue_sent_total counter
tikker_queue_sent_total 7
# TYPE tikker_channel_health_severity gauge
tikker_channel_health_severity{channel_id="01...",port="lte1"} 1
```

Metryki nie zawierają treści SMS ani numerów, ale endpoint `/metrics` jest
nieuwierzytelniony i ujawnia identyfikatory kanałów, nazwy portów, liczniki oraz
poziom sygnału (RSSI). Nie wystawiaj go publicznie: ogranicz dostęp zaporą
sieciową lub reverse proxy (np. allowlista adresów skanera Prometheus).