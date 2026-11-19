# tikker
SMS gateway using MikroTik devices.

Status: projektowanie. Zobacz:
- [Specyfikacja](docs/SPEC.md)
- [Plan realizacji](docs/PLAN.md)
- [ADR 0001 – architektura](docs/adr/0001-architektura.md)
- [ADR 0002 – gwarancje dostarczenia](docs/adr/0002-gwarancje-dostarczenia.md)
- [ADR 0003 – biblioteki i układ pakietów](docs/adr/0003-biblioteki.md)

Licencja: GNU GPL v3.0 – patrz [LICENSE](LICENSE).

MikroTik i RouterOS są znakami towarowymi MikroTik. Projekt nie jest powiązany z firmą MikroTik.

## Development

Wymagania: Go 1.24+, `make`, [golangci-lint](https://golangci-lint.run/) v2. Opcjonalnie `reuse` (`pip install reuse`).

```sh
make build                    # binarka bin/tikker (CGO_ENABLED=0, wersja z git describe)
make test                     # testy z -race i pokryciem
make lint                     # golangci-lint
make run ARGS="--version"     # uruchomienie z argumentami
reuse lint                    # zgodność nagłówków SPDX (REUSE)
```

Zasady współtworzenia (DCO, Conventional Commits, nagłówki SPDX): [CONTRIBUTING.md](CONTRIBUTING.md).
Instrukcje dla agentów AI: [AGENTS.md](AGENTS.md). Zgłaszanie luk: [SECURITY.md](SECURITY.md).

### Development (English summary)

Requirements: Go 1.24+, `make`, golangci-lint v2. Run `make build`, `make test`, `make lint`.
Contributions use the Developer Certificate of Origin (`git commit -s`) and Conventional Commits.
See [CONTRIBUTING.md](CONTRIBUTING.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) and [SECURITY.md](SECURITY.md).
