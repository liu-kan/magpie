// A source build shows the release and keeps Check. Download first uses
// the normal update sheet, but warns that this build will be replaced.
// Cancel and Escape send no install; confirming names the chosen release.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { watchSettingsReads, settingsDrawn } = require("./reader.cjs");

const assets = process.env.MAGPIE_SOURCE_ASSETS || path.resolve(__dirname, "../assets");
const words = {
  en: { check: "Check", download: "Download", later: "Later", source: "Development build", warning: "Your configuration is kept." },
  zh: { check: "检查", download: "下载", later: "稍后", source: "当前为开发版", warning: "现有配置会保留。" },
  "zh-TW": { check: "檢查", download: "下載", later: "稍後", source: "目前為開發版", warning: "現有設定會保留。" },
  ja: { check: "確認", download: "ダウンロード", later: "後で", source: "開発ビルド", warning: "現在の設定は保持されます。" },
  de: { check: "Prüfen", download: "Herunterladen", later: "Später", source: "Entwicklungsversion", warning: "Ihre Konfiguration bleibt erhalten." },
};

function server(lang, ctl, web) {
  const settings = { theme: "light", lang, version: "dev", gateway: "http://127.0.0.1:3425", fx: { rate: 7.2 }, noAutoUpdate: true };
  return async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:${web}};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings });
    if (url.pathname === "/api/settings") return json(settings);
    if (url.pathname === "/api/update/check") {
      ctl.checks++;
      ctl.update = { state: "source", current: "dev", latest: "0.1.1234", notes: "Release fixture" };
      return json(ctl.update);
    }
    if (url.pathname === "/api/update/install") {
      ctl.installs.push(req.postDataJSON());
      ctl.update = { state: "downloading", current: "dev", latest: "0.1.1234", done: 10, total: 100 };
      return json(ctl.update);
    }
    if (url.pathname === "/api/update/notes") return json({ latest: "0.1.1234", releases: [{ version: "0.1.1234", notes: "Release fixture" }] });
    if (url.pathname === "/api/update") return json(ctl.update);
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": source updates require confirmation", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of (process.env.GUI_TEST_LANG ? [process.env.GUI_TEST_LANG] : Object.keys(words))) for (const web of [false, true]) for (const width of [420, 900]) {
      await t.test(`${lang}, ${web ? "web" : "desktop"}, ${width}px`, async () => {
        const w = words[lang], ctl = { checks: 0, installs: [], update: { state: "", current: "dev" } };
        const context = await browser.newContext({ viewport: { width, height: 760 }, reducedMotion: "reduce" });
        const page = await context.newPage();
        t.after(() => context.close());
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        page.setDefaultTimeout(5000);
        await page.route("**/*", server(lang, ctl, web));
        await watchSettingsReads(page);
        await page.goto("http://magpie.test/?view=settings&tab=about");
        await settingsDrawn(page);
        const row = page.locator("#about .row.pref").first();
        await row.getByRole("button", { name: w.check, exact: true }).click();
        await row.locator(".sub", { hasText: "0.1.1234" }).waitFor();
        assert.match(await row.innerText(), new RegExp(w.source));
        assert.equal(await row.locator("code").innerText(), "dev");
        assert.equal(await row.getByRole("button", { name: w.check, exact: true }).isEnabled(), true);
        await row.getByRole("button", { name: w.check, exact: true }).click();
        assert.equal(ctl.checks, 2);
        assert.deepEqual(ctl.installs, []);
        assert.equal(await row.evaluate((r) => r.scrollWidth <= r.clientWidth), true, "the version row fits the window");
        if (process.env.ARTIFACT_DIR) {
          await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-${web ? "web" : "desktop"}-${width}-row.png`), animations: "disabled" });
        }
        const download = row.getByRole("button", { name: w.download, exact: true });
        const ask = page.locator("#modal .update-ask");
        const scroll = await page.locator("#view-settings").evaluate((v) => v.scrollTop);
        await download.click();
        await ask.waitFor();
        assert.match(await ask.locator(".lib-confirm").innerText(), new RegExp(w.warning));
        assert.match(await ask.innerText(), /dev/);
        assert.match(await ask.innerText(), /0\.1\.1234/);
        assert.deepEqual(ctl.installs, []);
        await ask.getByRole("button", { name: w.later, exact: true }).click();
        await ask.waitFor({ state: "detached" });
        assert.deepEqual(ctl.installs, []);
        await download.click();
        await ask.waitFor();
        await page.keyboard.press("Escape");
        await ask.waitFor({ state: "detached" });
        assert.deepEqual(ctl.installs, []);
        await download.click();
        await ask.waitFor();
        if (process.env.ARTIFACT_DIR) {
          await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-${web ? "web" : "desktop"}-${width}-source.png`), animations: "disabled" });
        }
        await ask.getByRole("button", { name: w.download, exact: true }).click();
        await row.locator(".sub", { hasText: "10%" }).waitFor();
        assert.equal(ctl.installs.length, 1);
        assert.equal(ctl.installs[0].replaceSource, "0.1.1234");
        assert.equal(await page.locator("#view-settings").evaluate((v) => v.scrollTop), scroll, "no click scrolls the settings page");
        assert.deepEqual(errors, []);
        await context.close();
      });
    }
  });
}
