'use strict';

const assert = require('node:assert/strict');
const { before, after, test } = require('node:test');
const { createServer } = require('node:http');
const { readFile, mkdir } = require('node:fs/promises');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { chromium, expect } = require('playwright/test');

const ROOT = path.resolve(__dirname, '../..');
const ASSETS = path.join(ROOT, 'static/checkout/official/assets');
const CHROME = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const NOW = Date.UTC(2026, 8, 7, 4);
const TRADE_ID = 'checkout-browser-test';
const SOLANA_ADDRESS = '7YWHMfk9JZeQLMg1auHuiSxhL2ZKpN8QbVt3rDc5E6Fa';
const EVM_ADDRESS = '0xAbCdEf0123456789aBcDEF0123456789AbCdEf01';
const TRON_ADDRESS = 'TJRabPrwbZy45sbavfcjinPJC18kjpRTv8';
const MIME = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css',
    '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png',
    '.woff2': 'font/woff2', '.woff': 'font/woff' };
let browser;
let server;
let origin;
const unmockedApi = [];

function invoice(overrides = {}) {
    return {
        trade_id: TRADE_ID, order_id: 'merchant-order-42', status: 1,
        trade_type: 'usdc.solana', actual_amount: '42.123456', money: '299.99', fiat: 'CNY',
        token: SOLANA_ADDRESS, created_at: NOW / 1000 - 600, expired_at: NOW / 1000 + 1800,
        network: { crypto: 'USDC', name: 'Solana', network: 'solana' },
        selected_payment: { currency: 'USDC', network: 'solana', token_net_name: 'Solana' },
        redirect_url: '/merchant-return', reselect: true, ...overrides,
    };
}

before(async () => {
    server = createServer(async (request, response) => {
        const pathname = new URL(request.url, 'http://localhost').pathname;
        if (pathname.startsWith('/api/')) {
            unmockedApi.push(pathname);
            response.writeHead(500).end('Payment API must be intercepted');
            return;
        }
        let filename;
        if (pathname === `/pay/${TRADE_ID}`) {
            filename = path.join(ROOT, 'static/checkout/official/views/checkout.html');
        } else if (pathname.startsWith('/checkout/official/assets/')) {
            filename = path.resolve(ASSETS, decodeURIComponent(pathname.slice('/checkout/official/assets/'.length)));
            if (!filename.startsWith(ASSETS + path.sep)) {
                response.writeHead(403).end();
                return;
            }
        } else {
            response.writeHead(404).end();
            return;
        }
        try {
            const body = await readFile(filename);
            response.writeHead(200, { 'Content-Type': MIME[path.extname(filename)] || 'application/octet-stream',
                'Cache-Control': 'no-store' });
            response.end(body);
        } catch (error) {
            response.writeHead(error.code === 'ENOENT' ? 404 : 500).end();
        }
    });
    await new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(0, '127.0.0.1', resolve);
    });
    origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ executablePath: CHROME, headless: true });
});

after(async () => {
    try {
        if (browser) await browser.close();
    } finally {
        if (server) await new Promise((resolve) => server.close(resolve));
    }
    assert.deepEqual(unmockedApi, [], 'No payment API may reach the static server');
});

async function openCheckout(t, options = {}) {
    const order = invoice(options.order);
    const context = await browser.newContext({ locale: options.locale || 'en-US',
        viewport: { width: options.width || 1280, height: 900 }, reducedMotion: 'reduce',
        colorScheme: options.colorScheme || 'light', serviceWorkers: 'block' });
    const page = await context.newPage();
    const requests = [];
    const blocked = [];
    const errors = [];
    const state = { order, methods: options.methods || [], updateResponse: options.updateResponse,
        missingIcons: [] };
    page.setDefaultTimeout(5000);
    page.on('pageerror', (error) => errors.push(error.message));
    t.after(async () => {
        await context.close();
        assert.deepEqual(blocked, [], 'Unexpected requests were blocked before reaching the network');
        assert.deepEqual(errors, [], 'Checkout must not raise browser exceptions');
        for (const request of requests) {
            assert.equal(request.method, 'POST', request.path);
            assert.equal(request.body.trade_id, TRADE_ID, request.path);
        }
    });
    await context.route('**/*', async (route) => {
        const request = route.request();
        const url = new URL(request.url());
        if (url.origin !== origin) {
            blocked.push(request.url());
            await route.abort('blockedbyclient');
        } else if (url.pathname.startsWith('/api/')) {
            const body = request.postDataJSON();
            requests.push({ path: url.pathname, method: request.method(), body });
            let data;
            if (url.pathname === '/api/v1/pay/info') data = state.order;
            else if (url.pathname === '/api/v1/pay/methods') {
                data = { methods: state.methods, network_sort: 'solana,tron,ethereum,bsc,base' };
            } else if (url.pathname === '/api/v1/pay/update-order') {
                await route.fulfill({ json: state.updateResponse || { status_code: 200,
                    data: { payment_url: `${origin}/pay/${TRADE_ID}?updated=1` } } });
                return;
            } else {
                blocked.push(url.pathname);
                await route.abort('blockedbyclient');
                return;
            }
            await route.fulfill({ json: { status_code: 200, data } });
        } else if (options.missingIcons && url.pathname.includes('/web3icons/')) {
            state.missingIcons.push(url.pathname);
            await route.fulfill({ status: 404, body: '' });
        } else await route.continue();
    });
    await page.addInitScript(() => {
        window.__clipboardWrites = [];
        window.__qrTexts = [];
        Object.defineProperty(navigator, 'clipboard', { configurable: true,
            value: { writeText: async (text) => { window.__clipboardWrites.push(text); } } });
        // Observe the real plugin input without replacing its canvas rendering.
        document.addEventListener('DOMContentLoaded', () => {
            const render = window.jQuery.fn.qrcode;
            window.jQuery.fn.qrcode = function (options) {
                window.__qrTexts.push(typeof options === 'string' ? options : options.text);
                return render.apply(this, arguments);
            };
        });
    });
    await page.clock.install({ time: new Date(NOW - 1000) });
    await page.clock.pauseAt(new Date(NOW));
    await page.goto(`${origin}/pay/${TRADE_ID}`, { waitUntil: 'load' });
    await expect.poll(() => requests.filter((request) => request.path === '/api/v1/pay/info').length).toBeGreaterThan(0);
    return { page, state, requests, order };
}

