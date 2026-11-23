# SPDX-FileCopyrightText: 2026 The tikker Authors
# SPDX-License-Identifier: GPL-3.0-or-later
#
# tikker: RouterOS 7 setup script (T41/T42). Guides: docs/deploy-mikrotik.md, docs/deploy-server.md.
#
# STATUS: NOT VERIFIED ON HARDWARE (task T00 is deferred). Commands follow the RouterOS 7
# documentation and docs/research/routeros-sms.md; items marked [WERYFIKACJA] are assumptions.
# Review the script and the variables below, then upload it and run:
#
#   /import file-name=setup.rsc
#
# The script is idempotent: objects are looked up with find (by name or comment) before they
# are created; re-running updates settings instead of duplicating objects. It never removes
# anything. mode="container" configures the router and runs tikker in a RouterOS container;
# mode="server" only prepares the router for a tikker instance running elsewhere.
#
# Do NOT commit a copy of this file with real passwords filled in.

# ---------------------------------------------------------------- settings
# Operating mode: "container" (tikker runs on this router) or "server" (tikker runs elsewhere).
:local mode "container"

# LTE/5G port as shown by /interface lte print (and used by /tool sms).
:local ltePort "lte1"

# Dedicated RouterOS account used by tikker (one account per tikker instance).
:local apiUser "tikker"
# Password of that account. MUST be changed: the script stops while it is CHANGE-ME.
# Generate a long random one (min. 12, better 24+ characters) and enter the same value in tikker.
:local apiPassword "CHANGE-ME"

# Optional /tool sms "secret". Leave empty to skip. [WERYFIKACJA: the docs say receiving
# needs a secret; whether it affects sending over REST is unclear, see routeros-sms.md W17]
:local smsSecret ""

# Address of this router as seen by tikker (goes into the certificate SAN).
# container mode: the veth gateway (must match vethGateway below).
# server mode: the router's LAN or VPN address that tikker connects to.
:local routerAddr "172.17.0.1"

# server mode only: IP of the machine running tikker (www-ssl and the firewall are limited to it).
:local serverIp "192.168.88.10"

# ---- container mode only
:local vethName "veth-tikker"
:local bridgeName "br-tikker"
:local vethGateway "172.17.0.1"
:local vethAddress "172.17.0.2"
:local vethPrefix "24"
:local vethNetwork "172.17.0.0/24"

# Image (the published image name is a placeholder until the first release).
:local image "ghcr.io/m4drak/tikker:latest"
# Storage medium (USB/microSD recommended: internal flash wears out, see the guide).
# Path relative to the router root, e.g. "usb1/tikker" or "disk1/tikker".
:local mediaPath "usb1/tikker"
# Container user. The mount created by RouterOS is owned by root, while the image runs as
# 65532:65532 and might not be able to write to /data. "0:0" works around that.
# For least privilege (F-04) run as the image's non-root user and give it the data mount:
#   :local ctrUser "65532:65532"
# and make the media dir writable by that UID (e.g. chown 65532:65532 on the host that
# mounted the media, if RouterOS/media support it). Keep 0:0 only when you cannot chown.
# [WERYFIKACJA: whether /container supports user= on your version and whether /data is
# writable; if the router rejects the option, remove it from the "container add" line below.]
:local ctrUser "0:0"
# Memory limit of the container. [WERYFIKACJA: syntax and units of memory-max]
:local ctrMemoryMax "64M"

# Admin password created on the first start (min. 12 characters). MUST be changed.
:local adminPassword "CHANGE-ME"
# Public URL of the panel (cookies and links); leave empty if unknown.
:local baseUrl ""

# Publish the panel (tcp/8080) on the router with dst-nat? "no" is the safe default.
# WARNING: "yes" exposes an admin panel without TLS (plain HTTP) to the interfaces in the
# panelLanList interface list. Never expose it to the WAN.
:local exposePanel "no"
:local panelLanList "LAN"

# ---------------------------------------------------------------- checks
:if ($mode != "container" && $mode != "server") do={ :error "setup.rsc: mode must be container or server" }
:if ($apiPassword = "CHANGE-ME" || [:len $apiPassword] < 12) do={ :error "setup.rsc: set apiPassword (min. 12 characters)" }
:if ($mode = "container" && ($adminPassword = "CHANGE-ME" || [:len $adminPassword] < 12)) do={ :error "setup.rsc: set adminPassword (min. 12 characters)" }

