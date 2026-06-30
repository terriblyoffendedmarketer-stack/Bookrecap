'use strict';

// --- State ---
const state = {
  authed: false,
  email: '',
  book: null,        // { id, name, mime_type }
  chapterCount: 0,   // total chapters in the book
  upTo: 1,           // user-selected "read up to chapter N"
  chatHistory: [],   // [{role, content}]
  photoB64: null,
  photoMediaType: 'image/jpeg',
  streaming: false,
  activeTab: 'recap',
};

// --- DOM refs ---
const $ = id => document.getElementById(id);
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

function showLogin() {
  loginScreen.classList.remove('hidden');
  appScreen.classList.add('hidden');
}

function showApp() {
  loginScreen.classList.add('hidden');
  appScreen.classList.remove('hidden');
  userEmail.textContent = state.email;
  switchTab('recap');
}

// --- Book search ---
let searchTimer;
searchInput.addEventListener('input', () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(doSearch, 350);
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
  bookChapters.textContent = 'Loading chapters…';
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
  state.upTo = data.chapter_count; // default: all chapters read
  bookChapters.textContent = `${data.chapter_count} chapters`;
  chapterInput.max = data.chapter_count;
  chapterInput.value = data.chapter_count;
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
});

// --- Tab switching ---
tabs.forEach(tab => {
  tab.addEventListener('click', () => switchTab(tab.dataset.tab));
});

function switchTab(name) {
  state.activeTab = name;
  tabs.forEach(t => t.classList.toggle('active', t.dataset.tab === name));
  recapPanel.classList.toggle('hidden', name !== 'recap');
  chatPanel.classList.toggle('hidden', name !== 'chat');
  photoPanel.classList.toggle('hidden', name !== 'photo');
}

function clearPanels() {
  $('recap-output').innerHTML = '';
  $('recap-output').classList.add('empty');
  $('chat-messages').innerHTML = '';
  clearPhoto();
}

// --- Recap ---
$('btn-recap').addEventListener('click', doRecap);

async function doRecap() {
  if (!state.book || state.streaming) return;
  switchTab('recap');
  const out = $('recap-output');
  out.innerHTML = '';
  out.classList.remove('empty');
  out.innerHTML = '<div class="loader"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div>';

  state.streaming = true;
  let text = '';

  await streamSSE('/api/recap', {
    file_id: state.book.id,
    title: state.book.name,
    chapter_count: state.upTo,
  }, chunk => {
    if (out.querySelector('.loader')) out.innerHTML = '';
    text += chunk;
    out.innerHTML = renderMarkdown(text);
  });

  state.streaming = false;
}

// --- Chat ---
const chatInput = $('chat-input');
const btnSend   = $('btn-send');
const chatMessages = $('chat-messages');

$('btn-ask').addEventListener('click', () => switchTab('chat'));

chatInput.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    sendChat();
  }
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
  appendMessage('user', msg);

  const assistantEl = appendMessage('assistant', '');
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

function appendMessage(role, text) {
  const el = document.createElement('div');
  el.className = `message ${role}`;
  el.innerHTML = text ? renderMarkdown(text) : '';
  chatMessages.appendChild(el);
  chatMessages.scrollTop = chatMessages.scrollHeight;
  return el;
}

// --- Photo ---
const photoInput  = $('photo-input');
const photoPreview = $('photo-preview');
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
    // strip "data:image/...;base64,"
    state.photoB64 = dataUrl.split(',')[1];
    photoPreview.innerHTML = `<img src="${dataUrl}" alt="book page">`;
    photoPreview.classList.remove('hidden');
    photoDetected.classList.add('hidden');
    photoDetected.textContent = '';
  };
  reader.readAsDataURL(file);
}

function clearPhoto() {
  state.photoB64 = null;
  photoPreview.innerHTML = '';
  photoPreview.classList.add('hidden');
  photoDetected.classList.add('hidden');
  photoQuestion.value = '';
}

$('btn-photo-explain').addEventListener('click', () => doPhotoAction('explain'));
$('btn-photo-recap').addEventListener('click', () => doPhotoAction('recap'));

async function doPhotoAction(mode) {
  if (!state.book || !state.photoB64 || state.streaming) return;
  const question = photoQuestion.value.trim() ||
    (mode === 'recap' ? 'Give me a full recap of everything up to this point.' : "What's happening in this passage? Explain it clearly in plain English.");

  const out = $('photo-output');
  out.innerHTML = '<div class="loader"><div class="dot"></div><div class="dot"></div><div class="dot"></div></div>';
  out.classList.remove('empty', 'hidden');

  state.streaming = true;
  let text = '';

  const endpoint = mode === 'recap' ? '/api/photo-recap' : '/api/photo';
  const body = {
    file_id: state.book.id,
    title: state.book.name,
    chapter_count: -1,  // auto-detect from photo
    image_b64: state.photoB64,
    media_type: state.photoMediaType,
    question,
  };

  await streamSSE(endpoint, body, (chunk, raw) => {
    // Check for chapter_detected event
    try {
      const evt = JSON.parse(chunk);
      if (evt.chapter_detected) {
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
