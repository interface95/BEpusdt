'use strict';

// NODE_PATH=/Users/wujun/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/node_modules node --test --test-concurrency=1 tests/admin/order-user-ui.test.cjs
const assert = require('node:assert/strict');
const { before, after, test } = require('node:test');
const { mkdir } = require('node:fs/promises');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { chromium, expect } = require('playwright/test');
const { userUiConfig, importWeb } = require('./order-user-vite.cjs');

const SCREENSHOTS = path.join(tmpdir(), 'bepusdt-order-user-ui');
const LONG_NAME = '这是用于验证订单用户完整名称的超长显示名称'.repeat(8);
const LONG_USERNAME = 'long_username_without_spaces_'.repeat(10);
const LONG_ID = '000000123456789012345678901234567890';
let server;
let browser;
let origin;
const unmockedApi = [];

function order(id, overrides = {}) {
    return {
        id, order_id: `merchant-${id}`, trade_id: `trade-${id}`, trade_type: 'usdt.trc20',
        rate: '7.1', amount: '42.5', money: 301.75, crypto: 'USDT', fiat: 'CNY',
        address: 'TMockWalletAddressForUserUiOnly', from_address: '', status: 3,
        name: '测试商品', api_type: 'native', return_url: '', notify_url: '',
        notify_num: 0, notify_state: 0, ref_hash: '', ref_block_num: 0,
        expired_at: '2026-10-07 12:00:00', created_at: '2026-10-07 11:00:00',
        ...overrides,
    };
}

before(async () => {
    await mkdir(SCREENSHOTS, { recursive: true });
    const { createServer } = await importWeb('vite');
    server = await createServer(await userUiConfig(unmockedApi));
    await server.listen();
    origin = `http://127.0.0.1:${server.httpServer.address().port}`;
    browser = await chromium.launch({
        executablePath: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', headless: true,
    });
});

after(async () => {
    try {
        if (browser) await browser.close();
    } finally {
        if (server) await server.close();
    }
    assert.deepEqual(unmockedApi, [], 'No API request may reach a real server');
});

async function openOrders(t, orders, options = {}) {
    const context = await browser.newContext({ locale: 'zh-CN',
        viewport: { width: options.width || 1440, height: options.height || 1000 },
        reducedMotion: 'reduce', serviceWorkers: 'block', isMobile: !!options.mobile,
        hasTouch: !!options.mobile });
    const page = await context.newPage();
    page.setDefaultTimeout(7000);
    const requests = [];
    const blocked = [];
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    t.after(async () => {
        await context.close();
        assert.deepEqual(blocked, [], 'Unexpected network requests must not escape the mock');
        assert.deepEqual(errors, [], 'The real Vue app must not raise browser exceptions');
    });
    await context.route('**/*', async (route) => {
        const request = route.request();
        const url = new URL(request.url());
        if (url.origin !== origin) {
            blocked.push(request.url());
            return route.abort('blockedbyclient');
        }
        if (!url.pathname.startsWith('/api/')) return route.continue();
        const body = request.postData() ? request.postDataJSON() : undefined;
        requests.push({ path: url.pathname, method: request.method(), body });
        let data;
        if (url.pathname === '/api/auth/info') {
            data = { admin_username: 'UI测试', trade_type: { 'usdt.trc20': 'USDT / TRON' } };
        } else if (url.pathname === '/api/auth/menu') {
            data = [{ path: '/order', name: 'order', component: 'order/order',
                meta: { title: '订单管理', icon: 'icon-list', keepAlive: false } }];
        } else if (url.pathname === '/api/order/list') {
            assert.equal(request.method(), 'POST');
            data = orders;
        } else if (url.pathname === '/api/order/detail') {
            assert.equal(request.method(), 'POST');
            data = (options.details || orders).find((item) => item.id === body.id);
            assert.ok(data, 'Detail requests must select an existing mocked order');
        } else {
            blocked.push(url.pathname);
            return route.abort('blockedbyclient');
        }
        return route.fulfill({ json: { code: 200, data, total: 30 } });
    });
    await page.addInitScript(() => {
        localStorage.setItem('user-info', JSON.stringify({ token: 'mock-user-ui-only' }));
        window.__userUiXss = false;
    });
    await page.goto(`${origin}/#/order`, { waitUntil: 'networkidle' });
    try {
        await expect(page.locator('.arco-table')).toContainText(orders[0].order_id);
    } catch (error) {
        console.error({ url: page.url(), body: await page.locator('body').innerText(), requests, errors });
        await page.screenshot({ path: path.join(SCREENSHOTS, 'app-load-failure.png'), fullPage: true });
        throw error;
    }
    return { page, requests };
}

