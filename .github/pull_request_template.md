## Zadanie

- ID zadania z `docs/PLAN.md` (jeden PR = jedno zadanie): <!-- np. T02 -->
- Krótki opis zmiany:

## Modele AI

- Model główny (autor kodu): <!-- np. Claude Haiku 5.5 -->
- Model alternatywny (jeśli użyty, do jakiej części): <!-- lub „brak” -->
- Model przeglądający (przegląd krzyżowy, PLAN §1.2 pkt 1): <!-- inny dostawca niż autor -->
- Zadanie L3 (architektura, współbieżność, bezpieczeństwo, integracja z RouterOS, kontrakty)? <!-- tak/nie; tak = przegląd człowieka -->

## Wynik przeglądu

<!-- Najważniejsze uwagi z przeglądu krzyżowego i sposób ich rozwiązania. -->

## Definicja ukończenia (PLAN §1.2 pkt 4)

- [ ] `make lint test` przechodzi lokalnie
- [ ] testy obejmują nowy kod
- [ ] nagłówki SPDX są obecne, `reuse lint` jest czysty
- [ ] brak nowych zależności spoza dozwolonych licencji (`make licenses`)
- [ ] dokumentacja zaktualizowana (jeśli zmiana tego wymaga)
- [ ] commity podpisane (`git commit -s`, DCO: linia `Signed-off-by`)
- [ ] tytuł i commity w Conventional Commits

## Zmiany w zależnościach

<!-- Nowe zależności: moduł, licencja, uzasadnienie. Brak zmian: „brak”. -->
