'use strict';

// --- State ---
const state = {
  authed: false,
  email: '',
  book: null,          // { id, name, mime_type }
  chapterCount: 0,
  upTo: 1,
  recapWindow: 5,      // how many chapters to cover; 0 = full story
  chatHistory: [],     // main Ask tab
  recapHistory: [],    // seeded after recap completes
  photoHistory: [],    // seeded after photo explain/recap completes
  photoB64: null,
  photoMediaType: 'image/jpeg',
  streaming: false,
  activeTab: 'recap',
};

// --- DOM refs ---
const $ = id => document.getElementById(id);
const setupScreen  = $('setup-screen');
const loginScreen  = $('login-screen');
const appScreen    = $('app-screen');
const userEmail    = $('user-email');
const searchInput  = $('search-input');
const dropdown     = $('search-dropdown');
const bookCard     = $('book-card');
const bookTitle    = $('book-title');
const bookChapters = $('book-chapters');
const chapterInput = $('chapter-input');
const chapterRow   = $('chapter-row');
const actionRow    = $('action-row');
const recapPanel   = $('panel-recap');
const chatPanel    = $('panel-chat');
const photoPanel   = $('panel-photo');
const tabs         = document.querySelectorAll('.tab');

// --- Init ---
async function init() {
  const cfgRes = await fetch('/api/config');
  const cfg = await cfgRes.json();
  if (!cfg.ready) {
    showSetup(cfg);
    return;
  }

  const res = await fetch('/auth/status');
  const data = await res.json();
  if (data.authed) {
    state.authed = true;
    state.email = data.email;
    showApp();
  } else {
    showLogin();
  }
}

function showSetup(cfg) {
  setupScreen.classList.remove('hidden');
  loginScreen.classList.add('hidden');
  appScreen.classList.add('hidden');
  if (cfg.google_client_id) $('setup-gcid').value = cfg.google_client_id;
  if (cfg.google_redirect_uri) $('setup-redirect').value = cfg.google_redirect_uri;
  else $('setup-redirect').value = window.location.origin + '/auth/callback';
}

function showLogin() {
  setupScreen.classList.add('hidden');
  loginScreen.classList.remove('hidden');
  appScreen.classList.add('hidden');
}

function showApp() {
  setupScreen.classList.add('hidden');
  loginScreen.classList.add('hidden');
  appScreen.classList.remove('hidden');
  userEmail.textContent = state.email;
  switchTab('recap');
}

// --- Setup screen ---
$('btn-setup-save').addEventListener('click', async () => {
  const btn = $('btn-setup-save');
  const err = $('setup-error');
  err.style.display = 'none';
  btn.disabled = true;
  btn.textContent = 'Saving…';

  const body = {
    anthropic_api_key:    $('setup-anthropic').value.trim(),
    google_client_id:     $('setup-gcid').value.trim(),
    google_client_secret: $('setup-gcsecret').value.trim(),
    google_redirect_uri:  $('setup-redirect').value.trim() || window.location.origin + '/auth/callback',
  };

  const res = await fetch('/api/config', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const data = await res.json();
  btn.disabled = false;
  btn.textContent = 'Save & Continue';

  if (data.ready) {
    showLogin();
  } else {
    err.textContent = 'Missing required fields. Please fill in all keys.';
    err.style.display = 'block';
  }
});

// --- Book search ---
let searchTimer;
searchInput.addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(doSearch, 200);
});

searchInput.addEventListener('focus', () => {
  if (searchInput.value.trim() === '') doSearch();
});

document.addEventListener('click', e => {
  if (!e.target.closest('.search-wrap')) dropdown.classList.add('hidden');
});

async function doSearch() {
  const q = searchInput.value.trim();
  const res = await fetch('/api/search?q=' + encodeURIComponent(q));
  if (!res.ok) return;
  const files = await res.json();
  renderDropdown(files);
}

function renderDropdown(files) {
  if (!files || files.length === 0) {
    dropdown.classList.add('hidden');
    return;
  }
  dropdown.innerHTML = '';
  files.forEach(f => {
    const item = document.createElement('div');
    item.className = 'dropdown-item';
    const ext = f.mime_type.includes('epub') ? 'epub' : 'pdf';
    item.innerHTML = `<span class="name">${esc(f.name)}</span><span class="meta">${ext}</span>`;
    item.addEventListener('click', () => selectBook(f));
    dropdown.appendChild(item);
  });
  dropdown.classList.remove('hidden');
}

