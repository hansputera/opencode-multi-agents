// GetKeyView: PoW-gated free API key issuance.
// Flow: pick plan -> acknowledge warning -> solve challenge (CPU workers +
// optional WebGPU kernel running simultaneously on disjoint counter lanes)
// -> redeem -> key stored in localStorage (neo.apiKey) and shown.
//
// Protocol: preimage = "pow-v1|<id>|<resource>|<algo>|<difficulty>|<salt>|<bind>|<counter>"
// where <counter> is fixed-width 10-digit decimal. The browser solves
// SHA-256 challenges (the gateway defaults browser clients to algo=sha256);
// native clients may request blake3.
const GetKeyView = {
  timer: null,
  visHandler: null,
  run: null,

  render(root) {
    root.innerHTML = `
      <div class="min-h-screen flex flex-col">
        <header class="border-b-[3px] border-ink bg-yellow px-6 py-4 flex items-center justify-between gap-4">
          <div class="flex items-center gap-3">
            <a href="#/dashboard" class="flex items-center gap-2">
              <div class="neo-card-flat bg-ink text-yellow font-black text-2xl w-12 h-12 flex items-center justify-center -rotate-3">⚡</div>
            </a>
            <h1 class="text-2xl font-black tracking-tight border-[3px] border-ink bg-cream px-4 py-1 -rotate-1 neo-card-flat shadow-[4px_4px_0_0_#1B1B1B]">Free API Key</h1>
          </div>
          <div class="flex items-center gap-2">
            <a href="#/chat" class="neo-btn px-5 py-2 text-sm">💬 Chat</a>
            <a href="#/dashboard" class="neo-btn px-5 py-2 text-sm">📊 Dashboard</a>
          </div>
        </header>
        <main class="flex-1 p-6 max-w-4xl w-full mx-auto space-y-6">
          <div id="getkey-stage"></div>
        </main>
      </div>`;
    this.showPlans();
  },

  destroy() {
    this.cancel(true);
  },

  // ---------- Stage 1: plan picker ----------
  showPlans() {
    const stage = $('#getkey-stage');
    const plans = [
      { id: 'basic', name: 'Basic', rpm: '~100 req/min', effort: '≈ 30–90 s', color: 'bg-green',
        desc: 'One easy hash puzzle. Perfect for trying the gateway.' },
      { id: 'plus', name: 'Plus', rpm: '~250 req/min', effort: '≈ 8–25 min · much faster with GPU', color: 'bg-purple',
        desc: '16× harder puzzle for busier workloads.' },
      { id: 'pro', name: 'Pro', rpm: '~500 req/min', effort: 'Heavy — GPU strongly recommended', color: 'bg-pink',
        desc: '256× harder puzzle for serious traffic.' },
    ];
    stage.innerHTML = `
      <h2 class="text-xl font-black mb-2">Pick your plan</h2>
      <p class="text-sm font-semibold opacity-70 mb-4">Harder puzzles unlock higher rate limits. Keys expire after 7 days. Bursting above 5 req/s cools the key down for 5 minutes.</p>
      <div class="grid md:grid-cols-3 gap-4">
        ${plans.map((p) => `
          <button data-plan="${p.id}" class="plan-pick neo-card p-5 ${p.color} text-left hover:-translate-y-1 transition-transform">
            <div class="text-lg font-black">${p.name}</div>
            <div class="badge bg-white mt-2">${p.rpm}</div>
            <div class="text-xs font-bold mt-2 opacity-80">${p.effort}</div>
            <p class="text-xs font-semibold mt-2 opacity-70">${p.desc}</p>
          </button>`).join('')}
      </div>`;
    $$('.plan-pick').forEach((btn) =>
      btn.addEventListener('click', () => this.showWarning(btn.dataset.plan)));
  },

  // ---------- Stage 2: warning ----------
  showWarning(plan) {
    const stage = $('#getkey-stage');
    stage.innerHTML = `
      <div class="neo-card p-6 bg-orange space-y-3">
        <h2 class="text-xl font-black">⚠️ Before we start</h2>
        <ul class="text-sm font-bold space-y-1 list-disc pl-5">
          <li>This will push your <b>CPU${plan !== 'basic' ? ' and GPU' : ''} hard</b> for the duration of the puzzle.</li>
          <li>Your device <b>will get hot</b>, fans will spin up, and laptop batteries drain fast.</li>
          <li>Solving <b>pauses automatically</b> when you switch away from this tab.</li>
          <li>You can cancel any time — you only lose progress.</li>
        </ul>
        <div class="flex gap-3 pt-2">
          <button id="btn-start-pow" class="neo-btn px-6 py-3 bg-ink text-white font-black">I understand — start solving (${esc(plan)})</button>
          <button id="btn-back-plans" class="neo-btn px-4 py-3 bg-white">← Back</button>
        </div>
      </div>`;
    $('#btn-back-plans').addEventListener('click', () => this.showPlans());
    $('#btn-start-pow').addEventListener('click', () => this.start(plan));
  },

  // ---------- Stage 3: solving ----------
  async start(plan) {
    const stage = $('#getkey-stage');
    stage.innerHTML = `<div class="neo-card p-6 bg-cream"><p class="font-black">Fetching challenge…</p></div>`;
    let ch;
    try {
      ch = (await api(`/api/pow/challenge?plan=${encodeURIComponent(plan)}`)).challenge;
    } catch (err) {
      stage.innerHTML = this.errCard('Could not get challenge: ' + esc(err.message));
      return;
    }

    const run = {
      ch,
      cancelled: false,
      paused: document.hidden,
      found: null,
      workers: [],
      gpu: null,
      gpuActive: false,
      hpsCpu: 0,
      hpsGpu: 0,
      startedAt: Date.now(),
    };
    this.run = run;

    stage.innerHTML = `
      <div class="neo-card p-6 bg-white space-y-4">
        <div class="flex items-center justify-between flex-wrap gap-2">
          <h2 class="text-lg font-black">Solving ${esc(ch.plan)} <span class="badge bg-purple">${ch.difficulty} bits</span></h2>
          <button id="btn-cancel-pow" class="neo-btn px-4 py-2 bg-red text-white">⏹ Cancel</button>
        </div>
        <div class="h-8 border-[3px] border-ink rounded-lg bg-cream overflow-hidden">
          <div id="pow-progress" class="h-full bg-green transition-all" style="width:0%"></div>
        </div>
        <div class="grid grid-cols-3 gap-3 text-center">
          <div class="neo-card-flat p-3 bg-cream"><div id="pow-hps" class="text-xl font-black">0</div><div class="text-[11px] font-bold opacity-60">hashes/sec</div></div>
          <div class="neo-card-flat p-3 bg-cream"><div id="pow-eta" class="text-xl font-black">—</div><div class="text-[11px] font-bold opacity-60">ETA (est.)</div></div>
          <div class="neo-card-flat p-3 bg-cream"><div id="pow-engines" class="text-xl font-black">…</div><div class="text-[11px] font-bold opacity-60">engines</div></div>
        </div>
        <p id="pow-note" class="text-[11px] font-bold opacity-50">Statistical average work: ${fmtNum(Math.pow(2, ch.difficulty))} hashes. Solving pauses while this tab is hidden.</p>
      </div>`;
    $('#btn-cancel-pow').addEventListener('click', () => this.cancel());

    this.visHandler = () => { if (this.run) this.run.paused = document.hidden; };
    document.addEventListener('visibilitychange', this.visHandler);

    await this.launchEngines(run);
    this.updateEngineLabel(run);
    this.timer = setInterval(() => this.tick(run), 500);
  },

  async launchEngines(run) {
    const hwCores = Math.max(2, Math.min(16, navigator.hardwareConcurrency || 4));
    let gpu = null;
    try {
      if (navigator.gpu) {
        const adapter = await navigator.gpu.requestAdapter();
        if (adapter) {
          const device = await adapter.requestDevice();
          const candidate = new GPUSolver(device, run.ch);
          if (await candidate.selfTest()) {
            gpu = candidate;
          } else {
            candidate.destroy();
            const note = $('#pow-note');
            if (note) note.textContent += ' (GPU self-test failed — using CPU only)';
          }
        }
      }
    } catch (e) {
      const note = $('#pow-note');
      if (note) note.textContent += ' (WebGPU unavailable — using CPU only)';
    }

    const nCpu = hwCores;
    const totalLanes = nCpu + (gpu ? 1 : 0);

    for (let i = 0; i < nCpu; i++) {
      const lane = (gpu ? 1 : 0) + i;
      const worker = new Worker(makeWorkerURL());
      const w = { worker, hps: 0 };
      worker.onmessage = (e) => {
        const msg = e.data;
        if (!run.found && !run.cancelled && msg.type === 'found') {
          this.onFound(run, msg.counter, 'cpu');
        } else if (msg.type === 'progress') {
          w.hps = msg.hps;
          run.hpsCpu = run.workers.reduce((a, x) => a + x.hps, 0);
        }
      };
      worker.postMessage({
        cmd: 'solve',
        prefix: preimagePrefix(run.ch),
        difficulty: run.ch.difficulty,
        lane,
        lanes: totalLanes,
      });
      run.workers.push(w);
    }

    if (gpu) {
      run.gpu = gpu;
      run.gpuActive = true;
      gpu.start(totalLanes, {
        onFound: (ctr) => this.onFound(run, ctr, 'gpu'),
        onHps: (h) => { run.hpsGpu = h; },
        onExhausted: () => { run.hpsGpu = 0; run.gpuActive = false; this.updateEngineLabel(run); },
      });
    }
  },

  onFound(run, counter, source) {
    if (run.found || run.cancelled) return;
    run.found = String(counter);
    run.foundBy = source;
    this.redeem(run);
  },

  tick(run) {
    const el = $('#pow-hps');
    if (!el || run.found) return;
    const hps = run.hpsCpu + run.hpsGpu;
    el.textContent = fmtCompactNum(hps);
    const expected = Math.pow(2, run.ch.difficulty);
    const done = hps * (Date.now() - run.startedAt) / 1000;
    const bar = $('#pow-progress');
    if (bar) bar.style.width = Math.min(99, done / expected * 100).toFixed(1) + '%';
    const etaEl = $('#pow-eta');
    if (etaEl) etaEl.textContent = hps > 0 ? fmtDuration((expected - done) / hps) : '—';
  },

  updateEngineLabel(run) {
    const el = $('#pow-engines');
    if (!el) return;
    const parts = [];
    if (run.workers.length) parts.push(run.workers.length + '×CPU');
    if (run.gpuActive) parts.push('GPU');
    el.textContent = parts.join(' + ') || '—';
  },

  async redeem(run) {
    this.stopEngines(run);
    const stage = $('#getkey-stage');
    stage.innerHTML = `<div class="neo-card p-6 bg-cream"><p class="font-black">✅ Solved (${esc(run.foundBy)}) — verifying…</p></div>`;
    try {
      const res = await api('/api/pow/redeem', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ challenge_id: run.ch.id, counters: [run.found] }),
      });
      localStorage.setItem('neo.apiKey', res.api_key);
      stage.innerHTML = `
        <div class="neo-card p-6 bg-green space-y-3">
          <h2 class="text-2xl font-black">🎉 Your API key is ready!</h2>
          <div class="flex items-center justify-between gap-3 bg-white border-[3px] border-ink rounded-xl p-3">
            <code id="key-text" class="text-sm font-bold break-all">${esc(res.api_key)}</code>
            <button id="btn-copy-key" class="neo-btn px-3 py-2 bg-yellow text-sm shrink-0">📋 Copy</button>
          </div>
          <p class="text-sm font-bold">Plan: <span class="badge bg-purple">${esc(res.plan)}</span> · Rate: <b>${res.rpm} req/min</b> · Expires: ${new Date(res.expires_at * 1000).toLocaleDateString()}</p>
          <p class="text-xs font-semibold opacity-70">Saved in this browser and used automatically in Chat. Copy it somewhere safe — it cannot be recovered later.</p>
          <div class="flex gap-3 pt-1">
            <a href="#/chat" class="neo-btn px-5 py-2 bg-blue text-white">💬 Start chatting</a>
            <button id="btn-again" class="neo-btn px-4 py-2 bg-white">Get another key</button>
          </div>
        </div>`;
      $('#btn-copy-key').addEventListener('click', () => navigator.clipboard.writeText(res.api_key));
      $('#btn-again').addEventListener('click', () => this.showPlans());
    } catch (err) {
      stage.innerHTML = this.errCard('Redemption failed: ' + esc(err.message));
    }
  },

  errCard(msg) {
    return `<div class="neo-card p-6 bg-red text-white space-y-3">
      <h2 class="text-lg font-black">😕 ${msg}</h2>
      <button class="neo-btn px-4 py-2 bg-white text-ink" onclick="location.reload()">Start over</button>
    </div>`;
  },

  stopEngines(run) {
    run.workers.forEach(({ worker }) => worker.terminate());
    run.workers = [];
    if (run.gpu) { run.gpu.destroy(); run.gpu = null; run.gpuActive = false; }
    if (this.timer) { clearInterval(this.timer); this.timer = null; }
  },

  cancel(silent) {
    if (this.visHandler) document.removeEventListener('visibilitychange', this.visHandler);
    if (this.run) {
      this.run.cancelled = true;
      this.stopEngines(this.run);
      this.run = null;
    } else if (!silent) {
      return;
    }
    if (!silent) {
      const stage = $('#getkey-stage');
      if (stage) stage.innerHTML = `
        <div class="neo-card p-6 bg-cream space-y-3">
          <h2 class="text-lg font-black">Cancelled.</h2>
          <button class="neo-btn px-4 py-2 bg-yellow" onclick="location.reload()">Try again</button>
        </div>`;
    }
  },
};

