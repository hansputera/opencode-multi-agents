const App = {
  timer: null,
  current: null,

  init() {
    window.addEventListener('hashchange', () => this.route());
    this.route();
  },

  route() {
    let hash = window.location.hash;
    if (!hash) {
      hash = '#/dashboard';
      history.replaceState(null, '', hash);
    }
    const name = hash.replace(/^#\/?/, '').split('?')[0] || 'dashboard';
    const view = name === 'chat' ? ChatView : DashboardView;

    if (this.current && this.current.destroy) {
      this.current.destroy();
    }
    const el = document.getElementById('view');
    el.innerHTML = '';
    view.render(el);
    this.current = view;
  },
};

function $(sel, root = document) {
  return root.querySelector(sel);
}

function $$(sel, root = document) {
  return Array.from(root.querySelectorAll(sel));
}

async function api(path, options = {}) {
  try {
    const res = await fetch(path, options);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      const msg = data.error && data.error.message ? data.error.message : `HTTP ${res.status}`;
      throw new Error(msg);
    }
    return data;
  } catch (err) {
    console.error(`API error ${path}:`, err);
    throw err;
  }
}

function esc(str) {
  return String(str).replace(/[&<>"']/g, (ch) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  })[ch]);
}

function shortModel(model) {
  let m = String(model || 'unknown');
  const parts = m.split('/');
  m = parts[parts.length - 1];
  return m.replace(/:free$/, '');
}

function fmtNum(n) {
  return new Intl.NumberFormat('en-US').format(n || 0);
}

function fmtDuration(seconds) {
  const s = Math.floor(seconds || 0);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h ${m}m`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${s % 60}s`;
}

function fmtClock(ts) {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function fmtTime(ts) {
  const d = new Date(ts);
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function renderMarkdown(text) {
  let t = esc(text);
  const lines = t.split('\n');
  const out = [];
  let inCode = false;
  let codeBuf = [];
  let inList = false;

  for (const line of lines) {
    const codeMatch = line.match(/^```(\w*)/);
    if (codeMatch) {
      if (!inCode) {
        out.push('<pre><code>');
        inCode = true;
        codeBuf = [];
      } else {
        codeBuf.push('</code></pre>');
        out.push(codeBuf.join('\n'));
        inCode = false;
      }
      continue;
    }
    if (inCode) {
      codeBuf.push(line);
      continue;
    }
    if (/^\s*$/.test(line)) {
      if (inList) {
        out.push('</ul>');
        inList = false;
      }
      continue;
    }
    const h3 = line.match(/^###?\s+(.*)/);
    if (h3) {
      if (inList) {
        out.push('</ul>');
        inList = false;
      }
      out.push(`<h3>${inline(h3[1])}</h3>`);
      continue;
    }
    const li = line.match(/^\s*[-*]\s+(.*)/);
    if (li) {
      if (!inList) {
        out.push('<ul>');
        inList = true;
      }
      out.push(`<li>${inline(li[1])}</li>`);
      continue;
    }
    const num = line.match(/^\s*\d+\.\s+(.*)/);
    if (num) {
      if (!inList) {
        out.push('<ol>');
        inList = true;
      }
      out.push(`<li>${inline(num[1])}</li>`);
      continue;
    }
    if (inList) {
      out.push('</ul>');
      inList = false;
    }
    if (line.startsWith('&gt;')) {
      out.push(`<blockquote>${inline(line.slice(4))}</blockquote>`);
      continue;
    }
    out.push(`<p>${inline(line)}</p>`);
  }
  if (inCode) {
    out.push(codeBuf.join('\n'));
    out.push('</code></pre>');
  }
  if (inList) {
    out.push('</ul>');
  }
  return out.join('\n');
}

function inline(t) {
  let s = t
    .replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>')
    .replace(/\*(.+?)\*/g, '<em>$1</em>')
    .replace(/`(.+?)`/g, '<code>$1</code>');
  s = s.replace(/\[(.+?)\]\((https?:\/\/[^\s)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  return s;
}

const NEO_COLORS = ['#FFD43B', '#FF90E8', '#4D96FF', '#C3A6FF', '#FFA24C', '#B1F1CB', '#FF6B6B'];