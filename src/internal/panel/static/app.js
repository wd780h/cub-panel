/* cub-panel client.
 *
 * Every value that originates from the server or the user is written with
 * textContent or setAttribute. innerHTML is never used with dynamic data, so
 * a hostile label or error string cannot become markup.
 */
(function () {
  'use strict';

  var csrf = (document.querySelector('meta[name="csrf"]') || {}).content || '';

  /* ---------- toast ---------- */

  function toastHost() {
    var h = document.querySelector('.toast-host');
    if (!h) {
      h = document.createElement('div');
      h.className = 'toast-host';
      document.body.appendChild(h);
    }
    return h;
  }

  function toast(msg, kind) {
    var el = document.createElement('div');
    el.className = 'toast ' + (kind || '');
    el.textContent = msg;            // never innerHTML
    toastHost().appendChild(el);
    setTimeout(function () { el.remove(); }, 4200);
  }
  window.panelToast = toast;

  /* ---------- theme toggle ---------- */

  var themeBtn = document.getElementById('themeToggle');
  if (themeBtn) {
    themeBtn.addEventListener('click', function () {
      var root = document.documentElement;
      var cur = root.getAttribute('data-theme');
      if (!cur) {
        // No override yet: flip away from whatever the OS currently shows.
        cur = window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
      }
      var next = cur === 'dark' ? 'light' : 'dark';
      root.setAttribute('data-theme', next);
      try { localStorage.setItem('theme', next); } catch (e) {}
    });
  }

  /* ---------- nav ---------- */

  var toggle = document.getElementById('navToggle');
  var links = document.getElementById('navLinks');
  if (toggle && links) {
    toggle.addEventListener('click', function () {
      var open = links.classList.toggle('open');
      toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
  }

  /* ---------- copy to clipboard ---------- */

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-copy]');
    if (!btn) return;
    e.preventDefault();

    var text = btn.getAttribute('data-copy');
    if (text === '__sibling__') {
      var code = btn.parentElement.querySelector('code');
      text = code ? code.textContent : '';
    }
    if (!text) return;

    var done = function () {
      var old = btn.textContent;
      btn.textContent = '已复制';
      setTimeout(function () { btn.textContent = old; }, 1400);
    };

    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, function () { fallbackCopy(text, done); });
    } else {
      fallbackCopy(text, done);
    }
  });

  function fallbackCopy(text, done) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); done(); } catch (_) { toast('复制失败，请手动选择', 'bad'); }
    ta.remove();
  }

  /* ---------- confirm-guarded forms ---------- */

  document.addEventListener('submit', function (e) {
    var msg = e.target.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  /* ---------- fetch helper ---------- */

  function post(url, params) {
    var body = new URLSearchParams(params || {});
    body.set('_csrf', csrf);
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        'X-CSRF-Token': csrf,
        'X-Requested-With': 'fetch',
        'Accept': 'application/json'
      },
      body: body
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || ('请求失败 (' + r.status + ')'));
        return j;
      });
    });
  }

  /* ---------- instance actions ---------- */

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-action]');
    if (!btn) return;
    e.preventDefault();

    var id = btn.getAttribute('data-id');
    var action = btn.getAttribute('data-action');
    var confirmMsg = btn.getAttribute('data-confirm-msg');
    if (confirmMsg && !window.confirm(confirmMsg)) return;

    btn.classList.add('is-busy');

    if (action === 'password') {
      post('/api/instances/' + encodeURIComponent(id) + '/password', {})
        .then(function (j) { showPassword(j.password); })
        .catch(function (err) { toast(err.message, 'bad'); })
        .finally(function () { btn.classList.remove('is-busy'); });
      return;
    }

    post('/api/instances/' + encodeURIComponent(id) + '/action', { action: action })
      .then(function (j) {
        toast('操作已提交', 'ok');
        var card = document.querySelector('[data-inst="' + cssEscape(id) + '"]');
        if (card) applyState(card, { status: j.status });
        setTimeout(function () { refreshOne(id); }, 1500);
      })
      .catch(function (err) { toast(err.message, 'bad'); })
      .finally(function () { btn.classList.remove('is-busy'); });
  });

  function showPassword(pw) {
    var box = document.getElementById('pwResult');
    if (!box) { toast('新密码：' + pw, 'ok'); return; }
    var code = box.querySelector('code');
    if (code) code.textContent = pw;     // textContent, not innerHTML
    box.hidden = false;
    toast('root 密码已重置', 'ok');
  }

  /* ---------- label editing ---------- */

  var labelForm = document.getElementById('labelForm');
  if (labelForm) {
    labelForm.addEventListener('submit', function (e) {
      e.preventDefault();
      var id = labelForm.getAttribute('data-id');
      var input = labelForm.querySelector('input[name=label]');
      post('/api/instances/' + encodeURIComponent(id) + '/label', { label: input.value })
        .then(function () { toast('备注已保存', 'ok'); })
        .catch(function (err) { toast(err.message, 'bad'); });
    });
  }

  /* ---------- live state polling ---------- */

  var STATUS_TEXT = {
    running: '运行中', stopped: '已停止', frozen: '已冻结',
    provisioning: '创建中', expired: '已到期', overquota: '流量超限',
    error: '异常', failed: '异常'
  };
  var STATUS_CLASS = {
    running: 'ok', stopped: 'off', frozen: 'off',
    provisioning: 'wait', expired: 'bad', overquota: 'bad', error: 'bad', failed: 'bad'
  };

  function applyState(card, st) {
    var badge = card.querySelector('[data-field=status]');
    if (badge && st.status) {
      badge.textContent = STATUS_TEXT[st.status] || st.status;
      badge.className = 'badge ' + (STATUS_CLASS[st.status] || 'off');
    }
    setField(card, 'ipv4', st.ipv4);
    setField(card, 'ipv6', st.ipv6);
    if (typeof st.mem_used === 'number' && st.mem_max) {
      setField(card, 'mem', humanBytes(st.mem_used) + ' / ' + humanBytes(st.mem_max));
      var meter = card.querySelector('[data-field=mem-meter]');
      if (meter) meter.style.width = Math.min(100, Math.round(st.mem_used * 100 / st.mem_max)) + '%';
    }
    if (typeof st.disk === 'number' && st.disk > 0) setField(card, 'disk', humanBytes(st.disk));
    if (typeof st.net_rx === 'number') {
      setField(card, 'net', '↓ ' + humanBytes(st.net_rx) + '  ↑ ' + humanBytes(st.net_tx));
    }
    if (typeof st.uptime === 'number' && st.uptime > 0) setField(card, 'uptime', duration(st.uptime));
  }

  function setField(card, name, value) {
    if (value === undefined || value === null || value === '') return;
    var el = card.querySelector('[data-field=' + name + ']');
    if (el) el.textContent = value;      // textContent, not innerHTML
  }

  function refreshOne(id) {
    var card = document.querySelector('[data-inst="' + cssEscape(id) + '"]');
    if (!card) return Promise.resolve();
    return fetch('/api/instances/' + encodeURIComponent(id) + '/state', {
      credentials: 'same-origin',
      headers: { 'Accept': 'application/json', 'X-Requested-With': 'fetch' }
    })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) { if (j) applyState(card, j); })
      .catch(function () { /* transient node failure: keep the last view */ });
  }

  function refreshAll() {
    var cards = document.querySelectorAll('[data-inst]');
    for (var i = 0; i < cards.length; i++) {
      refreshOne(cards[i].getAttribute('data-inst'));
    }
  }

  if (document.querySelector('[data-inst]')) {
    refreshAll();
    var timer = setInterval(function () {
      if (document.visibilityState === 'visible') refreshAll();
    }, 12000);
    window.addEventListener('pagehide', function () { clearInterval(timer); });
  }

  /* ---------- node probe (admin) ---------- */

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-probe]');
    if (!btn) return;
    e.preventDefault();
    btn.classList.add('is-busy');
    post('/admin/nodes/probe', { id: btn.getAttribute('data-probe') })
      .then(function (j) {
        if (j.warning) {
          // Config mismatch: the node answers but provisioning would fail.
          toast(j.warning, 'bad');
        } else {
          toast('节点在线 · LXD ' + (j.lxd_version || '?') + ' · ' + (j.instances || 0) + ' 个容器', 'ok');
        }
        var cell = document.querySelector('[data-node-status="' + cssEscape(btn.getAttribute('data-probe')) + '"]');
        if (cell) {
          cell.textContent = j.warning ? '配置不符' : '在线';
          cell.className = j.warning ? 'badge bad' : 'badge ok';
          if (j.warning) cell.title = j.warning;
        }
      })
      .catch(function (err) { toast(err.message, 'bad'); })
      .finally(function () { btn.classList.remove('is-busy'); });
  });

  /* ---------- edit-in-place forms (admin) ---------- */

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-edit]');
    if (!btn) return;
    e.preventDefault();

    var src = document.getElementById(btn.getAttribute('data-edit'));
    var form = document.getElementById(btn.getAttribute('data-target'));
    if (!src || !form) return;

    // Values ride on data-* attributes and are copied into inputs by value,
    // so nothing is ever parsed as HTML.
    for (var i = 0; i < src.attributes.length; i++) {
      var a = src.attributes[i];
      if (a.name.indexOf('data-f-') !== 0) continue;
      var fields = form.querySelectorAll('[name="' + a.name.slice(7) + '"]');
      if (!fields.length) continue;
      if (fields.length > 1 && fields[0].type === 'checkbox') {
        // Checkbox group (e.g. image list): the data value is a comma list
        // of the values that should be ticked.
        var want = a.value.split(',').map(function (s) { return s.trim(); });
        for (var k = 0; k < fields.length; k++) {
          fields[k].checked = want.indexOf(fields[k].value) !== -1;
        }
        continue;
      }
      var field = fields[0];
      if (field.type === 'checkbox') field.checked = a.value === '1';
      else field.value = a.value;
    }
    var details = form.closest('details');
    if (details) details.open = true;
    form.scrollIntoView({ behavior: 'smooth', block: 'center' });
  });

  /* ---------- toggle a <details> panel from a button ---------- */

  document.addEventListener('click', function (e) {
    var btn = e.target.closest('[data-toggle]');
    if (!btn) return;
    e.preventDefault();
    var panel = document.querySelector(btn.getAttribute('data-toggle'));
    if (!panel) return;
    panel.open = !panel.open;
    if (panel.open) panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  });

  /* ---------- auto-submit selects & table filter (admin) ---------- */

  document.addEventListener('change', function (e) {
    var sel = e.target.closest('select[data-autosubmit]');
    if (sel && sel.form) sel.form.submit();
  });

  document.addEventListener('input', function (e) {
    var box = e.target.closest('input[data-filter]');
    if (!box) return;
    var table = document.querySelector(box.getAttribute('data-filter'));
    if (!table) return;
    var q = box.value.trim().toLowerCase();
    var rows = table.querySelectorAll('tbody tr');
    for (var i = 0; i < rows.length; i++) {
      rows[i].hidden = q !== '' && rows[i].textContent.toLowerCase().indexOf(q) === -1;
    }
  });

  /* ---------- snapshots (instance page) ---------- */

  var snapCard = document.getElementById('snapCard');
  if (snapCard) {
    var sid = snapCard.getAttribute('data-id');
    var snapList = document.getElementById('snapList');
    var snapForm = document.getElementById('snapForm');

    var loadSnaps = function () {
      fetch('/api/instances/' + encodeURIComponent(sid) + '/snapshots', {
        credentials: 'same-origin',
        headers: { 'Accept': 'application/json', 'X-Requested-With': 'fetch' }
      })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (j) { renderSnaps(j && j.snapshots); })
        .catch(function () { snapList.textContent = '读取失败'; });
    };

    var renderSnaps = function (snaps) {
      snapList.textContent = '';
      if (!snaps || !snaps.length) { snapList.textContent = '暂无快照'; return; }
      snaps.forEach(function (s) {
        var row = document.createElement('div');
        row.className = 'flex items-center justify-between gap-1';
        row.style.padding = '5px 0';
        var name = document.createElement('b');
        name.className = 'mono';
        name.textContent = s.name;                       // textContent, not innerHTML
        var btns = document.createElement('span');
        btns.appendChild(mkSnapBtn('还原', 'snapshots/restore', s.name, '还原到快照 ' + s.name + '？之后的改动会丢失。'));
        btns.appendChild(mkSnapBtn('删除', 'snapshots/delete', s.name, '删除快照 ' + s.name + '？'));
        row.appendChild(name);
        row.appendChild(btns);
        snapList.appendChild(row);
      });
    };

    var mkSnapBtn = function (label, path, snap, confirmMsg) {
      var b = document.createElement('button');
      b.className = 'btn btn-sm';
      b.textContent = label;
      b.style.marginLeft = '6px';
      b.addEventListener('click', function () {
        if (!window.confirm(confirmMsg)) return;
        b.classList.add('is-busy');
        post('/api/instances/' + encodeURIComponent(sid) + '/' + path, { snapshot: snap })
          .then(function () { toast('操作完成', 'ok'); loadSnaps(); })
          .catch(function (err) { toast(err.message, 'bad'); })
          .finally(function () { b.classList.remove('is-busy'); });
      });
      return b;
    };

    snapForm.addEventListener('submit', function (e) {
      e.preventDefault();
      var input = snapForm.querySelector('input[name=snapshot]');
      if (!input.value.trim()) return;
      post('/api/instances/' + encodeURIComponent(sid) + '/snapshots', { snapshot: input.value.trim() })
        .then(function () { toast('快照已创建', 'ok'); input.value = ''; loadSnaps(); })
        .catch(function (err) { toast(err.message, 'bad'); });
    });

    loadSnaps();
  }

  /* ---------- ISO mount (VM instance page) ---------- */

  var isoCard = document.getElementById('isoCard');
  if (isoCard) {
    var iid = isoCard.getAttribute('data-id');
    var isoState = document.getElementById('isoState');

    var renderISO = function (isos) {
      isoState.textContent = '';
      var wrap = document.createElement('div');
      var sel = document.createElement('select');
      sel.style.marginRight = '6px';
      if (!isos || !isos.length) {
        var opt = document.createElement('option');
        opt.textContent = '节点无可用 ISO';
        opt.disabled = true;
        sel.appendChild(opt);
      } else {
        isos.forEach(function (iso) {
          var o = document.createElement('option');
          o.value = iso.name;
          o.textContent = iso.name + ' (' + humanBytes(iso.size_bytes) + ')';
          sel.appendChild(o);
        });
      }
      var bootLbl = document.createElement('label');
      bootLbl.className = 'check';
      bootLbl.style.margin = '0 8px';
      var bootCb = document.createElement('input');
      bootCb.type = 'checkbox';
      var bootTxt = document.createElement('span');
      bootTxt.textContent = '从 ISO 引导';
      bootLbl.appendChild(bootCb);
      bootLbl.appendChild(bootTxt);

      var attach = document.createElement('button');
      attach.className = 'btn btn-sm btn-primary';
      attach.textContent = '挂载';
      attach.disabled = !isos || !isos.length;
      attach.addEventListener('click', function () {
        attach.classList.add('is-busy');
        post('/api/instances/' + encodeURIComponent(iid) + '/iso/attach',
          { iso: sel.value, boot: bootCb.checked ? '1' : '0' })
          .then(function () { toast('已挂载，重启虚拟机生效', 'ok'); })
          .catch(function (err) { toast(err.message, 'bad'); })
          .finally(function () { attach.classList.remove('is-busy'); });
      });

      var detach = document.createElement('button');
      detach.className = 'btn btn-sm';
      detach.textContent = '卸载';
      detach.style.marginLeft = '6px';
      detach.addEventListener('click', function () {
        detach.classList.add('is-busy');
        post('/api/instances/' + encodeURIComponent(iid) + '/iso/detach', {})
          .then(function () { toast('已卸载，重启虚拟机生效', 'ok'); })
          .catch(function (err) { toast(err.message, 'bad'); })
          .finally(function () { detach.classList.remove('is-busy'); });
      });

      wrap.appendChild(sel);
      wrap.appendChild(bootLbl);
      wrap.appendChild(attach);
      wrap.appendChild(detach);
      isoState.appendChild(wrap);
    };

    fetch('/api/instances/' + encodeURIComponent(iid) + '/isos', {
      credentials: 'same-origin',
      headers: { 'Accept': 'application/json', 'X-Requested-With': 'fetch' }
    })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) { renderISO(j && j.isos); })
      .catch(function () { isoState.textContent = '读取 ISO 库失败'; });
  }

  /* ---------- helpers ---------- */

  function humanBytes(n) {
    if (!n || n < 0) return '0 B';
    var u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + ' ' + u[i];
  }

  function duration(sec) {
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    if (d > 0) return d + ' 天 ' + h + ' 小时';
    if (h > 0) return h + ' 小时 ' + m + ' 分';
    return m + ' 分钟';
  }

  // Escape a value for safe use inside a CSS attribute selector.
  function cssEscape(v) {
    if (window.CSS && CSS.escape) return CSS.escape(v);
    return String(v).replace(/["\\]/g, '\\$&');
  }
})();
