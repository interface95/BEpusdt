# Checkout Browser Tests

Run from the repository root with Node 22+ and the installed Google Chrome:

```sh
NODE_PATH=/Users/wujun/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules \
  node --test --test-concurrency=1 tests/checkout/checkout-ui.test.cjs
```

The suite uses `node:test` and `playwright/test`, one headless browser, and serial,
isolated contexts. Chrome is launched from
`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`; no browser download
or production server is needed. An ephemeral loopback HTTP server serves the real
checkout HTML, scripts, styles, locales, and icons. All payment APIs are intercepted;
off-origin requests are blocked and fail the test. Invoice fixtures follow the API
shape but contain synthetic data, not real orders or payment destinations.

Coverage includes arbitrary invoice values, nine currency/network combinations,
exact clipboard strings, real QR canvas pixels and observed QR input, locale
switching, absent icons, removed calculators, payment statuses and transitions,
late-payment recovery after server expiry with a behind-client clock, computed
dark-brand contrast, reselection POST payloads, and long-value layout at 320, 390,
and 1280 pixels. USDC/BNB network searches retain the BSC alias and submit the raw
`bsc` network payload. The default suite reports 55 tests; screenshot mode adds 12
serial review scenarios and their parent test (68 reported tests total).
The QR observer wraps the real jQuery plugin without replacing its renderer;
it verifies input and nonblank output, not decoding/scannability. A paused browser
clock advances polling and countdowns without wall-clock sleeps. Layout checks use
rendered text fragments and controls to detect horizontal clipping and overlap.
Journey parity is asserted even without screenshots: unselected selection, quoted
selection, and payment must share header/brand geometry, card width, colors, brand
contrast, centered amounts, and footer placement. The 100 CNY / 14.9 USDC fixture
runs in zh/en, light/dark, and all three widths. Currency changes and reselection
must discard stale quotes; locale switching must retain the chosen values.

The targeted pre-redesign RED check was executed and failed because the new
`#paymentCurrencyQ` element was absent:

```sh
NODE_PATH=/Users/wujun/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules \
  node --test --test-concurrency=1 --test-name-pattern='approved payment summary' \
  tests/checkout/checkout-ui.test.cjs
```

The additional selection redesign RED test was run before the markup update and
failed because `#selectionStage` lacked the shared `payment-checkout` class.

Optional screenshots (74 PNGs: 36 three-state journey images, 12 long-value layout
images, 24 normal-invoice images, and two dark-brand regression images):

```sh
CHECKOUT_SCREENSHOTS=/tmp/bepusdt-checkout-qa \
NODE_PATH=/Users/wujun/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules \
  node --test --test-concurrency=1 tests/checkout/checkout-ui.test.cjs
```

`CHECKOUT_SCREENSHOTS=1` uses the OS temporary directory. Screenshots are disabled
by default and are not golden-image comparisons. Exit status is nonzero on failure.
To capture only the 24 normal-invoice review screenshots without rerunning the full
suite, add `--test-name-pattern='visual review screenshots'` before the test path.
The 100 CNY / 14.9 USDC sets use `journey-payment-*`,
`journey-selection-unselected-*`, and `journey-selection-selected-*` filenames.
Use `--test-name-pattern='journey visual parity'` to run just these 12 paired cases.