// ---------- shared helpers ----------

function preimagePrefix(ch) {
  return `pow-v${ch.version}|${ch.id}|${ch.resource}|${ch.algo}|${ch.difficulty}|${ch.salt}|${ch.bind}|`;
}

function padCounter(n) {
  let s = String(n);
  while (s.length < 10) s = '0' + s;
  return s;
}

function strToBytes(s) {
  const b = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) b[i] = s.charCodeAt(i) & 0xff;
  return b;
}

function jsLeadingZeroBits(d) {
  let bits = 0;
  for (let i = 0; i < d.length; i++) {
    if (d[i] === 0) { bits += 8; continue; }
    for (let s = 7; s >= 0; s--) { if (d[i] & (1 << s)) return bits; bits++; }
  }
  return bits;
}

function fmtCompactNum(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + ' GH/s';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + ' MH/s';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + ' kH/s';
  return Math.round(n) + ' H/s';
}

// ---------- CPU worker (pure-JS SHA-256, built from an inline Blob) ----------

function makeWorkerURL() {
  return URL.createObjectURL(new Blob([WORKER_SRC + SHA256_JS], { type: 'application/javascript' }));
}

const WORKER_SRC = String.raw`
let paused = false, found = false;

self.onmessage = function(e) {
  const m = e.data;
  if (m.cmd === 'pause') paused = true;
  else if (m.cmd === 'resume') paused = false;
  else if (m.cmd !== 'solve') return;

  const prefixBytes = strBytes(m.prefix);
  let counter = m.lane;
  let windowStart = performance.now();
  let windowHashes = 0;

  function chunk() {
    if (found) return;
    if (paused) { setTimeout(chunk, 300); return; }
    const bytes = new Uint8Array(prefixBytes.length + 10);
    bytes.set(prefixBytes);
    for (let i = 0; i < 4000; i++) {
      const cs = pad(counter);
      for (let j = 0; j < 10; j++) bytes[prefixBytes.length + j] = cs.charCodeAt(j);
      const d = sha256(bytes);
      if (leadingZeroBits(d) >= m.difficulty) {
        found = true;
        self.postMessage({ type: 'found', counter: cs });
        return;
      }
      counter += m.lanes;
      if (counter > 4294967295) { self.postMessage({ type: 'exhausted' }); return; }
      windowHashes++;
    }
    const now = performance.now();
    if (now - windowStart > 400) {
      self.postMessage({ type: 'progress', hps: windowHashes / ((now - windowStart) / 1000) });
      windowStart = now; windowHashes = 0;
    }
    setTimeout(chunk, 0);
  }
  chunk();

  function pad(n) { let s = String(n); while (s.length < 10) s = '0' + s; return s; }
  function strBytes(s) { const b = new Uint8Array(s.length); for (let i = 0; i < s.length; i++) b[i] = s.charCodeAt(i) & 0xff; return b; }
};
`;

