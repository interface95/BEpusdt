'use strict';

const { createRequire } = require('node:module');
const { mkdtemp } = require('node:fs/promises');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

const WEB = path.resolve(__dirname, '../../web');
const webRequire = createRequire(path.join(WEB, 'package.json'));
const importWeb = (name) => import(pathToFileURL(webRequire.resolve(name)).href);

// Use the real app and component resolvers without generating declarations in web/src.
async function userUiConfig(unmockedApi = []) {
    const { default: vue } = await importWeb('@vitejs/plugin-vue');
    const { default: AutoImport } = webRequire('unplugin-auto-import/vite');
    const { default: Components } = webRequire('unplugin-vue-components/vite');
    const { ArcoResolver } = webRequire('unplugin-vue-components/resolvers');
    const { createSvgIconsPlugin } = await importWeb('vite-plugin-svg-icons');
    const { default: postcssPresetEnv } = await importWeb('postcss-preset-env');
    const cacheDir = await mkdtemp(path.join(tmpdir(), 'bepusdt-user-vite-'));
    return {
        root: WEB, configFile: false, cacheDir, base: '/', logLevel: 'error',
        plugins: [
            vue(),
            createSvgIconsPlugin({ iconDirs: [path.join(WEB, 'src/assets/svgs')], symbolId: 'icon-[dir]-[name]' }),
            AutoImport({ imports: ['vue', 'vue-router'], dirs: [path.join(WEB, 'src/globals')],
                resolvers: [ArcoResolver()], dts: false, eslintrc: { enabled: false } }),
            Components({ dirs: [path.join(WEB, 'src/components')], extensions: ['vue'],
                resolvers: [ArcoResolver({ sideEffect: true })], dts: false }),
            { name: 'block-unmocked-admin-api', configureServer(server) {
                server.middlewares.use((request, response, next) => {
                    if (request.url.startsWith('/api/')) {
                        unmockedApi.push(request.url);
                        response.writeHead(500).end('API requests must be mocked');
                    } else next();
                });
            } },
        ],
        resolve: { alias: { '@': path.join(WEB, 'src'), '@assets': path.join(WEB, 'src/assets') } },
        optimizeDeps: { noDiscovery: true, include: ['vue', 'vue-router', 'pinia', 'pinia-plugin-persistedstate',
            'axios', 'nprogress', '@arco-design/web-vue', '@arco-design/web-vue/es/icon', '@vueuse/core',
            'vue-i18n', '@arco-design/color', 'vue-pick-colors'] },
        css: { postcss: { plugins: [postcssPresetEnv()] }, preprocessorOptions: {
            scss: { additionalData: '@use "@/style/var/index.scss" as *; ', silenceDeprecations: ['legacy-js-api'] },
        } },
        server: { host: '127.0.0.1', port: 0, open: false, hmr: false },
    };
}

module.exports = { userUiConfig, importWeb };
