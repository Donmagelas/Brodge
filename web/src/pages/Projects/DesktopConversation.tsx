import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import api from '@/api/client';
import { Card, Button, Input } from '@/components/ui';
import DesktopFeishuSetup from './DesktopFeishuSetup';

type State = { error?: string; allowed_threads: string[]; native_answers_enabled: boolean; accessibility_granted: boolean; conversations: {session_key:string;thread:string}[]; bindings: { session_key: string; chat: string; actor: string; thread: string; revision: number }[]; messages: { id: string; thread: string; state: string; error: string }[]; outbox: {id:string;state:string}[] };
export default function DesktopConversation({ name }: { name: string }) {
  const { t } = useTranslation();
  const [state, setState] = useState<State | null>(null);
  const [key, setKey] = useState('');
  const [thread, setThread] = useState('');
  const [error, setError] = useState('');
  const recoveringRef = useRef(new Set<string>());
  const [recovering, setRecovering] = useState<Record<string, boolean>>({});
  const load = () => api.get<State>(`/projects/${encodeURIComponent(name)}/conversation`).then(s => { setState(s); setError(s.error || ''); }).catch(e => setError(String(e.message)));
  useEffect(() => {
    let active = true, inFlight = false;
    const refresh = async () => {
      if (inFlight) return;
      inFlight = true;
      try { const s = await api.get<State>(`/projects/${encodeURIComponent(name)}/conversation`); if (active) { setState(s); setError(s.error || ''); } }
      catch (e: any) { if (active) setError(e.message); }
      finally { inFlight = false; }
    };
    refresh(); const timer = setInterval(refresh, 3000);
    return () => { active = false; clearInterval(timer); };
  }, [name]);
  const bind = async () => { try { await api.post(`/projects/${encodeURIComponent(name)}/conversation`, { session_key: key, thread }); setError(''); await load(); } catch (e: any) { setError(e.message); } };
  const recover = async (id: string) => {
    if (recoveringRef.current.has(id)) return;
    recoveringRef.current.add(id); setRecovering(v => ({ ...v, [id]: true }));
    try { await api.post(`/projects/${encodeURIComponent(name)}/conversation`, { action: 'recover_creation', id }); await load(); }
    catch (e: any) { setError(e.message); }
    finally { recoveringRef.current.delete(id); setRecovering(v => ({ ...v, [id]: false })); }
  };
  const creationLabels: Record<string, string> = { create_pending: '等待新建会话', create_preparing: '准备新会话', create_sending: '正在发送首条消息', create_verifying: '正在核对新会话', create_uncertain: '首次发送待确认', create_failed: '新建未发送' };
  return <div className="space-y-4 text-gray-900 dark:text-gray-100">
    <DesktopFeishuSetup name={name} />
    <Card><h3 className="font-semibold mb-3">{t('desktop.title')}</h3><p className="text-sm text-gray-500 mb-4">{t('desktop.hint')}</p>
      <Input placeholder="codex://threads/..." label={t('desktop.thread')} value={thread} onChange={e => setThread(e.target.value)} />
      <label className="block text-sm mt-3">{t('desktop.chat')}
        <select value={key} onChange={e => setKey(e.target.value)} className="block w-full border rounded p-2 bg-transparent">
          <option value="">{t('desktop.select')}</option>
          {state?.bindings.map(b => <option key={b.session_key} value={b.session_key}>{b.chat} · {b.actor}</option>)}
        </select>
      </label>
      <Button onClick={bind} className="mt-3" disabled={!key || !thread}>{t('desktop.bind')}</Button>{error && <p className="text-red-500 mt-2">{error}</p>}
      <p className="text-xs text-gray-500 mt-3">{t('desktop.allowed')}：{state?.allowed_threads.join(', ')}</p>
      <p className="text-xs text-gray-500">{t('desktop.native')}：{t(state?.native_answers_enabled ? 'desktop.nativeEnabled' : 'desktop.nativeDisabled')}</p>
      {state && <p className="text-xs text-gray-500">Brodge 辅助功能权限：{state.accessibility_granted ? '已允许' : '尚未允许（自动新建会话需要）'}</p>}
    </Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.bindings')}</h3>{state?.bindings.length === 0 && <p className="text-sm text-gray-500">尚未绑定会话</p>}{state?.bindings.map(b => <div key={b.session_key} className="text-sm break-all py-2"><button onClick={() => { setKey(b.session_key); setThread(b.thread); }} className="text-left">当前：{b.thread || t('desktop.empty')}<br /><span className="text-gray-500 text-xs">{b.chat} · {b.actor}</span></button><div className="text-xs mt-1">该入口的会话：{state.conversations?.filter(c=>c.session_key===b.session_key).map(c=><button key={c.thread} onClick={()=>{setKey(b.session_key);setThread(c.thread);}} className="underline mr-3">{c.thread}{c.thread===b.thread?'（当前）':''}</button>)}</div></div>)}</Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.messages')}</h3>{state?.messages.length === 0 && <p className="text-sm text-gray-500">尚未收到消息</p>}{state?.messages.map(m => <div className="text-sm py-2 break-all" key={m.id}><p>{m.thread} · {creationLabels[m.state] || t(`desktop.${m.state}`, m.state)}{m.error && ` · ${m.error}`}</p>{m.state==='create_failed' && <Button className="mt-2" disabled={recovering[m.id]} onClick={()=>recover(m.id)}>{recovering[m.id] ? '正在准备，请稍候…' : '重试创建（原消息尚未发送）'}</Button>}{['create_uncertain','create_verifying'].includes(m.state) && <p className="mt-2">正在自动核对新会话，确认后会自动绑定并回传结果。不会重复发送，无需填写会话链接。</p>}</div>)}</Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.outbox')}</h3>{state?.outbox.length === 0 && <p className="text-sm text-gray-500">没有待发送或异常回复</p>}{state?.outbox.map(o => <p className="text-sm py-1" key={o.id}>{t(`desktop.${o.state}`, o.state)}</p>)}</Card>
  </div>;
}
