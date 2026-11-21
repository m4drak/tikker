# Wdrożenie serwerowe

Tikker działa na zwykłym serwerze Linux (amd64, arm64, armv7) i łączy się z jednym lub wieloma routerami MikroTik
przez REST API (LAN lub VPN). To tryb dla routerów bez obsługi kontenerów (MIPS, mało RAM), dla wielu urządzeń
i dla osób, które wolą trzymać panel poza routerem. Wariant kontenerowy na routerze:
[deploy-mikrotik.md](deploy-mikrotik.md).

> **Status weryfikacji.** Zweryfikowano: składnię `compose.yaml` i `Caddyfile` (`docker compose config`,
> `caddy validate`), unit systemd (`systemd-analyze verify`) oraz start binarki z przykładową konfiguracją.
> **Niezweryfikowane na sprzęcie** (T00 odłożone): skrypt `setup.rsc` i konfiguracja WireGuard po stronie RouterOS,
> polityki użytkownika RouterOS, zachowanie `/tool sms` przez REST. Zob. [docs/research/routeros-sms.md](research/routeros-sms.md).

## 1. Konfiguracja routera (mode=server)

Router potrzebuje: użytkownika z minimalnymi politykami, włączonego SMS, `www-ssl` z certyfikatem i reguły firewall.
Skrypt [`deploy/routeros/setup.rsc`](../deploy/routeros/setup.rsc) robi to w trybie `server`:

1. Edytuj zmienne na górze skryptu:
   - `mode` = `"server"`;
   - `ltePort` (np. `lte1`), `apiUser`, `apiPassword` (min. 12 znaków, losowe);
   - `routerAddr` = adres routera, pod którym tikker będzie go widział (LAN albo adres tunelu WireGuard);
   - `serverIp` = adres IP serwera tikkera (z punktu widzenia routera: w tunelu to adres klienta WireGuard);
2. Wgraj i uruchom: `/import file-name=setup.rsc`. Szczegóły działania skryptu, polityk i firewalla: sekcja 4
   w [deploy-mikrotik.md](deploy-mikrotik.md). W trybie `server` pomijane są `veth`, bridge, mount i kontener.
3. Zapisz wypisany **odcisk SHA-256** certyfikatu (`/certificate get tikker-api fingerprint`): wpiszesz go
   w tikkerze (`tls_mode=fingerprint`). `www-ssl` i użytkownik są ograniczone adresem `serverIp/32`.

Router wymaga RouterOS ≥ 7.16 (research W1, robocze minimum). Wszystko w skrypcie jest niezweryfikowane na sprzęcie.

## 2. Docker Compose (zalecane)

Plik [`deploy/docker/compose.yaml`](../deploy/docker/compose.yaml) uruchamia tikkera z wolumenem `/data`,
healthcheckiem, `read_only` i `cap_drop: ALL`, oraz opcjonalnie Caddy (profil `tls`) z automatycznym certyfikatem.

```sh
cd deploy/docker
cat > .env <<'EOF'
TIKKER_ADMIN_PASSWORD=<min. 12 znaków; tylko przy pustej bazie>
TIKKER_BASE_URL=https://sms.example.com
TIKKER_DOMAIN=sms.example.com
EOF
chmod 600 .env
docker compose up -d                  # sam tikker na 127.0.0.1:8080
docker compose --profile tls up -d    # + Caddy na portach 80/443
docker compose ps                     # kolumna "healthy"
```

- Obraz: `TIKKER_IMAGE` (domyślnie `ghcr.io/m4drak/tikker:latest`, **placeholder do czasu publikacji obrazów**).
  Własny obraz: `make docker` (tag `tikker:<wersja>`), potem `TIKKER_IMAGE=tikker:<wersja>`.
- Port jest domyślnie publikowany tylko na `127.0.0.1`. Bez profilu `tls` panel działa po HTTP, więc
  w zaufanej sieci ustaw `TIKKER_BIND=0.0.0.0`, a w Internecie zawsze używaj `tls`.
- `TIKKER_TRUSTED_PROXIES` jest ustawione na podsieć sieci compose (`172.28.0.0/24`), z której przychodzi
  Caddy; tikker wierzy nagłówkowi `X-Forwarded-For` tylko stamtąd (limity logowania, audyt).
- Router dostępny po `http://`? Tylko w labie: `TIKKER_ALLOWED_HTTP_HOSTS=<IP>`. Domyślnie wymagane jest HTTPS.
- Token API: panel albo `docker compose exec tikker /tikker token create --name ci --scope sms:send`
  (obraz nie ma powłoki, ale samo `/tikker` jest wykonywalne).
