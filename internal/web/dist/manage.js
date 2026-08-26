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
      <div class="max-w-5xl mx-auto p-6">
        <div class="flex items-center justify-between mb-6">
          <h1 class="text-2xl font-bold">Pool Manager</h1>
          <a href="#/dashboard" class="neo-btn text-sm">Back to Dashboard</a>
        </div>

        <div class="flex gap-2 mb-6">
          <button onclick="ManageView.switchTab('accounts')" id="tab-accounts" class="neo-btn text-sm tab-btn">Accounts</button>
          <button onclick="ManageView.switchTab('proxies')" id="tab-proxies" class="neo-btn text-sm tab-btn">Proxies</button>
          <button onclick="ManageView.switchTab('settings')" id="tab-settings" class="neo-btn text-sm tab-btn">Settings</button>
          <button onclick="ManageView.switchTab('pool')" id="tab-pool" class="neo-btn text-sm tab-btn">Live Pool</button>
        </div>

        <div id="manage-content"></div>
      </div>
    `;
  },

  switchTab(tab) {
    this.tab = tab;
    document.querySelectorAll('.tab-btn').forEach(b => b.style.background = '');
    const btn = document.getElementById('tab-' + tab);
    if (btn) btn.style.background = 'var(--yellow)';
    this.loadTab();
  },

  startRefresh() {
    this.switchTab(this.tab);
    this.refreshTimer = setInterval(() => {
      if (this.tab === 'pool') this.loadPool();
    }, 3000);
  },

  async loadTab() {
    document.querySelectorAll('.tab-btn').forEach(b => b.style.background = '');
    const btn = document.getElementById('tab-' + this.tab);
    if (btn) btn.style.background = 'var(--yellow)';

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
    let html = `<div class="space-y-3">`;
    if (this.accounts.length === 0) {
      html += `<div class="neo-card-flat p-4 text-center" style="opacity:0.6">No accounts configured</div>`;
    }
    for (const a of this.accounts) {
      const authMethod = a.session_cookies ? 'Cookies' : 'Password';
      const authDetail = a.session_cookies
        ? `Cookies: ${a.session_cookies.substring(0, 30)}...`
        : `Password: ${a.password ? '••••••••' : '(empty)'}`;
      html += `
        <div class="neo-card p-4" style="background:var(--cream)">
          <div class="flex items-center justify-between">
            <div>
              <div class="font-bold">${this.esc(a.username)}</div>
              <div class="text-sm" style="opacity:0.7">${authMethod} — ${this.esc(authDetail)}</div>
              <div class="text-xs mt-1">${a.enabled ? '<span style="color:var(--green)">✓ Active</span>' : '<span style="color:var(--red)">✗ Disabled</span>'}</div>
            </div>
            <div class="flex gap-2">
              <button onclick="ManageView.editAccount(${a.id})" class="neo-btn text-xs">Edit</button>
              <button onclick="ManageView.deleteAccount(${a.id})" class="neo-btn text-xs" style="background:var(--red);color:white">Delete</button>
            </div>
          </div>
        </div>`;
    }
    html += `</div>
      <button onclick="ManageView.showAddAccount()" class="neo-btn mt-4" style="background:var(--green);color:white">+ Add Account</button>`;
    el.innerHTML = html;
  },

  showAddAccount() {
    const el = document.getElementById('manage-content');
    el.innerHTML = `
      <div class="neo-card p-6" style="background:var(--cream)">
        <h3 class="font-bold mb-4">Add ProtonVPN Account</h3>
        <div class="space-y-3">
          <div><label class="text-sm font-bold">Username (email)</label><input id="ma-user" class="neo-input w-full" placeholder="user@proton.me"></div>
          <div><label class="text-sm font-bold">Password</label><input id="ma-pass" type="password" class="neo-input w-full" placeholder="password"></div>
          <div><label class="text-sm font-bold">Session Cookies (optional)</label><textarea id="ma-cookies" class="neo-input w-full" rows="3" placeholder="Paste browser cookies from account.protonvpn.com"></textarea></div>
        </div>
        <div class="flex gap-2 mt-4">
          <button onclick="ManageView.createAccount()" class="neo-btn" style="background:var(--green);color:white">Create</button>
          <button onclick="ManageView.loadAccounts()" class="neo-btn">Cancel</button>
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
      <div class="neo-card p-6" style="background:var(--cream)">
        <h3 class="font-bold mb-4">Edit Account: ${this.esc(a.username)}</h3>
        <div class="space-y-3">
          <div><label class="text-sm font-bold">Username</label><input id="me-user" class="neo-input w-full" value="${this.esc(a.username)}"></div>
          <div><label class="text-sm font-bold">Password</label><input id="me-pass" type="password" class="neo-input w-full" placeholder="Leave blank to keep"></div>
          <div><label class="text-sm font-bold">Session Cookies</label><textarea id="me-cookies" class="neo-input w-full" rows="3">${this.esc(a.session_cookies || '')}</textarea></div>
          <div><label class="text-sm font-bold">Enabled</label><input id="me-enabled" type="checkbox" ${a.enabled ? 'checked' : ''}></div>
        </div>
        <div class="flex gap-2 mt-4">
          <button onclick="ManageView.updateAccount(${a.id})" class="neo-btn" style="background:var(--green);color:white">Save</button>
          <button onclick="ManageView.loadAccounts()" class="neo-btn">Cancel</button>
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
    if (!confirm('Delete this account?')) return;
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
    let html = `<div class="space-y-3">`;
    if (this.proxies.length === 0) {
      html += `<div class="neo-card-flat p-4 text-center" style="opacity:0.6">No external proxies configured</div>`;
    }
    for (const p of this.proxies) {
      html += `
        <div class="neo-card p-4" style="background:var(--cream)">
          <div class="flex items-center justify-between">
            <div>
              <div class="font-bold font-mono text-sm">${this.esc(p.address)}</div>
              <div class="text-xs mt-1">${p.enabled ? '<span style="color:var(--green)">✓ Active</span>' : '<span style="color:var(--red)">✗ Disabled</span>'}</div>
            </div>
            <button onclick="ManageView.deleteProxy(${p.id})" class="neo-btn text-xs" style="background:var(--red);color:white">Delete</button>
          </div>
        </div>`;
    }
    html += `</div>
      <button onclick="ManageView.showAddProxy()" class="neo-btn mt-4" style="background:var(--green);color:white">+ Add Proxy</button>`;
    el.innerHTML = html;
  },

  showAddProxy() {
    const el = document.getElementById('manage-content');
    el.innerHTML = `
      <div class="neo-card p-6" style="background:var(--cream)">
        <h3 class="font-bold mb-4">Add External SOCKS5 Proxy</h3>
        <div class="space-y-3">
          <div><label class="text-sm font-bold">Address</label><input id="mp-addr" class="neo-input w-full" placeholder="socks5://host:port or socks5://user:pass@host:port"></div>
        </div>
        <div class="flex gap-2 mt-4">
          <button onclick="ManageView.createProxy()" class="neo-btn" style="background:var(--green);color:white">Create</button>
          <button onclick="ManageView.loadProxies()" class="neo-btn">Cancel</button>
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
    const fields = [
      { key: 'pool_size', label: 'Pool Size', type: 'number', min: 1, max: 20 },
      { key: 'regions', label: 'VPN Regions', type: 'text', placeholder: 'NL,US,JP,DE' },
      { key: 'ip_check_url', label: 'IP Check URL', type: 'text' },
      { key: 'cooldown_duration', label: 'Cooldown Duration', type: 'text', placeholder: '5m0s' },
      { key: 'ip_ban_duration', label: 'IP Ban Duration', type: 'text', placeholder: '10m0s' },
      { key: 'health_check_period', label: 'Health Check Period', type: 'text', placeholder: '30s' },
      { key: 'resource_cpu_limit', label: 'CPU Limit', type: 'text', placeholder: '0.25' },
      { key: 'resource_memory_limit', label: 'Memory Limit', type: 'text', placeholder: '512M' },
      { key: 'max_retries', label: 'Max Retries', type: 'number', min: 0, max: 10 },
      { key: 'request_timeout', label: 'Request Timeout', type: 'text', placeholder: '60s' },
    ];
    let html = `<div class="neo-card p-6" style="background:var(--cream)"><div class="space-y-3">`;
    for (const f of fields) {
      const val = s[f.key] || '';
      html += `<div>
        <label class="text-sm font-bold">${f.label}</label>
        <input id="ms-${f.key}" class="neo-input w-full" type="${f.type}" value="${this.esc(val)}" ${f.min != null ? 'min="'+f.min+'"' : ''} ${f.max != null ? 'max="'+f.max+'"' : ''} placeholder="${f.placeholder || ''}">
      </div>`;
    }
    html += `</div>
      <button onclick="ManageView.saveSettings()" class="neo-btn mt-4" style="background:var(--green);color:white">Save Settings</button>
      <div class="text-xs mt-2" style="opacity:0.5">Pool size changes take effect immediately. Other settings are saved for future restarts.</div>
    </div>`;
    el.innerHTML = html;
  },

  async saveSettings() {
    const fields = ['pool_size', 'regions', 'ip_check_url', 'cooldown_duration', 'ip_ban_duration', 'health_check_period', 'resource_cpu_limit', 'resource_memory_limit', 'max_retries', 'request_timeout'];
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

  renderPool() {
    const el = document.getElementById('manage-content');
    if (!this.pool) {
      el.innerHTML = `<div class="neo-card-flat p-4 text-center" style="opacity:0.6">Loading pool status...</div>`;
      return;
    }
    const stats = this.pool.stats || {};
    const proxies = this.pool.proxies || [];
    let html = `
      <div class="flex gap-3 mb-4">
        <div class="badge" style="background:var(--green);color:white">Active: ${stats.active || 0}</div>
        <div class="badge" style="background:var(--blue);color:white">Idle: ${stats.idle || 0}</div>
        <div class="badge" style="background:var(--orange);color:white">Cooldown: ${stats.cooldown || 0}</div>
        <div class="badge" style="background:var(--red);color:white">Unhealthy: ${stats.unhealthy || 0}</div>
        <div class="badge" style="background:var(--purple);color:white">Total: ${stats.total || 0}</div>
      </div>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-3">`;
    for (const p of proxies) {
      const stateColor = { idle: 'var(--green)', active: 'var(--blue)', cooldown: 'var(--orange)', unhealthy: 'var(--red)' }[p.state] || 'var(--ink)';
      html += `
        <div class="neo-card p-3" style="background:var(--cream)">
          <div class="flex items-center justify-between">
            <div class="font-mono text-xs">${this.esc(p.id)}</div>
            <div class="badge text-xs" style="background:${stateColor};color:white">${p.state}</div>
          </div>
          <div class="text-xs mt-1 font-mono" style="opacity:0.7">${this.esc(p.socks5_addr || '')}</div>
          ${p.egress_ip ? `<div class="text-xs mt-1">IP: <span class="font-mono">${this.esc(p.egress_ip)}</span></div>` : ''}
          <div class="text-xs mt-1">Requests: ${p.requests_sent} | Errors: ${p.error_count}</div>
        </div>`;
    }
    html += `</div>`;
    el.innerHTML = html;
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