const rowFor = (page, id) => page.locator('.arco-table tbody tr').filter({ hasText: `merchant-${id}` });
const userCell = (page, id) => rowFor(page, id).locator('.order-user');
const detailField = (page, label) => page.locator('.arco-modal .detail-item')
    .filter({ has: page.locator('.detail-label', { hasText: new RegExp(`^${label}$`) }) }).locator('.detail-value');

async function openDetail(page, id) {
    await rowFor(page, id).getByRole('button', { name: '详情', exact: true }).click();
    await expect(page.locator('.arco-modal:visible')).toBeVisible();
    await expect(page.locator('.arco-modal:visible')).not.toHaveClass(/enter-(from|active)/);
}

test('user column follows merchant order and preserves IDs, preferred names, unique usernames and old rows', async (t) => {
    const { page } = await openOrders(t, [
        order(1, { user_id: '000042', display_name: '张三', username: 'zhangsan' }),
        order(2, { user_id: '2', display_name: 'same_name', username: 'same_name' }),
        order(3, { username: 'username_only' }),
        order(4), order(5, { user_id: '', username: '', display_name: '' }),
        order(6, { user_id: '000006' }), order(7, { display_name: '仅显示名称' }),
    ]);
    const headers = await page.locator('.arco-table-th').allTextContents();
    assert.equal(headers[headers.indexOf('商户订单') + 1], '用户');
    await expect(userCell(page, 1)).toContainText('000042');
    await expect(userCell(page, 1).locator('.order-user-name')).toHaveText('张三');
    await expect(userCell(page, 1).locator('.order-user-username')).toContainText('zhangsan');
    assert.equal((await userCell(page, 2).innerText()).split('same_name').length - 1, 1);
    await expect(userCell(page, 3).locator('.order-user-name')).toHaveText('username_only');
    await expect(userCell(page, 4)).toHaveText('未提供');
    await expect(userCell(page, 5)).toHaveText('未提供');
    await expect(userCell(page, 6)).toContainText('000006');
    await expect(userCell(page, 7)).toContainText('仅显示名称');
    await page.screenshot({ path: path.join(SCREENSHOTS, 'desktop-list.png'), fullPage: true, animations: 'disabled' });
});

test('detail reads complete user fields from detail API and old orders do not retain a previous user', async (t) => {
    const { page, requests } = await openOrders(t, [order(1), order(2)], { details: [
        order(1, { user_id: LONG_ID, username: LONG_USERNAME, display_name: LONG_NAME }), order(2),
    ] });
    await openDetail(page, 1);
    await expect(detailField(page, '用户ID')).toHaveText(LONG_ID);
    await expect(detailField(page, '用户名')).toHaveText(LONG_USERNAME);
    await expect(detailField(page, '显示名称')).toHaveText(LONG_NAME);
    await page.screenshot({ path: path.join(SCREENSHOTS, 'desktop-detail.png'), fullPage: true, animations: 'disabled' });
    await page.getByRole('button', { name: '关闭窗口' }).click();
    await openDetail(page, 2);
    for (const field of ['用户ID', '用户名', '显示名称']) {
        await expect(detailField(page, field)).toHaveText('未提供');
    }
    assert.deepEqual(requests.filter((request) => request.path === '/api/order/detail').map((request) => request.body),
        [{ id: 1 }, { id: 2 }]);
});

test('expanded user filters send exact string ID and name keyword, query starts at page 1 and reset clears both', async (t) => {
    const { page, requests } = await openOrders(t, [order(1)]);
    const listRequests = () => requests.filter((request) => request.path === '/api/order/list');
    await page.locator('.arco-pagination-item').filter({ hasText: /^2$/ }).click();
    await expect.poll(() => listRequests().at(-1).body.page).toBe(2);
    await expect(page.getByPlaceholder('请输入用户ID')).toHaveCount(0);
    await page.getByRole('button', { name: '展开', exact: true }).click();
    await page.getByPlaceholder('请输入用户ID').fill('000042');
    await page.getByPlaceholder('请输入用户名或显示名称').fill('张三_50%');
    const beforeQuery = listRequests().length;
    await page.getByRole('button', { name: '查询', exact: true }).click();
    await expect.poll(() => listRequests().length).toBe(beforeQuery + 1);
    assert.equal(listRequests().at(-1).body.user_id, '000042');
    assert.equal(listRequests().at(-1).body.username, '张三_50%');
    assert.equal(listRequests().at(-1).body.page, 1);
    await page.locator('.arco-pagination-item').filter({ hasText: /^2$/ }).click();
    await expect.poll(() => listRequests().at(-1).body.page).toBe(2);
    assert.equal(listRequests().at(-1).body.user_id, '000042');
    const beforeReset = listRequests().length;
    await page.getByRole('button', { name: '重置', exact: true }).click();
    await expect.poll(() => listRequests().length).toBe(beforeReset + 1);
    assert.equal(listRequests().at(-1).body.page, 1);
    assert.equal(listRequests().at(-1).body.user_id, '');
    assert.equal(listRequests().at(-1).body.username, '');
    await expect(page.getByPlaceholder('请输入用户ID')).toHaveValue('');
    await expect(page.getByPlaceholder('请输入用户名或显示名称')).toHaveValue('');
    await expect(page.locator('.arco-table .arco-spin-loading')).toHaveCount(0);
    await page.screenshot({ path: path.join(SCREENSHOTS, 'desktop-filters.png'), fullPage: true, animations: 'disabled' });
});