- Podman: `podman compose` lub `podman-compose` z tym samym plikiem [niezweryfikowane].

## 3. Binarka i systemd

1. Zbuduj lub pobierz binarkę (`make build` → `bin/tikker`, `CGO_ENABLED=0`) i zainstaluj:
   `install -m 0755 bin/tikker /usr/local/bin/tikker`.
2. Plik środowiska (hasła, tajne wartości; **nie** w repozytorium):

   ```sh
   install -d -m 0750 /etc/tikker
   install -m 0600 /dev/null /etc/tikker/tikker.env
   cat > /etc/tikker/tikker.env <<'EOF'
   TIKKER_BASE_URL=https://sms.example.com
   TIKKER_TRUSTED_PROXIES=127.0.0.1
   TIKKER_ADMIN_PASSWORD=<min. 12 znaków>
   EOF
   ```
3. Unit [`deploy/systemd/tikker.service`](../deploy/systemd/tikker.service):
   `cp deploy/systemd/tikker.service /etc/systemd/system/ && systemctl daemon-reload && systemctl enable --now tikker`.
   - `DynamicUser=yes` + `StateDirectory=tikker` → dane w `/var/lib/tikker` (baza, `secret.key`);
   - hardening: `ProtectSystem=strict`, `NoNewPrivileges`, `PrivateTmp`, pusty `CapabilityBoundingSet`,
     `RestrictAddressFamilies`, `SystemCallFilter`, `MemoryMax=256M`;
   - nasłuch `127.0.0.1:8080` (reverse proxy z TLS z sekcji 5); zmienisz przez `TIKKER_LISTEN` w pliku środowiska.
4. `systemctl status tikker`, `journalctl -u tikker -f`, `/tikker healthcheck` lub `curl http://127.0.0.1:8080/readyz`.

Token API utwórz w panelu (przy `DynamicUser` nie ma stałego konta systemowego, z którego dałoby się uruchomić `tikker token create`).

## 4. Dodanie routera w tikkerze

W panelu (urządzenia) lub przez `POST /api/v1/devices`:

- `base_url`: `https://<adres routera>` (LAN lub adres w tunelu WireGuard);
- `username`/`password`: konto `apiUser` z `setup.rsc`;
- `tls_mode`: `fingerprint` z odciskiem certyfikatu `tikker-api`;
- potem test (`POST /devices/{id}/test`) i kanał dla portu LTE.

## 5. TLS: Caddy

Profil `tls` w compose używa [`deploy/docker/Caddyfile`](../deploy/docker/Caddyfile): automatyczny certyfikat
Let's Encrypt dla `TIKKER_DOMAIN`, `reverse_proxy tikker:8080`, HSTS. Wymagania: rekord DNS wskazujący na serwer,
otwarte porty 80 i 443 (challenge ACME), `TIKKER_BASE_URL=https://<domena>`. Przy binarce + systemd wystarczy
analogiczny wpis, np. `sms.example.com { reverse_proxy 127.0.0.1:8080 }`, i `TIKKER_TRUSTED_PROXIES=127.0.0.1`.
Alternatywa: wbudowany HTTPS tikkera z plików (`TIKKER_TLS_CERT_FILE`, `TIKKER_TLS_KEY_FILE`), zalecany jednak reverse proxy.

## 6. Routery za CGNAT: WireGuard

Gdy router LTE nie ma publicznego adresu (CGNAT operatora), serwer nie może zainicjować połączenia do routera.
W MVP rozwiązaniem jest tunel, który zestawia **router** do serwera z publicznym IP (konektor `agent`
jest poza MVP, SPEC §2.1). Poniższe adresy są przykładowe; konfiguracja po stronie RouterOS jest
**niezweryfikowana na sprzęcie** (składnia wg dokumentacji RouterOS 7).

Założenia: tunel `10.99.0.0/24`, serwer `10.99.0.1`, router `10.99.0.2`, serwer ma publiczny adres
`vpn.example.com` i UDP/51820.

**Serwer (Linux, `wg-quick`)** `/etc/wireguard/wg0.conf`:

```ini
[Interface]
Address = 10.99.0.1/24
ListenPort = 51820
PrivateKey = <klucz prywatny serwera: wg genkey>

[Peer]
# router MikroTik
PublicKey = <klucz publiczny routera>
AllowedIPs = 10.99.0.2/32
```

