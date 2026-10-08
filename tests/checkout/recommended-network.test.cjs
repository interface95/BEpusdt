'use strict';

const assert = require('node:assert/strict');
const { before, after, test } = require('node:test');
const { createServer } = require('node:http');
const { readFile, mkdir } = require('node:fs/promises');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { chromium, expect } = require('playwright/test');

const ROOT = path.resolve(__dirname, '../../static');
const NOW = Date.UTC(2026, 9, 8, 12);
const TRADE_ID = 'recommended-network-test';
const mime = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json', '.svg': 'image/svg+xml', '.png': 'image/png', '.woff2': 'font/woff2' };
let server, browser, origin;
const unexpected = [];
const method = (currency, network, name, amount = '14.912345') => ({
    currency, network, token_net_name: name, actual_amount: amount,
    amount: '100', fiat: 'CNY', exchange_rate: '6.71', is_popular: false,
});
const preferred = method('USDT', 'bsc', 'BEP20');
const alternatives = [method('USDT', 'tron', 'TRC20'), method('USDC', 'base', 'Base'), preferred];

before(async () => {
    server = createServer(async (req, res) => {
        const pathname = new URL(req.url, 'http://localhost').pathname;
        if (pathname.startsWith('/api/')) {
            unexpected.push(pathname);
            res.writeHead(500).end('API requests must be mocked');
            return;
        }
        const file = pathname === `/pay/${TRADE_ID}`
            ? path.join(ROOT, 'checkout/official/views/checkout.html')
            : path.resolve(ROOT, '.' + decodeURIComponent(pathname));
        if (!file.startsWith(ROOT + path.sep)) { res.writeHead(403).end(); return; }
        try {
            const content = await readFile(file);
            res.writeHead(200, { 'Content-Type': mime[path.extname(file)] || 'application/octet-stream' });
            res.end(content);
        } catch { res.writeHead(404).end(); }
    });
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    origin = `http://127.0.0.1:${server.address().port}`;
    browser = await chromium.launch({ executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true });
});

after(async () => {
    try { if (browser) await browser.close(); }
    finally { if (server) await new Promise(resolve => server.close(resolve)); }
    assert.deepEqual(unexpected, []);
});

async function open(t, options = {}) {
    const context = await browser.newContext({ locale: options.locale || 'zh-CN',
        viewport: { width: options.width || 1280, height: 1000 }, colorScheme: options.scheme || 'light',
        reducedMotion: 'reduce', serviceWorkers: 'block' });
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const requests = [], errors = [];
    const state = { methods: options.methods || alternatives, methodsCode: options.methodsCode || 200,
        order: { trade_id: TRADE_ID, order_id: 'DEMO-100', status: 1, trade_type: '', token: '',
            money: '100', fiat: 'CNY', created_at: NOW / 1000, expired_at: NOW / 1000 + 1200,
            redirect_url: '/merchant-return', reselect: true, ...options.order } };
    page.on('pageerror', error => errors.push(error.message));
    t.after(async () => { await context.close(); assert.deepEqual(errors, []); });
    await context.route('**/*', async route => {
        const req = route.request(), url = new URL(req.url());
        if (url.origin !== origin) { unexpected.push(req.url()); await route.abort(); return; }
        if (options.missingIcons && url.pathname.includes('/web3icons/')) {
            await route.fulfill({ status: 404, body: '' }); return;
        }
        if (!url.pathname.startsWith('/api/')) { await route.continue(); return; }
        const body = req.postDataJSON();
        requests.push({ path: url.pathname, body });
        assert.equal(body.trade_id, TRADE_ID);
        if (url.pathname === '/api/v1/pay/info') {
            await route.fulfill({ json: { status_code: 200, data: state.order } });
        } else if (url.pathname === '/api/v1/pay/methods') {
            await route.fulfill({ json: { status_code: state.methodsCode,
                data: { methods: state.methods, network_sort: 'tron,ethereum,bsc,base' } } });
        } else if (url.pathname === '/api/v1/pay/update-order') {
            await route.fulfill({ json: { status_code: 400, message: 'Mock payment was not submitted' } });
        } else { unexpected.push(url.pathname); await route.abort(); }
    });
    await page.clock.install({ time: new Date(NOW - 1000) });
    await page.clock.pauseAt(new Date(NOW));
    await page.goto(`${origin}/pay/${TRADE_ID}`, { waitUntil: 'networkidle' });
    return { page, requests, state };
}