:local sourceCidr $vethNetwork
:if ($mode = "server") do={ :set sourceCidr "$serverIp/32" }

:log info "tikker setup: start, mode=$mode"

# ---------------------------------------------------------------- user and group
# Minimal policies: api + rest-api (REST), read + write (/tool sms send, inbox).
# [WERYFIKACJA] `test` may also be required for /tool commands (forum reports differ,
# routeros-sms.md W2); the T00 policy matrix will confirm the minimal set. REST caches
# credentials for ~10 minutes after a change.
:if ([:len [/user group find name=tikker]] = 0) do={
  /user group add name=tikker policy=api,rest-api,read,write comment="tikker SMS gateway"
} else={
  /user group set [find name=tikker] policy=api,rest-api,read,write
}
:if ([:len [/user find name=$apiUser]] = 0) do={
  /user add name=$apiUser group=tikker password=$apiPassword address=$sourceCidr comment="tikker SMS gateway"
} else={
  /user set [find name=$apiUser] group=tikker password=$apiPassword address=$sourceCidr
}

# ---------------------------------------------------------------- SMS
# auto-erase: the router removes the oldest messages when storage is full (docs: since 6.44.6).
/tool sms set port=$ltePort receive-enabled=yes auto-erase=yes
# Not present in every version: do not fail the whole script. [WERYFIKACJA]
:do { /tool sms set remove-sent-sms-after-send=yes } on-error={ :log warning "tikker setup: remove-sent-sms-after-send is not supported here" }
:if ([:len $smsSecret] > 0) do={ /tool sms set secret=$smsSecret }

# ---------------------------------------------------------------- certificate and www-ssl
# Self-signed certificate for the REST API. tikker pins its SHA-256 fingerprint
# (/certificate get tikker-api fingerprint), so CA and hostname checks are not needed.
:if ([:len [/certificate find name=tikker-api]] = 0) do={
  /certificate add name=tikker-api common-name=$routerAddr subject-alt-name="IP:$routerAddr" key-usage=digital-signature,key-encipherment,tls-server key-size=2048 days-valid=3650
  /certificate sign tikker-api
  # sign may run asynchronously: wait until the private key exists (up to ~60 s).
  :local waited 0
  :while ([:len [/certificate find name=tikker-api private-key=yes]] = 0 && $waited < 60) do={ :delay 1s; :set waited ($waited + 1) }
}
:if ([:len [/certificate find name=tikker-api private-key=yes]] = 0) do={ :error "setup.rsc: certificate tikker-api was not signed" }

# Only the tikker source may reach the REST API (container: veth subnet; server: server IP).
/ip service set www-ssl certificate=tikker-api address=$sourceCidr disabled=no
# Plain HTTP REST stays off.
/ip service set www disabled=yes

# ---------------------------------------------------------------- firewall (input)
# The default config drops input from outside the LAN. Allow tikker to reach www-ssl; the rule
# goes first (place-before=0) so it is evaluated before the final drop.
:if ([:len [/ip firewall filter find comment="tikker: REST API"]] = 0) do={
  /ip firewall filter add chain=input action=accept protocol=tcp dst-port=443 src-address=$sourceCidr place-before=0 comment="tikker: REST API"
}

