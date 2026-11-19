# Polityka bezpieczeństwa

## Obsługiwane wersje

Projekt jest w fazie MVP i nie ma jeszcze wydań. Poprawki bezpieczeństwa dotyczą gałęzi `main`.
Po pierwszym wydaniu zostanie tu dopisana tabela obsługiwanych wersji.

## Zgłaszanie luk

**Nie zgłaszaj luk bezpieczeństwa w publicznych issues ani PR.**

Używaj prywatnego zgłaszania GitHub (*private vulnerability reporting*):
w repozytorium wybierz zakładkę **Security** → **Report a vulnerability**.
Zgłoszenie jest widoczne tylko dla opiekunów projektu.

W zgłoszeniu podaj, jeśli to możliwe:

- opis problemu i jego potencjalny wpływ;
- kroki do odtworzenia (np. żądanie HTTP, fragment konfiguracji, wersja lub commit);
- proponowaną poprawkę, jeśli ją masz.

Nie dołączaj prawdziwych numerów telefonów, haseł do routerów, tokenów API ani kluczy szyfrujących.
Używaj przykładowych danych lub zanonimizowanych fragmentów.

## Co dalej

- Potwierdzamy otrzymanie zgłoszenia i oceniamy jego wagę.
- Naprawiamy problem w prywatnej gałęzi (GitHub Security Advisory), a po wydaniu poprawki publikujemy opis.
- Zgłaszający jest wymieniany w opisie, jeśli sobie tego życzy.

Tikker jest projektem open source (GPL-3.0-or-later). Ryzyka po stronie wdrożenia (np. konfiguracja
usługi `www-ssl` na routerze lub uprawnienia użytkownika RouterOS) opisuje dokumentacja wdrożeniowa.