async function choose(page, id, value) {
    await page.locator(`#${id} .select-trigger`).click();
    await page.locator(`#${id} .dropdown-item[data-value="${value}"]`).click();
}

test('defaults to the available USDT BEP20 quote without submitting a payment', async t => {
    const { page, requests } = await open(t);
    await expect(page.locator('#currencySelect .select-text')).toHaveText('USDT');
    await expect(page.locator('#networkSelect .select-text')).toHaveText('BNB Smart Chain (BEP20)');
    await expect(page.locator('#recommendedNetwork')).toBeVisible();
    await expect(page.locator('#recommendedNetwork')).toContainText('推荐使用');
    await expect(page.locator('#recommendedNetwork')).toContainText('以钱包或交易所为准');
    await expect(page.locator('#payAmountCrypto')).toHaveText(/14\.912345\s*USDT/);
    await expect(page.locator('#payBtn')).toBeEnabled();
    assert.equal(requests.filter(r => r.path.endsWith('/update-order')).length, 0);
    await page.locator('#networkSelect .select-trigger').click();
    await expect(page.locator('#networkSelect .dropdown-item.selected')).toHaveAttribute('data-value', 'BEP20');
    await expect(page.locator('#networkSelect [data-value="BEP20"] .item-badge')).toHaveText('推荐');
    await page.locator('#networkSelect .dropdown-search').fill('BSC');
    await expect(page.locator('#networkSelect .dropdown-item.selected')).toHaveCount(1);
    await page.locator('.brand-name').first().click();
    await page.locator('#payBtn').click();
    await expect(page.locator('#cmusToast')).toHaveText('Mock payment was not submitted');
    assert.deepEqual(requests.filter(r => r.path.endsWith('/update-order')).map(r => r.body),
        [{ trade_id: TRADE_ID, currency: 'USDT', network: 'bsc' }]);
});

for (const [name, methods, code] of [
    ['USDT unavailable', [method('USDC', 'bsc', 'BEP20')], 200],
    ['BEP20 unavailable', [method('USDT', 'tron', 'TRC20')], 200],
    ['native BNB only', [method('BNB', 'bsc', 'BEP20')], 200],
    ['different pairs cannot be combined', [method('USDT', 'tron', 'TRC20'), method('USDC', 'bsc', 'BEP20')], 200],
    ['empty methods', [], 200], ['methods error', [preferred], 400],
]) {
    test(`${name}: do not suggest or preselect an unsupported pair`, async t => {
        const { page, requests } = await open(t, { methods, methodsCode: code });
        await expect(page.locator('#recommendedNetwork')).toBeHidden();
        await expect(page.locator('#payBtn')).toBeDisabled();
        await expect(page.locator('#currencySelect .select-text')).toHaveClass(/placeholder/);
        await expect(page.locator('#networkSelect .select-text')).toHaveClass(/placeholder/);
        assert.equal(requests.filter(r => r.path.endsWith('/update-order')).length, 0);
    });
}

test('manual choices persist through polling and locale changes; changing currency still clears its network', async t => {
    const { page, requests } = await open(t);
    await expect(page.locator('#networkSelect .select-text')).toHaveText('BNB Smart Chain (BEP20)');
    await choose(page, 'networkSelect', 'TRC20');
    await page.clock.runFor(5000);
    await page.evaluate(() => Payment.switchLang('en'));
    await expect(page.locator('#recommendedNetwork')).toContainText('Recommended');
    await expect(page.locator('#networkSelect .select-text')).toHaveText('TRON (TRC20)');
    await choose(page, 'currencySelect', 'USDC');
    await expect(page.locator('#payBtn')).toBeDisabled();
    await expect(page.locator('#networkSelect .select-text')).toHaveClass(/placeholder/);
    await choose(page, 'networkSelect', 'Base');
    await page.locator('#payBtn').click();
    await expect(page.locator('#cmusToast')).toHaveText('Mock payment was not submitted');
    assert.deepEqual(requests.filter(r => r.path.endsWith('/update-order')).map(r => r.body),
        [{ trade_id: TRADE_ID, currency: 'USDC', network: 'base' }]);
});