async function selectBook(file) {
  dropdown.classList.add('hidden');
  searchInput.value = '';
  state.book = file;
  state.chapterCount = 0;
  state.upTo = 1;
  state.chatHistory = [];

  bookTitle.textContent = file.name;
  bookChapters.textContent = 'Loading & indexing chapters… (first time may take ~30s)';
  bookCard.classList.remove('hidden');
  chapterRow.classList.add('hidden');
  actionRow.classList.add('hidden');

  const res = await fetch('/api/context', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ file_id: file.id, file_name: file.name, mime_type: file.mime_type }),
  });
  if (!res.ok) {
    bookChapters.textContent = 'Failed to load book.';
    return;
  }
  const data = await res.json();
  state.chapterCount = data.chapter_count;
  // Default to the first chapter, not the last — an accidental Enter/click
  // should never accidentally spoil the whole book.
  state.upTo = 1;
  bookChapters.textContent = `${data.chapter_count} chapters`;
  chapterInput.max = data.chapter_count;
  chapterInput.value = 1;
  chapterRow.classList.remove('hidden');
  actionRow.classList.remove('hidden');
}

$('btn-change-book').addEventListener('click', () => {
  state.book = null;
  bookCard.classList.add('hidden');
  chapterRow.classList.add('hidden');
  actionRow.classList.add('hidden');
  searchInput.focus();
  clearPanels();
});

chapterInput.addEventListener('change', () => {
  let v = parseInt(chapterInput.value, 10) || 1;
  if (v < 1) v = 1;
  if (v > state.chapterCount) v = state.chapterCount;
  chapterInput.value = v;
  state.upTo = v;
  state.chatHistory = [];
  state.recapHistory = [];
  state.photoHistory = [];
});

// --- Tab switching ---
const settingsPanel = $('panel-settings');

tabs.forEach(tab => {
  tab.addEventListener('click', () => switchTab(tab.dataset.tab));
});

function switchTab(name) {
  state.activeTab = name;
  tabs.forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  recapPanel.classList.toggle('hidden', name !== 'recap');
  chatPanel.classList.toggle('hidden', name !== 'chat');
  photoPanel.classList.toggle('hidden', name !== 'photo');
  settingsPanel.classList.toggle('hidden', name !== 'settings');
  if (name === 'settings') loadSettingsPanel();
}

async function loadSettingsPanel() {
  const res = await fetch('/api/config');
  const cfg = await res.json();
  $('cfg-gcid').value = cfg.google_client_id || '';
  $('cfg-gcsecret').value = cfg.google_client_secret || '';
  $('cfg-redirect').value = cfg.google_redirect_uri || '';
  $('cfg-anthropic').value = cfg.anthropic_api_key || '';
  $('cfg-folder').value = cfg.drive_folder_id || '';
}

$('btn-cfg-save').addEventListener('click', async () => {
  const btn = $('btn-cfg-save');
  const status = $('cfg-status');
  btn.disabled = true;
  btn.textContent = 'Saving…';
  status.style.display = 'none';

  const body = {
    anthropic_api_key:    $('cfg-anthropic').value.trim(),
    google_client_id:     $('cfg-gcid').value.trim(),
    google_client_secret: $('cfg-gcsecret').value.trim(),
    google_redirect_uri:  $('cfg-redirect').value.trim(),
    drive_folder_id:      $('cfg-folder').value.trim(),
  };

  await fetch('/api/config', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

  btn.disabled = false;
  btn.textContent = 'Save';
  status.textContent = '✓ Saved';
  status.style.color = '#7c6af7';
  status.style.display = 'block';
  setTimeout(() => { status.style.display = 'none'; }, 2500);
});

function clearFollowThread(panel) {
  $(`${panel}-follow`).classList.add('hidden');
  $(`${panel}-follow-messages`).innerHTML = '';
  if (panel === 'recap') state.recapHistory = [];
  else state.photoHistory = [];
}

function clearPanels() {
  $('recap-output').innerHTML = '';
  $('recap-output').classList.add('empty');
  $('chat-messages').innerHTML = '';
  clearFollowThread('recap');
  clearPhoto();
}

// --- Recap window chips ---
document.querySelectorAll('.window-chip').forEach(chip => {
  chip.addEventListener('click', () => {
    document.querySelectorAll('.window-chip').forEach(c => c.classList.remove('active'));
    chip.classList.add('active');
    state.recapWindow = parseInt(chip.dataset.window, 10);
  });
});

// --- Recap ---
$('btn-recap').addEventListener('click', doRecap);

async function doRecap() {
  if (!state.book || state.streaming) return;
  switchTab('recap');
  clearFollowThread('recap');

  const out = $('recap-output');
  out.innerHTML = '<div class="loader"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div>';
  out.classList.remove('empty');

  state.streaming = true;
  let text = '';

  // fromChapter: 0 means full story; otherwise start at (upTo - window + 1)
  const fromChapter = state.recapWindow > 0
    ? Math.max(1, state.upTo - state.recapWindow + 1)
    : 0;

  await streamSSE('/api/recap', {
    file_id: state.book.id,
    title: state.book.name,
    chapter_count: state.upTo,
    from_chapter: fromChapter,
  }, chunk => {
    if (out.querySelector('.loader')) out.innerHTML = '';
    text += chunk;
    out.innerHTML = renderMarkdown(text);
  });

  state.streaming = false;

  // Seed follow-up conversation with this recap as context
  if (text) {
    state.recapHistory = [
      { role: 'user', content: `Give me a "previously on" recap of "${state.book.name}" up to chapter ${state.upTo}.` },
      { role: 'assistant', content: text },
    ];
    $('recap-follow-messages').innerHTML = '';
    $('recap-follow').classList.remove('hidden');
  }
}

// --- Chat (Ask tab) ---
const chatInput    = $('chat-input');
const btnSend      = $('btn-send');
const chatMessages = $('chat-messages');

$('btn-ask').addEventListener('click', () => switchTab('chat'));

chatInput.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendChat(); }
});
btnSend.addEventListener('click', sendChat);

