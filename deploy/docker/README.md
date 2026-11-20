# Obraz kontenera tikker

Obraz to statyczna binarka `tikker` na `FROM scratch`: bez powłoki i menedżera pakietów,
z certyfikatami CA i strefami czasowymi wbudowanymi w binarkę, uruchamiany jako użytkownik
`65532:65532`. Jedyny zapisywalny katalog to `/data`. Rozmiar: ok. 19 MB nieskompresowany,
ok. 8 MB w archiwum (`docker save | gzip`). Architektury: `linux/amd64`, `linux/arm64`, `linux/arm/v7`.

## Budowanie

Z katalogu głównego repozytorium:

```sh
make docker          # obraz dla bieżącej platformy, załadowany do demona: tikker:<wersja>
make docker-buildx   # obraz multi-arch do dist/tikker-<wersja>.oci.tar (bez publikacji)
```

Wersja, commit i data trafiają do `--build-arg` z `git describe` (zob. `Makefile`).
Kompilacja odbywa się natywnie na `$BUILDPLATFORM` i krzyżowo dla celu, więc emulacja QEMU nie jest potrzebna.

Bazowy obraz budowania (`golang:1.26-alpine`) jest przypięty do digestu. Aby go odświeżyć:
`docker buildx imagetools inspect golang:1.26-alpine`.

## Uruchomienie

```sh
docker volume create tikker-data
docker run -d --name tikker \
  --read-only --tmpfs /tmp \
  -v tikker-data:/data \
  -e TIKKER_ADMIN_PASSWORD='<hasło administratora>' \
  -p 8080:8080 \
  tikker:<wersja>
```

Hasło administratora czytane jest z `TIKKER_ADMIN_PASSWORD` tylko przy pustej bazie.
Po pierwszym uruchomieniu można je usunąć z poleceń i z pliku środowiska.

Konfiguracja: zmienne `TIKKER_*` (SPEC §8). Obraz ustawia `TIKKER_DATA_DIR=/data` i `TIKKER_LISTEN=:8080`.

## Wolumen `/data` i klucz `secret.key`

- `/data/tikker.db` to baza SQLite, a `/data/secret.key` to klucz szyfrujący hasła urządzeń (AES-256-GCM).
- Jeśli `secret.key` nie istnieje, serwer generuje go przy pierwszym starcie i zapisuje z uprawnieniami `0600`.
  Klucz trzeba **zachować razem z kopią bazy**: bez niego zapisane hasła routerów nie dają się odszyfrować.
- Alternatywnie klucz można podać przez `TIKKER_SECRET_KEY` (wtedy plik nie jest potrzebny).
- Wolumen musi należeć do `65532:65532`. Nowy wolumen Docker kopiuje właściciela z obrazu, więc działa bez zmian.
  Przy bind mount na hoście wykonaj `chown -R 65532:65532 <katalog>`.

## Uprawnienia

- Proces działa jako `65532:65532`, bez `CAP_*` (zalecane `--cap-drop ALL`) i z `--read-only`.
- Obraz nie zawiera powłoki, więc `docker exec` uruchamia wyłącznie samo `/tikker`.
- Panel i API nasłuchują na `:8080`. Przed Internetem postaw reverse proxy z TLS (zob. T42).

## Healthcheck

Obraz ma `HEALTHCHECK CMD ["/tikker","healthcheck"]`, który odpytuje lokalny `/readyz`
(exit 0 tylko przy 200 OK). Stan: `docker inspect --format '{{.State.Health.Status}}' tikker`.
Do sondy liveness bez zależności od bazy służy `/tikker healthcheck --live` (`/healthz`).

## Tworzenie tokenu API

Token wypisywany jest raz, więc zapisz go od razu:

```sh
docker exec tikker /tikker token create --name ci --scope sms:send --scope sms:read
```

Dostępne zakresy: `sms:send`, `sms:read`, `admin` (domyślnie `admin`). Opcje `--user` i `--ttl` są opisane w `/tikker --help`.

## Pamięć

Obraz ustawia `GOGC=30` i `GOMEMLIMIT=24MiB`. Bez nich RSS w spoczynku wynosił ok. 60 MB
(cel SPEC §2.2: ≤ 40 MB). Limit jest miękki: przy większym ruchu Go wykonuje częstsze odśmiecanie,
czyli więcej pracy CPU, ale nie kończy się to błędem. Wartości można nadpisać przez `-e`.
