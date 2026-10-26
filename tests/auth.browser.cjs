// PLAYWRIGHT_MODULE=/path/to/playwright node tests/auth.browser.cjs
// Uses only a temporary HTTPS fixture with illustrative agents; no Kubernetes.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname,'..');
const dir = fs.mkdtempSync(path.join(os.tmpdir(),'k8gb-auth-browser-'));
const artifacts = path.resolve(process.env.ARTIFACTS_DIR || path.join(root,'artifacts/authentication'));
const fixture = spawn('go',['test','./internal/console','-run','^TestBuiltinBrowserFixture$','-count=1','-timeout=3m'],{cwd:root,env:{...process.env,K8GB_AUTH_BROWSER_DIR:dir},stdio:['ignore','pipe','pipe']});
let output = '', completed = false;
fixture.stdout.on('data',chunk => output += chunk);
fixture.stderr.on('data',chunk => output += chunk);
const done = new Promise((resolve,reject) => { fixture.on('error',reject); fixture.on('exit',code => { completed = true; resolve(code); }); });
const pause = ms => new Promise(resolve => setTimeout(resolve,ms));
(async () => {
  let browser;
  const failures = [];
  try {
    fs.mkdirSync(artifacts,{recursive:true});
    const ready = path.join(dir,'ready.json');
    for (let i=0; !fs.existsSync(ready); i++) {
      if (completed || i > 600) throw new Error(`HTTPS fixture did not start: ${output}`);
      await pause(100);
    }
    const {url} = JSON.parse(fs.readFileSync(ready,'utf8'));
    browser = await chromium.launch({headless:true});
    for (const viewport of [{width:1440,height:1000},{width:390,height:844}]) {
      for (const theme of ['light','dark']) {
        const context = await browser.newContext({ignoreHTTPSErrors:true,viewport});
        await context.addInitScript(theme => { if (theme === 'dark') localStorage.setItem('k8gb-ui-theme','dark'); },theme);
        const page = await context.newPage();
        page.on('pageerror',error => failures.push(error.message));
        await page.goto(url,{waitUntil:'networkidle'});
        assert.equal(new URL(page.url()).pathname,'/auth/login');
        await page.getByRole('heading',{name:'Sign in',exact:true}).waitFor();
        assert.equal(await page.locator('html').getAttribute('data-theme'),theme);
        assert.equal(await page.locator('#password').getAttribute('type'),'password');
        assert.equal((await page.request.get(`${url}/api/v1/session`)).status(),401);
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        const panel = await page.locator('.k8gb-login-panel').boundingBox();
        assert.ok(panel.width <= 380 && panel.x >= 0 && panel.x + panel.width <= viewport.width);
        await page.screenshot({path:path.join(artifacts,`login-${theme}-${viewport.width}.png`),fullPage:true});
        if (viewport.width === 1440 && theme === 'light') {
          await page.getByLabel('Username',{exact:true}).fill('reader');
          await page.getByLabel('Password',{exact:true}).fill('not the test password');
          const rejectedResponse = page.waitForResponse(response => response.url().endsWith('/auth/login') && response.request().method() === 'POST');
          await page.getByRole('button',{name:'Sign in',exact:true}).click();
          const rejected = await rejectedResponse;
          assert.equal(rejected.status(),401,`Unexpected rejection: ${rejected.status()}, Origin=${rejected.request().headers().origin}, fetch=${rejected.request().headers()['sec-fetch-site']}, text=${await page.locator('body').innerText()}`);
          await page.getByRole('alert').filter({hasText:'Invalid username or password.'}).waitFor();
          await page.getByLabel('Username',{exact:true}).fill('reader');
          await page.getByLabel('Password',{exact:true}).fill('only-a-test-passphrase');
          await page.getByRole('button',{name:'Sign in',exact:true}).click();
          await page.locator('#namespace:not([disabled])').waitFor();
          const session = await (await page.request.get(`${url}/api/v1/session`)).json();
          assert.equal(session.subject,'builtin:reader');
          assert.equal(session.mode,'builtin');
          assert.equal(session.readOnly,true);
          const names = await (await page.request.get(`${url}/api/v1/namespaces`)).json();
          assert.deepEqual(names.namespaces,['demo']);
          assert.equal((await page.request.get(`${url}/api/v1/overview?namespace=private`)).status(),403);
          assert.equal((await page.request.post(`${url}/api/v1/mutations/plan`,{headers:{Origin:url,'X-K8gb-Intent':'mutation'},data:{targets:[]}})).status(),403);
          assert.equal(await page.getByRole('button',{name:'Sign out',exact:true}).isVisible(),true);
          const cookies = await context.cookies();
          const cookie = cookies.find(item => item.name === '__Host-k8gb-session');
          assert.ok(cookie && cookie.httpOnly && cookie.secure && cookie.sameSite === 'Lax');
          await page.getByRole('button',{name:'Sign out',exact:true}).click();
          await page.getByRole('heading',{name:'Sign in',exact:true}).waitFor();
          assert.equal((await page.request.get(`${url}/api/v1/session`)).status(),401);
        }
        await context.close();
      }
    }
    assert.deepEqual(failures,[]);
    fs.writeFileSync(path.join(artifacts,'acceptance.json'),JSON.stringify({login:'HTTPS, CSRF, invalid credentials, successful sign-in and sign-out',authorization:'viewer scope and API write rejection',themes:['light','dark'],viewports:[1440,390],kubernetesAccess:false,failures},null,2));
    process.stdout.write(`Built-in auth browser acceptance passed; screenshots: ${artifacts}\n`);
  } finally {
    if (browser) await browser.close();
    fs.writeFileSync(path.join(dir,'stop'),'stop');
    const code = await done;
    fs.rmSync(dir,{recursive:true,force:true});
    if (code !== 0) throw new Error(`HTTPS fixture failed: ${output}`);
  }
})().catch(error => { process.stderr.write(`${error.stack}\n`); process.exitCode = 1; });
