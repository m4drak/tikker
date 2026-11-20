/*
SPDX-FileCopyrightText: 2026 The tikker Authors
SPDX-License-Identifier: GPL-3.0-or-later
*/

// Panel glue code. It must stay a static file: the Content-Security-Policy
// forbids inline scripts.
(function () {
  "use strict";

  // Every htmx request carries the CSRF token of the session
  // (<meta name="csrf-token"> is rendered only for signed-in users).
  document.addEventListener("htmx:configRequest", function (event) {
    var meta = document.querySelector('meta[name="csrf-token"]');
    if (meta && meta.content) {
      event.detail.headers["X-CSRF-Token"] = meta.content;
    }
  });
})();