test('untrusted user values remain literal text in table and detail', async (t) => {
    const payload = '<img src=x onerror="window.__userUiXss=true">';
    const { page } = await openOrders(t, [order(1, { user_id: payload, username: '<script>alert(1)</script>', display_name: payload })]);
    await expect(userCell(page, 1)).toContainText(payload);
    await expect(userCell(page, 1).locator('img, script')).toHaveCount(0);
    await openDetail(page, 1);
    await expect(detailField(page, '用户ID')).toHaveText(payload);
    await expect(detailField(page, '显示名称')).toHaveText(payload);
    await expect(detailField(page, '用户名')).toHaveText('<script>alert(1)</script>');
    await expect(page.locator('.arco-modal img, .arco-modal script')).toHaveCount(0);
    assert.equal(await page.evaluate(() => window.__userUiXss), false);
});

for (const viewport of [{ width: 375, height: 812, name: 'mobile' }, { width: 812, height: 375, name: 'landscape' }]) {
    test(`${viewport.name}: long user values stay compact in table and wrap fully in reachable detail`, async (t) => {
        const { page } = await openOrders(t, [order(1, { user_id: LONG_ID, username: LONG_USERNAME, display_name: LONG_NAME })],
            { ...viewport, mobile: true });
        const cell = userCell(page, 1);
        await expect(cell).toContainText(LONG_ID);
        const name = cell.locator('.order-user-name');
        await expect(name).toHaveText(LONG_NAME);
        assert.ok(await name.evaluate((element) => element.scrollWidth > element.clientWidth), 'Long table name uses ellipsis');
        assert.equal(await name.evaluate((element) => getComputedStyle(element).textOverflow), 'ellipsis');
        assert.ok((await cell.boundingBox()).height <= 90, 'User column must not create very tall rows');
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'No document-level overflow');
        const scroller = page.locator('.arco-table-body, .arco-table-content-scroll-x').first();
        await scroller.evaluate((element) => { element.scrollLeft = 320; });
        await expect.poll(() => name.evaluate((element) => {
            const bounds = element.getBoundingClientRect();
            const hit = document.elementFromPoint(bounds.right - 2, bounds.y + bounds.height / 2);
            return hit === element || element.contains(hit);
        })).toBe(true);
        await page.screenshot({ path: path.join(SCREENSHOTS, `${viewport.name}-list.png`), fullPage: true, animations: 'disabled' });
        // The existing detail action is a keyboard/touch-accessible path to all truncated values.
        const detailButton = rowFor(page, 1).getByRole('button', { name: '详情', exact: true });
        await detailButton.focus();
        await page.keyboard.press('Enter');
        await expect(page.locator('.arco-modal:visible')).not.toHaveClass(/enter-(from|active)/);
        await expect(detailField(page, '显示名称')).toHaveText(LONG_NAME);
        await expect(detailField(page, '用户名')).toHaveText(LONG_USERNAME);
        for (const label of ['用户ID', '用户名', '显示名称']) {
            const value = detailField(page, label);
            await value.scrollIntoViewIfNeeded();
            const bounds = await value.boundingBox();
            assert.ok(bounds.x >= 0 && bounds.x + bounds.width <= viewport.width, `${label} fits the viewport`);
            assert.ok(await value.evaluate((element) => element.scrollWidth <= element.clientWidth + 1), `${label} wraps without clipping`);
        }
        await detailField(page, '显示名称').scrollIntoViewIfNeeded();
        await page.screenshot({ path: path.join(SCREENSHOTS, `${viewport.name}-detail.png`), fullPage: true, animations: 'disabled' });
        await page.getByRole('button', { name: '关闭窗口' }).click();
        await page.getByRole('button', { name: '展开', exact: true }).click();
        await page.getByPlaceholder('请输入用户名或显示名称').fill('移动端');
        await page.getByRole('button', { name: '查询', exact: true }).click();
        await expect(page.getByPlaceholder('请输入用户名或显示名称')).toHaveValue('移动端');
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    });
}