chatInput.addEventListener('input', () => {
  chatInput.style.height = 'auto';
  chatInput.style.height = Math.min(chatInput.scrollHeight, 140) + 'px';
});

async function sendChat() {
  const msg = chatInput.value.trim();
  if (!msg || !state.book || state.streaming) return;
  chatInput.value = '';
  chatInput.style.height = 'auto';

  state.chatHistory.push({ role: 'user', content: msg });
  appendMessage(chatMessages, 'user', msg);

  const assistantEl = appendMessage(chatMessages, 'assistant', '');
  assistantEl.classList.add('streaming');
  btnSend.disabled = true;
  state.streaming = true;
  let reply = '';

  await streamSSE('/api/chat', {
    file_id: state.book.id,
    title: state.book.name,
    chapter_count: state.upTo,
    messages: state.chatHistory,
  }, chunk => {
    reply += chunk;
    assistantEl.innerHTML = renderMarkdown(reply);
    chatMessages.scrollTop = chatMessages.scrollHeight;
  });

  assistantEl.classList.remove('streaming');
  state.chatHistory.push({ role: 'assistant', content: reply });
  state.streaming = false;
  btnSend.disabled = false;
}

function appendMessage(container, role, text) {
  const el = document.createElement('div');
  el.className = `message ${role}`;
  el.innerHTML = text ? renderMarkdown(text) : '';
  container.appendChild(el);
  container.scrollTop = container.scrollHeight;
  return el;
}

// --- Follow-up threads (recap + photo) ---

async function sendFollowUp(panel) {
  const inputEl   = $(`${panel}-follow-input`);
  const msgsEl    = $(`${panel}-follow-messages`);
  const sendEl    = $(`${panel}-follow-send`);
  const history   = panel === 'recap' ? state.recapHistory : state.photoHistory;

  const msg = inputEl.value.trim();
  if (!msg || !state.book || state.streaming) return;
  inputEl.value = '';
  inputEl.style.height = 'auto';

  history.push({ role: 'user', content: msg });
  appendMessage(msgsEl, 'user', msg);

  const assistantEl = appendMessage(msgsEl, 'assistant', '');
  assistantEl.classList.add('streaming');
  sendEl.disabled = true;
  state.streaming = true;
  let reply = '';

  await streamSSE('/api/chat', {
    file_id: state.book.id,
    title: state.book.name,
    chapter_count: state.upTo,
    messages: history,
  }, chunk => {
    reply += chunk;
    assistantEl.innerHTML = renderMarkdown(reply);
    msgsEl.scrollTop = msgsEl.scrollHeight;
  });

  assistantEl.classList.remove('streaming');
  history.push({ role: 'assistant', content: reply });
  state.streaming = false;
  sendEl.disabled = false;
}

['recap', 'photo'].forEach(panel => {
  $(`${panel}-follow-send`).addEventListener('click', () => sendFollowUp(panel));
  $(`${panel}-follow-input`).addEventListener('keydown', e => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendFollowUp(panel); }
  });
  $(`${panel}-follow-input`).addEventListener('input', () => {
    const el = $(`${panel}-follow-input`);
    el.style.height = 'auto';
    el.style.height = Math.min(el.scrollHeight, 140) + 'px';
  });
});

// --- Photo ---
const photoInput    = $('photo-input');
const photoPreview  = $('photo-preview');
const photoDetected = $('photo-detected');
const photoQuestion = $('photo-question');

