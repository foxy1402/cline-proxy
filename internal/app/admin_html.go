package app

const adminHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Cline Proxy Admin</title>
<style>
:root{
  --bg:#0d0f12;--bg2:#131519;--bg3:#1a1d23;--panel:rgba(255,255,255,.028);
  --border:rgba(255,255,255,.08);--border-strong:rgba(255,255,255,.16);
  --text:#d6dae2;--text2:#8b919d;--text3:#5d626c;
  --accent:#8ab4f8;--accent2:#7ee2a8;--amber:#d9b04a;--danger:#e0705f;
  --accent-grad:linear-gradient(135deg,#8ab4f8,#a8c7fa);
  --glow:0 0 0 1px rgba(138,180,248,.2);
  --status-active-bg:rgba(126,226,168,.1);--status-cooldown-bg:rgba(217,176,74,.1);
  --status-expired-bg:rgba(224,112,95,.1);
  --btn-primary-bg:#2b4a77;--btn-primary-hover:#34578c;
  --btn-success-bg:#27513c;--btn-success-hover:#2f6249;
  --radius:10px;--radius-sm:7px;
}
[data-theme="light"]{
  --bg:#f6f7f8;--bg2:#ffffff;--bg3:#eef0f2;--panel:#ffffff;
  --border:rgba(15,18,22,.1);--border-strong:rgba(15,18,22,.22);
  --text:#1c1f24;--text2:#5c636e;--text3:#9aa0aa;
  --accent:#33629c;--accent2:#2b7a4b;--amber:#96700f;--danger:#b3442f;
  --accent-grad:linear-gradient(135deg,#33629c,#4a7ab5);
  --glow:0 0 0 1px rgba(51,98,156,.18);
  --status-active-bg:rgba(43,122,75,.1);--status-cooldown-bg:rgba(150,112,15,.1);
  --status-expired-bg:rgba(179,68,47,.08);
  --btn-primary-bg:#33629c;--btn-primary-hover:#2a5284;
  --btn-success-bg:#2b7a4b;--btn-success-hover:#246a40;
}
*{margin:0;padding:0;box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{font-family:'Inter','Segoe UI','PingFang SC','Microsoft YaHei',system-ui,sans-serif;background:var(--bg);color:var(--text);font-size:14px;line-height:1.55;min-height:100vh}
.mono,code{font-family:'JetBrains Mono','Cascadia Code','Fira Code',Consolas,monospace;font-size:12px}

/* ===== Layout ===== */
.layout{display:flex;min-height:100vh}
.sidebar{width:236px;background:var(--panel);backdrop-filter:blur(14px);border-right:1px solid var(--border);padding:18px 10px;flex-shrink:0;display:flex;flex-direction:column;position:sticky;top:0;height:100vh;overflow-y:auto}
.sidebar h1{font-size:15px;font-weight:700;padding:2px 10px 16px;border-bottom:1px solid var(--border);margin-bottom:10px;display:flex;align-items:center;gap:8px;letter-spacing:.02em}
.sidebar h1 .logo{width:28px;height:28px;border-radius:7px;background:var(--btn-primary-bg);display:inline-flex;align-items:center;justify-content:center;color:#fff;box-shadow:var(--glow)}
.sidebar h1 .logo svg{width:15px;height:15px}
.sidebar h1 .brand-name{color:var(--text)}
.sidebar h1 .theme-toggle{margin-left:auto;padding:4px 8px}
.sidebar h1 span{color:var(--accent)}
.nav-item{display:flex;align-items:center;gap:10px;padding:8px 12px;border-radius:8px;cursor:pointer;color:var(--text2);transition:.15s;font-size:13.5px;margin-bottom:1px;position:relative}
.nav-item .nav-ico{width:18px;height:18px;display:inline-flex;align-items:center;justify-content:center;flex-shrink:0}
.nav-item .nav-ico svg{width:16px;height:16px;stroke:currentColor;fill:none;stroke-width:1.7;stroke-linecap:round;stroke-linejoin:round}
.nav-item:hover{color:var(--text);background:rgba(255,255,255,.05)}
.nav-item.active{color:var(--text);background:rgba(255,255,255,.07);font-weight:600}
.nav-item.active::before{content:'';position:absolute;left:-10px;top:22%;bottom:22%;width:2px;border-radius:2px;background:var(--accent)}
.nav-item.active .nav-ico svg{stroke:var(--accent)}
.nav-group{font-size:10.5px;font-weight:600;letter-spacing:.12em;text-transform:uppercase;color:var(--text3);padding:14px 12px 5px;user-select:none}
.sidebar-footer{margin-top:auto;padding:12px 8px 4px;font-size:11.5px;color:var(--text3);border-top:1px solid var(--border)}
.sidebar-footer a{color:var(--accent);text-decoration:none}
.main{flex:1;padding:26px 34px 60px;min-width:0;max-width:1500px;margin:0 auto;width:100%}
h2{font-size:21px;margin-bottom:18px;font-weight:700;letter-spacing:.01em}

/* ===== Cards ===== */
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:14px;margin-bottom:26px}
.card{background:var(--panel);border:1px solid var(--border);border-radius:var(--radius);padding:18px;position:relative;overflow:hidden;transition:.2s}
.card:hover{border-color:var(--border-strong)}
.card .num{font-size:30px;font-weight:700;font-variant-numeric:tabular-nums;letter-spacing:-.02em;color:var(--text)}
.card .label{font-size:12px;color:var(--text2);margin-top:5px;display:flex;align-items:center;gap:6px}
.card .label::before{content:'';width:7px;height:7px;border-radius:50%;background:var(--lab-c,var(--accent))}
.card .num.green{color:var(--accent2);--lab-c:var(--accent2)}
.card .num.red{color:var(--danger);--lab-c:var(--danger)}
.card .num.yellow{color:var(--amber);--lab-c:var(--amber)}
.card .num.blue{color:var(--accent);--lab-c:var(--accent)}
.cards .card{animation:rise .45s ease both}
.cards .card:nth-child(1){animation-delay:.02s}
.cards .card:nth-child(2){animation-delay:.08s}
.cards .card:nth-child(3){animation-delay:.14s}
.cards .card:nth-child(4){animation-delay:.2s}
@keyframes rise{from{opacity:0;transform:translateY(10px)}to{opacity:1;transform:none}}

/* ===== Sections ===== */
.section{background:var(--panel);border:1px solid var(--border);border-radius:var(--radius);margin-bottom:22px;overflow:hidden;animation:rise .4s ease both}
.section-title{padding:13px 18px;border-bottom:1px solid var(--border);font-weight:600;font-size:14px;display:flex;align-items:center;gap:8px}
.section-title .sec-ico{width:17px;height:17px;display:inline-flex;align-items:center;justify-content:center;flex-shrink:0}
.section-title .sec-ico svg{width:15px;height:15px;stroke:var(--text2);fill:none;stroke-width:1.7;stroke-linecap:round;stroke-linejoin:round}
.section-body{padding:18px}
.tabs{display:flex;border-bottom:1px solid var(--border);padding:0 8px;gap:4px;overflow-x:auto}
.tab{padding:11px 18px;cursor:pointer;color:var(--text2);border-bottom:2px solid transparent;font-size:13px;white-space:nowrap;transition:.15s}
.tab:hover{color:var(--text);background:rgba(255,255,255,.04)}
.tab.active{color:var(--text);border-bottom-color:var(--accent);font-weight:600}
.tab-content{display:none;padding:18px}
.tab-content.active{display:block;animation:rise .25s ease both}

/* ===== Tables ===== */
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--border);font-size:13px;white-space:nowrap}
th{color:var(--text3);font-weight:600;font-size:11.5px;text-transform:uppercase;letter-spacing:.06em}
tbody tr{transition:.12s}
tbody tr:hover{background:rgba(255,255,255,.035)}
tbody tr:last-child td{border-bottom:none}

/* ===== Status badges ===== */
.status{display:inline-flex;align-items:center;gap:6px;padding:3px 10px;border-radius:999px;font-size:12px;font-weight:600}
.status.active{background:var(--status-active-bg);color:var(--accent2)}
.status.cooldown{background:var(--status-cooldown-bg);color:var(--amber)}
.status.expired{background:var(--status-expired-bg);color:var(--danger)}
.status-dot{width:7px;height:7px;border-radius:50%;display:inline-block}
.status-dot.active{background:var(--accent2)}
.status-dot.cooldown{background:var(--amber)}
.status-dot.expired{background:var(--danger)}

/* ===== Buttons ===== */
.btn{display:inline-flex;align-items:center;justify-content:center;gap:6px;padding:7px 15px;border:1px solid var(--border);border-radius:var(--radius-sm);background:rgba(255,255,255,.04);color:var(--text);cursor:pointer;font-size:13px;transition:.15s;text-decoration:none;font-family:inherit;white-space:nowrap}
.btn:hover{background:rgba(255,255,255,.09);border-color:var(--border-strong)}
.btn:active{transform:none}
.btn svg{width:14px;height:14px;stroke:currentColor;fill:none;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round;flex-shrink:0}
.btn-primary{background:var(--btn-primary-bg);border-color:transparent;color:#fff;font-weight:600}
.btn-primary:hover{background:var(--btn-primary-hover)}
.btn-success{background:var(--btn-success-bg);border-color:transparent;color:#fff;font-weight:600}
.btn-success:hover{background:var(--btn-success-hover)}
.btn-danger{border-color:rgba(224,112,95,.4);color:var(--danger);background:transparent}
.btn-danger:hover{background:rgba(224,112,95,.1);border-color:var(--danger)}
.btn-sm{padding:3px 10px;font-size:12px;border-radius:6px}
.btn-sm svg{width:13px;height:13px}

/* ===== Forms ===== */
input,textarea,select{width:100%;padding:9px 13px;background:rgba(0,0,0,.25);border:1px solid var(--border);border-radius:var(--radius-sm);color:var(--text);font-size:13px;font-family:inherit;transition:.15s}
[data-theme="light"] input,[data-theme="light"] textarea,[data-theme="light"] select{background:#fff}
input::placeholder,textarea::placeholder{color:var(--text3)}
input:focus,textarea:focus,select:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px rgba(138,180,248,.12)}
textarea{resize:vertical;min-height:84px;font-family:'JetBrains Mono','Cascadia Code',Consolas,monospace;font-size:12px}
select{cursor:pointer;appearance:none;background-image:linear-gradient(45deg,transparent 50%,var(--text2) 50%),linear-gradient(135deg,var(--text2) 50%,transparent 50%);background-position:calc(100% - 18px) 55%,calc(100% - 13px) 55%;background-size:5px 5px;background-repeat:no-repeat;padding-right:32px}
.form-row{display:flex;gap:14px;align-items:flex-end;margin-bottom:14px;flex-wrap:wrap}
.form-row .field{flex:1;min-width:180px}
.form-row .field label{display:block;font-size:12px;color:var(--text2);margin-bottom:6px;font-weight:500}
.form-actions{display:flex;gap:10px;margin-top:14px;flex-wrap:wrap}
.flex{display:flex;align-items:center;gap:8px}
.gap-4{gap:4px}
.text-right{text-align:right}
.mt-8{margin-top:8px}
.inline-flex{display:inline-flex;align-items:center;gap:6px}
.justify-between{display:flex;justify-content:space-between;align-items:center;gap:10px;flex-wrap:wrap}
.hint{font-size:12px;color:var(--text2);margin-top:8px;line-height:1.6}
.hint strong{color:var(--text)}

/* ===== Dashboard: quick start & endpoints ===== */
.steps{display:flex;flex-direction:column;gap:0}
.step{display:flex;gap:14px;position:relative;padding-bottom:18px}
.step:last-child{padding-bottom:0}
.step::before{content:'';position:absolute;left:13px;top:30px;bottom:2px;width:2px;background:var(--border);border-radius:2px}
.step:last-child::before{display:none}
.step-no{width:27px;height:27px;flex-shrink:0;border-radius:50%;border:1px solid var(--border-strong);color:var(--text2);font-weight:600;font-size:12.5px;display:flex;align-items:center;justify-content:center;background:rgba(255,255,255,.03)}
.step-title{font-weight:600;font-size:13.5px;margin-top:3px}
.step-desc{font-size:12.5px;color:var(--text2);margin-top:3px;line-height:1.6}
.step-desc code{background:rgba(255,255,255,.07);padding:1px 7px;border-radius:5px}
.endpoint-list{display:flex;flex-direction:column;gap:8px}
.endpoint{display:flex;align-items:center;gap:12px;padding:9px 13px;background:rgba(255,255,255,.025);border:1px solid var(--border);border-radius:8px;flex-wrap:wrap}
.endpoint code{background:rgba(138,180,248,.08);border:1px solid rgba(138,180,248,.2);color:var(--accent);padding:3px 10px;border-radius:6px;font-size:11.5px;white-space:nowrap}
.endpoint span{font-size:12px;color:var(--text2);flex:1;min-width:160px}
.danger-zone{border-color:rgba(224,112,95,.28)}
.danger-zone .section-title{color:var(--danger)}
.auto-pill{font-size:11px;color:var(--text2);background:rgba(255,255,255,.05);border:1px solid var(--border);border-radius:999px;padding:2px 10px;white-space:nowrap}
.auto-pill.paused{color:var(--text3);border-color:var(--border)}

/* ===== Toast ===== */
.toast{position:fixed;top:22px;right:22px;padding:12px 20px;border-radius:12px;color:#fff;z-index:9999;opacity:0;transform:translateY(-12px) scale(.97);transition:.3s cubic-bezier(.2,.9,.3,1.2);font-size:13px;max-width:420px;backdrop-filter:blur(12px);border:1px solid rgba(255,255,255,.14);box-shadow:0 12px 40px rgba(2,6,23,.5);white-space:pre-line}
.toast.show{opacity:1;transform:none}
.toast.success{background:#2b5a44}
.toast.error{background:#7a3a30}
.toast.info{background:#31465e}
.toast.warning{background:#6e5619}

/* ===== Misc ===== */
.loading{display:inline-block;width:14px;height:14px;border:2px solid var(--text3);border-top-color:var(--accent);border-radius:50%;animation:spin .7s linear infinite;vertical-align:-2px}
@keyframes spin{to{transform:rotate(360deg)}}
.empty{padding:30px;text-align:center;color:var(--text2)}
.empty-state{padding:44px 20px;text-align:center;color:var(--text2)}
.empty-state .icon{font-size:40px;margin-bottom:10px;display:block;opacity:.8}
.key-display{background:rgba(0,0,0,.3);padding:9px 13px;border-radius:var(--radius-sm);border:1px solid var(--border);font-family:'JetBrains Mono','Cascadia Code',Consolas,monospace;font-size:12px;word-break:break-all;cursor:pointer;transition:.15s}
.key-display:hover{border-color:var(--accent)}
.copy-icon{cursor:pointer;color:var(--text2);padding:2px 6px;border-radius:4px}
.copy-icon:hover{color:var(--text);background:var(--bg3)}
.model-tag{display:inline-block;padding:2px 9px;border-radius:6px;font-size:11px;background:rgba(255,255,255,.06);color:var(--text2);margin:2px;letter-spacing:.02em}
.model-tag.free{border:1px solid rgba(126,226,168,.35);color:var(--accent2);background:rgba(126,226,168,.07)}
.model-tag.pass{border:1px solid rgba(217,176,74,.35);color:var(--amber);background:rgba(217,176,74,.07)}
.theme-toggle{display:inline-flex;align-items:center;gap:5px;padding:4px 9px;border:1px solid var(--border);border-radius:7px;background:rgba(255,255,255,.04);color:var(--text2);cursor:pointer;font-size:12px;transition:.15s;font-family:inherit}
.theme-toggle:hover{color:var(--text);background:rgba(255,255,255,.09)}
[data-theme="dark"] .theme-toggle .light-label{display:none}
body:not([data-theme="dark"]) .theme-toggle .dark-label{display:none}
.probe-pill{font-size:11px;color:var(--text3)}
.oauth-card{border:1px solid var(--border);border-radius:10px;padding:16px;background:rgba(255,255,255,.03);margin-top:14px}
.stat-mini{font-family:'JetBrains Mono',Consolas,monospace;font-size:12px;color:var(--text2)}

/* ===== Responsive ===== */
@media (max-width:980px){
  .layout{flex-direction:column}
  .sidebar{width:100%;height:auto;position:sticky;top:0;flex-direction:row;align-items:center;padding:10px 12px;overflow-x:auto;gap:4px;z-index:50}
  .sidebar h1{display:flex;align-items:center;border-bottom:none;margin:0;padding:0 6px 0 0;gap:6px;font-size:13px;white-space:nowrap}
  .sidebar h1 .brand-name{display:none}
  .sidebar .nav-item{padding:7px 11px;white-space:nowrap}
  .sidebar .nav-item.active::before{display:none}
  .nav-group{display:none}
  .sidebar .theme-toggle{margin-left:4px}
  .sidebar-footer{display:none}
  .main{padding:20px 16px 48px}
}
@media (max-width:560px){
  .cards{grid-template-columns:repeat(2,1fr)}
  h2{font-size:18px}
  .section-title{padding:11px 14px}
  .section-body{padding:14px}
  .form-row .field{min-width:100%}
}
</style>
</head>
<body>
<div class="layout">
<div class="sidebar">
<h1><span class="logo"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/></svg></span><span class="brand-name">Cline Proxy</span><button class="theme-toggle" onclick="toggleTheme()" title="Toggle theme"><span class="icon" id="themeIcon"></span><span class="light-label">Light</span><span class="dark-label">Dark</span></button></h1>
<div class="nav-item active" data-tab="dashboard"><span class="nav-ico"><svg viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/></svg></span> Dashboard</div>
<div class="nav-group">Account pool</div>
<div class="nav-item" data-tab="accounts"><span class="nav-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4 3.6-6.5 8-6.5s8 2.5 8 6.5"/></svg></span> Accounts</div>
<div class="nav-item" data-tab="import"><span class="nav-ico"><svg viewBox="0 0 24 24"><path d="M12 3v10m0 0 4-4m-4 4-4-4"/><path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/></svg></span> Add accounts</div>
<div class="nav-group">Services</div>
<div class="nav-item" data-tab="settings"><span class="nav-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.7 1.7 0 0 0-1.87-.34 1.7 1.7 0 0 0-1 1.55V21a2 2 0 1 1-4 0v-.09a1.7 1.7 0 0 0-1-1.55 1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.7 1.7 0 0 0 .34-1.87 1.7 1.7 0 0 0-1.55-1H3a2 2 0 1 1 0-4h.09a1.7 1.7 0 0 0 1.55-1 1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.7 1.7 0 0 0 1.87.34h0a1.7 1.7 0 0 0 1-1.55V3a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1 1.55h0a1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.7 1.7 0 0 0-.34 1.87v0a1.7 1.7 0 0 0 1.55 1H21a2 2 0 1 1 0 4h-.09a1.7 1.7 0 0 0-1.55 1z"/></svg></span> Gateway settings</div>
<div class="nav-item" data-tab="logs"><span class="nav-ico"><svg viewBox="0 0 24 24"><path d="M8 6h13M8 12h13M8 18h13"/><path d="M3 6h.01M3 12h.01M3 18h.01"/></svg></span> Request logs</div>
<div class="nav-item" data-tab="proxypool"><span class="nav-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="5" r="2.5"/><circle cx="5" cy="19" r="2.5"/><circle cx="19" cy="19" r="2.5"/><path d="M12 7.5v4m0 0-5.5 5m5.5-5 5.5 5"/></svg></span> Proxy pool</div>
<div class="nav-item" data-tab="opencode"><span class="nav-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14.5 14.5 0 0 1 0 18 14.5 14.5 0 0 1 0-18z"/></svg></span> opencode free models</div>
<div class="nav-item" data-tab="combos"><span class="nav-ico"><svg viewBox="0 0 24 24"><path d="M8 3v5a4 4 0 0 1-4 4 4 4 0 0 1 4 4v5M16 3v5a4 4 0 0 0 4 4 4 4 0 0 0-4 4v5"/></svg></span> Combos</div>
<div class="sidebar-footer">
  <div>Admin panel: <a href="/admin/">/admin/</a></div>
  <div>API address: <span id="footerApiAddr">http://127.0.0.1:3457</span></div>
</div>
</div>

<div class="main">

<div id="tab-dashboard" class="tab-panel">
<h2>Dashboard</h2>
<div class="cards">
  <div class="card"><div class="num blue" id="statTotal">-</div><div class="label">Total accounts</div></div>
  <div class="card"><div class="num green" id="statActive">-</div><div class="label">Active</div></div>
  <div class="card"><div class="num yellow" id="statCooldown">-</div><div class="label">Cooldown</div></div>
  <div class="card"><div class="num red" id="statExpired">-</div><div class="label">Expired</div></div>
</div>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/></svg></span> Quick actions</div>
  <div class="section-body" style="display:flex;gap:10px;flex-wrap:wrap">
    <button class="btn btn-primary" onclick="switchTab('import')">Add account</button>
    <button class="btn" onclick="refreshAllTokens()">Refresh all tokens</button>
    <button class="btn" onclick="document.getElementById('fileInput').click()">Import from file</button>
    <input type="file" id="fileInput" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    <button class="btn" onclick="switchTab('settings');generateKey()">Generate API key</button>
  </div>
</div>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/></svg></span> Quick start</div>
  <div class="section-body">
    <div class="steps">
      <div class="step"><div class="step-no">1</div><div class="step-body"><div class="step-title">Add Cline accounts</div><div class="step-desc">On the Add accounts page: browser OAuth, a refreshToken, or a static API key (<code>sk_...</code>) — accounts are the proxy's upstream quota.</div></div></div>
      <div class="step"><div class="step-no">2</div><div class="step-body"><div class="step-title">Generate an API key</div><div class="step-desc">Generate a key under Proxy settings → API keys to authenticate clients; with no keys configured, unauthenticated access is allowed.</div></div></div>
      <div class="step"><div class="step-no">3</div><div class="step-body"><div class="step-title">Configure your client</div><div class="step-desc">Set the Base URL to <code id="quickstartApi">http://127.0.0.1:3457/v1</code> and pick any model ID from the available models list.</div></div></div>
    </div>
  </div>
</div>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M9 12h6M12 9v6"/><circle cx="12" cy="12" r="9"/></svg></span> Endpoints</div>
  <div class="section-body">
    <div class="endpoint-list">
      <div class="endpoint"><code>POST /v1/chat/completions</code><span>OpenAI Chat format — works with most tools out of the box</span></div>
      <div class="endpoint"><code>POST /v1/messages</code><span>Anthropic Messages format — for Claude Code / Cline and similar clients</span></div>
      <div class="endpoint"><code>POST /v1/responses</code><span>OpenAI Responses format — for Cursor and similar clients</span></div>
      <div class="endpoint"><code>GET /v1/models</code><span>Model list</span></div>
    </div>
  </div>
</div>
</div>

<div id="tab-accounts" class="tab-panel" style="display:none">
  <div class="flex justify-between" style="margin-bottom:16px">
  <h2>Accounts</h2>
  <div style="display:flex;gap:8px">
    <button class="btn btn-sm" onclick="exportAccounts()">Export accounts</button>
    <button class="btn btn-primary btn-sm" onclick="switchTab('import')">Add</button>
    <button class="btn btn-sm" onclick="loadAccounts()">Refresh</button>
  </div>
</div>
<div class="hint" style="margin:-4px 0 16px;padding:11px 14px;border:1px solid var(--border);border-radius:10px;background:rgba(148,163,184,.05)">
  <strong style="color:var(--text)">Tokens</strong> are counted locally by this proxy (input+output; exact when upstream returns usage, otherwise estimated from the request body) to gauge headroom before official rate limits. The Test button sends a real probe request. The Reset button <strong style="color:var(--text)">probes upstream rate-limit status</strong>: if still limited it stays in cooldown and shows the estimated recovery time; only a passing probe lifts the cooldown and resets today's stats.
</div>
<div class="section">
  <div class="section-body" style="padding:6px">
    <div class="table-wrap">
    <table>
      <thead>
        <tr><th>Email</th><th>Status</th><th title="Counted locally by this proxy — not the official free quota">Today/Total tokens</th><th>Last used</th><th>Created</th><th>Actions</th></tr>
      </thead>
      <tbody id="accountTableBody">
        <tr><td colspan="6" class="empty">Loading...</td></tr>
      </tbody>
    </table>
    </div>
  </div>
</div>
</div>

<div id="tab-import" class="tab-panel" style="display:none">
<h2>Add accounts</h2>
<div class="section">
  <div class="tabs" id="importTabs">
    <div class="tab active" data-tab="oauth">OAuth browser login</div>
    <div class="tab" data-tab="token">Manual entry</div>
    <div class="tab" data-tab="batch">Batch import</div>
  </div>

  <div id="import-oauth" class="tab-content active">
    <p class="hint">Cline accounts via browser OAuth — Google/GitHub/email sign-in supported; the refreshToken is captured automatically.</p>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="startOAuth()" id="oauthBtn">Start OAuth login</button>
    </div>
    <div id="oauthProgress" style="display:none;margin-top:14px" class="oauth-card">
      <div style="display:flex;align-items:center;gap:14px">
        <div class="loading"></div>
        <div>
          <div style="font-weight:600" id="oauthStatus">Waiting for browser authorization...</div>
          <div class="hint">
            Open <a href="#" id="oauthUrl" target="_blank" style="color:var(--accent)"></a>
            and enter the code: <strong style="color:var(--accent);font-size:15px;letter-spacing:2px" id="oauthUserCode"></strong>
          </div>
        </div>
      </div>
    </div>
    <div id="oauthResult" style="display:none;margin-top:14px"></div>
  </div>

  <div id="import-token" class="tab-content">
    <p class="hint">Paste a Cline OAuth refreshToken or a static Cline API key (<code>sk_...</code>) — the type is detected automatically. API keys are used as-is (no validation call); refresh tokens are verified against the upstream first.</p>
    <div class="form-row">
      <div class="field">
        <label>Token or API key *</label>
        <input type="text" id="tokenInput" placeholder="workos-... refreshToken or sk_... API key" style="font-family:'JetBrains Mono',Consolas,monospace">
      </div>
      <div class="field">
        <label>Email / label (optional; auto-generated if empty)</label>
        <input type="text" id="tokenEmail" placeholder="user@example.com">
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="addByToken()">Add account</button>
    </div>
    <div id="tokenResult" style="margin-top:8px"></div>
  </div>

  <div id="import-batch" class="tab-content">
    <p class="hint">Import multiple accounts at once. Accepts a JSON array (entries with <code>refreshToken</code> or <code>apiToken</code>) or one credential per line — lines starting with <code>sk_</code> are treated as API keys, everything else as refresh tokens.</p>
    <div class="form-row">
      <div class="field">
        <label>JSON array or one credential per line</label>
        <textarea id="batchInput" placeholder='[{"refreshToken":"xxx","email":"u1@x.com"},{"apiToken":"sk_yyy","email":"u2@x.com"}]'></textarea>
      </div>
    </div>
    <div class="form-actions">
      <button class="btn btn-primary" onclick="batchImport()">Import all</button>
      <button class="btn" onclick="document.getElementById('fileInput2').click()">Choose file</button>
      <input type="file" id="fileInput2" accept=".json,.txt" style="display:none" onchange="handleFileImport(event)">
    </div>
    <div id="batchResult" style="margin-top:8px"></div>
  </div>
</div>
</div>

<div id="tab-proxypool" class="tab-panel" style="display:none">
<h2>Proxy pool</h2>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="5" r="2.5"/><circle cx="5" cy="19" r="2.5"/><circle cx="19" cy="19" r="2.5"/><path d="M12 7.5v4m0 0-5.5 5m5.5-5 5.5 5"/></svg></span> Shared egress proxies</div>
  <div class="section-body">
    <div class="hint" style="margin-bottom:10px">One proxy list serves every upstream that opts in below. One proxy per line: <code>http://user:pass@host:port</code> or <code>socks5://host:port</code>. Requests rotate across healthy proxies per request; a proxy that hits a rate limit is cooled down and skipped automatically.</div>
    <div class="form-row">
      <div class="field" style="flex:3"><label>Proxy list</label>
        <textarea id="ppProxies" rows="4" placeholder="one per line: http://user:pass@host:port or socks5://host:port"></textarea>
      </div>
      <div class="field"><label>Rotation strategy</label>
        <select id="ppStrategy"><option value="round_robin">Round-robin (round_robin)</option><option value="random">Random (random)</option><option value="fill">Fill (fill)</option></select>
      </div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveProxyPool()">Save proxy pool</button></div>
    <div id="ppSaveResult" style="margin-top:8px"></div>
  </div>
</div>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg></span> Where this pool is used</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Cline upstream</label>
        <div style="display:flex;gap:6px;align-items:center">
          <select id="ppCline" style="flex:1"><option value="true">Use proxy pool</option><option value="false">Direct connection</option></select>
          <button class="btn btn-sm btn-primary" onclick="saveClineProxies()">Save</button>
        </div>
        <div class="hint">Off (direct) by default. The <code>CLINE_USE_PROXIES=true</code> env var always forces this on. Individual Combos can also opt in from the Combos page.</div>
      </div>
      <div class="field"><label>opencode zen upstream</label>
        <div class="hint" style="margin-top:6px" id="ppZenState">-</div>
        <div class="hint">Zen automatically routes through the pool above whenever the list is non-empty — no separate switch needed.</div>
      </div>
    </div>
  </div>
</div>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M12 8v4m0 4h.01"/></svg></span> Cooldown status</div>
  <div class="section-body">
    <div class="hint" id="ppCooldownInfo" style="margin:0">-</div>
  </div>
</div>
</div>

<div id="tab-settings" class="tab-panel" style="display:none">
<h2>Gateway settings</h2>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M21 2l-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.78 7.78 5.5 5.5 0 0 1 7.78-7.78zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg></span> API keys</div>
  <div class="section-body">
    <p class="hint">Generated keys authenticate client access to the proxy API (sent as the x-api-key or Authorization header).</p>
    <div class="form-actions" style="margin-bottom:14px">
      <button class="btn btn-success" onclick="generateKey()">Generate new key</button>
    </div>
    <div id="keysList"></div>
    <div id="keyGenResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><rect x="2" y="7" width="20" height="14" rx="2"/><path d="M16 7V5a2 2 0 0 0-2-2h-4a2 2 0 0 0-2 2v2"/><path d="M12 12v3M8.5 13.5v0M15.5 13.5v0"/></svg></span> Available models <span id="modelsProbeInfo" class="probe-pill" style="font-weight:normal"></span></div>
  <div class="section-body">
    <div class="flex" style="margin-bottom:10px;gap:10px">
      <button class="btn btn-sm btn-primary" onclick="refreshModels()">Refresh models</button>
      <span class="hint" style="margin:0">Auto-syncs the official upstream free-model feed (60s); only quota-free models are shown</span>
    </div>
    <div id="modelsList">Loading...</div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="3"/><path d="M12 2v4m0 12v4M2 12h4m12 0h4M4.9 4.9l2.8 2.8m8.6 8.6 2.8 2.8m0-14.2-2.8 2.8M7.7 16.3l-2.8 2.8"/></svg></span> General config</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Listen address</label><input type="text" id="settingAddr" disabled></div>
      <div class="field">
        <label>Default model</label>
        <div style="display:flex;gap:6px;align-items:center">
          <select id="settingDefModel" style="flex:1;font-family:'JetBrains Mono',Consolas,monospace"></select>
          <button class="btn btn-sm btn-primary" onclick="saveDefaultModel()">Save</button>
        </div>
      </div>
    </div>
    <div class="form-row">
      <div class="field">
        <label>Scheduling strategy</label>
        <select id="settingStrategy" onchange="updateConfig()">
          <option value="round_robin">Round-robin (round_robin)</option>
          <option value="fill">Fill (fill)</option>
          <option value="random">Random (random)</option>
        </select>
      </div>
      <div class="field"><label>Engine version</label><input type="text" id="settingVersion" disabled></div>
    </div>
    <div class="form-row">
      <div class="field"><label>Accounts file</label><input type="text" id="settingPoolPath" disabled></div>
    </div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M4 7h16M4 12h16M4 17h10"/></svg></span> Request headers (mimicking the Cline CLI)</div>
  <div class="section-body">
    <div class="table-wrap">
    <table>
      <thead><tr><th style="width:220px">Header</th><th>Value</th><th style="width:40px"></th></tr></thead>
      <tbody id="headersTableBody">
        <tr><td colspan="3" class="empty">Loading...</td></tr>
      </tbody>
    </table>
    </div>
    <div class="form-actions">
      <button class="btn btn-sm" onclick="addHeaderRow()">Add header</button>
      <button class="btn btn-sm btn-primary" onclick="saveHeaders()">Save headers</button>
    </div>
    <div class="hint">These headers are attached to every request forwarded to the Cline API to mimic the official client.</div>
    <div id="headerSaveResult" style="margin-top:8px"></div>
  </div>
</div>

<div class="section danger-zone">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/></svg></span> Danger zone</div>
  <div class="section-body">
    <div style="display:flex;gap:10px;flex-wrap:wrap">
      <button class="btn btn-danger" onclick="deleteAllAccounts()">Delete all accounts</button>
      <button class="btn btn-danger" onclick="deleteAllKeys()">Delete all keys</button>
    </div>
  </div>
</div>
</div>

<div id="tab-logs" class="tab-panel" style="display:none">
<div class="flex justify-between" style="margin-bottom:16px">
  <h2>Request logs <span class="probe-pill" style="font-weight:normal">last 500 entries, persisted to data/requests.jsonl</span></h2>
  <div style="display:flex;gap:8px;align-items:center">
    <span class="auto-pill" id="logsAutoPill">Auto-refreshing</span>
    <button class="btn btn-sm" onclick="toggleLogsAuto()" id="logsAutoBtn">Pause</button>
    <button class="btn btn-sm" onclick="loadLogs()">Refresh</button>
  </div>
</div>
<div class="section">
  <div class="section-body" style="padding:6px">
    <div class="table-wrap">
    <table>
      <thead>
        <tr><th>Time</th><th>Source</th><th>Method</th><th>Path</th><th>Model</th><th>Route</th><th>Status</th><th>Duration</th></tr>
      </thead>
      <tbody id="logsTableBody">
        <tr><td colspan="8" class="empty">Loading...</td></tr>
      </tbody>
    </table>
    </div>
  </div>
</div>
</div>

<div id="tab-combos" class="tab-panel" style="display:none">
<h2>Combos (custom alias models)</h2>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M12 5v14M5 12h14"/></svg></span> Create combo</div>
  <div class="section-body">
    <div class="hint" style="margin-bottom:10px">Clients request the alias ID as their model, and the proxy rewrites it to the target model for the chosen platform. Alias IDs are user-defined (e.g. <code>cline-glm-5.3</code>) and must not collide with real model IDs; the target must belong to the same platform — cross-platform targets are not allowed.</div>
    <div class="form-row">
      <div class="field"><label>Alias ID</label><input type="text" id="comboId" placeholder="cline-glm-5.3"></div>
      <div class="field"><label>Platform</label>
        <select id="comboPlatform" onchange="fillComboModels()"><option value="cline">cline</option><option value="zen">opencode zen</option></select>
      </div>
      <div class="field" style="flex:2"><label>Target model (same platform only)</label><select id="comboTarget"></select></div>
      <div class="field" style="flex:0 0 auto;display:flex;align-items:flex-end"><label style="display:flex;gap:6px;align-items:center;white-space:nowrap;padding-bottom:8px"><input type="checkbox" id="comboUseProxies"> Use proxy pool</label></div>
    </div>
    <button class="btn btn-primary" onclick="createCombo()">Create combo</button>
  </div>
</div>
<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M8 6h13M8 12h13M8 18h13"/><path d="M3 6h.01M3 12h.01M3 18h.01"/></svg></span> Existing combos</div>
  <div class="section-body" id="combosList">Loading...</div>
</div>
</div>

<div id="tab-opencode" class="tab-panel" style="display:none">
<h2>opencode free models (unified gateway)</h2>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14.5 14.5 0 0 1 0 18 14.5 14.5 0 0 1 0-18z"/></svg></span> Upstream config</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Enable opencode upstream</label>
        <select id="ocEnabled"><option value="true">On</option><option value="false">Off</option></select>
      </div>
      <div class="field" style="flex:2"><label>API keys / Console OAuth Tokens (one per line, supports <code>token#org_id</code> for multi-accounts)</label><textarea id="ocKeys" rows="3" placeholder="public"></textarea><small id="ocConsoleHint" style="color:var(--text2);display:block;margin-top:4px"></small></div>
    </div>
    <div class="flex" style="gap:10px;margin-bottom:8px;align-items:center;flex-wrap:wrap">
      <label class="hint" style="margin:0">Probe model (used by the Test buttons):</label>
      <select id="ocProbeModel" style="max-width:360px"><option value="">auto — big-pickle first, then live models</option></select>
    </div>
    <div class="table-wrap" style="margin-bottom:10px">
      <table>
        <thead><tr><th style="width:50px">#</th><th style="width:110px">Key</th><th style="width:70px">Usage</th><th style="width:110px">Session</th><th>Cooldown</th><th style="width:90px"></th></tr></thead>
        <tbody id="ocKeysBody"><tr><td colspan="6" class="empty">Loading...</td></tr></tbody>
      </table>
    </div>
    <div class="form-row">
      <div class="field"><label>Base URL</label><input type="text" id="ocBaseURL" placeholder="https://opencode.ai/zen/v1"></div>
      <div class="field">
        <label>Egress proxies</label>
        <div class="hint" style="margin-top:6px" id="ocProxyState">-</div>
        <div class="hint">Managed on the <a href="#" onclick="switchTab('proxypool');return false" style="color:var(--accent);cursor:pointer">Proxy pool</a> page — shared with the Cline upstream.</div>
      </div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">Save config</button></div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg></span> Rate-limit defense</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Max concurrency</label><input type="text" id="ocMaxConc" placeholder="8"></div>
      <div class="field"><label>Rate-limit retries</label><input type="text" id="ocRetries" placeholder="3"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>Failover</label>
        <select id="ocFailover"><option value="true">On (switch to cline pool)</option><option value="false">Off</option></select>
      </div>
      <div class="field"><label>Failure threshold</label><input type="text" id="ocFailoverCount" placeholder="3"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>Failover window (min)</label><input type="text" id="ocFailoverMinutes" placeholder="5"></div>
      <div class="field"><label>Current status</label><span id="ocFailoverInfo" class="stat-mini" style="align-self:center">-</span></div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">Save rate-limit config</button></div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M4 8V6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v2M4 8v10a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8M4 8h16"/><path d="M10 12h4"/></svg></span> Context compaction (opencode official mechanism)</div>
  <div class="section-body">
    <div class="form-row">
      <div class="field"><label>Auto-compaction</label>
        <select id="ocCompactAuto"><option value="true">On</option><option value="false">Off</option></select>
      </div>
      <div class="field"><label>Reserved buffer</label><input type="text" id="ocCompactBuffer" placeholder="20000"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>Tail tokens kept</label><input type="text" id="ocKeepTokens" placeholder="8000"></div>
      <div class="field"><label>Summary model</label><input type="text" id="ocSummaryModel" placeholder="empty = same as request model"></div>
    </div>
    <div class="form-row">
      <div class="field"><label>Summary cap</label><input type="text" id="ocMaxSummary" placeholder="4096"></div>
    </div>
    <div class="form-actions"><button class="btn btn-primary" onclick="saveOcConfig()">Save compaction config</button></div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg></span> Live session IDs (zen FreeTier gate)
    <span id="ocSessSummary" class="probe-pill" style="font-weight:normal;margin-left:auto"></span>
  </div>
  <div class="section-body">
    <p class="hint" style="margin-top:0" id="ocSessHint">The free tier only accepts session IDs the upstream has actually seen, minted by the opencode CLI. A key without a live session <b>always</b> fails with 403 — normally the harvester mints one on startup, on repeated 403s, and every few hours; use the buttons below to mint immediately (e.g. right after a fresh deploy with many keys).</p>
    <div class="flex" style="gap:10px;margin-bottom:10px;flex-wrap:wrap">
      <button class="btn btn-primary" id="ocSessBtnMissing" onclick="mintZenSessions(false)">Mint missing sessions</button>
      <button class="btn" id="ocSessBtnForce" onclick="mintZenSessions(true)">Force mint / refresh all</button>
      <span class="hint" style="margin:0" id="ocSessTimer"></span>
    </div>
    <div class="table-wrap">
      <table>
        <thead><tr><th style="width:70px">Key</th><th style="width:130px">Session</th><th style="width:150px">State</th><th>Last minted</th></tr></thead>
        <tbody id="ocSessBody"><tr><td colspan="4" class="empty">Loading...</td></tr></tbody>
      </table>
    </div>
    <div id="ocSessResult" style="margin-top:10px"></div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><rect x="2" y="7" width="20" height="14" rx="2"/><path d="M16 7V5a2 2 0 0 0-2-2h-4a2 2 0 0 0-2 2v2"/></svg></span> opencode model list
    <button class="btn btn-sm" onclick="refreshOcModels()" style="margin-left:auto">Sync now</button>
  </div>
  <div class="section-body" style="padding:6px">
    <div id="ocModelsList" style="padding:12px">Loading...</div>
  </div>
</div>

<div class="section">
  <div class="section-title"><span class="sec-ico"><svg viewBox="0 0 24 24"><path d="M3 3v18h18"/><path d="M7 15l4-4 3 3 5-6"/></svg></span> opencode stats</div>
  <div class="section-body">
    <div class="table-wrap"><div id="ocStatsBox"></div></div>
    <div id="ocModelStatsBox" style="margin-top:14px"></div>
  </div>
</div>
</div>

</div>
</div>

<div id="toast" class="toast"></div>

<script>
const API = '/admin/api';

// ========== Theme toggle ==========
function getTheme() {
  return localStorage.getItem('theme') || 'dark';
}
function applyTheme(t) {
  if (t === 'dark') {
    document.documentElement.setAttribute('data-theme', 'dark');
    const ic = document.getElementById('themeIcon');
    if (ic) ic.innerHTML = '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>';
  } else {
    document.documentElement.setAttribute('data-theme', 'light');
    const ic = document.getElementById('themeIcon');
    if (ic) ic.innerHTML = '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>';
  }
}
function toggleTheme() {
  const cur = getTheme();
  const next = cur === 'dark' ? 'light' : 'dark';
  localStorage.setItem('theme', next);
  applyTheme(next);
}
applyTheme(getTheme());

const _ = id => document.getElementById(id);
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmtNum = n => (n || 0).toLocaleString('en-US');
// fmtWhen 把服务器发来的 RFC3339 时刻渲染成浏览器的本地时间（同一时刻在不同
// 时区看到各自的钟点）。容器时区是 UTC，直接显示服务器格式化的读数会和本地
// 时间差一个时差；解析失败则原样返回，至少不丢信息。
// 零值时间（Go 的 time.Time{} = 0001-01-01T00:00:00Z）视为"没有这个时刻"：
// 它是 truthy，会被 new Date 解析成年份 1 并渲染成 "1/1/1, 12:00:00 AM"。
const fmtWhen = s => {
  if (!s || String(s).startsWith('0001-')) return '';
  const d = new Date(s);
  return isNaN(d.getTime()) ? s : d.toLocaleString('en-US');
};
const fmtTokens = n => {
  n = n || 0;
  if (n >= 1000000) return (n / 1000000).toFixed(2).replace(/\.?0+$/, '') + 'M';
  if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'K';
  return String(n);
};
if (window.location.host) {
  _('footerApiAddr').textContent = 'http://' + window.location.host;
  _('quickstartApi').textContent = 'http://' + window.location.host + '/v1';
}

function toast(msg, t, duration) {
  const el = _('toast');
  el.textContent = msg;
  el.style.whiteSpace = 'pre-line';
  el.className = 'toast ' + (t || 'info') + ' show';
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.classList.remove('show'), duration || 3500);
}

// ========== Navigation ==========
document.querySelectorAll('.nav-item').forEach(el => {
  el.addEventListener('click', () => {
    if (el.classList.contains('active')) return;
    document.querySelectorAll('.nav-item').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
    _('tab-' + el.dataset.tab).style.display = 'block';
    if (el.dataset.tab === 'dashboard') { loadStats(); loadAccounts(); }
    if (el.dataset.tab === 'accounts') loadAccounts();
    if (el.dataset.tab === 'settings') { loadKeys(); loadModels(); loadConfig(); }
    if (el.dataset.tab === 'logs') loadLogs();
    if (el.dataset.tab === 'proxypool') loadProxyPool();
    if (el.dataset.tab === 'opencode') { loadOcConfig(); loadOcModels(); loadOcStats(); loadOcSessions(); }
    if (el.dataset.tab === 'combos') { loadCombos(); fillComboModels(); }
  });
});

function switchTab(name) {
  document.querySelectorAll('.nav-item').forEach(e => {
    e.classList.toggle('active', e.dataset.tab === name);
  });
  document.querySelectorAll('.tab-panel').forEach(e => e.style.display = 'none');
  _('tab-' + name).style.display = 'block';
  if (name === 'dashboard') { loadStats(); loadAccounts(); }
  if (name === 'accounts') loadAccounts();
  if (name === 'settings') { loadKeys(); loadModels(); }
  if (name === 'logs') loadLogs();
  if (name === 'proxypool') loadProxyPool();
  if (name === 'opencode') { loadOcConfig(); loadOcModels(); loadOcStats(); loadOcSessions(); }
  if (name === 'combos') { loadCombos(); fillComboModels(); }
}

// Import sub-tabs
document.querySelectorAll('#importTabs .tab').forEach(el => {
  el.addEventListener('click', () => {
    document.querySelectorAll('#importTabs .tab').forEach(e => e.classList.remove('active'));
    el.classList.add('active');
    document.querySelectorAll('#import-oauth,#import-token,#import-batch').forEach(e => e.classList.remove('active'));
    _('import-' + el.dataset.tab).classList.add('active');
  });
});

// ========== API helper ==========
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(body); }
  const res = await fetch(API + path, opts);
  if (res.status === 401 && path !== '/login') { location.reload(); return new Promise(() => {}); }
  const data = await res.json();
  if (!data.success && data.error) throw new Error(data.error);
  return data;
}

// ========== Dashboard ==========
async function loadStats() {
  try {
    const d = await api('GET', '/stats');
    const s = d.data;
    _('statTotal').textContent = s.total;
    _('statActive').textContent = s.active;
    _('statCooldown').textContent = s.cooldown;
    _('statExpired').textContent = s.expired;
    if (s.version) _('settingVersion').value = s.version;
    // 策略下拉由 /config 加载填写（loadConfig），统计刷新不回写表单，
    // 避免把用户正在编辑的选项覆盖掉
  } catch (e) { /* ignore */ }
}

// ========== Accounts ==========
async function loadAccounts() {
  try {
    const d = await api('GET', '/accounts');
    const list = d.data.accounts;
    const tbody = _('accountTableBody');
    if (!list || list.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty">No accounts yet — add one on the <a href="#" onclick="switchTab(\'import\')" style="color:var(--accent);cursor:pointer">Add accounts</a> page</td></tr>';
      return;
    }
    const sn = { active: 'Active', cooldown: 'Cooldown', expired: 'Expired' };
    tbody.innerHTML = list.map(a => {
      const lu = a.lastUsed ? new Date(a.lastUsed).toLocaleString('en-US') : '-';
      const cr = a.createdAt ? new Date(a.createdAt).toLocaleString('en-US') : '-';
      // Cooldown label: show estimated recovery time
      let statusExtra = '';
      if (a.status === 'cooldown') {
        const until = a.cooldownUntil ? new Date(a.cooldownUntil).toLocaleString('en-US') : '';
        statusExtra = until ? '<div style="font-size:10px;color:var(--text3);margin-top:2px">Recovers ' + esc(until) + '</div>' : '';
      }
      const st = esc(a.status);
      return '<tr>' +
        '<td>' + esc(a.email) + '</td>' +
        '<td><span class="status ' + st + '"><span class="status-dot ' + st + '"></span>' + (sn[a.status] || st) + '</span>' + statusExtra + '</td>' +
          '<td title="Today ' + fmtNum(a.tokensToday) + ' / total ' + fmtNum(a.tokensTotal) + ' tokens (exact when upstream returns usage, otherwise estimated)">' + fmtTokens(a.tokensToday) + ' / ' + fmtTokens(a.tokensTotal) + '</td>' +
        '<td class="mono" style="font-size:11px">' + lu + '</td>' +
        '<td class="mono" style="font-size:11px">' + cr + '</td>' +
        '<td style="white-space:nowrap">' +
          // Action buttons dispatch via data-* attributes + delegated listeners:
          // inline onclick string concatenation gets HTML-decoded before JS parsing,
          // so even escaped quotes could be bypassed
          '<button class="btn btn-sm" data-act="test" data-acc="' + esc(a.accountId) + '" title="Test whether the account works (success clears cooldown/expired state)">Test</button> ' +
          '<button class="btn btn-sm" data-act="reset" data-acc="' + esc(a.accountId) + '" title="Probe the rate limit and lift it: probes upstream; if still limited, stays in cooldown and shows recovery time">Reset</button> ' +
          '<button class="btn btn-sm btn-danger" data-act="delete" data-acc="' + esc(a.accountId) + '" title="Delete">Delete</button>' +
        '</td></tr>';
    }).join('');
    tbody.onclick = e => {
      const b = e.target.closest('button[data-act]');
      if (!b) return;
      const id = b.dataset.acc;
      if (b.dataset.act === 'test') testAccount(id, b);
      else if (b.dataset.act === 'reset') resetAccount(id, b);
      else if (b.dataset.act === 'delete') deleteAccount(id);
    };
  } catch (e) { toast('Failed to load accounts: ' + e.message, 'error'); }
}

async function testAccount(id, btn) {
  const original = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span>Testing'; }
  try {
    const d = await api('POST', '/accounts/test', { accountId: id });
    const r = d.data || {};
    const statusMap = { active: 'Available', cooldown: 'Cooldown', expired: 'Expired', error: 'Error' };
    const label = statusMap[r.status] || r.status;
    const prevMap = { active: 'Active', cooldown: 'Cooldown', expired: 'Expired', '': '' };
    // toast 用 textContent 渲染，参数一律不要再 esc()：转义会以字面量显示
    //（429 的错误体就变成可见的 &quot;error&quot;:…）
    let msg = 'Account ' + (r.email || '') + ' — ' + label;
    if (r.prevStatus && r.prevStatus !== r.status) msg += ' (was: ' + (prevMap[r.prevStatus] || r.prevStatus) + ')';
    if (r.cooldownUntil) msg += '\nEstimated recovery: ' + fmtWhen(r.cooldownUntil);
    if (r.remaining) msg += ' (remaining ' + r.remaining + ')';
    if (r.reason) msg += '\nReason: ' + r.reason;
    if (r.httpStatus) msg += '\nHTTP: ' + r.httpStatus;
    const type = r.status === 'active' ? 'success' : (r.status === 'cooldown' ? 'warning' : 'error');
    toast(msg, type, 6000);
    loadAccounts(); loadStats();
  } catch (e) {
    toast('Test failed: ' + e.message, 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = original; }
  }
}

async function deleteAccount(id) {
  if (!confirm('Delete this account?')) return;
  try {
    await api('POST', '/accounts/delete', { accountId: id });
    toast('Account deleted', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('Delete failed: ' + e.message, 'error'); }
}

async function resetAccount(id, btn) {
  const original = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span>Checking'; }
  try {
    const d = await api('POST', '/accounts/reset', { accountId: id });
    const r = d.data || {};
    const type = d.success ? 'success' : (r.status === 'cooldown' ? 'warning' : 'error');
    let msg = d.message || 'Check complete';
    // 恢复时刻与剩余时长都由面板渲染：服务器发的是 RFC3339 时刻（浏览器本地
    // 时区显示），remaining 只在这里加一次，避免与服务器消息重复。
    // toast 走 textContent，不要再 esc()（会显示成字面量 &quot;）
    if (r.cooldownUntil && r.status === 'cooldown') msg += ' (estimated recovery ' + fmtWhen(r.cooldownUntil) + ')';
    if (r.remaining && r.status !== 'active') msg += ' (remaining ' + r.remaining + ')';
    toast(msg, type, 6000);
    loadAccounts(); loadStats();
  } catch (e) {
    toast('Check failed: ' + e.message, 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = original; }
  }
}

async function deleteAllAccounts() {
  if (!confirm('Delete ALL accounts? This cannot be undone!')) return;
  try {
    await api('POST', '/accounts/delete-all', {});
    toast('All accounts deleted', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('Delete failed: ' + e.message, 'error'); }
}

async function refreshAllTokens() {
  try {
    await api('POST', '/accounts/refresh-all', {});
    toast('All tokens refreshed', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('Refresh failed: ' + e.message, 'error'); }
}

// ========== OAuth login ==========
async function startOAuth() {
  const btn = _('oauthBtn');
  btn.disabled = true;
  btn.innerHTML = '<span class="loading"></span> Starting...';
  _('oauthProgress').style.display = 'block';
  _('oauthResult').style.display = 'none';
  _('oauthStatus').textContent = 'Connecting to WorkOS...';
  try {
    const d = await api('POST', '/oauth/start');
    const s = d.data;
    _('oauthStatus').textContent = 'Open the link in your browser and enter the code';
    const u = _('oauthUrl');
    const vuri = String(s.verificationUri || '');
    u.textContent = vuri;
    // 仅对 https 链接设置 href：非 https 的 verificationUri 只展示不可点，
    // 防止 javascript: 等异常 scheme 成为注入面
    if (/^https:\/\//i.test(vuri)) { u.href = vuri; } else { u.removeAttribute('href'); }
    _('oauthUserCode').textContent = s.userCode;
    let pollFails = 0;
    const poll = setInterval(async () => {
      try {
        const r = await api('GET', '/oauth/status?sessionId=' + s.sessionId);
        pollFails = 0;
        if (r.data && r.data.done) {
          clearInterval(poll);
          btn.disabled = false;
          btn.innerHTML = 'Start OAuth login';
          if (r.data.success) {
            _('oauthProgress').style.display = 'none';
            _('oauthResult').innerHTML = '<div style="color:var(--accent2);font-weight:600;font-size:14px">Account added: ' + esc(r.data.email) + '</div>';
            _('oauthResult').style.display = 'block';
            loadAccounts(); loadStats();
            toast('Account added!', 'success');
          } else {
            _('oauthStatus').textContent = 'Failed: ' + (r.data.error || 'unknown error');
            toast('OAuth failed', 'error');
          }
        }
      } catch(e) {
        // 连续失败(会话丢失/服务重启)后停止轮询并恢复按钮，避免永久卡死；
        // 偶发网络抖动在 5 次(约10s)内自愈
        if (++pollFails >= 5) {
          clearInterval(poll);
          btn.disabled = false;
          btn.innerHTML = 'Start OAuth login';
          _('oauthStatus').textContent = 'Polling failed: ' + (e && e.message ? e.message : 'session not found');
          toast('OAuth polling stopped', 'error');
        }
      }
    }, 2000);
  } catch (e) {
    btn.disabled = false;
    btn.innerHTML = 'Start OAuth login';
    _('oauthStatus').textContent = 'Error: ' + e.message;
    toast('OAuth failed: ' + e.message, 'error');
  }
}

// ========== Token import ==========
// Credential type auto-detection: "sk_..." = static Cline API key, anything
// else = OAuth refreshToken (the backend validates refresh tokens upstream;
// API keys are pooled as-is).
const credPayload = t => t.startsWith('sk_') ? { apiToken: t } : { refreshToken: t };

async function addByToken() {
  const token = _('tokenInput').value.trim();
  if (!token) { toast('Enter a token or API key', 'error'); return; }
  const email = _('tokenEmail').value.trim();
  try {
    const d = await api('POST', '/accounts/add', { ...credPayload(token), email: email || undefined });
    toast('Account added: ' + (d.data.email || ''), 'success');
    _('tokenInput').value = '';
    _('tokenEmail').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('Add failed: ' + e.message, 'error'); }
}

// ========== Batch import ==========
const parseBatchInput = raw => {
  try {
    let tokens = JSON.parse(raw);
    if (!Array.isArray(tokens)) tokens = [tokens];
    return tokens;
  } catch {
    return raw.split('\n').map(t => t.trim()).filter(Boolean).map(credPayload);
  }
};

async function batchImport() {
  const raw = _('batchInput').value.trim();
  if (!raw) { toast('Enter account data', 'error'); return; }
  const tokens = parseBatchInput(raw);
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || 'Import complete', 'success');
    _('batchInput').value = '';
    loadAccounts(); loadStats();
  } catch (e) { toast('Import failed: ' + e.message, 'error'); }
}

async function handleFileImport(event) {
  const file = event.target.files[0];
  if (!file) return;
  const text = await file.text();
  const tokens = parseBatchInput(text);
  try {
    const d = await api('POST', '/batch-import', { tokens });
    toast(d.message || 'Imported ' + tokens.length + ' accounts', 'success');
    loadAccounts(); loadStats();
  } catch (e) { toast('Import failed: ' + e.message, 'error'); }
  event.target.value = '';
}

// ========== API keys ==========
async function loadKeys() {
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys;
    const el = _('keysList');
    if (!keys || keys.length === 0) {
      el.innerHTML = '<div class="empty-state"><span class="icon"><svg viewBox="0 0 24 24" width="36" height="36" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.6 7.6a5.5 5.5 0 1 1-7.78 7.78 5.5 5.5 0 0 1 7.78-7.78zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg></span>No API keys yet</div>';
      return;
    }
    el.innerHTML = keys.map(k =>
      '<div class="flex" style="margin-bottom:8px">' +
        '<span class="key-display" style="flex:1" data-copy="' + esc(k) + '" title="Click to copy">' + esc(k) + '</span>' +
        '<button class="btn btn-sm btn-danger" data-delkey="' + esc(k) + '">Delete</button>' +
      '</div>'
    ).join('');
    el.onclick = e => {
      const c = e.target.closest('[data-copy]');
      if (c) { copyText(c.dataset.copy); return; }
      const d = e.target.closest('button[data-delkey]');
      if (d) deleteKey(d.dataset.delkey);
    };
  } catch (e) { _('keysList').innerHTML = '<div class="empty">Failed to load</div>'; }
}

async function generateKey() {
  try {
    const d = await api('POST', '/keys/generate');
    const key = d.data.key;
    _('keyGenResult').innerHTML =
      '<div style="background:rgba(52,211,153,.08);border:1px solid rgba(52,211,153,.4);border-radius:10px;padding:12px">' +
        '<div style="color:var(--accent2);font-weight:600;margin-bottom:8px">New key generated (click to copy)</div>' +
        '<div class="key-display" data-copy="' + esc(key) + '">' + esc(key) + '</div>' +
      '</div>';
    const kr = _('keyGenResult').querySelector('[data-copy]');
    if (kr) kr.onclick = () => copyText(kr.dataset.copy);
    loadKeys();
    toast('Key generated', 'success');
    setTimeout(() => _('keyGenResult').innerHTML = '', 8000);
  } catch (e) { toast('Generation failed: ' + e.message, 'error'); }
}

async function deleteKey(key) {
  if (!confirm('Delete this key?')) return;
  try {
    await api('POST', '/keys/delete', { key });
    toast('Key deleted', 'success');
    loadKeys();
  } catch (e) { toast('Delete failed: ' + e.message, 'error'); }
}

async function deleteAllKeys() {
  if (!confirm('Delete ALL API keys?')) return;
  try {
    const d = await api('GET', '/keys');
    const keys = d.data.keys || [];
    for (const k of keys) await api('POST', '/keys/delete', { key: k });
    toast('All keys deleted', 'success');
    loadKeys();
  } catch (e) { toast('Delete failed: ' + e.message, 'error'); }
}

function copyText(t) {
  navigator.clipboard.writeText(t).then(() => toast('Copied to clipboard', 'success')).catch(() => {
    const ta = document.createElement('textarea');
    ta.value = t; document.body.appendChild(ta); ta.select(); document.execCommand('copy'); document.body.removeChild(ta);
    toast('Copied to clipboard', 'success');
  });
}

// ========== Request logs ==========
const ROUTE_LABEL = { zen: 'opencode', cline: 'cline pool', admin: 'admin', meta: 'meta', other: 'other' };
const STATUS_CLASS = s => s >= 500 ? 'color:var(--danger)' : (s >= 400 ? 'color:var(--amber)' : 'color:var(--accent2)');
let logsAuto = true;

function toggleLogsAuto() {
  logsAuto = !logsAuto;
  _('logsAutoBtn').textContent = logsAuto ? 'Pause' : 'Resume';
  const pill = _('logsAutoPill');
  pill.textContent = logsAuto ? 'Auto-refreshing' : 'Paused';
  pill.classList.toggle('paused', !logsAuto);
  if (logsAuto) loadLogs();
}

async function loadLogs() {
  const tbody = _('logsTableBody');
  try {
    const d = await api('GET', '/logs');
    const logs = d.data.logs || [];
    if (!logs.length) { tbody.innerHTML = '<tr><td colspan="8" class="empty">No requests logged</td></tr>'; return; }
    tbody.innerHTML = logs.map(l => {
      const t = l.time ? new Date(l.time).toLocaleString('en-US') : '-';
      const route = ROUTE_LABEL[l.route] || l.route || '-';
      const st = l.status || 0;
      return '<tr>' +
        '<td class="mono" style="font-size:11px">' + t + '</td>' +
        '<td class="mono" style="font-size:11px">' + esc(l.client || '-') + '</td>' +
        '<td>' + esc(l.method || '-') + '</td>' +
        '<td class="mono" style="font-size:11px">' + esc(l.path || '-') + '</td>' +
        '<td class="mono" style="font-size:12px">' + esc(l.model || '-') + '</td>' +
        '<td><span class="model-tag">' + esc(route) + '</span></td>' +
        '<td style="font-weight:600;color:' + STATUS_CLASS(st) + '">' + st + '</td>' +
        '<td class="mono" style="font-size:11px">' + (l.duration_ms != null ? l.duration_ms + ' ms' : '-') + '</td>' +
      '</tr>';
    }).join('');
  } catch (e) { tbody.innerHTML = '<tr><td colspan="8" class="empty">Failed to load</td></tr>'; }
}

// ========== Export accounts ==========
async function exportAccounts() {
  try {
    const res = await fetch(API + '/accounts/export');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = 'cline-accounts-export.json';
    document.body.appendChild(a); a.click(); document.body.removeChild(a);
    URL.revokeObjectURL(url);
    toast('Accounts exported (JSON)', 'success');
  } catch (e) { toast('Export failed: ' + e.message, 'error'); }
}

// ========== Config updates ==========
async function updateConfig() {
  const strategy = _('settingStrategy').value;
  try {
    await api('POST', '/config/update', { strategy });
    toast('Strategy updated to: ' + strategy, 'success');
  } catch (e) { toast('Update failed: ' + e.message, 'error'); }
}

function addHeaderRow() {
  const tbody = _('headersTableBody');
  const tr = document.createElement('tr');
  tr.innerHTML =
    '<td><input type="text" class="header-key" placeholder="Header-Name" style="font-size:12px;font-family:monospace"></td>' +
    '<td><input type="text" class="header-val" placeholder="value" style="font-size:12px;font-family:monospace"></td>' +
    '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">Delete</button></td>';
  tbody.appendChild(tr);
}

async function saveHeaders() {
  const tbody = _('headersTableBody');
  const rows = tbody.querySelectorAll('tr');
  const headers = {};
  let hasEmpty = false;
  rows.forEach(tr => {
    const keyInput = tr.querySelector('.header-key');
    const valInput = tr.querySelector('.header-val');
    if (keyInput && valInput) {
      const k = keyInput.value.trim();
      const v = valInput.value.trim();
      if (k) { headers[k] = v; }
      else if (v) { hasEmpty = true; }
    }
  });
  if (hasEmpty) { toast('Rows with a value but no key were ignored', 'info'); }
  try {
    const d = await api('POST', '/config/update', { headers });
    toast('Headers saved', 'success');
    _('headerSaveResult').innerHTML =
      '<div style="color:var(--accent2);font-size:12px">Saved ' + Object.keys(d.data.headers).length + ' headers</div>';
    setTimeout(() => _('headerSaveResult').innerHTML = '', 5000);
    loadConfig();
  } catch (e) { toast('Save failed: ' + e.message, 'error'); }
}

const MODEL_STYLE = {
  active:  { label: 'available', css: 'color:var(--accent2);border:1px solid rgba(52,211,153,.5);background:rgba(52,211,153,.08)' },
  empty:   { label: 'empty response', css: 'color:var(--amber);border:1px solid rgba(245,158,11,.5);background:rgba(245,158,11,.08)' },
  pass:    { label: 'subscription', css: 'color:var(--amber);border:1px solid rgba(245,158,11,.5);background:rgba(245,158,11,.08)' },
  removed: { label: 'removed', css: 'color:var(--text3);border:1px solid var(--border)' },
  error:   { label: 'error', css: 'color:var(--danger);border:1px solid rgba(248,113,113,.5);background:rgba(248,113,113,.08)' },
  unknown: { label: 'unprobed', css: 'color:var(--text3);border:1px dashed var(--border-strong)' }
};
const COST_LABEL = { free: 'free', pass: 'subscription', quota: 'uses quota' };

async function loadModels() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    let info = '';
    if (d.data.lastSync) info += '· official feed: ' + new Date(d.data.lastSync).toLocaleTimeString('en-US');
    _('modelsProbeInfo').textContent = info;
    if (!models.length) { _('modelsList').innerHTML = '<div class="empty">No models</div>'; return; }
    _('modelsList').innerHTML = models.map(m => {
      const st = MODEL_STYLE[m.status] || MODEL_STYLE.unknown;
      const cost = COST_LABEL[m.cost] || m.cost || '';
      const synced = m.syncedAt ? new Date(m.syncedAt).toLocaleTimeString('en-US') : '-';
      return '<div style="display:flex;align-items:center;gap:10px;padding:8px 12px;margin:5px 0;background:rgba(148,163,184,.06);border:1px solid var(--border);border-radius:10px;transition:.15s">' +
        '<span style="font-family:\'JetBrains Mono\',monospace;font-size:13px;flex:1">' + esc(m.id) + '</span>' +
        (m.cost === 'free' ? '<span style="font-size:11px;color:var(--accent2)">no charge</span>' : '') +
        (cost ? '<span class="model-tag">' + esc(cost) + '</span>' : '') +
        '<span class="model-tag" style="' + st.css + '">' + st.label + '</span>' +
        '<span style="font-size:11px;color:var(--text3);min-width:60px;text-align:right">' + synced + '</span>' +
        '</div>';
    }).join('');
  } catch (e) { _('modelsList').textContent = 'Failed to load'; }
}

async function refreshModels() {
  try {
    _('modelsProbeInfo').textContent = '· syncing...';
    const d = await api('POST', '/models/refresh');
    toast(d.message || (d.data && d.data.message) || 'Sync started', 'info');
    setTimeout(loadModels, 3000);
  } catch (e) { toast('Refresh failed: ' + e.message, 'error'); _('modelsProbeInfo').textContent = ''; }
}

async function loadModelOptions() {
  try {
    const d = await api('GET', '/models');
    const models = d.data.models || [];
    const sel = _('settingDefModel');
    if (!sel) return;
    sel.innerHTML = models.map(m => {
      const st = MODEL_STYLE[m.status] || MODEL_STYLE.unknown;
      return '<option value="' + esc(m.id) + '">' + esc(m.id) + ' (' + st.label + ')</option>';
    }).join('');
    const c = await api('GET', '/config');
    if (c.data.defaultModel) sel.value = c.data.defaultModel;
    if (!sel.value && models.length) sel.value = models[0].id;
  } catch (e) { /* ignore */ }
}

async function saveDefaultModel() {
  const v = _('settingDefModel').value;
  if (!v) { toast('Select a model', 'error'); return; }
  try {
    const d = await api('POST', '/config/update', { defaultModel: v });
    toast('Default model saved: ' + d.data.defaultModel, 'success');
  } catch (e) { toast('Save failed: ' + e.message, 'error'); }
}

// ========== Config load ==========
async function loadConfig() {
  try {
    const d = await api('GET', '/config');
    const c = d.data;
    if (c.address) _('settingAddr').value = c.address;
    if (c.strategy) _('settingStrategy').value = c.strategy;
    if (c.version) _('settingVersion').value = c.version;
    if (c.poolPath) _('settingPoolPath').value = c.poolPath;
    loadModelOptions();
    if (c.headers) {
      const tbody = _('headersTableBody');
      tbody.innerHTML = Object.entries(c.headers).map(([k, v]) =>
        '<tr>' +
          '<td><input type="text" class="header-key" value="' + esc(k) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><input type="text" class="header-val" value="' + esc(v) + '" style="font-size:12px;font-family:monospace;width:100%"></td>' +
          '<td><button class="btn btn-sm btn-danger" onclick="this.closest(\'tr\').remove()">Delete</button></td>' +
        '</tr>'
      ).join('');
    }
  } catch (e) { /* ignore */ }
}

// ========== opencode free models ==========
// Last-seen zen config; the proxy pool fields live on the Proxy pool page now,
// so saves must carry the cached values instead of removed form inputs.
let ocCfgCache = {};

async function loadOcConfig() {
  try {
    const d = await api('GET', '/opencode/config');
    const c = d.data;
    ocCfgCache = c;
    _('ocEnabled').value = String(c.enabled);
    _('ocKeys').value = (c.keys && c.keys.length ? c.keys : [c.key || 'public']).join('\n');
    const ks = c.keyStates || [];
    renderOcKeyStates(ks);
    _('ocBaseURL').value = c.baseURL || '';
    _('ocProxyState').textContent = (c.proxies && c.proxies.length)
      ? c.proxies.length + ' prox' + (c.proxies.length === 1 ? 'y' : 'ies') + ' configured (' + (c.proxyStrategy || 'round_robin') + ')'
      : 'none configured';
    _('ocMaxConc').value = c.maxConcurrency || 8;
    _('ocRetries').value = c.retries || 3;
    _('ocFailover').value = String(c.failover);
    _('ocFailoverCount').value = c.failoverCount || 3;
    _('ocFailoverMinutes').value = c.failoverMinutes || 5;
    _('ocCompactAuto').value = String(c.compaction ? c.compaction.auto : true);
    _('ocCompactBuffer').value = c.compaction ? c.compaction.buffer : 20000;
    _('ocKeepTokens').value = c.compaction ? c.compaction.keepTokens : 8000;
    _('ocSummaryModel').value = c.compaction ? (c.compaction.summaryModel || '') : '';
    _('ocMaxSummary').value = c.compaction ? c.compaction.maxSummary : 4096;
    const rt = c.runtime || {};
    _('ocFailoverInfo').innerHTML = rt.failoverActive
      ? '<span style="color:var(--danger)">Failover active (opencode unavailable, requests go to the cline pool)</span>'
      : '<span style="color:var(--accent2)">Normal</span>';
    const hint = _('ocConsoleHint');
    if (hint) {
      if (c.consoleAuth && c.consoleAuth.available) {
        hint.innerHTML = '<span style="color:var(--accent2)">✓ Local OpenCode Console login detected</span> (' + esc(c.consoleAuth.tokenMask) + (c.consoleAuth.orgID ? ' · org: ' + esc(c.consoleAuth.orgID) : '') + '). Active if keys is "public", or enter multiple tokens above (format: <code>token#org_id</code>) to pool accounts.';
      } else {
        hint.textContent = 'Supports legacy Zen keys (oc_sk_...) and Console OAuth tokens (st_...#org_id).';
      }
    }
  } catch (e) { /* ignore */ }
}

async function saveOcConfig() {
  const keys = _('ocKeys').value.split('\n').map(s => s.trim()).filter(Boolean);
  if (!keys.length) { toast('API keys must not be empty (use "public" if you have no key)', 'error'); return; }
  const body = {
    enabled: _('ocEnabled').value === 'true',
    keys: keys,
    baseURL: _('ocBaseURL').value.trim(),
    proxies: ocCfgCache.proxies || [],
    proxyStrategy: ocCfgCache.proxyStrategy || 'round_robin',
    maxConcurrency: parseInt(_('ocMaxConc').value) || 8,
    retries: parseInt(_('ocRetries').value) || 3,
    failover: _('ocFailover').value === 'true',
    failoverCount: parseInt(_('ocFailoverCount').value) || 3,
    failoverMinutes: parseInt(_('ocFailoverMinutes').value) || 5,
    compaction: {
      auto: _('ocCompactAuto').value === 'true',
      buffer: parseInt(_('ocCompactBuffer').value) || 20000,
      keepTokens: parseInt(_('ocKeepTokens').value) || 8000,
      summaryModel: _('ocSummaryModel').value.trim(),
      maxSummary: parseInt(_('ocMaxSummary').value) || 4096
    }
  };
  try {
    const d = await api('POST', '/opencode/config/update', body);
    toast('opencode config saved', 'success');
    loadOcConfig();
  } catch (e) { toast('Save failed: ' + e.message, 'error'); }
}

// per-key 状态表：key 掩码 / 用量 / 会话 / 冷却（含预计恢复时刻）/ Test 按钮。
// Test 与 cline 账号的同语义：真实探测，成功即复位该 key 的冷却。
function renderOcKeyStates(ks) {
  const tb = _('ocKeysBody');
  if (!tb) return;
  tb.innerHTML = ks.length
    ? ks.map(k => {
        const st = k.sessionLive
          ? '<span style="color:var(--accent2)">live</span>'
          : (k.sessionMinted ? '<span style="color:var(--danger)">stale</span>' : '<span style="color:var(--danger)">not minted</span>');
        const cool = k.cooling
          ? '<span style="color:var(--danger)">cooling' + (k.cooldownUntil ? ' · until ' + esc(fmtWhen(k.cooldownUntil)) : '') + '</span>'
          : '<span style="color:var(--text2)">-</span>';
        return '<tr><td>#' + (k.index + 1) + (k.current ? ' <span style="color:var(--accent)" title="next in rotation">●</span>' : '') + '</td>' +
          '<td style="font-family:monospace;font-size:11px">' + esc(k.keyMask) + '</td>' +
          '<td>' + (k.usage || 0) + '</td>' +
          '<td>' + st + '</td>' +
          '<td>' + cool + '</td>' +
          '<td>' + (k.keyMask === 'public (no key)' ? '' : '<button class="btn btn-sm" data-zk="' + k.index + '">Test</button>') + '</td></tr>';
      }).join('')
    : '<tr><td colspan="6" class="empty">No zen keys configured</td></tr>';
  tb.onclick = e => {
    const b = e.target.closest('button[data-zk]');
    if (b) testZenKey(parseInt(b.dataset.zk, 10), b);
  };
}

async function testZenKey(index, btn) {
  const original = btn ? btn.innerHTML : '';
  if (btn) { btn.disabled = true; btn.innerHTML = '<span class="loading"></span>Testing'; }
  try {
    const d = await api('POST', '/zen/keys/test', {
      index: index,
      // 下拉里的探测模型；空串 = 后端自动选（big-pickle → live → 种子）
      model: (_('ocProbeModel') ? _('ocProbeModel').value : '')
    });
    const r = d.data || {};
    const label = { active: 'OK', cooldown: 'Cooldown', error: 'Error' }[r.status] || r.status;
    let msg = 'Key #' + (index + 1) + ' (' + (r.keyMask || '') + ') — ' + label;
    if (r.model) msg += ' via ' + r.model;
    if (r.latencyMs != null) msg += ' (' + r.latencyMs + 'ms)';
    if (r.status === 'active') msg += ' — cooldown cleared';
    if (r.cooldownUntil) msg += '\nEstimated recovery: ' + fmtWhen(r.cooldownUntil) + (r.remaining ? ' (remaining ' + r.remaining + ')' : '');
    if (r.reason && r.reason !== 'ok') msg += '\n' + r.reason;
    const type = r.status === 'active' ? 'success' : (r.status === 'cooldown' ? 'warning' : 'error');
    toast(msg, type, 6000);
  } catch (e) {
    toast('Test failed: ' + e.message, 'error');
  } finally {
    if (btn) { btn.disabled = false; btn.innerHTML = original; }
    loadOcConfig();  // 冷却被清除/新设置，立即刷新状态表
  }
}

// ========== Proxy pool ==========
async function loadProxyPool() {
  try {
    const [oc, cfg] = await Promise.all([api('GET', '/opencode/config'), api('GET', '/config')]);
    const c = oc.data;
    _('ppProxies').value = (c.proxies || []).join('\n');
    _('ppStrategy').value = c.proxyStrategy || 'round_robin';
    _('ppCline').value = String(!!cfg.data.clineUseProxies);
    _('ppZenState').innerHTML = c.enabled
      ? ((c.proxies && c.proxies.length)
          ? '<span style="color:var(--accent2)">routing through the pool</span>'
          : '<span style="color:var(--text2)">direct (pool list is empty)</span>')
      : '<span style="color:var(--text2)">upstream disabled</span>';
    const cd = (c.runtime || {}).proxyCooldowns || {};
    const keys = Object.keys(cd);
    _('ppCooldownInfo').textContent = keys.length
      ? keys.map(k => k + ' cooldown until ' + fmtWhen(cd[k])).join('; ')
      : 'No proxies cooling down';
  } catch (e) { /* ignore */ }
}

async function saveProxyPool() {
  const proxies = _('ppProxies').value.split('\n').map(s => s.trim()).filter(Boolean);
  const PROXY_RE = /^(https?|socks5h?):\/\/[^\s]+:\d+/;
  const bad = proxies.find(p => !PROXY_RE.test(p));
  if (bad) { toast('Invalid proxy format: ' + bad + ' (need http(s)://host:port or socks5://host:port)', 'error'); return; }
  try {
    await api('POST', '/opencode/config/update', { proxies: proxies, proxyStrategy: _('ppStrategy').value });
    _('ppSaveResult').innerHTML = '<div style="color:var(--accent2);font-size:12px">Saved ' + proxies.length + ' proxies</div>';
    setTimeout(() => _('ppSaveResult').innerHTML = '', 5000);
    toast('Proxy pool saved', 'success');
    loadProxyPool();
  } catch (e) { toast('Save failed: ' + e.message, 'error'); }
}

async function saveClineProxies() {
  try {
    await api('POST', '/config/update', { clineUseProxies: _('ppCline').value === 'true' });
    toast('Cline proxy setting saved', 'success');
    loadProxyPool();
  } catch (e) { toast('Save failed: ' + e.message, 'error'); }
}

async function loadOcModels() {
  try {
    const d = await api('GET', '/opencode/models');
    const models = d.data.models || [];
    _('ocModelsList').innerHTML = '<div class="table-wrap"><table><thead><tr><th style="text-align:left">Model ID</th><th>Context</th><th>Output</th><th>Tools</th><th>Reason</th><th>Attach</th><th>Endpoint</th><th>Source</th></tr></thead><tbody>' +
      models.map(m => '<tr><td style="text-align:left;font-family:monospace">' + esc(m.id) + '</td><td>' + esc(m.context) + '</td><td>' + esc(m.output) + '</td><td>' + (m.toolCall ? '✓' : '-') + '</td><td>' + (m.reasoning ? '✓' : '-') + '</td><td>' + (m.attach ? '✓' : '-') + '</td><td style="font-family:monospace;font-size:11px">' + esc(m.upstream === 'responses' ? 'responses' : 'chat') + '</td><td>' + esc(m.source) + '</td></tr>').join('') +
      '</tbody></table></div><div class="hint">' + models.length + ' free models total (auto-synced every 10 minutes from public registry)</div>';
    // 探测模型下拉：与模型表同一份数据；重渲染时保留用户当前的选择。
    const sel = _('ocProbeModel');
    if (sel) {
      const prev = sel.value;
      sel.innerHTML = '<option value="">auto — big-pickle first, then live models</option>' +
        models.map(m => '<option value="' + esc(m.id) + '">' + esc(m.id) + (m.source === 'live' ? '' : ' (seed)') + '</option>').join('');
      if (Array.from(sel.options).some(o => o.value === prev)) sel.value = prev;
    }
  } catch (e) { _('ocModelsList').textContent = 'Failed to load'; }
}

// ========== Live session IDs (zen FreeTier gate) ==========
// 未 mint 的 key 必 403；表格让"哪些 key 还是空会话"一眼可见，按钮给手动补收口。
let ocSessPoll = null;

async function loadOcSessions() {
  try {
    const d = await api('GET', '/opencode/sessions');
    renderOcSessions(d.data);
    return d.data;
  } catch (e) { return null; }
}

function renderOcSessions(s) {
  const rows = s.sessions || [];
  _('ocSessSummary').innerHTML = rows.length
    ? '<span style="color:' + (s.liveCount === s.total ? 'var(--accent2)' : 'var(--danger)') + '">' +
      s.liveCount + '/' + s.total + ' live</span>'
    : '';
  // 刷新节奏与并发写进提示行：用户据此判断"自动维护是否够用"，以及
  // 调 ZEN_HARVEST_INTERVAL_HOURS / ZEN_HARVEST_CONCURRENCY 要不要改。
  const h = _('ocSessHint');
  if (h) {
    const base = 'The free tier only accepts session IDs the upstream has actually seen, minted by the opencode CLI. A key without a live session <b>always</b> fails with 403.';
    h.innerHTML = base + (s.harvestEnabled
      ? ' Auto-refresh every <b>' + (s.intervalHours || 4) + 'h</b> (kept below the 5h quota window), minting <b>' +
        (s.concurrency || 1) + '</b> key(s) at a time' +
        ((s.concurrency || 1) === 1 ? ' (one after another — safest on small instances, the CLI is CPU/RAM hungry)' : '') + '.'
      : ' <b>Harvester unavailable</b> — no opencode CLI in this container (ZEN_HARVEST_BIN).');
  }
  // CLI 不在时后端会以 400 拒绝 mint，按钮先禁用，避免点了才发现。
  const usable = !!s.harvestEnabled;
  ['ocSessBtnMissing', 'ocSessBtnForce'].forEach(id => {
    const b = _(id);
    if (b) { b.disabled = !usable; b.title = usable ? '' : 'opencode CLI not available in this container'; }
  });
  _('ocSessBody').innerHTML = rows.length
    ? rows.map(k => {
        const state = k.noKey
          ? '<span style="color:var(--text2)">no key</span>'
          : (k.live
              ? '<span style="color:var(--accent2)">live</span>'
              : (k.minted ? '<span style="color:var(--danger)">stale</span>' : '<span style="color:var(--danger)">not minted</span>'));
        return '<tr><td>#' + (k.index + 1) + '</td>' +
          '<td style="font-family:monospace;font-size:11px">' + (k.session ? esc(k.session) + '…' : '-') + '</td>' +
          '<td>' + state + '</td>' +
          '<td style="font-size:12px">' + (k.harvested ? esc(fmtWhen(k.harvested)) : '-') + '</td></tr>';
      }).join('')
    : '<tr><td colspan="4" class="empty">No zen keys configured</td></tr>';

  const j = s.job;
  if (!j) { _('ocSessTimer').textContent = ''; return; }
  if (j.running) {
    _('ocSessTimer').innerHTML = '<span style="color:var(--accent)">minting ' + (j.done || 0) + '/' + (j.total || 0) + '…</span>';
    renderOcMintResults(j.results || [], true);
  } else {
    _('ocSessTimer').textContent = 'last mint ' + fmtWhen(j.startedAt) + (j.force ? ' (force)' : '');
    renderOcMintResults(j.results || [], false);
  }
}

function renderOcMintResults(results, running) {
  if (!results.length) { _('ocSessResult').innerHTML = ''; return; }
  const ok = results.filter(r => r.ok).length;
  const skipped = results.filter(r => r.skipped).length;
  const fail = results.filter(r => r.done && !r.ok && !r.skipped).length;
  const pending = results.filter(r => !r.done).length;
  const head = running
    ? '<span style="color:var(--accent)">Running — ' + ok + ' minted, ' + fail + ' failed, ' + pending + ' pending</span>'
    : (fail
        ? '<span style="color:var(--danger)">Done — ' + ok + ' minted, ' + fail + ' failed' + (skipped ? ', ' + skipped + ' skipped (already live)' : '') + '</span>'
        : '<span style="color:var(--accent2)">Done — ' + ok + ' minted' + (skipped ? ', ' + skipped + ' already live' : '') + '</span>');
  const details = results.filter(r => r.done && !r.ok && !r.skipped)
    .map(r => '#' + (r.index + 1) + ': ' + esc(r.error || 'failed')).join(' | ');
  _('ocSessResult').innerHTML = '<div style="font-size:12px">' + head + '</div>' +
    (details ? '<div class="hint" style="margin-top:4px">' + details + '</div>' : '');
}

async function mintZenSessions(force) {
  try {
    await api('POST', '/opencode/sessions/mint', { force: force });
    toast(force ? 'Force minting all sessions…' : 'Minting missing sessions…', 'success');
    // 任务在后端跑（串行 mint 时一批是 key 数 × 每 key 预算），按 2s 轮询进度。
    // 上限按后端批次预算推算：并发默认为 1 后 11 个 key 的批次上限可达几十分钟，
    // 写死 450 次（15 分钟）会让轮询提前停掉、进度条卡住不再更新。
    if (ocSessPoll) { clearInterval(ocSessPoll); ocSessPoll = null; }
    let ticks = 0;
    const s0 = await loadOcSessions();
    // 提前返回前必须把 ocSessPoll 置空：可见标签页的 20s 轮询用 !ocSessPoll
    // 判断"是否已有轮询在跑"，留一个已 clear 的非空句柄会让它永久停摆。
    if (!s0 || !s0.job || !s0.job.running) { ocSessPoll = null; return; }
    const j0 = s0.job;
    const workers = Math.max(1, s0.concurrency || 1);
    const perKey = s0.keyTimeoutSeconds || 150;
    // 下限 15 分钟与后端 harvestBatchTimeout 的下限对齐：key 少时后端照样跑满
    // 15 分钟，而 ceil(total/workers)×perKey 只有几分钟，不夹下限轮询会在后端
    // 放弃之前就停掉（之后只剩 20s 的可见标签页轮询，进度更新变粗）。
    const batchSeconds = Math.max(900, Math.ceil((j0.total || 1) / workers) * perKey + 120);
    const maxTicks = Math.min(1800, Math.ceil(batchSeconds / 2) + 30);
    ocSessPoll = setInterval(async () => {
      const s = await loadOcSessions();
      if (!s || !s.job || !s.job.running || ++ticks >= maxTicks) {
        clearInterval(ocSessPoll); ocSessPoll = null; loadOcConfig();
      }
    }, 2000);
  } catch (e) { toast('Mint failed: ' + e.message, 'error'); }
}

// ========== Combos (alias models) ==========
const comboModels = { cline: [], zen: [] };

async function fillComboModels() {
  const platform = _('comboPlatform').value;
  const sel = _('comboTarget');
  try {
    if (!comboModels[platform].length) {
      if (platform === 'cline') {
        const d = await api('GET', '/models');
        comboModels.cline = (d.data.models || []).map(m => m.id).filter(Boolean);
      } else {
        const d = await api('GET', '/opencode/models');
        comboModels.zen = (d.data.models || []).map(m => m.id).filter(Boolean);
      }
    }
    if (!comboModels[platform].length) { sel.innerHTML = '<option value="">No models on this platform</option>'; return; }
    sel.innerHTML = comboModels[platform].map(id => '<option value="' + esc(id) + '">' + esc(id) + '</option>').join('');
  } catch (e) { sel.innerHTML = '<option value="">Failed to load model list</option>'; }
}

async function loadCombos() {
  try {
    const d = await api('GET', '/combos');
    const combos = d.data.combos || [];
    if (!combos.length) { _('combosList').innerHTML = '<div class="hint">No combos yet — create one with the form above.</div>'; return; }
    _('combosList').innerHTML = '<div class="table-wrap"><table><thead><tr><th style="text-align:left">Alias ID</th><th>Platform</th><th style="text-align:left">Target model</th><th>Proxy</th><th>Created</th><th>Actions</th></tr></thead><tbody>' +
      combos.map(c => '<tr>' +
        '<td style="text-align:left;font-family:monospace;font-weight:600">' + esc(c.id) + '</td>' +
        '<td><span class="model-tag">' + esc(c.platform === 'zen' ? 'opencode-zen' : 'cline') + '</span></td>' +
        '<td style="text-align:left;font-family:monospace">' + esc(c.target) + '</td>' +
        '<td>' + (c.useProxies ? '<span class="model-tag" style="color:var(--accent2)">socks5 pool</span>' : '-') + '</td>' +
        '<td style="font-size:11px">' + (c.createdAt ? new Date(c.createdAt).toLocaleString('en-US') : '-') + '</td>' +
        '<td><button class="btn btn-sm btn-danger" data-delcombo="' + esc(c.id) + '">Delete</button></td>' +
      '</tr>').join('') +
      '</tbody></table></div>';
    _('combosList').onclick = e => {
      const b = e.target.closest('button[data-delcombo]');
      if (b) deleteCombo(b.dataset.delcombo);
    };
  } catch (e) { _('combosList').textContent = 'Failed to load: ' + e.message; }
}

async function createCombo() {
  const id = _('comboId').value.trim();
  const platform = _('comboPlatform').value;
  const target = _('comboTarget').value;
  if (!id) { toast('Enter an alias ID', 'error'); return; }
  if (!target) { toast('Select a target model', 'error'); return; }
  try {
    await api('POST', '/combos/create', { id, platform, target, useProxies: _('comboUseProxies').checked });
    toast('Combo created: ' + id + ' → ' + target, 'success');
    _('comboId').value = '';
    _('comboUseProxies').checked = false;
    loadCombos();
  } catch (e) { toast('Create failed: ' + e.message, 'error'); }
}

async function deleteCombo(id) {
  if (!confirm('Delete combo ' + id + '? Clients using this alias will no longer be able to call it.')) return;
  try {
    await api('POST', '/combos/delete', { id });
    toast('Combo deleted', 'success');
    loadCombos();
  } catch (e) { toast('Delete failed: ' + e.message, 'error'); }
}

async function refreshOcModels() {
  try {
    const d = await api('POST', '/opencode/models/refresh');
    toast(d.message || 'Sync complete', 'success');
    loadOcModels();
  } catch (e) { toast('Sync failed: ' + e.message, 'error'); }
}

async function loadOcStats() {
  try {
    const d = await api('GET', '/opencode/stats');
    const t = d.data.today || {}, s = d.data.total || {};
    _('ocStatsBox').innerHTML = '<table><thead><tr><th style="text-align:left"></th><th>Requests</th><th>Input tokens</th><th>Output tokens</th><th>Compaction</th><th>Rate-limited</th></tr></thead><tbody>' +
      '<tr><td style="text-align:left">Today</td><td>' + (t.requests || 0) + '</td><td>' + (t.promptTokens || 0) + '</td><td>' + (t.completionTokens || 0) + '</td><td>' + (t.compaction || 0) + '</td><td>' + (t.rateLimited || 0) + '</td></tr>' +
      '<tr><td style="text-align:left">Total</td><td>' + (s.requests || 0) + '</td><td>' + (s.promptTokens || 0) + '</td><td>' + (s.completionTokens || 0) + '</td><td>' + (s.compaction || 0) + '</td><td>' + (s.rateLimited || 0) + '</td></tr>' +
      '</tbody></table>';
    const bm = t.byModel || {};
    const rows = Object.keys(bm).map(k => '<tr><td style="text-align:left;font-family:monospace">' + esc(k) + '</td><td>' + bm[k].requests + '</td><td>' + bm[k].promptTokens + '</td><td>' + bm[k].completionTokens + '</td></tr>').join('');
    _('ocModelStatsBox').innerHTML = '<div style="font-size:13px;font-weight:600;margin-bottom:6px">Per-model breakdown (today)</div>' +
      '<div class="table-wrap"><table><thead><tr><th style="text-align:left">Model</th><th>Requests</th><th>Input tokens</th><th>Output tokens</th></tr></thead><tbody>' +
      (rows || '<tr><td colspan="4" style="text-align:left;color:var(--text2)">No data</td></tr>') + '</tbody></table></div>';
  } catch (e) { /* ignore */ }
}

// ========== Init ==========
loadStats();
loadAccounts();
loadKeys();
loadModels();
loadConfig();
setInterval(() => { loadStats(); }, 10000);
setInterval(() => { loadOcStats(); }, 15000);
// live 会话状态：只在 opencode 页可见时轮询（与日志页同样的省流约定）。
// mint 任务进行中由 mintZenSessions 自己的 2s 轮询接管。
setInterval(() => {
  if (_('tab-opencode').style.display !== 'none' && !ocSessPoll) loadOcSessions();
}, 20000);
setInterval(() => { if (logsAuto && _('tab-logs').style.display !== 'none') loadLogs(); }, 8000);
</script>
</body>
</html>`