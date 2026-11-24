# SPDX-FileCopyrightText: 2026 The tikker Authors
# SPDX-License-Identifier: GPL-3.0-or-later
#
# tikker "agent" connector: RouterOS side (pull model, works behind CGNAT).
# The router polls tikker over HTTPS; tikker never connects to the router.
# Protocol: docs/agent.md and internal/agent/wire.go (prefix /agent/v1).
#
# STATUS: NOT VERIFIED ON HARDWARE. [WERYFIKACJA T00/T51]
#   Written against the RouterOS 7 documentation only. Test it on a lab router
#   before production use: every line marked [WERYFIKACJA] is an assumption.
#
# Requirements:
#   - RouterOS 7.13 or newer (:serialize / :deserialize with JSON).
#   - The CA of tikker's certificate imported into /certificate (fetch runs
#     with check-certificate=yes).
#   - /tool sms port=<lte port> receive-enabled=yes for inbound SMS.
#   - The agent token must not contain a comma (it goes into
#     http-header-field, which is a comma-separated list).
#
# What this script does (deliberately small):
#   - check-in: RouterOS version, board and every /interface lte port with
#     its modem state;
#   - outbound: ONLY single-segment GSM-7 commands, sent with /tool sms send.
#     Anything else (UCS-2, multi-segment, PDU mode) is reported as
#     "permanent" with reason "unsupported", never as accepted. On the
#     tikker side set agent.ucs2: false and agent.max_segments: 1 so that
#     such commands are not produced;
#   - a result is reported only after /tool sms send returned; a failed send
#     is reported as "transient" (tikker retries it);
#   - deduplication by message_id in the global array tikkerDone (survives
#     script runs, NOT a reboot). A command seen again is answered with the
#     remembered final result; one whose send was started but never recorded
#     (run aborted mid-send) is answered "ambiguous" and never re-sent;
#   - inbound: entries of /tool sms inbox are pushed one by one; an entry is
#     removed only when tikker lists it in inbox_delete AND its phone and
#     timestamp still match (RouterOS reuses .id values). Every listed
#     reference is acknowledged.
#
# Install (one cycle per run; the scheduler interval is the poll interval):
#   /system script add name=tikker-agent policy=read,write,test source=<this file>
#   /system scheduler add name=tikker-agent interval=10s on-event="/system script run tikker-agent"
# [WERYFIKACJA: minimal policies for /tool fetch, /tool sms send and inbox]
#
# Do NOT commit a copy of this file with a real token filled in.

# ---------------------------------------------------------------- settings
:local base "https://tikker.example.com"
:local token "CHANGE-ME"
# Most commands taken per run (each send can take tens of seconds).
:local maxCommands 3
# Most inbox entries pushed per run.
:local maxPush 20
# A run older than this is considered dead and its lock is taken over.
:local lockTimeout 5m
:local agentVersion "tikker-agent.rsc/2"

# ---------------------------------------------------------------- helpers
# POST $body (a JSON string) to $base/agent/v1$path with the token. Returns
# the decoded JSON response (an array) or the string "" on any failure
# (network, TLS, non-2xx status, undecodable body).
:global tikkerPost do={
  :local res
  :local ok true
  :do {
    :set res [/tool fetch url=($base . "/agent/v1" . $path) check-certificate=yes \
      http-method=post http-data=$body output=user as-value \
      http-header-field=("Authorization: Bearer " . $token . ",Content-Type: application/json")]
  } on-error={ :set ok false }
  :if (!$ok) do={ :return "" }
  :if (($res->"status") != "finished") do={ :return "" }
  :local data ($res->"data")
  :if ([:len $data] = 0) do={ :return "" }
  :local out ""
  :do { :set out [:deserialize from=json value=$data] } on-error={ :set out "" }
  :return $out
}

# ---------------------------------------------------------------- state
:global tikkerDone
:global tikkerDoneOrder
:global tikkerBusy
:if ([:typeof $tikkerDone] != "array") do={ :set tikkerDone ({}) }
:if ([:typeof $tikkerDoneOrder] != "array") do={ :set tikkerDoneOrder ({}) }

:if ($token = "CHANGE-ME") do={
  :log error "tikker-agent: set the agent token in the script first"
  :error "tikker-agent: token not configured"
}

# One run at a time: the scheduler may start a run while the previous one is
# still sending. A lock older than lockTimeout belongs to a dead run.
:local now [/system clock get uptime]
:local run true
:if ([:typeof $tikkerBusy] = "time") do={
  :if (($now - $tikkerBusy) < $lockTimeout) do={
    :set run false
  } else={
    :log warning "tikker-agent: taking over a stale lock"
  }
}