// Compact SHA-256 used BOTH on the main thread (GPU self-test verification)
// and inside CPU workers (the same source text is injected into the Blob).
const _K = new Uint32Array([0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2]);

function jsRotr(x, n) { return ((x >>> n) | (x << (32 - n))) >>> 0; }

// jsSha256: Uint8Array -> Uint8Array(32)
function jsSha256(bytes) {
  const l = bytes.length;
  const total = Math.ceil((l + 9) / 64) * 64;
  const buf = new Uint8Array(total);
  buf.set(bytes);
  buf[l] = 0x80;
  const dv = new DataView(buf.buffer);
  dv.setUint32(total - 8, Math.floor(l / 0x20000000));
  dv.setUint32(total - 4, (l << 3) >>> 0);
  let h0=0x6a09e667,h1=0xbb67ae85,h2=0x3c6ef372,h3=0xa54ff53a,h4=0x510e527f,h5=0x9b05688c,h6=0x1f83d9ab,h7=0x5be0cd19;
  const w = new Uint32Array(64);
  for (let b = 0; b < total; b += 64) {
    for (let i = 0; i < 16; i++) w[i] = dv.getUint32(b + i * 4);
    for (let i = 16; i < 64; i++) {
      const s0 = jsRotr(w[i-15],7)^jsRotr(w[i-15],18)^(w[i-15]>>>3);
      const s1 = jsRotr(w[i-2],17)^jsRotr(w[i-2],19)^(w[i-2]>>>10);
      w[i] = (w[i-16]+s0+w[i-7]+s1)|0;
    }
    let a=h0,bv=h1,c=h2,d=h3,e=h4,f=h5,g=h6,h=h7;
    for (let i = 0; i < 64; i++) {
      const S1 = jsRotr(e,6)^jsRotr(e,11)^jsRotr(e,25);
      const chv = (e&f)^(~e&g);
      const t1 = (h+S1+chv+_K[i]+w[i])|0;
      const S0 = jsRotr(a,2)^jsRotr(a,13)^jsRotr(a,22);
      const maj = (a&bv)^(a&c)^(bv&c);
      const t2 = (S0+maj)|0;
      h=g; g=f; f=e; e=(d+t1)|0; d=c; c=bv; bv=a; a=(t1+t2)|0;
    }
    h0=(h0+a)|0; h1=(h1+bv)|0; h2=(h2+c)|0; h3=(h3+d)|0;
    h4=(h4+e)|0; h5=(h5+f)|0; h6=(h6+g)|0; h7=(h7+h)|0;
  }
  const out = new Uint8Array(32);
  const odv = new DataView(out.buffer);
  odv.setUint32(0,h0>>>0); odv.setUint32(4,h1>>>0); odv.setUint32(8,h2>>>0); odv.setUint32(12,h3>>>0);
  odv.setUint32(16,h4>>>0); odv.setUint32(20,h5>>>0); odv.setUint32(24,h6>>>0); odv.setUint32(28,h7>>>0);
  return out;
}

