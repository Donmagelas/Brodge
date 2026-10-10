import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import api from '@/api/client';
import { Card, Button, Input } from '@/components/ui';
import DesktopFeishuSetup from './DesktopFeishuSetup';

type State = { error?: string; allowed_threads: string[]; native_answers_enabled: boolean; conversations: {session_key:string;thread:string}[]; bindings: { session_key: string; chat: string; actor: string; thread: string; revision: number }[]; messages: { id: string; thread: string; state: string; error: string }[]; outbox: {id:string;state:string}[] };
export default function DesktopConversation({ name }: { name: string }) {
  const { t } = useTranslation();
  const [state, setState] = useState<State | null>(null);
  const [key, setKey] = useState('');
  const [thread, setThread] = useState('');
  const [error, setError] = useState('');
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
    </Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.bindings')}</h3>{state?.bindings.length === 0 && <p className="text-sm text-gray-500">尚未绑定会话</p>}{state?.bindings.map(b => <button key={b.session_key} onClick={() => { setKey(b.session_key); setThread(b.thread); }} className="block text-left text-sm break-all py-2">{b.thread || t('desktop.empty')}<br /><span className="text-xs">该入口的会话：{state.conversations?.filter(c=>c.session_key===b.session_key).map(c=>c.thread).join(', ')}</span><br /><span className="text-gray-500 text-xs">{b.chat} · {b.actor}</span></button>)}</Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.messages')}</h3>{state?.messages.length === 0 && <p className="text-sm text-gray-500">尚未收到消息</p>}{state?.messages.map(m => <p className="text-sm py-1 break-all" key={m.id}>{m.thread} · {t(`desktop.${m.state}`, m.state)}{m.error && ` · ${m.error}`}</p>)}</Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.outbox')}</h3>{state?.outbox.length === 0 && <p className="text-sm text-gray-500">没有待发送或异常回复</p>}{state?.outbox.map(o => <p className="text-sm py-1" key={o.id}>{t(`desktop.${o.state}`, o.state)}</p>)}</Card>
  </div>;
}