:if ($run) do={
:set tikkerBusy $now
:do {
  # ------------------------------------------------------------ check-in
  :local ports ({})
  :local portNames ({})
  :local monFields ({"registration"="registration-status"; "operator"="current-operator"; \
    "technology"="access-technology"; "rssi"="rssi"; "rsrp"="rsrp"; "rsrq"="rsrq"; "sinr"="sinr"})
  :foreach i in=[/interface lte find] do={
    :local name [:tostr [/interface lte get $i name]]
    :local p ({})
    :set ($p->"port") $name
    # [WERYFIKACJA T00: "running" and the monitor field names and formats]
    :set ($p->"running") [/interface lte get $i running]
    :do {
      :local m [/interface lte monitor $i once as-value]
      :foreach k,f in=$monFields do={
        # A missing key is "nil" (or "nothing" on some versions): omit it.
        :local v ($m->$f)
        :if ([:typeof $v] != "nothing") do={
          :if ([:typeof $v] != "nil") do={ :set ($p->$k) [:tostr $v] }
        }
      }
    } on-error={ :log warning ("tikker-agent: lte monitor failed on " . $name) }
    :set ($ports->[:len $ports]) $p
    :set ($portNames->[:len $portNames]) $name
  }
  :local ci ({})
  :set ($ci->"routeros_version") [/system resource get version]
  :set ($ci->"board") [/system resource get board-name]
  :set ($ci->"agent_version") $agentVersion
  :set ($ci->"ports") $ports
  :if ([:typeof [$tikkerPost base=$base token=$token path="/checkin" body=[:serialize to=json value=$ci]]] != "array") do={
    :log warning "tikker-agent: check-in failed"
  }

  # ------------------------------------------------------------ inbound push
  # The inbox belongs to the port set in /tool sms; tikker accepts entries
  # only for ports reported at check-in. [WERYFIKACJA T00: "get port"
  # returns the interface name; inbox field names; pdu is hex]
  # Every step is best-effort: a failure here must never stop the command
  # poll below, or outbound SMS would stall behind one unreadable entry.
  :local smsPort ""
  :do { :set smsPort [:tostr [/tool sms get port]] } on-error={ :log warning "tikker-agent: /tool sms get port failed" }
  :local smsPortKnown false
  :foreach n in=$portNames do={ :if ($n = $smsPort) do={ :set smsPortKnown true } }
  :if ($smsPortKnown) do={
    :local pushed 0
    :local ids ({})
    :do { :set ids [/tool sms inbox find] } on-error={ :log warning "tikker-agent: /tool sms inbox find failed" }
    :foreach id in=$ids do={
      :if ($pushed < $maxPush) do={
      :do {
        :local m ({})
        :set ($m->"port") $smsPort
        :set ($m->"remote_id") [:tostr $id]
        :set ($m->"phone") [:tostr [/tool sms inbox get $id phone]]
        :set ($m->"timestamp") [:tostr [/tool sms inbox get $id timestamp]]
        :local pdu [:tostr [/tool sms inbox get $id pdu]]
        # Prefer the PDU: tikker decodes it, and the router's decoded text
        # may not be valid UTF-8 (which would make tikker refuse the push).
        :if ([:len $pdu] > 0) do={
          :set ($m->"pdu") $pdu
        } else={
          :set ($m->"body") [:tostr [/tool sms inbox get $id message]]
        }
        :local list ({})
        :set ($list->0) $m
        :local doc ({})
        :set ($doc->"messages") $list
        :if ([:typeof [$tikkerPost base=$base token=$token path="/inbox" body=[:serialize to=json value=$doc]]] != "array") do={
          :log warning "tikker-agent: inbox push failed"
        }
      } on-error={ :log warning ("tikker-agent: skipping unreadable inbox entry " . [:tostr $id]) }
        :set pushed ($pushed + 1)
      }
    }
  }

  # ------------------------------------------------------------ commands
  :local poll ({})
  :set ($poll->"wait") 0
  :set ($poll->"max") $maxCommands
  :set ($poll->"ports") $portNames
  :local resp [$tikkerPost base=$base token=$token path="/commands" body=[:serialize to=json value=$poll]]
  :local deletes ({})
  :if ([:typeof $resp] = "array") do={
    :if ([:typeof ($resp->"inbox_delete")] = "array") do={ :set deletes ($resp->"inbox_delete") }
    :local cmds ({})
    :if ([:typeof ($resp->"commands")] = "array") do={ :set cmds ($resp->"commands") }
    :foreach c in=$cmds do={
      :local mid [:tostr ($c->"message_id")]
      :local status ""
      :local reason ""
      :local prev ($tikkerDone->$mid)
      :if ([:typeof $prev] = "str" && [:len $prev] > 0) do={
        # Seen before: never send again.
        :if ($prev = "sending") do={
          :set status "ambiguous"
          :set reason "agent: send was started but its result was not recorded"
        } else={
          :set status $prev
          :if ($prev = "permanent") do={ :set reason "agent: unsupported (remembered)" }
        }
      } else={
        :set ($tikkerDoneOrder->[:len $tikkerDoneOrder]) $mid
        :local enc [:tostr ($c->"encoding")]
        :local segs [:tonum ($c->"segments")]
        :local npdus 0
        :if ([:typeof ($c->"pdus")] = "array") do={ :set npdus [:len ($c->"pdus")] }
        :if ($enc != "gsm7" || $segs > 1 || $npdus > 1) do={
          :set status "permanent"
          :set reason ("agent: unsupported by " . $agentVersion . " (encoding " . $enc . ", " . $segs . " segments)")
        } else={
          # Recorded before sending: if this run dies mid-send, a
          # redelivery is answered "ambiguous" instead of sending again.
          :set ($tikkerDone->$mid) "sending"
          :local sent true
          # [WERYFIKACJA T00: send returns only after the modem accepted the
          #  SMS; encoding of non-ASCII GSM-7 characters]
          :do {
            /tool sms send port=[:tostr ($c->"port")] phone-number=[:tostr ($c->"to")] message=[:tostr ($c->"body")]
          } on-error={ :set sent false }
          :if ($sent) do={
            :set status "accepted"
          } else={
            :set status "transient"
            :set reason "agent: /tool sms send failed"
          }
        }
        # Only final results are remembered: a transient failure is retried
        # by tikker with the same message_id and must be sent then.
        :if ($status = "transient") do={
          :set ($tikkerDone->$mid) ""
        } else={
          :set ($tikkerDone->$mid) $status
        }
      }
      :local rb ({})
      :set ($rb->"status") $status
      :set ($rb->"reason") $reason
      :if ([:typeof [$tikkerPost base=$base token=$token path=("/commands/" . $mid . "/result") body=[:serialize to=json value=$rb]]] != "array") do={
        :log warning ("tikker-agent: result report failed for " . $mid)
      }
      :log info ("tikker-agent: command " . $mid . " -> " . $status)
    }
  } else={
    :log warning "tikker-agent: command poll failed"
  }

  # Keep the deduplication state bounded: the last 200 message IDs.
  :if ([:len $tikkerDoneOrder] > 400) do={
    :local n [:len $tikkerDoneOrder]
    :local keep [:pick $tikkerDoneOrder ($n - 200) $n]
    :local nd ({})
    :foreach k in=$keep do={ :set ($nd->$k) ($tikkerDone->$k) }
    :set tikkerDone $nd
    :set tikkerDoneOrder $keep
  }

  # ------------------------------------------------------------ inbox deletions
  # Remove only entries whose phone and timestamp still match what was
  # pushed. Acknowledge only references this run handled: its own port, and
  # either removed or already gone/reused (nothing left to delete). A failed
  # remove or another port's reference stays pending on tikker.
  :if ([:len $deletes] > 0) do={
    :local done ({})
    :foreach ref in=$deletes do={
      :if ([:len $smsPort] > 0) do={
      :if ([:tostr ($ref->"port")] = $smsPort) do={
        :do {
          :foreach id in=[/tool sms inbox find] do={
            :if ([:tostr $id] = [:tostr ($ref->"remote_id")]) do={
              :local match true
              :if ([:typeof ($ref->"phone")] = "str") do={
                :if (($ref->"phone") != [:tostr [/tool sms inbox get $id phone]]) do={ :set match false }
              }
              :if ([:typeof ($ref->"timestamp")] = "str") do={
                :if (($ref->"timestamp") != [:tostr [/tool sms inbox get $id timestamp]]) do={ :set match false }
              }
              :if ($match) do={ /tool sms inbox remove $id }
            }
          }
          :set ($done->[:len $done]) $ref
        } on-error={ :log warning ("tikker-agent: inbox delete failed for " . [:tostr ($ref->"remote_id")]) }
      }
      }
    }
    :if ([:len $done] > 0) do={
      :local ack ({})
      :set ($ack->"deleted") $done
      :if ([:typeof [$tikkerPost base=$base token=$token path="/inbox/ack" body=[:serialize to=json value=$ack]]] != "array") do={
        :log warning "tikker-agent: inbox ack failed"
      }
    }
  }
} on-error={
  :log error "tikker-agent: run aborted"
}
:set tikkerBusy ""
}