// Source text injected into CPU workers.
const SHA256_JS = `
const _K = new Uint32Array([${Array.from(_K).join(',')}]);
${jsRotr.toString()}
${jsSha256.toString().replace(/\bjsRotr\b/g, 'jsRotr').replace(/^function jsSha256/, 'function sha256')}
function leadingZeroBits(d) {
  let bits = 0;
  for (let i = 0; i < d.length; i++) {
    if (d[i] === 0) { bits += 8; continue; }
    for (let s = 7; s >= 0; s--) { if (d[i] & (1 << s)) return bits; bits++; }
  }
  return bits;
}
`;

// ---------- WebGPU solver ----------
//
// One compute shader invocation tests `items` counters on lane 0 of the lane
// set (CPU workers cover the remaining lanes), patching the 10 ASCII digits
// into the preimage template before each SHA-256. A winning counter is
// written atomically; the host polls between dispatch batches so the UI stays
// responsive and pause/cancel take effect immediately.
class GPUSolver {
  constructor(device, ch) {
    this.device = device;
    this.ch = ch;
    this.stopped = false;
    this.destroyed = false;
  }

  // Build the padded big-endian word template of
  // prefix + '0000000000' (+ SHA-256 padding) plus digit patch positions.
  buildTemplate() {
    const prefix = preimagePrefix(this.ch);
    const L = prefix.length + 10;
    const numWords = Math.ceil((L + 9) / 64) * 16; // whole 64-byte blocks
    if (numWords > 128) throw new Error('preimage too long for GPU kernel');
    const words = new Uint32Array(numWords);
    const all = prefix + '0000000000';
    for (let i = 0; i < all.length; i++) {
      words[i >> 2] |= (all.charCodeAt(i) & 0xff) << (24 - 8 * (i % 4)); // BE
    }
    words[L >> 2] |= 0x80 << (24 - 8 * (L % 4));
    words[numWords - 1] = (L * 8) >>> 0; // bit length (always < 2^32 here)

    const digitWord = [], digitShift = [];
    const off0 = prefix.length;
    for (let j = 0; j < 10; j++) {
      const p = off0 + j;
      digitWord.push(p >> 2);
      digitShift.push(24 - 8 * (p % 4));
    }
    return { words, numWords, digitWord, digitShift };
  }