# ---------------------------------------------------------------- container mode
:if ($mode = "container") do={

  # Requires device-mode container=yes (physical confirmation) and the container package.
  # Checked here only to give a clear message. [WERYFIKACJA: package name/status on 7.x]
  :if ([:len [/system package find name=container !disabled]] = 0) do={ :error "setup.rsc: the container package is not installed/enabled (see the guide)" }

  # --- network: veth + dedicated bridge
  :if ([:len [/interface bridge find name=$bridgeName]] = 0) do={ /interface bridge add name=$bridgeName comment="tikker" }
  :if ([:len [/interface veth find name=$vethName]] = 0) do={
    /interface veth add name=$vethName address="$vethAddress/$vethPrefix" gateway=$vethGateway comment="tikker"
  }
  :if ([:len [/ip address find address="$vethGateway/$vethPrefix" interface=$bridgeName]] = 0) do={
    /ip address add address="$vethGateway/$vethPrefix" interface=$bridgeName comment="tikker"
  }
  :if ([:len [/interface bridge port find bridge=$bridgeName interface=$vethName]] = 0) do={
    /interface bridge port add bridge=$bridgeName interface=$vethName comment="tikker"
  }
  # No masquerade is added: tikker only talks to the router at vethGateway, and the image is
  # pulled by RouterOS itself, not by the container.
  # Isolate the container from the LAN (F-04): by default forward out of the tikker bridge is
  # dropped. The only outbound routes are the container -> router REST call (input chain, the
  # accept rule above) and DNS at the gateway; the image is pulled by RouterOS, not by the
  # container. The optional panel dst-nat has its own forward accept above (place-before=0), so
  # this drop placed later does not break it. [WERYFIKACJA: default forward policy of the router]
  :if ([:len [/ip firewall filter find comment="tikker: block container to LAN"]] = 0) do={
    /ip firewall filter add chain=forward action=drop src-address=$vethNetwork place-before=6 comment="tikker: block container to LAN"
  }

  # --- optional panel publishing (dst-nat)
  :if ($exposePanel = "yes") do={
    :log warning "tikker setup: publishing the panel over HTTP to list $panelLanList (never to the WAN)"
    :if ([:len [/ip firewall nat find comment="tikker: panel dst-nat"]] = 0) do={
      /ip firewall nat add chain=dstnat action=dst-nat protocol=tcp dst-port=8080 in-interface-list=$panelLanList to-addresses=$vethAddress to-ports=8080 comment="tikker: panel dst-nat"
    }
    # The default forward chain may drop dst-nat'ed traffic; allow it from the LAN list only.
    :if ([:len [/ip firewall filter find comment="tikker: panel forward"]] = 0) do={
      /ip firewall filter add chain=forward action=accept protocol=tcp dst-port=8080 dst-address=$vethAddress in-interface-list=$panelLanList place-before=0 comment="tikker: panel forward"
    }
  }

  # --- /container/config: registry and temporary extraction directory.
  # tmpdir on external media, otherwise extracting the image fills the internal flash.
  /container config set registry-url=https://registry-1.docker.io tmpdir="$mediaPath/pull"

  # --- data mount (SQLite and secret.key live here; back it up!)
  :if ([:len [/container mounts find name=tikker-data]] = 0) do={
    /container mounts add name=tikker-data src="$mediaPath/data" dst=/data comment="tikker"
  }

  # --- environment variables. Values are visible in /container/envs and in router backups
  # and exports, so treat those as sensitive.
  :local setEnv do={
    :if ([:len [/container envs find name=tikker key=$1]] = 0) do={
      /container envs add name=tikker key=$1 value=$2
    } else={
      /container envs set [find name=tikker key=$1] value=$2
    }
  }
  $setEnv "TIKKER_ADMIN_PASSWORD" $adminPassword
  # https:// to the gateway needs no TIKKER_ALLOWED_HTTP_HOSTS: the REST API is reached over TLS
  # with a pinned fingerprint (see the guide).
  :if ([:len $baseUrl] > 0) do={ $setEnv "TIKKER_BASE_URL" $baseUrl }

  # --- container stdout goes to the "container" logging topic
  :if ([:len [/system logging find topics=container]] = 0) do={ /system logging add topics=container action=memory }

  # --- the container itself
  :if ([:len [/container find comment="tikker"]] = 0) do={
    /container add remote-image=$image interface=$vethName envlist=tikker mounts=tikker-data root-dir="$mediaPath/root" logging=yes start-on-boot=yes user=$ctrUser memory-max=$ctrMemoryMax dns=$vethGateway comment="tikker"
    :log info "tikker setup: container added, extracting the image"
    # Pulling and extracting is asynchronous: wait for status=stopped (up to ~5 min).
    :local waited 0
    :while ([/container get [find comment="tikker"] status] != "stopped" && $waited < 300) do={ :delay 1s; :set waited ($waited + 1) }
  } else={
    :log info "tikker setup: container already exists (updating the image: see the guide)"
  }
  :if ([/container get [find comment="tikker"] status] = "stopped") do={ /container start [find comment="tikker"] }
}

:log info "tikker setup: done"
:put "tikker setup: done. REST API certificate fingerprint (enter it when adding the device in tikker):"
:put [/certificate get tikker-api fingerprint]
:put "Router address for tikker: https://$routerAddr (user $apiUser)"
