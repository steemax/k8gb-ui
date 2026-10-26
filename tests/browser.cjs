// Run a local demo first, then:
// PLAYWRIGHT_MODULE=/path/to/playwright K8GB_UI_URL=http://127.0.0.1:8080 node tests/browser.cjs
const assert = require('node:assert/strict');
const path = require('node:path');
const fs = require('node:fs');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const base = process.env.K8GB_UI_URL || 'http://127.0.0.1:8080';
const artifacts = path.resolve(process.env.ARTIFACTS_DIR || 'artifacts');

(async () => {
  fs.mkdirSync(artifacts, {recursive:true});
  const browser = await chromium.launch({headless:true});
  const failures = [];
  let checks = 0;
  try {
    const context = await browser.newContext({viewport:{width:1440,height:1000}});
    const page = await context.newPage();
    const external = [];
    page.on('pageerror', error => failures.push(error.message));
    page.on('request', request => { if (!request.url().startsWith(new URL(base).origin)) external.push(request.url()); });
    await page.goto(base, {waitUntil:'networkidle'});
    await page.locator('.k8gb-tile').first().waitFor();
    await page.keyboard.press('Tab');
    assert.equal(await page.locator('.k8gb-skip').evaluate(node => getComputedStyle(node).clipPath),'none');
    await page.keyboard.press('Tab');
    assert.notEqual(await page.locator('.k8gb-skip').evaluate(node => getComputedStyle(node).clipPath),'none');
    assert.equal(await page.locator('html').getAttribute('data-theme'), 'light');
    assert.equal(await page.locator('.k8gb-tile').count(), 5);
    assert.equal(await page.locator('#demoBadge').isVisible(), true);
    assert.equal(await page.locator('button').filter({hasText:/create|apply|exclude|disable/i}).count(), 0);
    checks++;
    await page.screenshot({path:path.join(artifacts,'desktop-cards.png'),fullPage:true});

    await page.locator('#dnsFilter').check();
    assert.equal(await page.locator('.k8gb-tile').count(), 1);
    assert.match(await page.locator('#resources').innerText(), /catalog/);
    await page.locator('#dnsFilter').uncheck();
    await page.locator('#search').fill('checkout');
    assert.equal(await page.locator('.k8gb-tile').count(), 1);
    await page.locator('#search').fill('');
    await page.getByRole('button',{name:'Row view',exact:true}).click();
    assert.equal(await page.locator('.k8gb-object-row').count(), 5);
    await page.reload({waitUntil:'networkidle'});
    await page.locator('.k8gb-object-row').first().waitFor();
    assert.equal(await page.locator('.k8gb-object-row').count(), 5);
    checks++;
    await page.screenshot({path:path.join(artifacts,'desktop-rows.png'),fullPage:true});

    await page.getByRole('link',{name:'checkout',exact:true}).click();
    await page.getByRole('tab',{name:'DNS Policy',exact:true}).click();
    const diagram = page.locator('#k8gbPolicyDNS');
    await diagram.locator('svg[role="img"]').waitFor();
    assert.match(await diagram.innerText(), /checkout\.apps\.example\.test/);
    const before = await diagram.getAttribute('data-dns-step');
    await page.waitForTimeout(3800);
    assert.notEqual(await diagram.getAttribute('data-dns-step'), before);
    assert.ok(await diagram.locator('svg[role="img"]').evaluate(svg => svg.getBBox().width > 100));
    checks++;
    await page.screenshot({path:path.join(artifacts,'desktop-policy.png'),fullPage:true});

    await page.getByRole('button',{name:'Switch to dark theme'}).click();
    await page.reload({waitUntil:'networkidle'});
    assert.equal(await page.locator('html').getAttribute('data-theme'),'dark');
    await page.getByRole('button',{name:'Switch to light theme'}).click();
    await page.locator('#namespace').selectOption('sandbox');
    await page.waitForFunction(() => document.getElementById('snapshotScope').textContent.includes('sandbox'));
    assert.equal(await page.locator('.k8gb-tile').count(),0);
    await page.locator('#namespace').selectOption('demo');
    await page.waitForFunction(() => document.getElementById('snapshotScope').textContent.includes('demo'));
    checks++;

    // Retain and explicitly label the last valid snapshot on a failed refresh.
    await page.route('**/api/v1/overview?*', route => route.fulfill({status:502,contentType:'application/json',body:JSON.stringify({error:'Test agent outage'})}));
    await page.getByRole('button',{name:'Refresh snapshots',exact:true}).click();
    await page.getByText('Stale snapshot.',{exact:true}).waitFor();
    assert.ok((await page.locator('#resources').innerText()).includes('catalog'));
    await page.unroute('**/api/v1/overview?*');
    checks++;

    const mobile = await browser.newContext({viewport:{width:390,height:844},isMobile:true,deviceScaleFactor:1});
    const m = await mobile.newPage();
    m.on('pageerror', error => failures.push(error.message));
    await m.goto(base,{waitUntil:'networkidle'});
    await m.locator('.k8gb-tile').first().waitFor();
    assert.equal(await m.locator('html').getAttribute('data-theme'),'light');
    assert.ok(await m.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await m.screenshot({path:path.join(artifacts,'mobile-cards.png'),fullPage:true});
    await m.getByRole('link',{name:'checkout',exact:true}).click();
    await m.getByRole('tab',{name:'DNS Policy',exact:true}).click();
    await m.locator('#k8gbPolicyDNS svg[role="img"]').waitFor();
    assert.notEqual(await m.locator('.k8gb-skip').evaluate(node => getComputedStyle(node).clipPath),'none');
    assert.ok(await m.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await m.screenshot({path:path.join(artifacts,'mobile-policy.png'),fullPage:true});
    checks++;
    assert.deepEqual(external,[],'Runtime requested an external asset');
    assert.deepEqual(failures,[],'Browser exceptions');
    console.log(`${checks} browser scenarios passed; screenshots: ${artifacts}`);
    await mobile.close();
    await context.close();
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode=1; });
