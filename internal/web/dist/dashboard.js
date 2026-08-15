const DashboardView = {
  timer: null,
  data: null,

  render(root) {
    root.innerHTML = this.html();
    this.bind();
    this.refresh();
    this.timer = setInterval(() => this.refresh(), 5000);
  },

  destroy() {
    if (this.timer) clearInterval(this.timer);
  },

  html() {
    return `
      <div class="min-h-screen flex flex-col">
        <header class="border-b-[3px] border-ink bg-yellow px-6 py-4 flex items-center justify-between gap-4">
          <div class="flex items-center gap-3">
            <div class="neo-card-flat bg-ink text-yellow font-black text-2xl w-12 h-12 flex items-center justify-center -rotate-3">⚡</div>
            <h1 class="text-2xl font-black tracking-tight border-[3px] border-ink bg-cream px-4 py-1 -rotate-1 neo-card-flat shadow-[4px_4px_0_0_#1B1B1B]">
              Neo Gateway
            </h1>
          </div>
          <div class="flex items-center gap-3">
            <span id="health-pill" class="badge bg-cream">loading...</span>
            <a href="#/chat" class="neo-btn px-5 py-2 text-sm flex items-center gap-2">
              💬 Chat
            </a>
          </div>
        </header>

        <main class="flex-1 p-6 max-w-7xl w-full mx-auto space-y-6">
          <div class="flex items-end justify-between gap-4 flex-wrap">
            <div>
              <h2 class="text-3xl font-black tracking-tight">Dashboard</h2>
              <p class="text-sm font-semibold opacity-70">Live gateway metrics, traffic & model usage</p>
            </div>
            <div class="flex items-center gap-2">
              <span id="last-updated" class="badge bg-cream">—</span>
              <button id="btn-refresh" class="neo-btn px-4 py-2 text-sm">↻ Refresh</button>
            </div>
          </div>

          <div id="stats-grid" class="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-5"></div>

          <div id="pool-row" class="flex flex-wrap gap-3"></div>

          <div class="grid grid-cols-1 xl:grid-cols-5 gap-6">
            <div class="xl:col-span-3 neo-card p-5">
              <div class="flex items-center justify-between mb-4">
                <h3 class="text-lg font-black">Traffic / minute</h3>
                <span id="window-label" class="badge bg-purple">last 30 min</span>
              </div>
              <div id="traffic-chart" class="w-full"></div>
            </div>

            <div class="xl:col-span-2 neo-card p-5 bg-pink">
              <h3 class="text-lg font-black mb-4">Model usage</h3>
              <div id="model-usage" class="space-y-3"></div>
            </div>
          </div>

          <div class="neo-card p-5 bg-blue">
            <div class="flex items-center justify-between mb-4">
              <h3 class="text-lg font-black text-white">Proxy pool</h3>
              <span id="pool-total" class="badge bg-white">0 proxies</span>
            </div>
            <div id="proxy-list" class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4"></div>
          </div>
        </main>
      </div>
    `;
  },

  bind() {
    $('#btn-refresh').addEventListener('click', () => this.refresh());
  },

  async refresh() {
    try {
      const data = await api('/api/metrics');
      this.data = data;
      this.renderStats(data);
      this.renderTraffic(data.metrics);
      this.renderModels(data.metrics);
      this.renderPool(data);
      const pill = $('#health-pill');
      pill.className = 'badge ' + (data.pool.total > 0 && (data.pool.idle > 0 || data.pool.active > 0) ? 'bg-green' : 'bg-red');
      pill.textContent = data.pool.total > 0 ? '● gateway healthy' : '○ gateway degraded';
      $('#last-updated').textContent = 'updated ' + new Date().toLocaleTimeString();
    } catch (err) {
      $('#last-updated').textContent = 'failed to load: ' + err.message;
    }
  },

  renderStats(data) {
    const s = (data.metrics && data.metrics.summary) || {};
    const p = data.pool || {};
    const cards = [
      { label: 'Total Requests', value: fmtNum(s.total_requests), color: 'bg-yellow', icon: '📈' },
      { label: 'Success Rate', value: s.success_rate != null ? s.success_rate.toFixed(1) + '%' : '—', color: 'bg-green', icon: '✅', extra: fmtNum(s.total_errors) + ' errors' },
      { label: 'Avg Latency', value: s.avg_latency_ms != null ? s.avg_latency_ms.toFixed(0) + 'ms' : '—', color: 'bg-blue', icon: '⏱️' },
      { label: 'Streaming Reqs', value: fmtNum(s.stream_requests), color: 'bg-purple', icon: '🌊' },
      { label: 'Errors', value: fmtNum(s.total_errors), color: 'bg-red', icon: '🚨' },
      { label: 'Uptime', value: fmtDuration(s.uptime_seconds), color: 'bg-orange', icon: '⏳' },
    ];

    const grid = $('#stats-grid');
    grid.innerHTML = cards.map((c) => `
      <div class="neo-card p-4 ${c.color} relative">
        <div class="absolute -top-2 -right-2 w-8 h-8 rounded-full bg-ink text-white flex items-center justify-center text-sm">${c.icon}</div>
        <div class="text-2xl font-black leading-tight break-words pr-2">${c.value}</div>
        <div class="text-xs font-bold opacity-70 mt-1">${c.label}${c.extra ? ' · ' + c.extra : ''}</div>
      </div>
    `).join('');
  },

  renderTraffic(metrics) {
    const el = $('#traffic-chart');
    if (!metrics || !metrics.traffic || !metrics.traffic.length) {
      el.innerHTML = '<p class="text-sm font-semibold opacity-60">no traffic recorded yet</p>';
      return;
    }
    const points = metrics.traffic;
    $('#window-label').textContent = `last ${metrics.window || 30} min`;

    const W = 760;
    const H = 220;
    const padL = 40;
    const padB = 26;
    const padT = 10;
    const chartW = W - padL - 10;
    const chartH = H - padT - padB;
    const max = Math.max(...points.map((p) => p.requests), 1);
    const n = points.length;
    const bw = Math.max(4, chartW / n - 3);
    const maxY = max > 5 ? Math.ceil(max / 5) * 5 : 5;

    const yTicks = [0, 0.25, 0.5, 0.75, 1].map((f) => Math.round(maxY * f));
    let bars = '';
    points.forEach((p, i) => {
      const x = padL + i * (chartW / n) + 1.5;
      const hTotal = Math.max(p.requests, 0) / maxY * chartH;
      const hErr = p.errors / maxY * chartH;
      const yTotal = padT + chartH - hTotal;
      bars += `
        <rect x="${x}" y="${yTotal}" width="${bw}" height="${hTotal}" fill="#1B1B1B" rx="2">
          <title>${fmtClock(p.timestamp)} — ${p.requests} req(s)${p.errors ? ', ' + p.errors + ' error(s)' : ''}</title>
        </rect>`;
      if (hErr > 0.5) {
        bars += `
          <rect x="${x}" y="${padT + chartH - hErr}" width="${bw}" height="${hErr}" fill="#FF6B6B" rx="2">
            <title>${fmtClock(p.timestamp)} — ${p.errors} error(s)</title>
          </rect>`;
      }
    });

    let grid = '';
    let labels = '';
    yTicks.forEach((v) => {
      const y = padT + chartH - (v / maxY) * chartH;
      grid += `<line x1="${padL}" y1="${y}" x2="${W - 10}" y2="${y}" stroke="#1B1B1B" stroke-width="1.5" stroke-dasharray="4 4" opacity="0.25"/>`;
      labels += `<text x="${padL - 8}" y="${y + 4}" text-anchor="end" font-size="10" font-weight="700" fill="#1B1B1B">${v}</text>`;
    });
    points.forEach((p, i) => {
      if (i % 5 === 0 || i === n - 1) {
        const x = padL + i * (chartW / n) + bw / 2;
        labels += `<text x="${x}" y="${H - 8}" text-anchor="middle" font-size="10" font-weight="700" fill="#1B1B1B">${fmtClock(p.timestamp)}</text>`;
      }
    });

    el.innerHTML = `
      <svg viewBox="0 0 ${W} ${H}" class="w-full h-auto" role="img" aria-label="Requests per minute">
        ${grid}
        ${labels}
        ${bars}
        <rect x="${padL}" y="${padT}" width="${chartW}" height="${chartH}" fill="none" stroke="#1B1B1B" stroke-width="3"/>
      </svg>
      <div class="flex items-center gap-4 mt-3 text-xs font-bold">
        <span class="flex items-center gap-1.5"><span class="w-3 h-3 bg-ink inline-block rounded-sm"></span> requests</span>
        <span class="flex items-center gap-1.5"><span class="w-3 h-3 bg-red inline-block rounded-sm"></span> errors</span>
      </div>`;
  },

  renderModels(metrics) {
    const el = $('#model-usage');
    const models = (metrics && metrics.models) || [];
    if (!models.length) {
      el.innerHTML = '<p class="text-sm font-bold opacity-70 bg-white border-[3px] border-ink rounded-lg p-3">no model usage yet — try the chat!</p>';
      return;
    }
    const max = models[0].requests || 1;
    el.innerHTML = models.map((m, i) => `
      <div class="bg-white border-[3px] border-ink rounded-xl p-3 shadow-[4px_4px_0_0_#1B1B1B]">
        <div class="flex items-center justify-between gap-2 mb-2">
          <span class="text-xs font-bold break-all" title="${esc(m.model)}">${esc(shortModel(m.model))}</span>
          <span class="badge" style="background:${NEO_COLORS[i % NEO_COLORS.length]}">${fmtNum(m.requests)}</span>
        </div>
        <div class="h-4 border-[2px] border-ink rounded-md bg-cream overflow-hidden">
          <div class="h-full" style="width:${Math.max(4, m.requests / max * 100)}%;background:${NEO_COLORS[i % NEO_COLORS.length]};border-right:2px solid #1B1B1B"></div>
        </div>
      </div>
    `).join('');
  },

  renderPool(data) {
    const pool = data.pool || {};
    if (!pool) return;
    const states = [
      { key: 'active', label: 'Active', color: 'bg-green' },
      { key: 'idle', label: 'Idle', color: 'bg-yellow' },
      { key: 'cooldown', label: 'Cooldown', color: 'bg-orange' },
      { key: 'unhealthy', label: 'Unhealthy', color: 'bg-red' },
    ];
    $('#pool-total').textContent = `${pool.total} proxy(ies)`;
    $('#pool-row').innerHTML = ['active', 'idle', 'cooldown', 'unhealthy'].map((k) => {
      const s = states.find((x) => x.key === k);
      return `<span class="badge ${s.color} text-xs">${s.label}: ${pool[k] || 0}</span>`;
    }).join('');

    if (!data.proxies || !data.proxies.length) {
      $('#proxy-list').innerHTML = '<p class="text-white text-sm font-bold opacity-90">no proxies in pool</p>';
      return;
    }
    const ipCounts = {};
    (data.proxies || []).forEach((p) => { if (p.egress_ip) ipCounts[p.egress_ip] = (ipCounts[p.egress_ip] || 0) + 1; });
    $('#proxy-list').innerHTML = data.proxies.map((p) => {
      const state = states.find((s) => s.key === p.state) || { label: p.state, color: 'bg-cream' };
      const dup = p.egress_ip && ipCounts[p.egress_ip] > 1;
      return `
        <div class="bg-white border-[3px] border-ink rounded-xl p-3 shadow-[4px_4px_0_0_#1B1B1B] fade-up">
          <div class="flex items-center justify-between gap-2 mb-1">
            <span class="text-xs font-black break-all">${esc(p.id)}</span>
            <span class="badge ${state.color}">${state.label}</span>
          </div>
          <div class="text-[11px] font-bold opacity-70 break-all">${esc(p.socks5_addr)}</div>
          <div class="mt-1 flex items-center justify-between gap-2">
            <span class="text-[11px] font-black break-all" title="egress ip">${p.egress_ip ? 'IP: ' + esc(p.egress_ip) : 'IP: —'}</span>
            ${dup ? '<span class="badge bg-red text-xs">duplicate ip</span>' : ''}
          </div>
          <div class="flex gap-3 mt-2 text-[11px] font-bold">
            <span>${fmtNum(p.requests_sent)} req</span>
            <span>${fmtNum(p.error_count)} err</span>
          </div>
        </div>`;
    }).join('');
  },
};