  packParams(t, base, threads, items, lanes, difficulty) {
    const p = new Uint32Array(32);
    p[0] = t.numWords; p[1] = difficulty; p[2] = base; p[3] = items; p[4] = lanes;
    for (let j = 0; j < 10; j++) { p[6 + j] = t.digitWord[j]; p[16 + j] = t.digitShift[j]; }
    return p;
  }

  makeBindGroup(t, params) {
    const dev = this.device;
    const msgBuf = dev.createBuffer({ size: t.words.byteLength, usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_DST });
    dev.queue.writeBuffer(msgBuf, 0, t.words);
    const resBuf = dev.createBuffer({ size: 4, usage: GPUBufferUsage.STORAGE | GPUBufferUsage.COPY_DST | GPUBufferUsage.COPY_SRC });
    dev.queue.writeBuffer(resBuf, 0, new Uint32Array([0xFFFFFFFF]));
    const paramBuf = dev.createBuffer({ size: params.byteLength, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
    dev.queue.writeBuffer(paramBuf, 0, params);
    const readBuf = dev.createBuffer({ size: 4, usage: GPUBufferUsage.COPY_DST | GPUBufferUsage.MAP_READ });
    const bg = dev.createBindGroup({ layout: this.pipeline.getBindGroupLayout(0), entries: [
      { binding: 0, resource: { buffer: msgBuf } },
      { binding: 1, resource: { buffer: resBuf } },
      { binding: 2, resource: { buffer: paramBuf } },
    ]});
    return { msgBuf, resBuf, paramBuf, readBuf, bg };
  }

  async dispatchOnce(t, parts, base, threads, items, lanes) {
    const dev = this.device;
    const params = this.packParams(t, base, threads, items, lanes, this.ch.difficulty);
    dev.queue.writeBuffer(parts.paramBuf, 0, params);
    const enc = dev.createCommandEncoder();
    const pass = enc.beginComputePass();
    pass.setPipeline(this.pipeline);
    pass.setBindGroup(0, parts.bg);
    pass.dispatchWorkgroups(Math.ceil(threads / 256));
    pass.end();
    enc.copyBufferToBuffer(parts.resBuf, 0, parts.readBuf, 0, 4);
    dev.queue.submit([enc.finish()]);
    await parts.readBuf.mapAsync(GPUMapMode.READ);
    const v = new Uint32Array(parts.readBuf.getMappedRange().slice(0))[0];
    parts.readBuf.unmap();
    return v === 0xFFFFFFFF ? null : v - 1;
  }

  // Self-test: find a real solution in JS at difficulty 8, then confirm the
  // kernel returns a counter that ALSO satisfies it (guards against driver/
  // shader quirks falling back gracefully instead of producing garbage).
  async selfTest() {
    try {
      const prefix = preimagePrefix(this.ch);
      const savedCh = this.ch;
      const probeCh = Object.assign({}, savedCh, { difficulty: 8 });
      this.ch = probeCh;
      const t = this.buildTemplate();

      let expect = null;
      for (let c = 0; c < 4096; c++) {
        const all = strToBytes(prefix + padCounter(c));
        const digest = jsSha256(all);
        if (jsLeadingZeroBits(digest) >= 8) { expect = c; break; }
      }
      if (expect === null) { this.ch = savedCh; return false; }

      this.initPipeline(t);
      const parts = this.makeBindGroup(t, this.packParams(t, 0, 512, 8, 1, 8));
      const found = await this.dispatchOnce(t, parts, 0, 512, 8, 1);
      Object.values(parts).forEach((b) => b.destroy && b.destroy());
      this.ch = savedCh;
      if (found === null) return false;
      const d = jsSha256(strToBytes(prefix + padCounter(found)));
      return jsLeadingZeroBits(d) >= 8;
    } catch (e) {
      this.ch = savedCh;
      return false;
    }
  }

  initPipeline(t) {
    const module = this.device.createShaderModule({ code: WGSL_SRC });
    this.pipeline = this.device.createComputePipeline({
      layout: 'auto',
      compute: { module, entryPoint: 'main' },
    });
  }

  start(lanesTotal, cb) {
    this.lanesTotal = lanesTotal;
    this.cb = cb;
    const t = this.buildTemplate();
    const threads = 4096, items = 64; // 262144 counters per dispatch batch
    this.batchCounters = threads * items * lanesTotal;
    this.base = 0;
    this.lastTick = performance.now();
    this.tickHashes = 0;

    this.initPipeline(t);
    this.parts = this.makeBindGroup(t, this.packParams(t, 0, threads, items, lanesTotal, this.ch.difficulty));
    this.loop(t, threads, items);
  }

  async loop(t, threads, items) {
    while (!this.stopped && !this.destroyed) {
      if (this.paused) { await new Promise((r) => setTimeout(r, 300)); continue; }
      const maxBase = 4294967295 - this.batchCounters;
      if (this.base > Math.max(0, maxBase)) {
        this.cb.onExhausted && this.cb.onExhausted();
        return;
      }
      try {
        const found = await this.dispatchOnce(t, this.parts, this.base, threads, items, this.lanesTotal);
        if (this.destroyed || this.stopped) return;
        if (found !== null) {
          this.cb.onFound && this.cb.onFound(found);
          return;
        }
        const now = performance.now();
        const dt = now - this.lastTick;
        this.tickHashes += threads * items;
        if (dt > 400) {
          this.cb.onHps && this.cb.onHps(this.tickHashes / (dt / 1000));
          this.lastTick = now; this.tickHashes = 0;
        }
        this.base += this.batchCounters;
      } catch (e) {
        this.cb.onHps && this.cb.onHps(0);
        return;
      }
    }
  }

  destroy() {
    this.destroyed = true;
    this.stopped = true;
  }
}

// WGSL compute kernel: parallel SHA-256 over patched preimage copies.
const WGSL_SRC = `
struct Params {
  numWords : u32,
  difficulty: u32,
  base     : u32,
  items    : u32,
  lanes    : u32,
  _pad0    : u32,
  digitWord: array<u32, 10>,
  digitShift: array<u32, 10>,
};

@group(0) @binding(0) var<storage, read> msg : array<u32>;
@group(0) @binding(1) var<storage, read_write> result : atomic<u32>;
@group(0) @binding(2) var<uniform> params : Params;

var<private> state: array<u32, 8>;
var<private> blk: array<u32, 16>;

fn rotr(x: u32, n: u32) -> u32 { return (x >> n) | (x << (32u - n)); }

fn compress(blockOff: u32) {
  var w: array<u32, 64>;
  for (var i: u32 = 0u; i < 16u; i++) { w[i] = msg[blockOff + i]; }
  for (var i: u32 = 16u; i < 64u; i++) {
    let s0 = rotr(w[i-1u],7u) ^ rotr(w[i-1u],18u) ^ (w[i-1u] >> 3u);
    let s1 = rotr(w[i-2u],17u) ^ rotr(w[i-2u],19u) ^ (w[i-2u] >> 10u);
    w[i] = w[i-16u] + s0 + w[i-7u] + s1;
  }
  var a = state[0]; var b = state[1]; var c = state[2]; var d = state[3];
  var e = state[4]; var f = state[5]; var g = state[6]; var h = state[7];
  for (var i: u32 = 0u; i < 64u; i++) {
    let S1 = rotr(e,6u) ^ rotr(e,11u) ^ rotr(e,25u);
    let chv = (e & f) ^ ((~e) & g);
    let t1 = h + S1 +% chv +% K(i) +% w[i];
    let S0 = rotr(a,2u) ^ rotr(a,13u) ^ rotr(a,22u);
    let maj = (a & b) ^ (a & c) ^ (b & c);
    let t2 = S0 + maj;
    h = g; g = f; f = e; e = d + t1;
    d = c; c = b; b = a; a = t1 + t2;
  }
  state[0] = state[0] + a; state[1] = state[1] + b; state[2] = state[2] + c; state[3] = state[3] + d;
  state[4] = state[4] + e; state[5] = state[5] + f; state[6] = state[6] + g; state[7] = state[7] + h;
}

fn kConst(i: u32) -> u32 {
  var kk = array<u32,64>(
    0x428a2f98u,0x71374491u,0xb5c0fbcfu,0xe9b5dba5u,0x3956c25bu,0x59f111f1u,0x923f82a4u,0xab1c5ed5u,
    0xd807aa98u,0x12835b01u,0x243185beu,0x550c7dc3u,0x72be5d74u,0x80deb1feu,0x9bdc06a7u,0xc19bf174u,
    0xe49b69c1u,0xefbe4786u,0x0fc19dc6u,0x240ca1ccu,0x2de92c6fu,0x4a7484aau,0x5cb0a9dcu,0x76f988dau,
    0x983e5152u,0xa831c66du,0xb00327c8u,0xbf597fc7u,0xc6e00bf3u,0xd5a79147u,0x06ca6351u,0x14292967u,
    0x27b70a85u,0x2e1b2138u,0x4d2c6dfcu,0x53380d13u,0x650a7354u,0x766a0abbu,0x81c2c92eu,0x92722c85u,
    0xa2bfe8a1u,0xa81a664bu,0xc24b8b70u,0xc76c51a3u,0xd192e819u,0xd6990624u,0xf40e3585u,0x106aa070u,
    0x19a4c116u,0x1e376c08u,0x2748774cu,0x34b0bcb5u,0x391c0cb3u,0x4ed8aa4au,0x5b9cca4fu,0x682e6ff3u,
    0x748f82eeu,0x78a5636fu,0x84c87814u,0x8cc70208u,0x90befffau,0xa4506cebu,0xbef9a3f7u,0xc67178f2u);
  return kk[i];
}

fn leadingZero(hs: ptr<function, array<u32,8>>) -> u32 {
  var bits: u32 = 0u;
  for (var i: u32 = 0u; i < 8u; i++) {
    let cz = countLeadingZeros((*hs)[i]);
    bits = bits + cz;
    if (cz < 32u) { return bits; }
  }
  return bits;
}

@compute @workgroup_size(256)
fn main(@builtin(global_invocation_id) gid: vec3<u32>) {
  if (atomicLoad(&result) != 0xffffffffu) { return; }
  let tid = gid.x;

  // Local copy of the padded message template.
  var m: array<u32, 128>;
  for (var i: u32 = 0u; i < params.numWords; i++) { m[i] = msg[i]; }

  for (var j: u32 = 0u; j < params.items; j++) {
    let counter = params.base + (tid * params.items + j) * params.lanes;
    // Patch the 10 decimal digits of the counter into the message.
    var cc = counter;
    for (var p: i32 = 9; p >= 0; p--) {
      let dig = cc % 10u;
      cc = cc / 10u;
      let wi = params.digitWord[u32(p)];
      let sh = params.digitShift[u32(p)];
      m[wi] = (m[wi] & ~(0xFFu << sh)) | ((0x30u + dig) << sh);
    }
    // SHA-256 over the full padded message.
    state[0] = 0x6a09e667u; state[1] = 0xbb67ae85u; state[2] = 0x3c6ef372u; state[3] = 0xa54ff53au;
    state[4] = 0x510e527fu; state[5] = 0x9b05688cu; state[6] = 0x1f83d9abu; state[7] = 0x5be0cd19u;
    for (var off: u32 = 0u; off < params.numWords; off = off + 16u) { compress(off); }
    if (leadingZero(&state) >= params.difficulty) {
      atomicStore(&result, counter + 1u); // +1 so 0 is representable
      return;
    }
    if (atomicLoad(&result) != 0xffffffffu) { return; }
  }
}
`;