test('approved payment summary binds arbitrary USDC Solana invoice data', { concurrency: false }, async (t) => {
    const { page, order } = await openCheckout(t);
    await assertSummary(page, order, 'USDC', /Solana/i);
    await expect(page.locator('#selectionStage')).toBeHidden();
    await expect(page.locator('#paymentStage .deposit-notice')).toBeVisible();
    await expect(page.locator('.network-warning')).toBeVisible();
    await expect(page.locator('#timerDisplayQ')).toHaveText('30:00');
});
function paymentMethod(currency, network, tokenNetName, amount = '73.987654') {
    return { currency, network, token_net_name: tokenNetName, actual_amount: amount,
        amount: '299.99', fiat: 'CNY', exchange_rate: '1', is_popular: false };
}

function networkOrder(currency, network, name, tokenNetName, token) {
    return { trade_type: `${currency.toLowerCase()}.${network}`, token,
        network: { crypto: currency, name, network },
        selected_payment: paymentMethod(currency, network, tokenNetName) };
}

async function assertSummary(page, order, currency, networkPattern) {
    await expect(page.locator('#paymentStage')).toBeVisible();
    const summary = (await page.locator('#payAmountQ').innerText()).replace(/\s/g, '');
    assert.equal(summary, order.actual_amount + currency);
    await expect(page.locator('#paymentCurrencyQ')).toHaveText(currency);
    await expect(page.locator('#payNetworkQ')).toHaveText(networkPattern);
    await expect(page.locator('#paymentNetworkNoteQ')).toContainText(currency);
    await expect(page.locator('#networkCurrencyQ')).toHaveText(currency);
    const expected = (await page.locator('#expectedAmountQ').innerText()).replace(/\s/g, '');
    assert.equal(expected, order.actual_amount + currency, 'Fee warning must retain the exact invoice amount');
    await expect(page.locator('#addressNetworkQ')).toContainText(networkPattern);
    await expect(page.locator('#walletAddress')).toHaveText(order.token);
    await expect(page.locator('#orderIdQ')).toHaveText(order.order_id);
    await expect(page.locator('#orderMoneyQ')).toHaveText(order.money);
    await expect(page.locator('#orderFiatQ')).toHaveText(order.fiat);
    await expect(page.locator('#paymentStage')).not.toContainText('146.05');
}

