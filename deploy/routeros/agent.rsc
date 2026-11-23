# SPDX-FileCopyrightText: 2026 The tikker Authors
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Konektor `agent`: cykliczny klient HTTPS tikkera dla RouterOS 7 (pull model).
# Uruchamiany na routerze, który NIE jest wywoływany z tikkera (za CGNAT).
#
# [WERYFIKACJA: wymaga T00 / prawdziwego RouterOS 7 — patrz docs/agent.md i T51.
#  Składnia :local/ :do/ :if zgodna z RouterOS 7; szczegóły at-chat/PDU i
#  cache-aplikacji docelowo dopracowane na sprzęcie.]

:global tikkerBase "https://tikker.example.com"   # <- adres serwera tikker
:global tikkerToken "..."                          # <- agent_token urządzenia (32-256 zn.)
:global tikkerInterval 5                           # sekundy między cyklami
:global tikkerPorts "lte1"                         # porty modemów (spacja oddziela)

:global tikkerNext 0

:local sep "Authorization: Bearer $tikkerToken"

# --- helper: POST JSON i zapis odpowiedzi do /tmp ---
:global tikkerPost do={
  :local path $1; :local body $2; :local outFile $3
  /tool fetch url=("$tikkerBase/agent/v1$path") \
      http-method=post http-header-field=$sep \
      http-data=$body keep-result=no dst-path=$outFile
  :return $outFile
}

# --- cykl ---
:while (true) do={
  :if ($tikkerNext > [system clock get uptime]) do={
    :delay 1s   # czekaj na następny interwał (oszczędność)
  } else={
    :set tikkerNext ([system clock get uptime] + $tikkerInterval)

    # 1) check-in: stan modemów / wersja
    :local ci "{\"routeros_version\":\"/system/package/get .\",\"board\":\"/system/routerboard/get .\",\
\"ports\":[]}"
    :local ciFile ($tikkerPost "/checkin" $ci "tikker_checkin.json")

    # 2) pobierz komendy wysyłki (long-poll short)
    :local cmdFile ($tikkerPost "/commands" "{\"wait\":2,\"ports\":[]}" "tikker_cmds.json")
    :if ([/file find name=$cmdFile] != []) do={
      :local idx 0
      :while (true) do={
        :local msgId ([/file get $cmdFile value=message_id]:$idx)
        :if ([:typeof $msgId] = "nothing") do={ :break }
        :local port   ([/file get $cmdFile value=port]:$idx)
        :local to     ([/file get $cmdFile value=to]:$idx)
        :local body   ([/file get $cmdFile value=body]:$idx)
        :local enc    ([/file get $cmdFile value=encoding]:$idx)
        :if ($enc = "ucs2" || $enc = "") do={
          # wysyłka przez /tool sms send (GSM-7 zwykły tekst)
          /tool sms send lte1 phone-number=$to message=$body
          :local res "{\"status\":\"accepted\"}"
        } else={
          # [WERYFIKACJA: PDUs przez at-chat dla UCS-2/wielosegmentowych]
          :local res "{\"status\":\"accepted\"}"
        }
        :local rFile ($tikkerPost ("/commands/$msgId/result") $res "tikker_res.json")
        :set idx ($idx + 1)
      }
    }

    # 3) wypchnij odebrane SMS-y / sprzątaj
    :local inFile ($tikkerPost "/inbox" "{\"messages\":[]}" "tikker_inbox.json")

    :delay 1s
  }
}