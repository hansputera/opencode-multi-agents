const ManageView = {
  tab: 'accounts',
  accounts: [],
  proxies: [],
  settings: {},
  pool: null,
  refreshTimer: null,

  render(root) {
    root.innerHTML = this.html();
    this.loadTab();
    this.startRefresh();
  },

  destroy() {
    if (this.refreshTimer) clearInterval(this.refreshTimer);
  },

  html() {
    return `
      <div class="min-h-screen flex flex-col">
        <header class="border-b-[3px] border-ink bg-ink px-6 py-4 flex items-center justify-between gap-4">
          <div class="flex items-center gap-3">
            <div class="neo-badge bg-yellow text-ink">SYS</div>
            <h1 class="text-xl font-black tracking-tight text-cream">POOL MANAGER</h1>
          </div>
          <div class="flex items-center gap-2">
            <a href="/dashboard" class="neo-btn-industrial muted sm">← DASHBOARD</a>
          </div>
        </header>

        <div class="p-6 max-w-6xl w-full mx-auto flex-1 space-y-0">
          <div class="neo-tab-bar">
            <button onclick="ManageView.switchTab('accounts')" id="tab-accounts" class="neo-tab-btn active">Accounts</button>
            <button onclick="ManageView.switchTab('proxies')" id="tab-proxies" class="neo-tab-btn">Proxies</button>
            <button onclick="ManageView.switchTab('settings')" id="tab-settings" class="neo-tab-btn">Settings</button>
            <button onclick="ManageView.switchTab('pool')" id="tab-pool" class="neo-tab-btn">Live Pool</button>
          </div>

          <div class="neo-panel">
            <div id="manage-content"></div>
          </div>
        </div>
      </div>
    `;
  },

  switchTab(tab) {
    this.tab = tab;
    document.querySelectorAll('.neo-tab-btn').forEach(b => b.classList.remove('active'));
    const btn = document.getElementById('tab-' + tab);
    if (btn) btn.classList.add('active');
    this.loadTab();
  },

  startRefresh() {
    this.switchTab(this.tab);
    this.refreshTimer = setInterval(() => {
      if (this.tab === 'pool') this.loadPool();
    }, 3000);
  },

  async loadTab() {
    document.querySelectorAll('.neo-tab-btn').forEach(b => b.classList.remove('active'));
    const btn = document.getElementById('tab-' + this.tab);
    if (btn) btn.classList.add('active');

    const el = document.getElementById('manage-content');
    switch (this.tab) {
      case 'accounts': await this.loadAccounts(); break;
      case 'proxies': await this.loadProxies(); break;
      case 'settings': await this.loadSettings(); break;
      case 'pool': await this.loadPool(); break;
    }
  },

  // --- Accounts ---
  async loadAccounts() {
    try {
      const data = await api('/api/manage/accounts');
      this.accounts = data.accounts || [];
    } catch (e) { this.accounts = []; }
    this.renderAccounts();
  },

  renderAccounts() {
    const el = document.getElementById('manage-content');
    if (this.accounts.length === 0) {
      el.innerHTML = `
        <div class="neo-panel-header">
          <span>ACCOUNTS // 0 ENTRIES</span>
          <button onclick="ManageView.showAddAccount()" class="neo-btn-industrial success sm">+ NEW</button>
        </div>
        <div class="neo-panel-body text-center" style="opacity:0.5">
          <p class="text-sm font-bold">No accounts configured. Add a ProtonVPN account to start.</p>
        </div>`;
      return;
    }

    let rows = '';
    for (const a of this.accounts) {
      const method = a.session_cookies ? 'COOKIES' : 'PASS';
      const status = a.enabled ? '<span class="neo-status-dot green"></span>' : '<span class="neo-status-dot red"></span>';
      rows += `
        <tr>
          <td class="font-mono font-bold" style="max-width:200px;overflow:hidden;text-overflow:ellipsis">${status} ${this.esc(a.username)}</td>
          <td><span class="neo-badge bg-cream">${method}</span></td>
          <td style="opacity:0.6;font-size:0.75rem">${a.session_cookies ? this.esc(a.session_cookies.substring(0, 40)) + '…' : '••••••••'}</td>
          <td class="text-right" style="white-space:nowrap">
            <button onclick="ManageView.editAccount(${a.id})" class="neo-btn-industrial sm muted">EDIT</button>
            <button onclick="ManageView.deleteAccount(${a.id})" class="neo-btn-industrial sm danger">DEL</button>
          </td>
        </tr>`;
    }

    el.innerHTML = `
      <div class="neo-panel-header">
        <span>ACCOUNTS // ${this.accounts.length} ENTRIES</span>
        <button onclick="ManageView.showAddAccount()" class="neo-btn-industrial success sm">+ NEW</button>
      </div>
      <div style="overflow-x:auto">
        <table class="neo-table">
          <thead>
            <tr>
              <th>Username</th>
              <th>Auth</th>
              <th>Credential</th>
              <th style="text-align:right">Actions</th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
  },

  showAddAccount() {
    const el = document.getElementById('manage-content');
    el.innerHTML = `
      <div class="neo-panel-header">
        <span>NEW ACCOUNT</span>
        <button onclick="ManageView.loadAccounts()" class="neo-btn-industrial muted sm">← BACK</button>
      </div>
      <div class="neo-panel-body">
        <div class="neo-grid-2">
          <div>
            <label class="neo-label">Username (email)</label>
            <input id="ma-user" class="neo-input-industrial" placeholder="user@proton.me">
          </div>
          <div>
            <label class="neo-label">Password</label>
            <input id="ma-pass" type="password" class="neo-input-industrial" placeholder="password">
          </div>
        </div>
        <div style="margin-top:14px">
          <label class="neo-label">Session Cookies (optional)</label>
          <textarea id="ma-cookies" class="neo-input-industrial" rows="3" placeholder="Paste browser cookies from account.protonvpn.com" style="resize:vertical"></textarea>
        </div>
        <div style="margin-top:18px;display:flex;gap:10px">
          <button onclick="ManageView.createAccount()" class="neo-btn-industrial success">CREATE ACCOUNT</button>
          <button onclick="ManageView.loadAccounts()" class="neo-btn-industrial muted">CANCEL</button>
        </div>
      </div>`;
  },

  async createAccount() {
    const username = document.getElementById('ma-user').value.trim();
    const password = document.getElementById('ma-pass').value;
    const session_cookies = document.getElementById('ma-cookies').value.trim();
    if (!username) return alert('Username is required');
    try {
      await api('/api/manage/accounts', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password, session_cookies, enabled: true })
      });
      this.loadAccounts();
    } catch (e) { alert('Failed: ' + e.message); }
  },

  editAccount(id) {
    const a = this.accounts.find(x => x.id === id);
    if (!a) return;
    const el = document.getElementById('manage-content');
    el.innerHTML = `
      <div class="neo-panel-header">
        <span>EDIT // ${this.esc(a.username)}</span>
        <button onclick="ManageView.loadAccounts()" class="neo-btn-industrial muted sm">← BACK</button>
      </div>
      <div class="neo-panel-body">
        <div class="neo-grid-2">
          <div>
            <label class="neo-label">Username</label>
            <input id="me-user" class="neo-input-industrial" value="${this.esc(a.username)}">
          </div>
          <div>
            <label class="neo-label">Password (blank = keep)</label>
            <input id="me-pass" type="password" class="neo-input-industrial" placeholder="Leave blank to keep">
          </div>
        </div>
        <div style="margin-top:14px">
          <label class="neo-label">Session Cookies</label>
          <textarea id="me-cookies" class="neo-input-industrial" rows="3" style="resize:vertical">${this.esc(a.session_cookies || '')}</textarea>
        </div>
        <div style="margin-top:14px;display:flex;align-items:center;gap:10px">
          <label class="neo-label" style="margin:0">Enabled</label>
          <input id="me-enabled" type="checkbox" ${a.enabled ? 'checked' : ''} style="width:20px;height:20px;border:3px solid var(--ink);accent-color:var(--ink)">
        </div>
        <div style="margin-top:18px;display:flex;gap:10px">
          <button onclick="ManageView.updateAccount(${a.id})" class="neo-btn-industrial success">SAVE</button>
          <button onclick="ManageView.loadAccounts()" class="neo-btn-industrial muted">CANCEL</button>
        </div>
      </div>`;
  },

  async updateAccount(id) {
    const username = document.getElementById('me-user').value.trim();
    const password = document.getElementById('me-pass').value;
    const session_cookies = document.getElementById('me-cookies').value.trim();
    const enabled = document.getElementById('me-enabled').checked;
    const body = { username, enabled };
    if (password) body.password = password;
    if (session_cookies) body.session_cookies = session_cookies;
    try {
      await api('/api/manage/accounts/' + id, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      this.loadAccounts();
    } catch (e) { alert('Failed: ' + e.message); }
  },

  async deleteAccount(id) {
    if (!confirm('Delete this account permanently?')) return;
    try {
      await api('/api/manage/accounts/' + id, { method: 'DELETE' });
      this.loadAccounts();
    } catch (e) { alert('Failed: ' + e.message); }
  },

  // --- Proxies ---
  async loadProxies() {
    try {
      const data = await api('/api/manage/proxies');
      this.proxies = data.proxies || [];
    } catch (e) { this.proxies = []; }
    this.renderProxies();
  },

  renderProxies() {
    const el = document.getElementById('manage-content');
    if (this.proxies.length === 0) {
      el.innerHTML = `
        <div class="neo-panel-header">
          <span>PROXIES // 0 ENTRIES</span>
          <button onclick="ManageView.showAddProxy()" class="neo-btn-industrial success sm">+ NEW</button>
        </div>
        <div class="neo-panel-body text-center" style="opacity:0.5">
          <p class="text-sm font-bold">No external proxies configured.</p>
        </div>`;
      return;
    }

    let rows = '';
    for (const p of this.proxies) {
      const status = p.enabled ? '<span class="neo-status-dot green"></span>' : '<span class="neo-status-dot red"></span>';
      rows += `
        <tr>
          <td class="font-mono font-bold text-sm" style="max-width:350px;overflow:hidden;text-overflow:ellipsis">${status} ${this.esc(p.address)}</td>
          <td class="text-right">
            <button onclick="ManageView.deleteProxy(${p.id})" class="neo-btn-industrial sm danger">DEL</button>
          </td>
        </tr>`;
    }

    el.innerHTML = `
      <div class="neo-panel-header">
        <span>PROXIES // ${this.proxies.length} ENTRIES</span>
        <button onclick="ManageView.showAddProxy()" class="neo-btn-industrial success sm">+ NEW</button>
      </div>
      <div style="overflow-x:auto">
        <table class="neo-table">
          <thead>
            <tr>
              <th>Address</th>
              <th style="text-align:right">Actions</th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
  },

  showAddProxy() {
    const el = document.getElementById('manage-content');
    el.innerHTML = `
      <div class="neo-panel-header">
        <span>NEW PROXY</span>
        <button onclick="ManageView.loadProxies()" class="neo-btn-industrial muted sm">← BACK</button>
      </div>
      <div class="neo-panel-body">
        <label class="neo-label">SOCKS5 Address</label>
        <input id="mp-addr" class="neo-input-industrial" placeholder="socks5://host:port or socks5://user:pass@host:port">
        <div style="margin-top:18px;display:flex;gap:10px">
          <button onclick="ManageView.createProxy()" class="neo-btn-industrial success">ADD PROXY</button>
          <button onclick="ManageView.loadProxies()" class="neo-btn-industrial muted">CANCEL</button>
        </div>
      </div>`;
  },

  async createProxy() {
    const address = document.getElementById('mp-addr').value.trim();
    if (!address) return alert('Address is required');
    try {
      await api('/api/manage/proxies', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ address })
      });
      this.loadProxies();
    } catch (e) { alert('Failed: ' + e.message); }
  },

  async deleteProxy(id) {
    if (!confirm('Delete this proxy?')) return;
    try {
      await api('/api/manage/proxies/' + id, { method: 'DELETE' });
      this.loadProxies();
    } catch (e) { alert('Failed: ' + e.message); }
  },

  // --- Settings ---
  async loadSettings() {
    try {
      const data = await api('/api/manage/settings');
      this.settings = data.settings || {};
    } catch (e) { this.settings = {}; }
    this.renderSettings();
  },

  renderSettings() {
    const el = document.getElementById('manage-content');
    const s = this.settings;
    const groups = [
      { title: 'UPSTREAM', fields: [
        { key: 'upstream_base_url', label: 'Base URL', type: 'text', ph: 'https://opencode.ai/zen/v1', hot: true },
        { key: 'upstream_provider', label: 'Provider', type: 'select', options: ['zen','opencode','opencode-cli'], hot: true },
        { key: 'model_filter', label: 'Model Filter', type: 'text', ph: '-free (empty = no filter)', hot: true },
      ]},
      { title: 'POOL', fields: [
        { key: 'pool_size', label: 'Pool Size', type: 'number', min: 1, max: 20, ph: '3', hot: true },
        { key: 'proxy_base_port', label: 'Base Port', type: 'number', min: 1024, max: 65535, ph: '10801' },
        { key: 'max_concurrent', label: 'Max Concurrent', type: 'number', min: 1, ph: '100', hot: true },
        { key: 'vpn_image', label: 'VPN Image', type: 'text', ph: 'ghcr.io/tprasadtp/protonwire:latest' },
      ]},
      { title: 'PROTONVPN', fields: [
        { key: 'regions', label: 'Regions', type: 'text', ph: 'NL,US,JP,DE', hot: true },
        { key: 'ip_check_url', label: 'IP Check URL', type: 'text', ph: 'https://icanhazip.com/', hot: true },
        { key: 'protonvpn_api_base', label: 'API Base', type: 'text', ph: 'https://account.protonvpn.com' },
      ]},
      { title: 'RATE LIMITS', fields: [
        { key: 'cooldown_duration', label: 'Cooldown', type: 'text', ph: '5m0s', hot: true },
        { key: 'ip_ban_duration', label: 'IP Ban Duration', type: 'text', ph: '10m0s', hot: true },
        { key: 'rate_limit_fresh_ip_wait', label: 'Fresh IP Wait', type: 'text', ph: '90s', hot: true },
        { key: 'rate_limit_retry_after', label: 'Retry-After (sec)', type: 'number', ph: '60', hot: true },
      ]},
      { title: 'RETRY', fields: [
        { key: 'max_retries', label: 'Max Retries', type: 'number', min: 0, max: 10, ph: '3', hot: true },
        { key: 'retry_base_delay', label: 'Base Delay', type: 'text', ph: '1s' },
        { key: 'retry_max_delay', label: 'Max Delay', type: 'text', ph: '30s' },
      ]},
      { title: 'HEALTH', fields: [
        { key: 'health_check_period', label: 'Check Period', type: 'text', ph: '30s', hot: true },
        { key: 'request_timeout', label: 'Request Timeout', type: 'text', ph: '60s', hot: true },
        { key: 'sticky_session_ttl', label: 'Sticky Session TTL', type: 'text', ph: '10m', hot: true },
      ]},
      { title: 'RESOURCES', fields: [
        { key: 'resource_cpu_limit', label: 'CPU Limit', type: 'text', ph: '0.25' },
        { key: 'resource_memory_limit', label: 'Memory Limit', type: 'text', ph: '512M' },
      ]},
      { title: 'LOGGING', fields: [
        { key: 'log_level', label: 'Log Level', type: 'select', options: ['debug','info','warn','error'], hot: true },
        { key: 'log_format', label: 'Log Format', type: 'select', options: ['json','console'], hot: true },
        { key: 'cors_origin', label: 'CORS Origin', type: 'text', ph: '*' },
      ]},
      { title: 'POW (PoW-Gated Keys)', fields: [
        { key: 'pow_enabled', label: 'Enabled', type: 'select', options: ['true','false'], hot: true },
        { key: 'pow_base_difficulty', label: 'Base Difficulty', type: 'number', min: 1, ph: '20' },
        { key: 'pow_min_difficulty', label: 'Min Difficulty', type: 'number', min: 1, ph: '16' },
        { key: 'pow_max_difficulty', label: 'Max Difficulty', type: 'number', min: 1, ph: '28' },
        { key: 'pow_plan1_difficulty', label: 'Basic Difficulty', type: 'number', min: 1, ph: '20' },
        { key: 'pow_plan2_difficulty', label: 'Plus Difficulty', type: 'number', min: 1, ph: '24' },
        { key: 'pow_plan3_difficulty', label: 'Pro Difficulty', type: 'number', min: 1, ph: '28' },
        { key: 'pow_plan1_rpm', label: 'Basic RPM', type: 'number', min: 1, ph: '100' },
        { key: 'pow_plan2_rpm', label: 'Plus RPM', type: 'number', min: 1, ph: '250' },
        { key: 'pow_plan3_rpm', label: 'Pro RPM', type: 'number', min: 1, ph: '500' },
        { key: 'pow_burst_rps', label: 'Burst RPS', type: 'number', min: 1, ph: '5' },
        { key: 'pow_burst_cooldown', label: 'Burst Cooldown', type: 'text', ph: '5m' },
        { key: 'pow_challenge_per_min', label: 'Challenges/min', type: 'number', min: 0, ph: '5' },
        { key: 'pow_challenge_per_day', label: 'Challenges/day', type: 'number', min: 0, ph: '20' },
        { key: 'pow_key_ttl', label: 'Key TTL', type: 'text', ph: '168h (7 days)' },
      ]},
      { title: 'WEB SEARCH', fields: [
        { key: 'web_search_enabled', label: 'Enabled', type: 'select', options: ['true','false'], hot: true },
        { key: 'web_search_max_results', label: 'Max Results', type: 'number', min: 1, ph: '5', hot: true },
        { key: 'web_search_max_pages', label: 'Max Pages', type: 'number', min: 0, ph: '3' },
        { key: 'web_search_max_page_chars', label: 'Max Page Chars', type: 'number', min: 0, ph: '2000' },
        { key: 'web_search_max_rounds', label: 'Max Rounds', type: 'number', min: 1, ph: '3', hot: true },
        { key: 'searxng_url', label: 'SearXNG URL', type: 'text', ph: 'http://localhost:8888/search' },
      ]},
    ];

    let html = '';
    for (const g of groups) {
      html += `
        <div style="margin-bottom:18px">
          <div class="neo-panel-header" style="padding:6px 14px">
            <span>${g.title}</span>
            <span class="text-xs" style="opacity:0.5">${g.fields.filter(f=>f.hot).length} hot-reload</span>
          </div>
          <div class="neo-panel-body">
            <div class="neo-grid-2">
              ${g.fields.map(f => {
                const val = s[f.key] || '';
                if (f.type === 'select') {
                  return `
                    <div>
                      <label class="neo-label">${f.label}${f.hot ? ' ⚡' : ''}</label>
                      <select id="ms-${f.key}" class="neo-input-industrial">
                        ${f.options.map(o => `<option value="${o}" ${val===o?'selected':''}>${o}</option>`).join('')}
                      </select>
                    </div>`;
                }
                return `
                  <div>
                    <label class="neo-label">${f.label}${f.hot ? ' ⚡' : ''}</label>
                    <input id="ms-${f.key}" class="neo-input-industrial" type="${f.type}" value="${this.esc(val)}" ${f.min!=null?'min="'+f.min+'"':''} ${f.max!=null?'max="'+f.max+'"':''} placeholder="${f.ph||''}">
                  </div>`;
              }).join('')}
            </div>
          </div>
        </div>`;
    }

    html += `
      <div style="padding:0 14px 14px;display:flex;gap:10px;align-items:center">
        <button onclick="ManageView.saveSettings()" class="neo-btn-industrial success">SAVE ALL SETTINGS</button>
        <span class="text-xs font-bold" style="opacity:0.4">⚡ = hot-reloadable · saves to SQLite (survives restart)</span>
      </div>`;

    el.innerHTML = html;
  },

  async saveSettings() {
    const fields = [
      'upstream_base_url','upstream_provider','model_filter',
      'pool_size','proxy_base_port','max_concurrent','vpn_image',
      'regions','ip_check_url','protonvpn_api_base',
      'cooldown_duration','ip_ban_duration','rate_limit_fresh_ip_wait','rate_limit_retry_after',
      'max_retries','retry_base_delay','retry_max_delay',
      'health_check_period','request_timeout','sticky_session_ttl',
      'resource_cpu_limit','resource_memory_limit',
      'log_level','log_format','cors_origin',
      'pow_enabled','pow_base_difficulty','pow_min_difficulty','pow_max_difficulty',
      'pow_plan1_difficulty','pow_plan2_difficulty','pow_plan3_difficulty',
      'pow_plan1_rpm','pow_plan2_rpm','pow_plan3_rpm',
      'pow_burst_rps','pow_burst_cooldown','pow_challenge_per_min','pow_challenge_per_day','pow_key_ttl',
      'web_search_enabled','web_search_max_results','web_search_max_pages','web_search_max_page_chars','web_search_max_rounds','searxng_url',
    ];
    const body = {};
    for (const k of fields) {
      const el = document.getElementById('ms-' + k);
      if (el && el.value.trim()) body[k] = el.value.trim();
    }
    try {
      await api('/api/manage/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      this.loadSettings();
    } catch (e) { alert('Failed: ' + e.message); }
  },

  // --- Live Pool ---
  async loadPool() {
    try {
      this.pool = await api('/api/manage/pool');
    } catch (e) { this.pool = null; }
    this.renderPool();
  },

  async refreshPool() {
    const btn = document.getElementById('btn-refresh-pool');
    if (btn) { btn.textContent = '↻ REFRESHING…'; btn.disabled = true; }
    try {
      this.pool = await api('/api/manage/pool/refresh', { method: 'POST' });
    } catch (e) { /* keep existing pool data */ }
    this.renderPool();
  },

  renderPool() {
    const el = document.getElementById('manage-content');
    if (!this.pool) {
      el.innerHTML = `
        <div class="neo-panel-header"><span>LIVE POOL // LOADING</span></div>
        <div class="neo-panel-body text-center" style="opacity:0.5">
          <p class="text-sm font-bold">Connecting to pool…</p>
        </div>`;
      return;
    }
    const stats = this.pool.stats || {};
    const proxies = this.pool.proxies || [];

    const stateColors = {
      idle: 'green', active: 'blue', cooldown: 'orange', unhealthy: 'red'
    };

    let poolRows = '';
    if (proxies.length === 0) {
      poolRows = `<tr><td colspan="5" style="text-align:center;opacity:0.5">No proxies in pool</td></tr>`;
    } else {
      for (const p of proxies) {
        const dot = `<span class="neo-status-dot ${stateColors[p.state] || ''}"></span>`;
        poolRows += `
          <tr>
            <td class="font-mono font-bold text-sm">${dot} ${this.esc(p.id)}</td>
            <td class="font-mono text-sm" style="max-width:200px;overflow:hidden;text-overflow:ellipsis">${this.esc(p.socks5_addr || '')}</td>
            <td><span class="neo-badge bg-cream">${p.state.toUpperCase()}</span></td>
            <td class="font-mono text-sm">${p.egress_ip ? this.esc(p.egress_ip) : '—'}</td>
            <td class="text-right font-mono text-sm">${p.requests_sent} / ${p.error_count}</td>
          </tr>`;
      }
    }

    el.innerHTML = `
      <div class="neo-panel-header">
        <span>LIVE POOL // ${stats.total || 0} PROXIES</span>
        <div style="display:flex;gap:8px;align-items:center">
          <span class="neo-badge bg-green">Active: ${stats.active || 0}</span>
          <span class="neo-badge bg-yellow text-ink">Idle: ${stats.idle || 0}</span>
          <span class="neo-badge bg-orange">Cooldown: ${stats.cooldown || 0}</span>
          <span class="neo-badge bg-red text-white">Unhealthy: ${stats.unhealthy || 0}</span>
          <button onclick="ManageView.refreshPool()" class="neo-btn-industrial sm muted" id="btn-refresh-pool">↻ REFRESH</button>
        </div>
      </div>
      <div style="overflow-x:auto">
        <table class="neo-table">
          <thead>
            <tr>
              <th>ID</th>
              <th>SOCKS5</th>
              <th>State</th>
              <th>Egress IP</th>
              <th style="text-align:right">Reqs / Err</th>
            </tr>
          </thead>
          <tbody>${poolRows}</tbody>
        </table>
      </div>`;
  },

  esc(s) {
    if (!s) return '';
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
  }
};

// Register route
if (typeof App !== 'undefined') {
  App.views = App.views || {};
  App.views.manage = ManageView;
}
