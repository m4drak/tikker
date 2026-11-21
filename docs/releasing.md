# Wydawanie wersji

Wydanie powstaje z tagu `vX.Y.Z` (lub pre-release, np. `v0.1.0-rc1`). Całość robi
[`.github/workflows/release.yml`](../.github/workflows/release.yml) według
[`.goreleaser.yaml`](../.goreleaser.yaml).

## Jak wydać wersję

1. Upewnij się, że `main` jest zielony (CI) i że commity mają postać Conventional Commits
   (z nich powstaje changelog).
2. Sprawdź lokalnie: `make release-check` (walidacja konfiguracji i budowa snapshotu w `dist/`,
   bez publikacji i podpisów; wymaga `syft` w `PATH`).
3. Utwórz i wypchnij podpisany tag: `git tag -s v0.1.0 -m "v0.1.0" && git push origin v0.1.0`.
   Tag z myślnikiem (`-rc1`, `-beta.1`) jest oznaczany jako pre-release i nie dostaje tagów
   obrazu `X.Y` ani `latest`.

## Co się dzieje po wypchnięciu tagu

- **GitHub Release** z archiwami `tikker_<wersja>_linux_{amd64,arm64,armv7}.tar.gz`
  (binarka statyczna, `LICENSE`, `README.md`, `THIRD_PARTY_NOTICES`), sumami SHA-256
  (`*_checksums.txt`), SBOM-ami SPDX JSON (`*.spdx.json`) i changelogiem.
- **Archiwum źródeł** `tikker_<wersja>_source.tar.gz` (GPL-3.0 §6, kod odpowiadający binarkom).
- **Obraz** `ghcr.io/m4drak/tikker` (linux/amd64, arm64, arm/v7) z tagami `vX.Y.Z`, `X.Y`
  i `latest` (dwa ostatnie tylko dla wydań stabilnych). Obraz budowany z
  `deploy/docker/Dockerfile`; etykieta OCI `org.opencontainers.image.source` i tag wskazują
  dokładny commit.
- **Podpisy cosign (keyless)**: plik sum kontrolnych (`*_checksums.txt.sigstore.json`) oraz
  obraz (po digeście). Tożsamością jest workflow GitHub Actions, bez długo żyjących kluczy.

`THIRD_PARTY_NOTICES` jest generowany w trakcie wydania (`make notices`, `scripts/notices.sh`)
i nie jest w repozytorium.

## Weryfikacja podpisów

Wymagany [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) v2+.
Poniżej `TAG` to np. `v0.1.0`, a `VER` to wersja bez `v` (`0.1.0`).

```sh
IDENTITY="https://github.com/m4drak/tikker/.github/workflows/release.yml@refs/tags/TAG"
ISSUER="https://token.actions.githubusercontent.com"

# Obraz kontenera
cosign verify ghcr.io/m4drak/tikker:TAG \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"

# Pliki z GitHub Release: podpis sum kontrolnych, potem sumy archiwów
cosign verify-blob tikker_VER_checksums.txt \
  --bundle tikker_VER_checksums.txt.sigstore.json \
  --certificate-identity "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
sha256sum --ignore-missing -c tikker_VER_checksums.txt
```

Sumy kontrolne obejmują archiwa, archiwum źródeł i SBOM-y, więc jeden podpis uwierzytelnia całość.

## SBOM

Każde archiwum i archiwum źródeł ma SBOM w formacie SPDX 2.3 JSON (`<plik>.spdx.json`,
generowany przez `syft`). Licencje zależności (pełne teksty) są w `THIRD_PARTY_NOTICES`.

## Źródła

Kod źródłowy wydania: asset `tikker_<wersja>_source.tar.gz` w GitHub Release albo tag
`vX.Y.Z` w repozytorium. Binarkę można zbudować samodzielnie: `make build`
(Go z `go.mod`, `CGO_ENABLED=0`).

## Konfiguracja repozytorium (jednorazowo)

- Ustawienia Actions: workflow musi mieć prawo zapisu pakietów; pakiet `tikker` w GHCR po
  pierwszym wydaniu ustaw jako publiczny i powiąż z repozytorium.
- Włącz ochronę tagów `v*` (Rulesets), żeby tagi wydań mogły tworzyć tylko maintainerzy.
