package ui

// HTML is a small standalone management surface for customer API key policy.
const HTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Customer Keys</title>
<style>
:root{color-scheme:light dark;--bg:#f7f8fa;--fg:#171a1f;--muted:#667085;--line:#d7dde6;--panel:#fff;--accent:#1261a6;--bad:#b42318;--ok:#027a48}
@media (prefers-color-scheme:dark){:root{--bg:#14171c;--fg:#edf1f7;--muted:#a3adbd;--line:#303846;--panel:#1d222b;--accent:#69a7e8;--bad:#ff8a7a;--ok:#6bd7a0}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.4 system-ui,-apple-system,Segoe UI,sans-serif}
header{display:flex;gap:12px;align-items:center;justify-content:space-between;padding:14px 18px;border-bottom:1px solid var(--line);background:var(--panel)}
h1{font-size:18px;margin:0;font-weight:650;letter-spacing:0}main{display:grid;grid-template-columns:320px 1fr;gap:16px;padding:16px;max-width:1280px;margin:0 auto}
button,input,textarea,select{font:inherit}button{border:1px solid var(--line);background:var(--panel);color:var(--fg);border-radius:6px;padding:7px 10px;cursor:pointer}button.primary{background:var(--accent);border-color:var(--accent);color:white}
button.danger{color:var(--bad)}input,textarea,select{width:100%;border:1px solid var(--line);background:var(--panel);color:var(--fg);border-radius:6px;padding:7px 9px;min-width:0}
textarea{min-height:74px;resize:vertical}.panel{background:var(--panel);border:1px solid var(--line);border-radius:8px;overflow:hidden}.panel h2{font-size:14px;margin:0;padding:12px 14px;border-bottom:1px solid var(--line)}
.list{max-height:calc(100vh - 140px);overflow:auto}.key{display:block;width:100%;text-align:left;border:0;border-bottom:1px solid var(--line);border-radius:0;padding:11px 14px}.key.active{background:color-mix(in srgb,var(--accent) 14%,transparent)}
.muted{color:var(--muted);font-size:12px}.grid{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px}.form{padding:14px;display:grid;gap:14px}.row{display:grid;gap:6px}
.actions{display:flex;gap:8px;flex-wrap:wrap}.status{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:10px}.metric{border:1px solid var(--line);border-radius:8px;padding:10px}.metric b{display:block;font-size:18px}
table{width:100%;border-collapse:collapse}th,td{padding:8px 10px;border-top:1px solid var(--line);text-align:left;vertical-align:top}th{color:var(--muted);font-weight:600}.ok{color:var(--ok)}.bad{color:var(--bad)}
@media(max-width:820px){main{grid-template-columns:1fr}.grid,.status{grid-template-columns:1fr}}
</style>
</head>
<body>
<header><h1>Customer Keys</h1><div class="actions"><input id="mgmtKey" type="password" placeholder="Management key"><button id="saveKey">Use</button><button id="sync">Sync Prices</button></div></header>
<main>
<section class="panel"><h2>Keys</h2><div id="keys" class="list"></div></section>
<section class="panel"><h2>Policy</h2><div class="form">
<div class="status" id="status"></div>
<div class="grid">
<label class="row">Label<input id="label"></label>
<label class="row">Quota period<select id="period"><option value="daily">daily</option><option value="weekly">weekly</option><option value="monthly">monthly</option><option value="none">none</option><option value="1h">1h</option></select></label>
<label class="row">Enabled<select id="enabled"><option value="true">true</option><option value="false">false</option></select></label>
</div>
<div class="grid">
<label class="row">Requests<input id="requests" type="number" min="0"></label>
<label class="row">Tokens<input id="tokens" type="number" min="0"></label>
<label class="row">Cost USD<input id="cost" type="number" min="0" step="0.0001"></label>
</div>
<div class="grid">
<label class="row">Concurrency<input id="concurrency" type="number" min="0"></label>
<label class="row">Rate requests<input id="rateRequests" type="number" min="0"></label>
<label class="row">Rate period<input id="ratePeriod" placeholder="1m"></label>
</div>
<div class="grid">
<label class="row">Allowed models<textarea id="allowed" spellcheck="false"></textarea></label>
<label class="row">Denied models<textarea id="denied" spellcheck="false"></textarea></label>
<label class="row">Missing price<select id="missingPrice"><option value="">global</option><option value="true">fail closed</option><option value="false">allow</option></select></label>
</div>
<div class="actions"><button class="primary" id="save">Save</button><button class="danger" id="delete">Delete Policy</button><button id="refresh">Refresh</button></div>
<div><table><thead><tr><th>Time</th><th>Status</th><th>Model</th><th>Tokens</th><th>Cost</th><th>Reason</th></tr></thead><tbody id="records"></tbody></table></div>
</div></section>
</main>
<script>
const $=id=>document.getElementById(id);let items=[],selected=null;
const api=(path,opts={})=>fetch('/v0/management'+path,{...opts,headers:{'Content-Type':'application/json','X-Management-Key':sessionStorage.customerKeyPolicyMgmt||$('mgmtKey').value,...opts.headers}}).then(async r=>{const j=await r.json().catch(()=>({}));if(!r.ok)throw new Error(j.error||r.statusText);return j});
function lines(v){return String(v||'').split(/\n|,/).map(s=>s.trim()).filter(Boolean)}
function showMetric(name,value,cls=''){return '<div class="metric"><span class="muted">'+name+'</span><b class="'+cls+'">'+(value??'')+'</b></div>'}
function renderKeys(){const box=$('keys');box.innerHTML='';items.forEach(x=>{const b=document.createElement('button');b.className='key'+(selected&&selected.key_id===x.key_id?' active':'');b.innerHTML='<b>'+x.masked_key+'</b><div class="muted">'+x.key_id.slice(0,12)+(x.policy?' - policy':'')+'</div>';b.onclick=()=>select(x.key_id);box.appendChild(b)})}
function fill(item){selected=item;renderKeys();const p=item.policy||{};$('label').value=p.label||'';$('enabled').value=String(p.enabled!==false);$('period').value=(p.quota&&p.quota.period)||'monthly';$('requests').value=(p.quota&&p.quota.requests)||'';$('tokens').value=(p.quota&&p.quota.tokens)||'';$('cost').value=(p.quota&&p.quota.cost_usd)||'';$('concurrency').value=p.max_concurrent_requests||'';$('rateRequests').value=(p.rate_limit&&p.rate_limit.requests)||'';$('ratePeriod').value=(p.rate_limit&&p.rate_limit.period)||'1m';$('allowed').value=(p.allowed_models||[]).join('\n');$('denied').value=(p.denied_models||[]).join('\n');$('missingPrice').value=p.fail_closed_on_missing_price==null?'':String(p.fail_closed_on_missing_price);renderStatus(item.status);loadRecords()}
function renderStatus(s){$('status').innerHTML=showMetric('Requests',s.window?.requests||0)+showMetric('Tokens',s.window?.tokens||0)+showMetric('Cost',(s.window?.cost_usd||0).toFixed(6))+showMetric('In flight',s.in_flight||0)}
async function load(){const data=await api('/customer-key-policies');items=data.items||[];if(!selected&&items[0])selected=items[0];renderKeys();if(selected)fill(items.find(x=>x.key_id===selected.key_id)||items[0])}
async function select(id){selected=items.find(x=>x.key_id===id);fill(selected)}
async function loadRecords(){if(!selected)return;const data=await api('/customer-key-policies/'+selected.key_id+'/records?limit=80');$('records').innerHTML=(data.records||[]).map(r=>'<tr><td>'+new Date(r.timestamp).toLocaleString()+'</td><td class="'+(r.status==='blocked'||r.status==='failed'?'bad':'ok')+'">'+r.status+'</td><td>'+(r.alias||r.model||'')+'</td><td>'+(r.detail?.total_tokens||0)+'</td><td>'+(r.cost_usd||0).toFixed(6)+'</td><td>'+(r.block_reason||'')+'</td></tr>').join('')}
async function save(){if(!selected)return;let mp=$('missingPrice').value;const body={key_id:selected.key_id,label:$('label').value,enabled:$('enabled').value==='true',allowed_models:lines($('allowed').value),denied_models:lines($('denied').value),quota:{period:$('period').value,requests:+$('requests').value||0,tokens:+$('tokens').value||0,cost_usd:+$('cost').value||0},rate_limit:{requests:+$('rateRequests').value||0,period:$('ratePeriod').value||'1m'},max_concurrent_requests:+$('concurrency').value||0};if(mp)body.fail_closed_on_missing_price=mp==='true';await api('/customer-key-policies/'+selected.key_id,{method:'PATCH',body:JSON.stringify(body)});await load()}
$('save').onclick=()=>save().catch(e=>alert(e.message));$('delete').onclick=async()=>{if(selected){await api('/customer-key-policies/'+selected.key_id,{method:'DELETE'});await load()}};$('refresh').onclick=()=>load().catch(e=>alert(e.message));$('sync').onclick=()=>api('/customer-key-prices/sync',{method:'POST',body:'{}'}).then(load).catch(e=>alert(e.message));$('saveKey').onclick=()=>{sessionStorage.customerKeyPolicyMgmt=$('mgmtKey').value;load().catch(e=>alert(e.message))};$('mgmtKey').value=sessionStorage.customerKeyPolicyMgmt||'';load().catch(()=>{});
</script>
</body>
</html>`
