let eventPage=1,eventPageSize=20,eventListVersion=0;
const $ = id => document.getElementById(id);
const states = { PENDING: '等待投递', PROCESSING: '正在投递', BLOCKED: '业务拒绝', DELIVERED: '已接收', LATE_ADJUSTMENT: '已接收，待迟到调整', SUCCEEDED: '已接收' };
let busy = false;
async function api(path, body) {
  const response = await fetch(path, { method: body === undefined ? 'GET' : 'POST', headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || '请求失败');
  return result;
}
function notice(message) { $('notice').textContent = message; }
async function refresh() {
  const version=++eventListVersion;const data = await api('/api/simulation/events?'+new URLSearchParams({page:eventPage,page_size:eventPageSize}));if(version!==eventListVersion)return;
  const records = [...(eventPage===1?[{ ...data.registration, kind: 'REGISTRATION', business_id: '注册事件' }]:[]), ...data.items];
  $('records').replaceChildren(...records.map(row => {
    const tr = document.createElement('tr');
    for (const text of [(row.kind === 'REGISTRATION' ? '注册' : row.kind === 'BET_SETTLED' ? '投注结算' : row.kind === 'DEPOSIT_REFUNDED' ? '充值退款' : row.kind === 'BET_CANCELLED' ? '投注撤单' : '充值') + ' / ' + row.business_id, row.event_id, states[row.state] || row.state, row.attempts, row.last_code || '—', row.state === 'PENDING' && row.next_retry_at ? new Date(row.next_retry_at).toLocaleString() : '—']) {
      const td = document.createElement('td'); td.textContent = text; tr.append(td);
    }
    const td = document.createElement('td');
    if (row.state === 'PENDING' || row.state === 'BLOCKED') {
      const button = document.createElement('button'); button.textContent = '按原事实补发'; button.disabled = busy;
      button.onclick = async () => {
        if (busy) return;
        if (!$('reason').value.trim()) { notice('请先填写补发原因。'); return; }
        busy = true; button.disabled = true;
        try { await api('/api/simulation/resend', { kind: row.kind, business_id: row.business_id, reason: $('reason').value }); notice('原事件已重新排队，未修改业务事实。'); await refresh(); } catch (error) { notice(error.message); } finally { busy = false; button.disabled = false; }
      };
      td.append(button);
    } else td.textContent = '—';
    tr.append(td); return tr;
  }));
 let pager=document.getElementById('records-pager');if(!pager){pager=document.createElement('div');pager.id='records-pager';$('records').closest('table').after(pager)}pager.replaceChildren(pageControls({page:eventPage,size:eventPageSize,more:data.has_more,onChange:(page,size)=>{eventPage=page;eventPageSize=size;refresh().catch(e=>notice(e.message))}}));
}
$('kind').onchange = () => { const kind=$('kind').value,bet=kind==='BET_SETTLED',reverse=kind==='BET_CANCELLED'||kind==='DEPOSIT_REFUNDED';$('valid-label').hidden=!(bet||kind==='BET_CANCELLED');$('venue-label').hidden=!bet;$('venue').required=bet;$('original-label').hidden=!reverse;$('reversal-reason-label').hidden=!reverse;$('original').required=reverse;$('reversal-reason').required=reverse; };
$('event-form').onsubmit = async event => {
  event.preventDefault(); if (busy) return; busy = true; $('save').disabled = true;
  const bet = $('kind').value === 'BET_SETTLED';
  try { const result = await api('/api/simulation/events', { business_id: $('business').value, kind: $('kind').value, amount: $('amount').value, valid_bet: bet || $('kind').value==='BET_CANCELLED' ? $('valid').value : '0', venue_type_id: bet ? $('venue').value : '', original_business_id:$('original-label').hidden?'':$('original').value,reason:$('reversal-reason-label').hidden?'':$('reversal-reason').value }); notice('事件已保存：' + result.event_id + '。等待佣金系统接收。'); await refresh(); }
  catch (error) { notice(error.message); } finally { busy = false; $('save').disabled = false; }
};
$('refresh').onclick = () => refresh().catch(error => notice(error.message));
(async () => {
  try { const member = await api('/api/me'); $('member').textContent = '当前会员：' + member.display_name + '（' + member.member_id + '）'; const profile = await api('/api/simulation/profile'); $('currency').textContent = '币种：' + profile.currency + '，金额最多 ' + profile.currency_scale + ' 位小数。'; await refresh(); }
  catch (error) { notice(error.message); }
})();
setInterval(() => { if (!busy) refresh().catch(error => notice(error.message)); }, 5000);

// 分页状态保留在当前页面；按钮只改变查询范围，不提交或批准业务操作。
function pageControls({page=1,size=20,total,more=false,onChange}){
 const nav=document.createElement('nav');nav.className='list-pagination';nav.setAttribute('aria-label','列表分页');
 const count=document.createElement('span');count.textContent=total===undefined?'第 '+page+' 页':'共 '+total+' 条';nav.append(count);
 const label=document.createElement('label');label.append('每页 ');const select=document.createElement('select');select.setAttribute('aria-label','每页条数');for(const n of [20,50,100])select.add(new Option(n+' 条',String(n)));select.value=String(size);select.onchange=()=>onChange(1,Number(select.value));label.append(select);nav.append(label);
 const pages=total===undefined?null:Math.max(1,Math.ceil(total/size));
 for(const [text,next,disabled]of [['上一页',page-1,page<=1],['下一页',page+1,pages===null?!more:page>=pages]]){const b=document.createElement('button');b.type='button';b.textContent=text;b.disabled=disabled;b.onclick=()=>onChange(next,size);nav.append(b)}
 if(pages!==null){const info=document.createElement('span');info.textContent=page+' / '+pages;nav.append(info)}return nav;
}