$('btn-photo').addEventListener('click', () => switchTab('photo'));
$('photo-drop').addEventListener('click', () => photoInput.click());

$('photo-drop').addEventListener('dragover', e => {
  e.preventDefault();
  $('photo-drop').classList.add('dragging');
});
$('photo-drop').addEventListener('dragleave', () => $('photo-drop').classList.remove('dragging'));
$('photo-drop').addEventListener('drop', e => {
  e.preventDefault();
  $('photo-drop').classList.remove('dragging');
  const file = e.dataTransfer.files[0];
  if (file) loadPhoto(file);
});

photoInput.addEventListener('change', () => {
  if (photoInput.files[0]) loadPhoto(photoInput.files[0]);
});

function loadPhoto(file) {
  state.photoMediaType = file.type || 'image/jpeg';
  const reader = new FileReader();
  reader.onload = e => {
    const dataUrl = e.target.result;
    state.photoB64 = dataUrl.split(',')[1];
    photoPreview.innerHTML = `<img src="${dataUrl}" alt="book page">`;
    photoPreview.classList.remove('hidden');
    photoDetected.classList.add('hidden');
    photoDetected.textContent = '';
    clearFollowThread('photo');
  };
  reader.readAsDataURL(file);
}

function clearPhoto() {
  state.photoB64 = null;
  photoPreview.innerHTML = '';
  photoPreview.classList.add('hidden');
  photoDetected.classList.add('hidden');
  photoQuestion.value = '';
  $('photo-output').classList.add('hidden');
  $('photo-output').innerHTML = '';
  clearFollowThread('photo');
}

$('btn-photo-explain').addEventListener('click', () => doPhotoAction('explain'));
$('btn-photo-recap').addEventListener('click', () => doPhotoAction('recap'));

async function doPhotoAction(mode) {
  if (!state.book || !state.photoB64 || state.streaming) return;
  clearFollowThread('photo');

  const question = photoQuestion.value.trim() ||
    (mode === 'recap'
      ? 'Give me a full recap of everything up to this point.'
      : "What's happening in this passage? Explain it clearly in plain English.");

  const out = $('photo-output');
  out.innerHTML = '<div class="loader"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div>';
  out.classList.remove('empty', 'hidden');

  state.streaming = true;
  let text = '';
  let detectedChapter = state.upTo;

  const endpoint = mode === 'recap' ? '/api/photo-recap' : '/api/photo';
  const body = {
    file_id: state.book.id,
    title: state.book.name,
    chapter_count: -1,
    image_b64: state.photoB64,
    media_type: state.photoMediaType,
    question,
  };

  await streamSSE(endpoint, body, (chunk) => {
    try {
      const evt = JSON.parse(chunk);
      if (evt.chapter_detected) {
        detectedChapter = evt.chapter_detected;
        photoDetected.textContent = `📍 Detected: up to chapter ${evt.chapter_detected}`;
        photoDetected.classList.remove('hidden');
        return;
      }
    } catch (_) {}

    if (out.querySelector('.loader')) out.innerHTML = '';
    text += chunk;
    out.innerHTML = renderMarkdown(text);
  });

  state.streaming = false;

  // Seed follow-up with the photo + question as first exchange
  if (text) {
    state.photoHistory = [
      {
        role: 'user',
        content: [
          { type: 'image', source: { type: 'base64', media_type: state.photoMediaType, data: state.photoB64 } },
          { type: 'text', text: question },
        ],
      },
      { role: 'assistant', content: text },
    ];
    // Use detected chapter for subsequent follow-ups
    state.upTo = detectedChapter;
    $('photo-follow-messages').innerHTML = '';
    $('photo-follow').classList.remove('hidden');
  }
}

// --- SSE streaming helper ---
async function streamSSE(url, body, onChunk) {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const err = await res.text();
    onChunk(`\n\nError: ${err}`);
    return;
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const lines = buf.split('\n');
    buf = lines.pop();
    for (const line of lines) {
      if (!line.startsWith('data: ')) continue;
      const raw = line.slice(6);
      if (raw === '[DONE]') return;
      try {
        const chunk = JSON.parse(raw);
        if (typeof chunk === 'string') onChunk(chunk, raw);
        else onChunk(chunk, raw);
      } catch (_) {
        onChunk(raw);
      }
    }
  }
}

// --- Markdown renderer (minimal, safe) ---
function renderMarkdown(text) {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/\*([^*]+)\*/g, '<em>$1</em>')
    .replace(/^#{1,4} (.+)$/gm, '<strong>$1</strong>')
    .replace(/\n/g, '<br>');
}

function esc(s) {
  return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

// --- Boot ---
init();