test('an assigned invoice keeps its payment network and explicit reselection is not forced to BEP20', async t => {
    const { page, requests } = await open(t, { order: { trade_type: 'usdc.solana', token: 'demo-address', actual_amount: '14.9',
        network: { crypto: 'USDC', network: 'solana', name: 'Solana' } } });
    await expect(page.locator('#paymentStage')).toBeVisible();
    await expect(page.locator('#payNetworkQ')).toContainText('Solana');
    await expect(page.locator('#paymentCurrencyQ')).toHaveText('USDC');
    assert.equal(requests.filter(r => r.path.endsWith('/methods')).length, 0);
    await page.locator('#reselectPaymentBtn').click();
    await expect(page.locator('#recommendedNetwork')).toBeVisible();
    await expect(page.locator('#payBtn')).toBeDisabled();
    await expect(page.locator('#currencySelect .select-text')).toHaveClass(/placeholder/);
});

test('missing icons do not prevent the recommended default', async t => {
    const { page } = await open(t, { missingIcons: true });
    await expect(page.locator('#networkSelect .select-text')).toHaveText('BNB Smart Chain (BEP20)');
    await expect(page.locator('#payBtn')).toBeEnabled();
});

test('a late methods response cannot restore a recommendation or replace a newer manual choice', async t => {
    const { page, requests, state } = await open(t);
    await expect(page.locator('#recommendedNetwork')).toBeVisible();
    let olderRoute, requestCount = 0, notifyOlder;
    const olderRequested = new Promise(resolve => { notifyOlder = resolve; });
    await page.route('**/api/v1/pay/methods', async route => {
        requestCount++;
        if (requestCount === 1) {
            olderRoute = route;
            notifyOlder();
            return;
        }
        await route.fulfill({ json: { status_code: 200, data: {
            methods: [method('USDC', 'base', 'Base')], network_sort: 'base',
        } } });
    });
    const config = { ...state.order, preferRecommended: true };
    await page.evaluate(config => Payment.init(config), config);
    await olderRequested;
    await expect(page.locator('#recommendedNetwork')).toBeHidden();
    await expect(page.locator('#payBtn')).toBeDisabled();
    await page.evaluate(config => Payment.init(config), config);
    await choose(page, 'currencySelect', 'USDC');
    await choose(page, 'networkSelect', 'Base');
    const olderResponse = page.waitForResponse('**/api/v1/pay/methods');
    await olderRoute.fulfill({ json: { status_code: 200, data: { methods: alternatives } } });
    await (await olderResponse).finished();
    await page.clock.runFor(100);
    await expect(page.locator('#recommendedNetwork')).toBeHidden();
    await expect(page.locator('#currencySelect .select-text')).toHaveText('USDC');
    await expect(page.locator('#networkSelect .select-text')).toHaveText('Base');
    await expect(page.locator('#payBtn')).toBeEnabled();
    assert.equal(requests.filter(r => r.path.endsWith('/update-order')).length, 0);
});

for (const width of [320, 390, 1280]) {
    for (const scheme of ['light', 'dark']) {
        for (const locale of ['zh-CN', 'en-US']) {
            test(`recommended layout ${width}px ${scheme} ${locale}`, async t => {
                const { page } = await open(t, { width, scheme, locale });
                const note = page.locator('#recommendedNetwork');
                await expect(note).toBeVisible();
                await expect(page.locator('#networkSelect .select-text')).toHaveText('BNB Smart Chain (BEP20)');
                assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
                const noteBox = await note.boundingBox();
                const currencyBox = await page.locator('#currencySelect').boundingBox();
                assert.ok(noteBox.y + noteBox.height <= currencyBox.y, 'Recommendation must not overlap currency control');
                for (const element of await note.locator('strong, p').all()) {
                    assert.ok(await element.evaluate(el => el.scrollWidth <= el.clientWidth + 1), 'Recommendation text must fit');
                }
                const dir = path.join(tmpdir(), 'bepusdt-recommended-network');
                await mkdir(dir, { recursive: true });
                await page.screenshot({ path: path.join(dir, `${width}-${scheme}-${locale}.png`), fullPage: true, animations: 'disabled' });
            });
        }
    }
}
