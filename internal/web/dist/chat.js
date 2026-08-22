const ChatView = {
  state: {
    currentId: null,
    streaming: false,
    aborter: null,
    models: [],
    defaultModel: null,
  },

  render(root) {
    this.state.currentId = this.loadActive();
    root.innerHTML = this.html();
    this.bind();
    this.loadModels().then(() => this.renderSidebar());
    this.renderSidebar();
    this.switchTo(this.state.currentId);
  },

  destroy() {
    if (this.state.streaming) this.stop();
  },

  html() {
    return `
      <div class="min-h-screen flex">
        <aside class="w-64 shrink-0 border-r-[3px] border-ink bg-yellow flex flex-col">
          <div class="p-4">
            <a href="#/dashboard" class="flex items-center gap-2">
              <div class="neo-card-flat bg-ink text-yellow font-black text-lg w-10 h-10 flex items-center justify-center -rotate-3">⚡</div>
              <span class="text-lg font-black tracking-tight">Neo Chat</span>
            </a>
          </div>
          <div class="px-4 pb-3">
            <button id="btn-new-chat" class="neo-btn w-full py-2.5 text-sm flex items-center justify-center gap-2 bg-blue text-white">
              ＋ New chat
            </button>
          </div>
          <div id="conv-list" class="flex-1 overflow-y-auto px-3 pb-3 space-y-2"></div>
          <div class="p-4 border-t-[3px] border-ink">
            <button id="btn-clear-all" class="neo-btn w-full py-2 text-xs bg-cream">🗑 Clear all chats</button>
          </div>
        </aside>

        <main class="flex-1 flex flex-col min-w-0">
          <header class="border-b-[3px] border-ink bg-cream px-6 py-3 flex items-center justify-between gap-4">
            <div class="min-w-0">
              <h2 id="chat-title" class="text-lg font-black tracking-tight truncate">New chat</h2>
            </div>
            <div class="flex items-center gap-2 shrink-0">
              <select id="model-select" class="neo-input px-3 py-1.5 text-sm font-bold max-w-[220px]"></select>
              <a href="#/dashboard" class="neo-btn px-3 py-1.5 text-sm bg-purple">📊</a>
            </div>
          </header>

          <div id="msg-area" class="flex-1 overflow-y-auto px-4 md:px-8 py-6">
            <div id="msg-scroll" class="max-w-3xl mx-auto space-y-5"></div>
          </div>

          <div class="px-4 md:px-8 pb-6">
            <div class="max-w-3xl mx-auto">
              <div class="neo-card p-3 bg-white">
                <textarea id="chat-input" rows="1" placeholder="Message the gateway..." class="w-full resize-none bg-transparent outline-none font-medium leading-relaxed max-h-48"></textarea>
                <div class="flex items-center justify-between pt-2 border-t-[2px] border-ink">
                  <span id="chat-hint" class="text-[11px] font-bold opacity-50 hidden md:block">Enter to send · Shift+Enter for newline</span>
                  <div class="flex items-center gap-2">
                    <button id="btn-stop" class="neo-btn px-4 py-1.5 text-sm bg-red text-white hidden">⏹ Stop</button>
                    <button id="btn-send" class="neo-btn px-5 py-1.5 text-sm bg-yellow">Send ➤</button>
                  </div>
                </div>
              </div>
              <p class="text-center text-[11px] font-bold opacity-40 mt-3">Responses stream live through the proxy pool · conversation_id sticky sessions enabled</p>
            </div>
          </div>
        </main>
      </div>
    `;
  },

  bind() {
    $('#btn-new-chat').addEventListener('click', () => this.newChat());
    $('#btn-clear-all').addEventListener('click', () => this.clearAll());
    $('#btn-send').addEventListener('click', () => this.send());
    $('#btn-stop').addEventListener('click', () => this.stop());

    const input = $('#chat-input');
    input.addEventListener('input', () => {
      input.style.height = 'auto';
      input.style.height = Math.min(input.scrollHeight, 192) + 'px';
    });
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        this.send();
      }
    });

    $('#model-select').addEventListener('change', (e) => {
      const conv = this.conversation(this.state.currentId);
      if (conv) {
        conv.model = e.target.value;
        this.save();
      }
      localStorage.setItem('neo.defaultModel', e.target.value);
    });

    $('#msg-scroll').addEventListener('click', (e) => {
      const chip = e.target.closest('.chip');
      if (!chip) return;
      const input = $('#chat-input');
      input.value = chip.textContent.trim();
      this.send();
    });
  },

  async loadModels() {
    try {
      const data = await api('/v1/models');
      this.state.models = (data.data || []).map((m) => m.id);
      this.state.defaultModel = localStorage.getItem('neo.defaultModel') || this.state.models[0] || 'deepseek-v4-flash';
      const sel = $('#model-select');
      sel.innerHTML = this.state.models.map((m) =>
        `<option value="${esc(m)}">${esc(shortModel(m))}</option>`
      ).join('') || `<option value="${this.state.defaultModel}">${esc(shortModel(this.state.defaultModel))}</option>`;
      sel.value = this.state.defaultModel;
    } catch (err) {
      $('#model-select').innerHTML = `<option value="${this.state.defaultModel || ''}">${esc(this.state.defaultModel || 'model')}</option>`;
    }
  },

  all() {
    try {
      return JSON.parse(localStorage.getItem('neo.conversations') || '{}');
    } catch (e) {
      return {};
    }
  },

  save(conv) {
    const all = this.all();
    if (conv) all[conv.id] = conv;
    localStorage.setItem('neo.conversations', JSON.stringify(all));
  },

  loadActive() {
    const id = localStorage.getItem('neo.activeId');
    return this.all()[id] ? id : null;
  },

  saveActive(id) {
    localStorage.setItem('neo.activeId', id || '');
  },

  conversation(id) {
    return this.all()[id] || null;
  },

  newChat() {
    const conv = {
      id: crypto.randomUUID ? crypto.randomUUID() : String(Date.now()),
      title: 'New chat',
      model: $('#model-select').value || this.state.defaultModel,
      createdAt: Date.now(),
      updatedAt: Date.now(),
      messages: [],
    };
    const all = this.all();
    all[conv.id] = conv;
    localStorage.setItem('neo.conversations', JSON.stringify(all));
    this.switchTo(conv.id);
    this.renderSidebar();
    $('#chat-input').focus();
  },

  removeChat(id) {
    const all = this.all();
    delete all[id];
    localStorage.setItem('neo.conversations', JSON.stringify(all));
    if (this.state.currentId === id) {
      const keys = Object.keys(all);
      this.switchTo(keys[0] || null);
    }
    this.renderSidebar();
  },

  clearAll() {
    if (!confirm('Delete all conversations?')) return;
    localStorage.removeItem('neo.conversations');
    localStorage.removeItem('neo.activeId');
    this.switchTo(null);
    this.renderSidebar();
  },

  renderSidebar() {
    const list = $('#conv-list');
    if (!list) return;
    const convs = Object.values(this.all()).sort((a, b) => b.updatedAt - a.updatedAt);
    if (!convs.length) {
      list.innerHTML = '<p class="text-xs font-bold opacity-50 text-center py-6">No conversations yet.<br>Start a new chat!</p>';
      return;
    }
    list.innerHTML = convs.map((c) => `
      <div class="group flex items-center gap-1">
        <button data-id="${c.id}" class="conv-item flex-1 min-w-0 text-left px-3 py-2 text-sm font-bold border-[3px] border-ink rounded-xl bg-cream shadow-[3px_3px_0_0_#1B1B1B] transition-all hover:bg-white active:translate-x-[2px] active:translate-y-[2px] active:shadow-[1px_1px_0_0_#1B1B1B] ${c.id === this.state.currentId ? 'bg-white !shadow-[3px_3px_0_0_#FF6B6B]' : ''}">
          <div class="truncate">${esc(c.title)}</div>
        </button>
        <button data-del="${c.id}" class="conv-del shrink-0 w-7 h-7 ml-1 text-xs font-black border-[3px] border-ink rounded-lg bg-red text-white opacity-0 group-hover:opacity-100 transition-opacity hover:bg-white hover:text-red" title="Delete">✕</button>
      </div>
    `).join('');

    $$('.conv-item', list).forEach((btn) => {
      btn.addEventListener('click', () => this.switchTo(btn.dataset.id));
    });
    $$('.conv-del', list).forEach((btn) => {
      btn.addEventListener('click', () => this.removeChat(btn.dataset.del));
    });
  },

  switchTo(id) {
    if (this.state.streaming) this.stop();

    this.state.currentId = id;
    this.saveActive(id);
    this.renderSidebar();

    const title = $('#chat-title');
    const input = $('#chat-input');
    const sel = $('#model-select');

    if (!id) {
      title.textContent = 'New chat';
      input.value = '';
      $('#msg-scroll').innerHTML = this.emptyState();
      $('#btn-send').disabled = true;
      input.disabled = true;
      return;
    }

    const conv = this.conversation(id);
    title.textContent = conv.title;
    if (sel && conv.model) sel.value = conv.model;

    input.disabled = false;
    $('#btn-send').disabled = false;
    input.value = '';
    input.style.height = 'auto';

    const scroll = $('#msg-scroll');
    scroll.innerHTML = conv.messages.length
      ? conv.messages.map((m) => this.bubble(m)).join('')
      : this.emptyState();
    this.scrollBottom();
    input.focus();
  },

  emptyState() {
    const chips = [
      'Explain how the proxy pool rotates IPs',
      'Write a haiku about WARP containers',
      'Suggest a name for this gateway',
    ];
    return `
      <div class="text-center py-14 fade-up" data-empty="true">
        <div class="text-6xl mb-4">🤖</div>
        <h2 class="text-2xl font-black mb-2">How can I help?</h2>
        <p class="text-sm font-semibold opacity-60 mb-6">Pick a model above, type a message, and watch it stream.</p>
        <div class="flex flex-wrap justify-center gap-3">
          ${chips.map((c) => `<button class="chip neo-btn px-4 py-2 text-xs bg-purple">${esc(c)}</button>`).join('')}
        </div>
      </div>
    `;
  },

  bubble(m) {
    if (m.role === 'user') {
      return `
        <div class="flex justify-end fade-up">
          <div class="bubble-user max-w-[80%] px-4 py-2.5 text-sm whitespace-pre-wrap break-words">${esc(m.content)}</div>
        </div>`;
    }
    if (m.role === 'error') {
      return `
        <div class="flex justify-end fade-up">
          <div class="bubble-error max-w-[80%] px-4 py-2.5 text-sm whitespace-pre-wrap break-words">⚠️ ${esc(m.content)}</div>
        </div>`;
    }
    const think = m.thinking ? this.thinkingBlock(m.thinking, false) : '';
    return `
      <div class="flex gap-3 fade-up">
        <div class="w-8 h-8 shrink-0 rounded-full bg-ink text-yellow font-black text-sm flex items-center justify-center border-[3px] border-yellow">⚡</div>
        <div class="bubble-assistant max-w-[85%] px-4 py-2.5 chat-content text-sm break-words min-w-0">${think}${renderMarkdown(m.content)}</div>
      </div>`;
  },

  // thinkingBlock renders a collapsible "Thinking" section. While streaming
  // (streaming=true) it stays open and gets live-updated; after finish it is
  // collapsed by default (click to expand).
  thinkingBlock(text, streaming) {
    const open = streaming ? ' open' : '';
    return `
      <details class="think${open}">
        <summary class="think-summary">${streaming ? 'Thinking…' : 'Thinking'}</summary>
        <div class="think-body">${esc(text)}</div>
      </details>`;
  },

  // updateBubble renders the full assistant bubble (avatar + optional live
  // thinking block + markdown content + blinking cursor while streaming).
  // Rebuilding the whole bubble each frame guarantees thinking AND response
  // are both visible during streaming. If the user manually collapsed the
  // thinking section mid-stream, their choice is respected.
  updateBubble(el, content, thinking, streaming) {
    let keepOpen = true;
    if (streaming) {
      const prev = el.querySelector('details.think');
      keepOpen = !prev || prev.open;
    }
    const think = thinking ? this.thinkingBlock(thinking, streaming && keepOpen) : '';
    const body = content
      ? renderMarkdown(content)
      : (streaming ? '<span class="typing-dot"></span><span class="typing-dot"></span><span class="typing-dot"></span>' : '');
    const cursor = streaming ? '<span class="blink font-black">▌</span>' : '';
    el.innerHTML = `
      <div class="flex gap-3 fade-up">
        <div class="w-8 h-8 shrink-0 rounded-full bg-ink text-yellow font-black text-sm flex items-center justify-center border-[3px] border-yellow">⚡</div>
        <div class="bubble-assistant max-w-[85%] px-4 py-2.5 chat-content text-sm break-words min-w-0">${think}${body}${cursor}</div>
      </div>`;
  },

  typingIndicator() {
    return `
      <div class="flex gap-3 fade-up">
        <div class="w-8 h-8 shrink-0 rounded-full bg-ink text-yellow font-black text-sm flex items-center justify-center border-[3px] border-yellow">⚡</div>
        <div class="bubble-assistant px-5 py-3.5" id="typing-bubble">
          <span class="typing-dot"></span><span class="typing-dot"></span><span class="typing-dot"></span>
        </div>
      </div>`;
  },

  scrollBottom() {
    const area = $('#msg-area');
    if (area) area.scrollTop = area.scrollHeight;
  },

  send() {
    const input = $('#chat-input');
    const text = input.value.trim();
    if (!text || this.state.streaming) return;

    let conv = this.conversation(this.state.currentId);
    if (!conv) {
      this.newChat();
      conv = this.conversation(this.state.currentId);
    }

    conv.messages.push({ role: 'user', content: text });
    if (conv.title === 'New chat' || !conv.title) {
      conv.title = text.length > 42 ? text.slice(0, 42) + '…' : text;
    }
    conv.updatedAt = Date.now();
    this.save(conv);
    this.renderSidebar();

    const scroll = $('#msg-scroll');
    if ($('#msg-scroll [data-empty]')) {
      scroll.innerHTML = '';
    }
    scroll.insertAdjacentHTML('beforeend', this.bubble({ role: 'user', content: text }));

    const assistantEl = document.createElement('div');
    assistantEl.innerHTML = this.typingIndicator();
    scroll.appendChild(assistantEl);
    this.scrollBottom();

    input.value = '';
    input.style.height = 'auto';
    this.state.streaming = true;
    $('#btn-send').disabled = true;
    $('#btn-stop').classList.remove('hidden');
    $('#chat-hint').textContent = 'streaming…';

    const messages = conv.messages.map((m) => ({ role: m.role === 'error' ? 'user' : m.role, content: m.content }));
    const body = {
      model: conv.model,
      messages,
      stream: true,
      conversation_id: conv.id,
    };
    const convId = conv.id;

    const agent = this.state.aborter ? this.state.aborter : new AbortController();
    this.state.aborter = agent;

    let acc = '';
    let accThinking = '';
    let finished = false;
    let pendingRender = null;

    const renderLive = () => {
      pendingRender = null;
      this.updateBubble(assistantEl, acc, accThinking, true);
      this.scrollBottom();
    };

    const finish = (success, aborted) => {
      if (finished) return;
      finished = true;
      if (pendingRender !== null) cancelAnimationFrame(pendingRender);
      pendingRender = null;
      this.state.streaming = false;
      this.state.aborter = null;
      $('#btn-send').disabled = false;
      $('#btn-stop').classList.add('hidden');
      $('#chat-hint').textContent = 'Enter to send · Shift+Enter for newline';
      $$('#typing-bubble').forEach((el) => el.remove());

      const role = success || aborted ? 'assistant' : 'error';
      if (!success) {
        assistantEl.innerHTML = role === 'error'
          ? `<div class="flex gap-3"><div class="w-8 h-8 shrink-0 rounded-full bg-ink text-yellow font-black text-sm flex items-center justify-center border-[3px] border-yellow">⚡</div><div class="bubble-error max-w-[85%] px-4 py-2.5 text-sm whitespace-pre-wrap break-words">⚠️ ${esc(acc)}</div></div>`
          : '';
        this.scrollBottom();
      }
      // Always render the final bubble: thinking collapsed, full markdown,
      // no cursor. Previously the success path left the last raw dump.
      if (role === 'assistant') {
        this.updateBubble(assistantEl, acc, accThinking, false);
      }

    conv = this.conversation(convId);
      if (conv) {
        if (role === 'error') {
          conv.messages.push({ role: 'error', content: acc });
        } else {
          conv.messages.push({ role: 'assistant', content: acc, thinking: accThinking || undefined });
        }
        conv.updatedAt = Date.now();
        this.save(conv);
        this.renderSidebar();
      }
      this.scrollBottom();
    };

    fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: agent.signal,
    })
      .then(async (res) => {
        if (!res.ok) {
          let msg = 'HTTP ' + res.status;
          try {
            const j = await res.json();
            if (j.error && j.error.message) msg = j.error.message;
          } catch (e) {}
          throw new Error(msg);
        }
        return res.body.getReader();
      })
      .then((reader) => this.readStream(reader, (delta, thinking) => {
        acc += delta;
        if (thinking) accThinking += thinking;
        // Throttle re-renders to one per animation frame — markdown parsing
        // the full text on every token gets expensive on long responses.
        if (pendingRender === null) pendingRender = requestAnimationFrame(renderLive);
      }))
      .then(() => finish(true, false))
      .catch((err) => {
        if (err.name === 'AbortError') {
          acc = acc + (acc ? ' ' : '') + '(stopped)';
          finish(false, true);
        } else {
          acc = acc || err.message;
          finish(false, false);
        }
      });
  },

  async readStream(reader, onDelta) {
    const decoder = new TextDecoder();
    let buffer = '';
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buffer.indexOf('\n')) !== -1) {
        const line = buffer.slice(0, idx).trim();
        buffer = buffer.slice(idx + 1);
        if (!line.startsWith('data:')) continue;
        const payload = line.slice(5).trim();
        if (!payload || payload === '[DONE]') continue;
        let json;
        try {
          json = JSON.parse(payload);
        } catch (e) {
          continue;
        }
        if (json.error) {
          const msg = json.error.message || 'upstream error';
          throw new Error(msg);
        }
        const choices = json.choices || [];
        const ch = choices[0] || {};
        const d = ch.delta || {};
        const delta = d.content || ch.text || '';
        const msg0 = ch.message || {};
        // Thinking/reasoning tokens: Zen/DeepSeek emit delta.reasoning_content;
        // tolerate other providers' variants (delta.reasoning, delta.thinking,
        // choice/message-level reasoning).
        const thinking = d.reasoning_content || d.reasoning || d.thinking || ch.reasoning || msg0.reasoning || msg0.reasoning_content || '';
        if (delta) onDelta(delta, '');
        if (thinking) onDelta('', thinking);
      }
    }
  },

  stop() {
    if (this.state.aborter) {
      this.state.aborter.abort();
    }
  },
};

App.init();