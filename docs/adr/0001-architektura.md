# ADR 0001: Architektura – jeden kontener Go, uruchamiany na MikroTiku lub na serwerze

- Status: zaakceptowana
- Data: 2026-11-19

## Kontekst

Chcemy używać urządzeń MikroTik z modemem LTE/5G i kartą SIM jako bramek SMS.
Rozważone warianty:

| Wariant | Opis | Główna wada |
|---|---|---|
| A | Sam skrypt RouterOS (scheduler + `/tool fetch`) | ubogi język, brak kolejki, testów, UI; trudny przy wielu urządzeniach |
| B | Serwer odpytujący REST API RouterOS | router musi być osiągalny (CGNAT → VPN) |
| C | Serwer + agent-skrypt na routerze (pull przez HTTPS) | dwa komponenty, w tym skrypt RouterOS |
| D | Aplikacja w kontenerze na MikroTiku | tylko ARM/ARM64/x86 z pakietem `container` |

## Decyzja

Budujemy **jedną aplikację w Go** (`tikker`), dystrybuowaną jako **wieloarchitekturowy obraz OCI**
(`linux/amd64`, `linux/arm64`, `linux/arm/v7`) oraz jako statyczna binarka. Ten sam obraz działa:

1. **Na MikroTiku** (pakiet `container`): łączy się z lokalnym REST API routera przez interfejs `veth`.
2. **Na osobnym serwerze** (Docker/Podman/systemd): łączy się z REST API jednego lub wielu routerów
   (LAN lub VPN, np. WireGuard).

Komunikacja z routerem odbywa się przez wymienny **konektor**. W MVP jest jeden konektor, `routeros-rest` (wariant B),
który obsługuje oba tryby wdrożenia. Konektor `agent` (wariant C, pull, działa za CGNAT bez VPN)
jest zaplanowany po MVP. Interfejs konektora od początku musi go umożliwiać.

Licencja: GNU GPL v3.0 (patrz `LICENSE`).

## Konsekwencje

- (+) Jeden kod, jeden obraz, dwa scenariusze wdrożenia. Na routerze nie ma logiki w języku skryptowym RouterOS.
- (+) Pełna kontrola nad kolejką, ponowieniami, segmentacją, uprawnieniami i UI. Projekt da się testować.
- (−) Na urządzeniach MIPS/SMIPS (wiele tańszych modeli LTE) kontener nie ruszy. Tam potrzebny jest tryb serwerowy.
- (−) Kontener na RouterOS wymaga włączenia `device-mode` (z fizycznym potwierdzeniem) i zwykle zewnętrznego
  nośnika. Aplikacja musi być oszczędna w RAM i liczbie zapisów na flash.
- (−) Urządzenia za CGNAT w trybie serwerowym wymagają VPN, dopóki nie powstanie konektor `agent`.
