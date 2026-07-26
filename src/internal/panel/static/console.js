/* Web serial console: xterm.js over a websocket proxied to the node's LXD exec.
 *
 * Terminal bytes go over binary frames; resize notifications go over text
 * frames as JSON. Nothing from the socket is ever inserted into the DOM as
 * HTML — xterm renders it as terminal output only.
 */
(function () {
  'use strict';

  var host = document.getElementById('termHost');
  var statusEl = document.getElementById('termStatus');
  var reconnectBtn = document.getElementById('termReconnect');
  if (!host) return;

  var wsPath = host.getAttribute('data-ws');
  var term, fit, sock, closedByUser = false;

  var enc = new TextEncoder();
  var dec = new TextDecoder('utf-8', { fatal: false });

  function setStatus(text, color) {
    if (!statusEl) return;
    statusEl.textContent = text;          // textContent, never innerHTML
    statusEl.style.color = color || '';
  }

  function buildTerm() {
    term = new Terminal({
      cursorBlink: true,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize: window.innerWidth < 640 ? 12 : 13.5,
      scrollback: 5000,
      allowProposedApi: true,
      theme: {
        background: '#05050a', foreground: '#e9eaf2', cursor: '#7c5cff',
        selectionBackground: 'rgba(124,92,255,.35)',
        black: '#1b1c26', red: '#fb7185', green: '#34d399', yellow: '#fbbf24',
        blue: '#5b8cff', magenta: '#c084fc', cyan: '#22d3ee', white: '#d6d8e5',
        brightBlack: '#4a4f63', brightRed: '#ff9aa9', brightGreen: '#6ee7b7',
        brightYellow: '#fcd34d', brightBlue: '#93b4ff', brightMagenta: '#ddb2ff',
        brightCyan: '#67e8f9', brightWhite: '#ffffff'
      }
    });
    fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(host);
    doFit();
  }

  function doFit() {
    try { fit.fit(); } catch (_) { /* host not laid out yet */ }
  }

  function connect() {
    closedByUser = false;
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    var url = proto + location.host + wsPath +
      '?cols=' + encodeURIComponent(term.cols) + '&rows=' + encodeURIComponent(term.rows);

    setStatus('连接中…');
    sock = new WebSocket(url);
    sock.binaryType = 'arraybuffer';

    sock.onopen = function () {
      setStatus('已连接', '#34d399');
      sendResize();
      term.focus();
    };

    sock.onmessage = function (ev) {
      if (typeof ev.data === 'string') {
        term.write(ev.data);
      } else {
        term.write(dec.decode(new Uint8Array(ev.data), { stream: true }));
      }
    };

    sock.onerror = function () {
      setStatus('连接出错', '#fb7185');
    };

    sock.onclose = function (ev) {
      if (closedByUser) return;
      // 1006 with no prior open usually means the panel rejected the upgrade.
      var why = ev.code === 1006 ? '连接已断开' : '会话已结束';
      setStatus(why + '，可点击「重新连接」', '#fbbf24');
      term.write('\r\n\x1b[33m*** ' + why + ' ***\x1b[0m\r\n');
    };
  }

  function sendResize() {
    if (!sock || sock.readyState !== WebSocket.OPEN) return;
    sock.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  }

  buildTerm();

  term.onData(function (data) {
    if (sock && sock.readyState === WebSocket.OPEN) {
      sock.send(enc.encode(data));
    }
  });

  // Debounce resize so a drag does not flood the control channel.
  var resizeTimer;
  window.addEventListener('resize', function () {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () { doFit(); sendResize(); }, 140);
  });

  if (reconnectBtn) {
    reconnectBtn.addEventListener('click', function () {
      if (sock && sock.readyState <= WebSocket.OPEN) {
        closedByUser = true;
        sock.close();
      }
      term.reset();
      doFit();
      connect();
    });
  }

  window.addEventListener('pagehide', function () {
    closedByUser = true;
    if (sock) sock.close();
  });

  connect();
})();