const NETWORKS = [
    ['USDC', 'solana', 'Solana', 'Solana', SOLANA_ADDRESS, /Solana/i, /Solana\s*\(SOL\)/i],
    ['USDT', 'tron', 'Tron', 'Tron', TRON_ADDRESS, /TRON/i, /TRON\s*\(TRC20\)/i],
    ['USDC', 'ethereum', 'Ethereum', 'Ethereum', EVM_ADDRESS, /Ethereum/i, /Ethereum\s*\(ERC20\)/i],
    ['USDT', 'bsc', 'Bsc', 'Bsc', EVM_ADDRESS, /BSC|BNB Smart Chain/i, /(?:BSC|BNB Smart Chain).*BEP20/i],
    ['USDC', 'bsc', 'Bsc', 'Bsc', EVM_ADDRESS, /BNB Smart Chain/i, /BNB Smart Chain\s*\(BEP20\)/i],
    ['USDC', 'base', 'Base', 'Base', EVM_ADDRESS, /Base/i, /Base/i],
    ['TRX', 'tron', 'Tron', 'Tron', TRON_ADDRESS, /TRON/i, /^TRON$/i],
    ['ETH', 'ethereum', 'Ethereum', 'Ethereum', EVM_ADDRESS, /Ethereum/i, /^Ethereum$/i],
    ['BNB', 'bsc', 'Bsc', 'Bsc', EVM_ADDRESS, /BNB Smart Chain/i, /^BNB Smart Chain$/i],
];
for (const [currency, network, name, tokenNetName, token, pattern, label] of NETWORKS) {
    const searchBsc = network === 'bsc' && currency !== 'USDT';
    test(`network binding: ${currency} on ${network}${searchBsc ? ' / BSC search' : ''}`, async (t) => {
        const { page, order, requests } = await openCheckout(t, { order: networkOrder(currency, network, name, tokenNetName, token),
            methods: [paymentMethod(currency, network, tokenNetName, '42.123456'), ...(searchBsc ? [paymentMethod('USDC', 'solana', 'Solana')] : [])] });
        await assertSummary(page, order, currency, pattern);
        await expect(page.locator('#payNetworkQ')).toHaveText(label);
        if (currency === 'TRX') {
            for (const language of ['en', 'zh']) {
                await switchLanguage(page, language);
                await assertSummary(page, order, currency, pattern);
                const warnings = await page.locator('.network-warning, #paymentNetworkNoteQ').allTextContents();
                assert.doesNotMatch(warnings.join(' '),
                    /(?:do not|don't|never|must not|cannot|\u8bf7\u52ff|\u4e0d\u8981|\u7981\u6b62|\u5207\u52ff)[^.!?\u3002\uff01\uff1f]{0,100}\bTRX\b/i,
                    'A native TRX invoice must not prohibit transferring TRX');
            }
        }
        await page.locator('#reselectPaymentBtn').click();
        await choose(page, 'currencySelect', currency);
        await choose(page, 'networkSelect', tokenNetName, searchBsc ? 'BSC' : '');
        await expect(page.locator('#networkSelect .select-text')).toHaveText(label);
        if (searchBsc) {
            await page.locator('#payBtn').click();
            await page.waitForURL(`${origin}/pay/${TRADE_ID}?updated=1`);
            assert.deepEqual(requests.filter((request) => request.path === '/api/v1/pay/update-order').map((request) => request.body),
                [{ trade_id: TRADE_ID, currency, network: 'bsc' }]);
        }
    });
}
test('selected_payment fallback retains invoice currency and network', async (t) => {
    const { page, order } = await openCheckout(t, { order: { network: null } });
    await assertSummary(page, order, 'USDC', /Solana/i);
});

async function assertQr(page, address) {
    await expect(page.locator('#qrcode canvas')).toBeVisible();
    assert.deepEqual(await page.evaluate(() => window.__qrTexts), [address]);
    const pixels = await page.locator('#qrcode canvas').evaluate((canvas) => {
        const data = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data;
        let dark = 0;
        let light = 0;
        for (let index = 0; index < data.length; index += 4) {
            if (data[index + 3] && data[index] + data[index + 1] + data[index + 2] < 300) dark++;
            if (data[index + 3] && data[index] + data[index + 1] + data[index + 2] > 600) light++;
        }
        return { dark, light, total: canvas.width * canvas.height };
    });
    assert.ok(pixels.total > 0 && pixels.dark / pixels.total > 0.1 && pixels.light / pixels.total > 0.1,
        `QR must contain substantial dark and light modules: ${JSON.stringify(pixels)}`);
}

for (const amount of ['42.123456', '0.123456789123456789']) {
    test(`clipboard and actual QR preserve amount ${amount} and full address`, async (t) => {
        const { page, order } = await openCheckout(t, { order: { actual_amount: amount } });
        await expect(page.locator('#paymentStage')).toBeVisible();
        await page.locator('#copyAmountQBtn').click();
        await page.locator('#copyAddressBtn').click();
        assert.deepEqual(await page.evaluate(() => window.__clipboardWrites), [amount, order.token]);
        await assertQr(page, order.token);
    });
}

async function switchLanguage(page, language) {
    await page.evaluate((value) => window.Payment.switchLang(value), language);
    await expect(page.locator('html')).toHaveAttribute('lang', language === 'zh' ? 'zh-CN' : 'en');
}

for (const locale of ['zh-CN', 'en-US']) {
    test(`locale ${locale} and Payment.switchLang preserve dynamic values`, async (t) => {
        const { page, order } = await openCheckout(t, { locale });
        await expect(page.locator('html')).toHaveAttribute('lang', locale === 'zh-CN' ? 'zh-CN' : 'en');
        await assertSummary(page, order, 'USDC', /Solana/i);
        const originalNote = await page.locator('#paymentNetworkNoteQ').innerText();
        for (const language of locale === 'zh-CN' ? ['en', 'zh'] : ['zh', 'en']) {
            await switchLanguage(page, language);
            await assertSummary(page, order, 'USDC', /Solana/i);
            await expect(page.locator('#timerDisplayQ')).toHaveText('30:00');
        }
        assert.equal(await page.locator('#paymentNetworkNoteQ').innerText(), originalNote);
        await switchLanguage(page, locale === 'zh-CN' ? 'en' : 'zh');
        assert.notEqual(await page.locator('#paymentNetworkNoteQ').innerText(), originalNote,
            'The network note must actually change language');
        await page.locator('#copyAmountQBtn').click();
        await page.locator('#copyAddressBtn').click();
        assert.deepEqual(await page.evaluate(() => window.__clipboardWrites), [order.actual_amount, order.token]);
        await assertQr(page, order.token);
    });

    test(`no withdrawal fee calculator remains in ${locale}`, async (t) => {
        const { page } = await openCheckout(t, { locale });
        await expect(page.locator('#paymentStage')).toBeVisible();
        await expect(page.locator('input[type="number"], [id*="calculator" i], [class*="calculator" i]')).toHaveCount(0);
        await expect(page.locator('body')).not.toContainText(/calculator|trial calculation|\u8bd5\u7b97|\u8ba1\u7b97\u5668/i);
        await expect(page.locator('.deposit-notice #expectedAmountQ')).toBeVisible();
    });
}

test('missing token and network icons preserve payment text, copying, QR and selection', async (t) => {
    const { page, order, state } = await openCheckout(t, { missingIcons: true,
        methods: [paymentMethod('USDC', 'solana', 'Solana')] });
    await assertSummary(page, order, 'USDC', /Solana/i);
    await expect.poll(() => state.missingIcons.length).toBeGreaterThan(0);
    await expect(page.locator('#paymentTokenIconQ')).toBeHidden();
    await expect(page.locator('#paymentNetworkIconQ')).toBeHidden();
    await assertQr(page, order.token);
    await page.locator('#copyAddressBtn').click();
    assert.deepEqual(await page.evaluate(() => window.__clipboardWrites), [order.token]);
    await page.locator('#reselectPaymentBtn').click();
    await choose(page, 'currencySelect', 'USDC');
    await choose(page, 'networkSelect', 'Solana');
    await expect(page.locator('#currencySelect .select-text')).toHaveText('USDC');
    await expect(page.locator('#networkSelect .select-text')).toHaveText('Solana (SOL)');
    await expect(page.locator('#payBtn')).toBeEnabled();
});

async function assertModal(page, visibleId) {
    for (const id of ['confirmingModal', 'successModal', 'canceledModal', 'timeoutModal']) {
        if (id === visibleId) await expect(page.locator(`#${id}`)).toBeVisible();
        else await expect(page.locator(`#${id}`)).toBeHidden();
    }
}

for (const [name, status, modal] of [
    ['pending', 1, null], ['confirming', 5, 'confirmingModal'],
    ['success', 2, 'successModal'], ['canceled', 4, 'canceledModal'], ['expired', 3, 'timeoutModal'],
]) {
    test(`initial ${name} state keeps the appropriate payment UI`, async (t) => {
        const { page } = await openCheckout(t, { order: { status,
            ...(status === 3 ? { expired_at: NOW / 1000 - 1 } : {}) } });
        if (status !== 4) await expect(page.locator('#paymentStage')).toBeVisible();
        else {
            await expect(page.locator('#paymentStage')).toBeHidden();
            await expect(page.locator('#selectionStage')).toBeHidden();
        }
        await assertModal(page, modal);
        if (status === 1) {
            await expect(page.locator('#timerDisplayQ')).toHaveText('30:00');
            await page.clock.runFor(1000);
            await expect(page.locator('#timerDisplayQ')).toHaveText('29:59');
        }
    });
}

async function pollStatus(page, requests) {
    const previous = requests.filter((request) => request.path === '/api/v1/pay/info').length;
    await page.clock.runFor(5000);
    await expect.poll(() => requests.filter((request) => request.path === '/api/v1/pay/info').length)
        .toBeGreaterThan(previous);
}

test('polling progresses pending to confirming to success and preserves return links', async (t) => {
    const { page, state, requests } = await openCheckout(t);
    await expect(page.locator('#paymentStage')).toBeVisible();
    await assertModal(page, null);
    state.order = { ...state.order, status: 5 };
    await pollStatus(page, requests);
    await assertModal(page, 'confirmingModal');
    const tradeUrl = `${origin}/explorer/transaction/AbCdEf0123456789`;
    state.order = { ...state.order, status: 2, trade_url: tradeUrl };
    await pollStatus(page, requests);
    await assertModal(page, 'successModal');
    await expect(page.locator('#txHashLink')).toHaveAttribute('href', tradeUrl);
    await expect(page.locator('#returnBtn')).toHaveAttribute('href', '/merchant-return');
    const count = requests.length;
    const timer = await page.locator('#timerDisplayQ').textContent();
    await page.clock.runFor(6000);
    assert.equal(requests.length, count, 'Success must stop status polling');
    await expect(page.locator('#timerDisplayQ')).toHaveText(timer);
});

test('late payment 1 -> 3 -> 5 -> 2 removes timeout and stops countdown without stopping polling', async (t) => {
    const { page, state, requests, order } = await openCheckout(t, { order: { expired_at: NOW / 1000 + 20 } });
    await expect(page.locator('#timerDisplayQ')).toHaveText('00:20');
    await assertModal(page, null);
    state.order = { ...state.order, status: 3 };
    await pollStatus(page, requests);
    await assertModal(page, 'timeoutModal');
    await expect(page.locator('#timerDisplayQ')).toHaveText('00:15');
    state.order = { ...state.order, status: 5 };
    await pollStatus(page, requests);
    await assertModal(page, 'confirmingModal');
    await expect(page.locator('#timeoutModal')).toHaveCount(0);
    const timer = await page.locator('#timerDisplayQ').textContent();
    await page.evaluate(() => {
        window.__timeoutRecreated = false;
        new MutationObserver((records) => {
            if (records.some((record) => [...record.addedNodes].some((node) => node.id === 'timeoutModal'))) {
                window.__timeoutRecreated = true;
            }
        }).observe(document.body, { childList: true });
    });
    const confirmingRequests = requests.length;
    await page.clock.runFor(11000);
    assert.ok(await page.evaluate(() => Date.now()) > order.expired_at * 1000, 'Client clock must cross expiry');
    await expect.poll(() => requests.length).toBeGreaterThan(confirmingRequests);
    await expect(page.locator('#timerDisplayQ')).toHaveText(timer);
    await assertModal(page, 'confirmingModal');
    await expect(page.locator('#timeoutModal')).toHaveCount(0);
    assert.equal(await page.evaluate(() => window.__timeoutRecreated), false, 'Timeout must never be recreated during confirmation');
    state.order = { ...state.order, status: 2 };
    await pollStatus(page, requests);
    await assertModal(page, 'successModal');
    await expect(page.locator('#timeoutModal')).toHaveCount(0);
    await page.locator('#returnBtn').click({ trial: true });
    const completedRequests = requests.length;
    await page.clock.runFor(6000);
    assert.equal(requests.length, completedRequests, 'Success must stop polling');
    await assertModal(page, 'successModal');
    assert.equal(await page.evaluate(() => window.__timeoutRecreated), false);
});

for (const [status, modal] of [[2, 'successModal'], [4, 'canceledModal']]) {
    test(`late payment direct 3 -> ${status} removes timeout with client clock behind`, async (t) => {
        const { page, state, requests } = await openCheckout(t, { order:
            { status: 3, expired_at: NOW / 1000 + 20 } });
        await assertModal(page, 'timeoutModal');
        await expect(page.locator('#timerDisplayQ')).toHaveText('00:20');
        state.order = { ...state.order, status };
        await pollStatus(page, requests);
        await assertModal(page, modal);
        await expect(page.locator('#timeoutModal')).toHaveCount(0);
        await page.locator(`#${modal} .return-btn`).click({ trial: true });
        const count = requests.length;
        const timer = await page.locator('#timerDisplayQ').textContent();
        await page.clock.runFor(21000);
        await assertModal(page, modal);
        await expect(page.locator('#timeoutModal')).toHaveCount(0);
        await expect(page.locator('#timerDisplayQ')).toHaveText(timer);
        assert.equal(requests.length, count, 'Terminal status must stop polling');
    });
}

async function assertBrandContrast(page, stage, scheme) {
    const brand = page.locator(`${stage} .brand-name`);
    await expect(brand).toBeVisible();
    const colors = await brand.evaluate((element) => {
        const canvas = document.createElement('canvas');
        canvas.width = canvas.height = 1;
        const context = canvas.getContext('2d');
        function rgba(color) {
            context.clearRect(0, 0, 1, 1);
            context.fillStyle = color;
            context.fillRect(0, 0, 1, 1);
            return [...context.getImageData(0, 0, 1, 1).data];
        }
        return { text: rgba(getComputedStyle(element).color),
            surface: rgba(getComputedStyle(element.closest('.checkout-card')).backgroundColor) };
    });
    function luminance(rgba) {
        const linear = rgba.slice(0, 3).map((channel) => {
            const value = channel / 255;
            return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
        });
        return linear[0] * 0.2126 + linear[1] * 0.7152 + linear[2] * 0.0722;
    }
    assert.equal(colors.text[3], 255, 'Brand text must be opaque');
    assert.equal(colors.surface[3], 255, 'Payment card must provide an opaque contrast background');
    const textLuminance = luminance(colors.text);
    const backgroundLuminance = luminance(colors.surface);
    assert.ok(scheme === 'dark' ? backgroundLuminance < 0.15 : backgroundLuminance > 0.8,
        `${stage} must actually use the ${scheme} theme`);
    const contrast = (Math.max(textLuminance, backgroundLuminance) + 0.05)
        / (Math.min(textLuminance, backgroundLuminance) + 0.05);
    assert.ok(contrast >= 4.5, `Brand contrast must be at least 4.5:1, received ${contrast.toFixed(2)}:1`);
    return contrast;
}

for (const width of [320, 1280]) {
    test(`dark brand contrast remains readable at ${width}px`, async (t) => {
        const { page } = await openCheckout(t, { width, colorScheme: 'dark' });
        const contrast = await assertBrandContrast(page, '#paymentStage', 'dark');
        t.diagnostic(`Computed brand contrast ${contrast.toFixed(2)}:1`);
        await screenshot(page, `brand-fixed-${width}-en-US-dark`);
    });
}

test('cancellation while confirming removes payable UI and stops polling', async (t) => {
    const { page, state, requests } = await openCheckout(t, { order: { status: 5 } });
    await assertModal(page, 'confirmingModal');
    state.order = { ...state.order, status: 4 };
    await pollStatus(page, requests);
    await assertModal(page, 'canceledModal');
    await expect(page.locator('#paymentStage')).toBeHidden();
    await expect(page.locator('#selectionStage')).toBeHidden();
    await expect(page.locator('#canceledModal .return-btn')).toHaveAttribute('href', '/merchant-return');
    const count = requests.length;
    await page.clock.runFor(6000);
    assert.equal(requests.length, count, 'Cancellation must stop status polling');
});

test('countdown expiry opens the timeout modal', async (t) => {
    const { page } = await openCheckout(t, { order: { expired_at: NOW / 1000 + 2 } });
    await expect(page.locator('#timerDisplayQ')).toHaveText('00:02');
    await assertModal(page, null);
    await page.clock.runFor(2000);
    await expect(page.locator('#timerDisplayQ')).toHaveText('00:00');
    await assertModal(page, 'timeoutModal');
    await expect(page.locator('#timeoutModal .return-btn')).toHaveAttribute('href', '/merchant-return');
});

test('countdown formats hours only at or above one hour in both steps', async (t) => {
    const { page } = await openCheckout(t, { order: { expired_at: NOW / 1000 + 3601 } });
    await expect(page.locator('#timerDisplayQ')).toHaveText('01:00:01');
    await page.locator('#reselectPaymentBtn').click();
    await expect(page.locator('#timerDisplay')).toHaveText('01:00:01');
    await page.clock.runFor(2000);
    await expect(page.locator('#timerDisplay')).toHaveText('59:59');
});
async function choose(page, id, value, search = '') {
    const option = page.locator(`#${id} .dropdown-item[data-value=${JSON.stringify(value)}]`);
    await expect(option).toHaveCount(1);
    await page.locator(`#${id} .select-trigger`).click();
    if (search) {
        await page.locator(`#${id} .dropdown-search`).fill(search);
        await expect(page.locator(`#${id} .dropdown-item`)).toHaveCount(1);
    }
    await option.click();
}

test('reselection resets network on currency change and POSTs the exact chosen pair', async (t) => {
    const methods = [paymentMethod('USDC', 'solana', 'Solana'),
        paymentMethod('USDC', 'base', 'Base', '18.876543'),
        paymentMethod('USDT', 'tron', 'Tron')];
    const { page, requests, state } = await openCheckout(t, { methods });
    await page.locator('#reselectPaymentBtn').click();
    await expect(page.locator('#selectionStage')).toBeVisible();
    await expect(page.locator('#paymentStage')).toBeHidden();
    await expect(page.locator('#payBtn')).toBeDisabled();
    await choose(page, 'currencySelect', 'USDT');
    await expect(page.locator('#payBtn')).toBeDisabled();
    await choose(page, 'networkSelect', 'Tron');
    await expect(page.locator('#payBtn')).toBeEnabled();
    await choose(page, 'currencySelect', 'USDC');
    await expect(page.locator('#payBtn')).toBeDisabled();
    await expect(page.locator('#networkSelect .select-text')).toHaveClass(/placeholder/);
    await expect(page.locator('#selectionFiatPreview')).toBeVisible();
    await expect(page.locator('#selectionFiatCaption')).toBeHidden();
    await expect(page.locator('#payAmountCrypto')).toBeHidden();
    await expect(page.locator('#copyAmountBtn')).toBeHidden();
    await expect(page.locator('#networkTagSep')).toBeHidden();
    await expect(page.locator('#selectionAmountLabel')).toHaveAttribute('data-i18n', 'selectionOrderAmount');
    await choose(page, 'networkSelect', 'Base');
    await expect(page.locator('#payAmountCrypto')).toHaveText('18.876543 USDC');
    await expect(page.locator('#payBtn')).toBeEnabled();
    state.order = invoice({ ...networkOrder('USDC', 'base', 'Base', 'Base', EVM_ADDRESS),
        actual_amount: '18.876543' });
    await page.locator('#payBtn').click();
    await page.waitForURL(`${origin}/pay/${TRADE_ID}?updated=1`);
    const updates = requests.filter((request) => request.path === '/api/v1/pay/update-order');
    assert.deepEqual(updates, [{ path: '/api/v1/pay/update-order', method: 'POST',
        body: { trade_id: TRADE_ID, currency: 'USDC', network: 'base' } }]);
    await assertSummary(page, state.order, 'USDC', /Base/i);
    state.methods = [paymentMethod('USDC', 'base', 'Base', '19.123456')];
    await page.locator('#reselectPaymentBtn').click();
    await expect(page.locator('#selectionFiatPreview')).toBeVisible();
    await expect(page.locator('#payAmountCrypto')).toBeHidden();
    await expect(page.locator('#selectionFiatCaption')).toBeHidden();
    await expect(page.locator('#payBtn')).toBeDisabled();
    await choose(page, 'currencySelect', 'USDC');
    await choose(page, 'networkSelect', 'Base');
    await expect(page.locator('#payAmountCrypto')).toHaveText(/19\.123456\s*USDC/);
    await page.locator('#copyAmountBtn').click();
    assert.deepEqual(await page.evaluate(() => window.__clipboardWrites), ['19.123456']);
});

test('unassigned invoice opens selection and update failure preserves the chosen method', async (t) => {
    const { page, requests } = await openCheckout(t, { order: { trade_type: '', token: '' },
        methods: [paymentMethod('USDC', 'solana', 'Solana')],
        updateResponse: { status_code: 400, message: 'Test method temporarily unavailable' } });
    await expect(page.locator('#selectionStage')).toBeVisible();
    await expect(page.locator('#paymentStage')).toBeHidden();
    await choose(page, 'currencySelect', 'USDC');
    await choose(page, 'networkSelect', 'Solana');
    await page.locator('#payBtn').click();
    await expect(page.locator('#cmusToast')).toHaveText('Test method temporarily unavailable');
    await expect(page.locator('#selectionStage')).toBeVisible();
    await expect(page.locator('#networkSelect .select-text')).toHaveText('Solana (SOL)');
    await expect(page.locator('#payBtn')).toBeEnabled();
    assert.equal(requests.filter((request) => request.path === '/api/v1/pay/update-order').length, 1);
});

test('invoice with reselection disabled hides the reselect action', async (t) => {
    const { page } = await openCheckout(t, { order: { reselect: false } });
    await expect(page.locator('#paymentStage')).toBeVisible();
    await expect(page.locator('#reselectPaymentBtn')).toBeHidden();
});

test('selection redesign exposes the shared shell and unselected 100 CNY preview', async (t) => {
    const { page } = await openCheckout(t, { order: { trade_type: '', token: '', money: '100' },
        methods: [paymentMethod('USDC', 'solana', 'Solana', '14.9')] });
    const stage = page.locator('#selectionStage');
    await expect(stage).toBeVisible();
    await expect(stage).toHaveClass(/payment-checkout/);
    await expect(stage.locator('.payment-header')).toBeVisible();
    await expect(stage.locator('.payment-amount-section')).toBeVisible();
    await expect(stage.locator('.payment-footer')).toBeVisible();
    await expect(page.locator('#selectionFiatPreview')).toHaveText(/100\s*CNY/);
    await expect(page.locator('#selectionFiatCaption')).toBeHidden();
    await expect(page.locator('#payAmountCrypto')).toBeHidden();
    await expect(page.locator('#selectionAmountLabel')).toHaveAttribute('data-i18n', 'selectionOrderAmount');
    await expect(page.locator('#payBtn')).toBeDisabled();
});

async function designContract(page, stage, scheme) {
    const root = page.locator(stage);
    await expect(root).toHaveClass(/payment-checkout/);
    await expect(root.locator('.brand-logo')).toHaveText('B');
    for (const selector of ['.payment-header', '.payment-amount-section', '.payment-footer']) {
        await expect(root.locator(selector)).toBeVisible();
    }
    await assertBrandContrast(page, stage, scheme);
    const metrics = await root.evaluate((root) => {
        const element = (selector) => root.querySelector(selector);
        const rect = (selector) => element(selector).getBoundingClientRect();
        const css = (selector) => getComputedStyle(element(selector));
        const card = root.getBoundingClientRect();
        const selection = root.id === 'selectionStage';
        const amount = selection ? (element('#payAmountCrypto').getClientRects().length ? '#payAmountCrypto' : '#selectionFiatPreview') : '#payAmountQ';
        const copy = element(selection ? '#copyAmountBtn' : '#copyAmountQBtn');
        const boxes = [rect(amount), ...(copy.getClientRects().length ? [copy.getBoundingClientRect()] : [])];
        const center = (Math.min(...boxes.map((box) => box.left)) + Math.max(...boxes.map((box) => box.right))) / 2;
        return { width: card.width, headerWidth: rect('.payment-header').width, headerHeight: rect('.payment-header').height,
            logoWidth: rect('.brand-logo').width, logoHeight: rect('.brand-logo').height,
            colors: [getComputedStyle(root).backgroundColor, css('.brand-name').color, css('.payment-footer').backgroundColor],
            typography: [css('.brand-name').fontSize, css('.brand-name').fontWeight, css('.payment-footer').fontSize],
            headerPadding: css('.payment-header').padding, amountAlign: css('.payment-amount-section').textAlign,
            centerOffset: Math.abs(center - (card.left + card.right) / 2),
            ordered: rect('.payment-header').bottom <= rect('.payment-amount-section').top + 1,
            formOrdered: !selection || (['#currencySelect', '#networkSelect'].every((selector) =>
                rect(selector).top >= rect('.deposit-notice').bottom - 1 && rect(selector).bottom <= rect('#payBtn').top + 1)
                && rect('#orderIdS').top >= rect('#payBtn').bottom - 1 && rect('.payment-footer').top >= rect('#orderIdS').bottom - 1),
            compact: !selection || Math.min(rect('#currencySelect').top, rect('#networkSelect').top) - rect('.deposit-notice').bottom <= 64 };
    });
    assert.equal(metrics.amountAlign, 'center', `${stage} amount must be center aligned`);
    assert.ok(metrics.centerOffset <= 2, `${stage} amount/copy group must be centered, offset ${metrics.centerOffset}px`);
    assert.ok(metrics.ordered && metrics.formOrdered && metrics.compact, `${stage} must put compact selectors below the warning and order/footer after the form`);
    await assertLayout(page, stage);
    return metrics;
}

test('journey visual parity across unselected, quoted, and payment states', async (t) => {
    const cases = ['light', 'dark'].flatMap((scheme) => [320, 390, 1280]
        .flatMap((width) => ['zh-CN', 'en-US'].map((locale) => ({ scheme, width, locale }))));
    for (const { scheme, width, locale } of cases) {
        await t.test(`${width}px ${locale} ${scheme}`, async (t) => {
            const { page, order } = await openCheckout(t, { width, locale, colorScheme: scheme,
                order: { money: '100', actual_amount: '14.9' },
                methods: [{ ...paymentMethod('USDC', 'solana', 'Solana', '14.9'), amount: '100' }] });
            await assertSummary(page, order, 'USDC', /Solana/i);
            const payment = await designContract(page, '#paymentStage', scheme);
            const network = await page.locator('#payNetworkQ').innerText();
            await screenshot(page, `journey-payment-${width}-${locale}-${scheme}`);
            await page.locator('#reselectPaymentBtn').click();
            for (const quoted of [false, true]) {
                if (quoted) { await choose(page, 'currencySelect', 'USDC'); await choose(page, 'networkSelect', 'Solana'); }
                const selection = await designContract(page, '#selectionStage', scheme);
                for (const key of ['width', 'headerWidth', 'headerHeight', 'logoWidth', 'logoHeight']) {
                    assert.ok(Math.abs(selection[key] - payment[key]) <= 1, `${key} differs between selection and payment: ${selection[key]} vs ${payment[key]}`);
                }
                for (const key of ['colors', 'typography', 'headerPadding']) assert.deepEqual(selection[key], payment[key], `${key} must match between stages`);
                await expect(page.locator('#selectionAmountLabel')).toHaveAttribute('data-i18n', quoted ? 'estimatedReceiptLabel' : 'selectionOrderAmount');
                await expect(page.locator(quoted ? '#selectionFiatCaption' : '#selectionFiatPreview')).toBeVisible();
                await expect(page.locator(quoted ? '#selectionFiatPreview' : '#selectionFiatCaption')).toBeHidden();
                if (quoted) {
                    await expect(page.locator('#payAmountValueS')).toHaveText('14.9');
                    await expect(page.locator('#payCurrencyS')).toHaveText('USDC');
                    await expect(page.locator('#payAmountCrypto')).toHaveText(/14\.9\s*USDC/);
                    await expect(page.locator('#orderMoneyCaptionS')).toHaveText('100');
                    await expect(page.locator('#orderFiatCaptionS')).toHaveText('CNY');
                    await expect(page.locator('#payNetworkTag')).toHaveText(await page.evaluate(() => Payment.t('networkPrefix')) + network);
                    for (const language of locale === 'zh-CN' ? ['en', 'zh'] : ['zh', 'en']) {
                        await switchLanguage(page, language);
                        await expect(page.locator('#payAmountCrypto')).toHaveText(/14\.9\s*USDC/);
                        await expect(page.locator('#networkSelect .select-text')).toHaveText(network);
                        await expect(page.locator('#selectionAmountLabel')).toHaveText(await page.evaluate(() => Payment.t('estimatedReceiptLabel')));
                    }
                } else { await expect(page.locator('#selectionFiatPreview')).toHaveText(/100\s*CNY/); await expect(page.locator('#payAmountCrypto')).toBeHidden(); }
                await screenshot(page, `journey-selection-${quoted ? 'selected' : 'unselected'}-${width}-${locale}-${scheme}`);
            }
        });
    }
});

async function assertLayout(page, stage) {
    const violations = await page.locator(stage).evaluate((root) => {
        const problems = [];
        const viewport = document.documentElement.clientWidth;
        if (document.documentElement.scrollWidth > viewport + 2) problems.push('Document scrolls horizontally');
        const critical = root.id === 'paymentStage'
            ? '#payAmountQ, #paymentCurrencyQ, #payNetworkQ, #paymentNetworkNoteQ, #timerDisplayQ, #orderIdQ, #walletAddress, #addressNetworkQ, .deposit-notice, .network-warning'
            : '#orderMoneyS, #orderFiatS, #orderIdS, #payAmountCrypto, #payNetworkTag, #timerDisplay, #selectionAmountLabel, #selectionFiatCaption, .form-label, .select-text';
        const boxes = [];
        const seen = new Set();
        const rootBox = root.getBoundingClientRect();
        for (const element of root.querySelectorAll(critical)) {
            if (!element.getClientRects().length) continue;
            const name = element.id || element.className;
            if (element.clientWidth && element.scrollWidth > element.clientWidth + 2) problems.push(`${name} clips or overflows horizontally`);
            const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
            for (let node = walker.nextNode(); node; node = walker.nextNode()) {
                if (!node.textContent.trim() || seen.has(node)) continue;
                seen.add(node);
                const range = document.createRange();
                range.selectNodeContents(node);
                for (const rectangle of range.getClientRects()) {
                    if (rectangle.width && rectangle.height) boxes.push({ name, rectangle });
                }
            }
        }
        const controls = root.id === 'paymentStage'
            ? '#copyAmountQBtn, #copyAddressBtn, #qrcode canvas, #reselectPaymentBtn'
            : '#copyAmountBtn, #payBtn';
        for (const element of root.querySelectorAll(controls)) {
            if (element.getClientRects().length) boxes.push({ name: element.id || 'QR canvas', rectangle: element.getBoundingClientRect() });
        }
        for (const { name, rectangle } of boxes) {
            if (rectangle.left < Math.max(0, rootBox.left) - 2 || rectangle.right > Math.min(viewport, rootBox.right) + 2) {
                problems.push(`${name} exceeds the checkout/viewport boundary`);
            }
        }
        // Text fragments, not whole multiline containers, avoid reporting intentional whitespace as overlap.
        for (let first = 0; first < boxes.length; first++) {
            for (let second = first + 1; second < boxes.length; second++) {
                const a = boxes[first];
                const b = boxes[second];
                const x = Math.min(a.rectangle.right, b.rectangle.right) - Math.max(a.rectangle.left, b.rectangle.left);
                const y = Math.min(a.rectangle.bottom, b.rectangle.bottom) - Math.max(a.rectangle.top, b.rectangle.top);
                if (x > 1 && y > 1) problems.push(`${a.name} overlaps ${b.name}`);
            }
        }
        return [...new Set(problems)];
    });
    assert.deepEqual(violations, [], `${stage} must not clip text or overlap controls`);
}

async function screenshot(page, name) {
    const setting = process.env.CHECKOUT_SCREENSHOTS;
    if (!setting || setting === '0') return;
    const directory = setting === '1' ? path.join(tmpdir(), 'bepusdt-checkout-screenshots') : path.resolve(setting);
    await mkdir(directory, { recursive: true });
    await page.screenshot({ path: path.join(directory, `${name}.png`), fullPage: true, animations: 'disabled' });
}

for (const width of [320, 390, 1280]) {
    for (const locale of ['zh-CN', 'en-US']) {
        test(`layout ${width}px ${locale}: long values and 18-decimal amount in both stages`, async (t) => {
            const amount = '42.123456789123456789';
            const { page, order } = await openCheckout(t, { width, locale,
                order: { actual_amount: amount, order_id: `merchant-${'ABCDEF0123456789'.repeat(8)}`,
                    token: `0x${'AbCdEf0123456789'.repeat(4)}` },
                methods: [paymentMethod('USDC', 'solana', 'Solana', amount)] });
            await assertSummary(page, order, 'USDC', /Solana/i);
            await page.evaluate(() => document.fonts.ready);
            await assertQr(page, order.token);
            await screenshot(page, `payment-${width}-${locale}`);
            await assertLayout(page, '#paymentStage');
            await page.locator('#copyAmountQBtn').click();
            await page.locator('#copyAddressBtn').click();
            assert.deepEqual(await page.evaluate(() => window.__clipboardWrites), [amount, order.token]);
            await page.locator('#reselectPaymentBtn').click();
            await expect(page.locator('#selectionStage')).toBeVisible();
            await expect(page.locator('#orderIdS')).toHaveText(order.order_id);
            await choose(page, 'currencySelect', 'USDC');
            await choose(page, 'networkSelect', 'Solana');
            await expect(page.locator('#payAmountCrypto')).toHaveText(`${amount} USDC`);
            await screenshot(page, `selection-${width}-${locale}`);
            await assertLayout(page, '#selectionStage');
        });
    }
}

if (process.env.CHECKOUT_SCREENSHOTS && process.env.CHECKOUT_SCREENSHOTS !== '0') {
    test('visual review screenshots', async (t) => {
        for (const colorScheme of ['light', 'dark']) {
            for (const width of [320, 390, 1280]) {
                for (const locale of ['zh-CN', 'en-US']) {
                    await t.test(`${width}px ${locale} ${colorScheme}`, async (t) => {
                        const { page, order } = await openCheckout(t, { width, locale, colorScheme,
                            methods: [paymentMethod('USDC', 'solana', 'Solana', '42.123456')] });
                        await assertSummary(page, order, 'USDC', /Solana/i);
                        await assertQr(page, order.token);
                        await expect(page.locator('#paymentTokenIconQ')).toBeVisible();
                        await expect(page.locator('#paymentNetworkIconQ')).toBeVisible();
                        await page.evaluate(() => document.fonts.ready);
                        await screenshot(page, `review-payment-${width}-${locale}-${colorScheme}`);
                        await assertLayout(page, '#paymentStage');
                        await page.locator('#reselectPaymentBtn').click();
                        await choose(page, 'currencySelect', 'USDC');
                        await choose(page, 'networkSelect', 'Solana');
                        await screenshot(page, `review-selection-${width}-${locale}-${colorScheme}`);
                        await assertLayout(page, '#selectionStage');
                    });
                }
            }
        }
    });
}