`wg genkey | tee server.key | wg pubkey > server.pub`, `systemctl enable --now wg-quick@wg0`, otwórz UDP/51820.

**Router (RouterOS 7)**:

```
/interface wireguard add name=wg-tikker listen-port=13231 comment="tikker"
/interface wireguard print                                  # skopiuj public-key do [Peer] serwera
/ip address add address=10.99.0.2/24 interface=wg-tikker
/interface wireguard peers add interface=wg-tikker public-key="<klucz publiczny serwera>" \
    endpoint-address=vpn.example.com endpoint-port=51820 allowed-address=10.99.0.1/32 \
    persistent-keepalive=25s comment="tikker server"
# firewall: tunel nie jest w liście WAN, a tikker ma dostęp tylko do www-ssl
/ip firewall filter add chain=input action=accept protocol=udp dst-port=13231 place-before=0 comment="wg-tikker handshake"
```

- `persistent-keepalive` utrzymuje mapowanie NAT, dzięki czemu serwer może wołać REST routera przez tunel.
- W `setup.rsc` ustaw `mode="server"`, `routerAddr="10.99.0.2"`, `serverIp="10.99.0.1"`. Certyfikat dostaje SAN
  `IP:10.99.0.2`, a reguła firewall i `www-ssl` ograniczają dostęp do `10.99.0.1/32`.
- W tikkerze: `base_url=https://10.99.0.2`.
- Serwer: żadnych dodatkowych tras (adres `10.99.0.2` jest w podsieci tunelu); jeśli tikker działa w kontenerze, ruch
  idzie przez hosta (sieć `bridge` kontenera z NAT), więc wystarczy, że host ma interfejs `wg0`.
- Wiele routerów: kolejne `[Peer]` z `AllowedIPs = 10.99.0.3/32` itd., każdy router z własnym kluczem i kontem.
- Sprawdź: `wg show`, z serwera `curl -k https://10.99.0.2/` (odpowiedź 401/redirect świadczy o dostępności).

## 7. Kopie zapasowe

Backup obejmuje katalog danych (`tikker-data` w Dockerze lub `/var/lib/tikker`): `tikker.db`, pliki WAL i **`secret.key`**
(klucz AES-256-GCM haseł urządzeń). Bez klucza kopia bazy nie pozwoli odszyfrować haseł routerów; przechowuj oba
razem i chroń jak sekret. Zalecany sposób: zatrzymaj usługę na czas kopiowania albo użyj `sqlite3 tikker.db ".backup 'kopia.db'"`
[niezweryfikowane: narzędzie nie jest w obrazie, użyj go na hoście przy bind mount].

```sh
docker compose stop tikker
docker run --rm -v tikker_tikker-data:/data -v "$PWD":/backup alpine tar czf /backup/tikker-data.tgz -C /data .
docker compose start tikker
```

(Nazwa wolumenu zależy od nazwy projektu compose: `docker volume ls`.) Przywracanie: odwrotnie, na zatrzymanej usłudze,
z właścicielem plików `65532:65532`.

## 8. Aktualizacje

- Docker: zmień tag w `TIKKER_IMAGE` (zalecane przypięte wersje), `docker compose pull && docker compose up -d`.
  Migracje bazy uruchamiają się przy starcie; przed większą aktualizacją zrób kopię (sekcja 7).
- Binarka: podmień `/usr/local/bin/tikker`, `systemctl restart tikker`. Zatrzymanie jest łagodne (do
  `TIKKER_SHUTDOWN_TIMEOUT`, domyślnie 30 s, zwalnia dzierżawy wysyłek).
- Cofnięcie wersji wymaga kopii bazy sprzed aktualizacji.

## 9. Bezpieczeństwo

- Panel wystawiaj tylko przez HTTPS (Caddy/inny reverse proxy), z `TIKKER_BASE_URL` ustawionym na publiczny adres.
- `TIKKER_TRUSTED_PROXIES` wpisuj tylko adresy własnego proxy; błędna wartość pozwala podszywać się pod IP klienta.
- Konto RouterOS `tikker` ma tylko `api,rest-api,read,write` i jest ograniczone adresem; nie dawaj mu `policy`/`ssh`.
- Plik `.env` i `/etc/tikker/tikker.env` mają mieć uprawnienia `0600`. Nie commituj ich ani zrzutów z numerami.
- Zob. również sekcję „Bezpieczeństwo” w [deploy-mikrotik.md](deploy-mikrotik.md).
