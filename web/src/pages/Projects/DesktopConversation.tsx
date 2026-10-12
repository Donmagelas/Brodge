import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import api from '@/api/client';
import { Card, Button, Input } from '@/components/ui';
import DesktopFeishuSetup from './DesktopFeishuSetup';
import type { FeishuConnectionStatus } from './DesktopFeishuSetup';

type Conversation = { session_key: string; thread: string; title: string; latest_query: string };
type Binding = { session_key: string; chat: string; actor: string; display_name: string; thread: string; revision: number };
type State = {
  error?: string;
  allowed_threads: string[];
  native_answers_enabled: boolean;
  accessibility_granted: boolean;
  conversations: Conversation[];
  bindings: Binding[];
  messages: { id: string; session_key: string; thread: string; title: string; state: string; error: string; preview: string; delivery_state: string }[];
  outbox: { id: string; state: string }[];
};
export default function DesktopConversation({ name, feishuConnectionStatus }: { name: string; feishuConnectionStatus: FeishuConnectionStatus }) {
  const { t } = useTranslation();
  const [state, setState] = useState<State | null>(null);
  const [key, setKey] = useState('');
  const [thread, setThread] = useState('');
  const [error, setError] = useState('');
  const [removing, setRemoving] = useState<Conversation | null>(null);
  const [saving, setSaving] = useState(false);
  const recoveringRef = useRef(new Set<string>());
  const [recovering, setRecovering] = useState<Record<string, boolean>>({});
  const inspectingRef = useRef(false);
  const [inspecting, setInspecting] = useState(false);
  const [inspection, setInspection] = useState<{ id: string; report: string } | null>(null);
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
  const bind = async (sessionKey = key, threadID = thread) => {
    setSaving(true);
    try { await api.post(`/projects/${encodeURIComponent(name)}/conversation`, { session_key: sessionKey, thread: threadID }); setError(''); await load(); }
    catch (e: any) { setError(e.message); }
    finally { setSaving(false); }
  };
  const unbind = async (conversation: Conversation) => {
    setSaving(true);
    try {
      await api.post(`/projects/${encodeURIComponent(name)}/conversation`, { action: 'unbind', session_key: conversation.session_key, thread: conversation.thread });
      if (key === conversation.session_key && thread === conversation.thread) setThread('');
      setRemoving(null); setError(''); await load();
    } catch (e: any) { setError(e.message); }
    finally { setSaving(false); }
  };
  const recover = async (id: string) => {
    if (recoveringRef.current.has(id)) return;
    recoveringRef.current.add(id); setRecovering(v => ({ ...v, [id]: true }));
    try { await api.post(`/projects/${encodeURIComponent(name)}/conversation`, { action: 'recover_creation', id }); await load(); }
    catch (e: any) { setError(e.message); }
    finally { recoveringRef.current.delete(id); setRecovering(v => ({ ...v, [id]: false })); }
  };
  const inspectComposer = async (id: string) => {
    if (inspectingRef.current) return;
    inspectingRef.current = true; setInspecting(true); setInspection(null);
    try {
      const report = await api.post(`/projects/${encodeURIComponent(name)}/conversation`, { action: 'inspect_composer' });
      setInspection({ id, report: JSON.stringify(report, null, 2) });
    } catch (e: any) { setInspection({ id, report: e.message }); }
    finally { inspectingRef.current = false; setInspecting(false); }
  };
  const creationLabels: Record<string, string> = { create_pending: '等待新建会话', create_preparing: '准备新会话', create_sending: '正在发送首条消息', create_verifying: '正在核对新会话', create_uncertain: '首次发送待确认', create_failed: '新建未发送' };
  const messageStatus = (m: State['messages'][number]) => {
    if (creationLabels[m.state]) return creationLabels[m.state];
    if (m.state === 'finished') {
      const delivery: Record<string, string> = { pending: '待发送', sending: '发送中', sent: '已发送', uncertain: '发送结果待确认', blocked: '发送失败', none: '发送状态未知' };
      return `${m.error ? '处理未完成 · 提示' : '回复'}${delivery[m.delivery_state] || '发送状态未知'}`;
    }
    const progress: Record<string, string> = { queued: '等待发送给 Codex', claimed: '准备发送给 Codex', dispatch: '正在发送给 Codex', running: '已发送给 Codex · 处理中', waiting: '已发送给 Codex · 等待回答', uncertain: '正在核对 Codex 接收状态', blocked: '处理受阻' };
    return progress[m.state] || '状态待确认';
  };
  const boundChats = state?.bindings.filter(b => state.conversations.some(c => c.session_key === b.session_key)) ?? [];
  const newMessages = state?.messages.filter(m => m.state.startsWith('create_') && !state.conversations.some(c => c.session_key === m.session_key && c.thread === m.thread)) ?? [];
  const renderMessages = (messages: State['messages'], showChat = false) => messages.length === 0
    ? <p className="text-sm text-gray-500 mt-2">尚未收到消息</p>
    : messages.map(m => <div className="text-sm py-2 break-words border-b last:border-b-0 border-gray-200 dark:border-white/10" key={m.id}>
      {showChat && <p className="font-medium">{state?.bindings.find(b => b.session_key === m.session_key)?.display_name || '新会话'}</p>}
      <p className="text-gray-900 dark:text-gray-100 line-clamp-2" title={m.preview}>{m.preview || '（无文字消息）'}</p>
      <p className={`text-xs mt-1 ${m.error || m.delivery_state === 'blocked' ? 'text-red-600 dark:text-red-400' : m.state === 'finished' && m.delivery_state === 'sent' ? 'text-emerald-700 dark:text-emerald-400' : 'text-gray-600 dark:text-gray-300'}`}>{messageStatus(m)}</p>
      {m.error && <details className="text-xs mt-1"><summary className="cursor-pointer">异常详情</summary><p className="mt-1 break-all">{m.error}</p></details>}
      {m.state === 'create_failed' && <Button className="mt-2" disabled={recovering[m.id]} onClick={() => recover(m.id)}>{recovering[m.id] ? '正在准备，请稍候…' : '重试创建（原消息尚未发送）'}</Button>}
      {m.state === 'create_failed' && m.error.startsWith('desktop-composer-lookup') && <div className="mt-2">
        <Button variant="secondary" disabled={inspecting || Object.values(recovering).some(Boolean)} onClick={() => inspectComposer(m.id)}>{inspecting ? '正在检查…' : '检查输入框（不发送）'}</Button>
        <p className="text-xs mt-1">请先让 Codex 停留在出错的新建页面。检查当前窗口的输入框结构，不切换页面、不输入或发送消息、不记录聊天正文。</p>
        {inspection?.id === m.id && <details open className="mt-2"><summary className="cursor-pointer">本次检查结果（不是失败时刻的快照）</summary><pre className="text-xs whitespace-pre-wrap break-all max-h-80 overflow-auto p-2 bg-gray-50 dark:bg-white/5">{inspection.report}</pre></details>}
      </div>}
      {['create_uncertain', 'create_verifying'].includes(m.state) && <p className="mt-2">正在自动核对新会话，确认后会自动绑定并回传结果。不会重复发送，无需填写会话链接。</p>}
    </div>);
  return <div className="space-y-4 text-gray-900 dark:text-gray-100">
    <DesktopFeishuSetup name={name} connectionStatus={feishuConnectionStatus} />
    <Card><h3 className="font-semibold mb-3">{t('desktop.title')}</h3><p className="text-sm text-gray-500 mb-4">{t('desktop.hint')}</p>
      <Input placeholder="codex://threads/..." label={t('desktop.thread')} value={thread} onChange={e => setThread(e.target.value)} />
      <label className="block text-sm mt-3">{t('desktop.chat')}
        <select value={key} onChange={e => setKey(e.target.value)} className="block w-full border border-gray-300 rounded p-2 bg-white text-gray-900 dark:border-gray-600 dark:bg-gray-900 dark:text-gray-100">
          <option value="">{t('desktop.select')}</option>
          {state?.bindings.map(b => <option key={b.session_key} value={b.session_key}>{b.display_name}</option>)}
        </select>
      </label>
      <Button onClick={() => bind()} className="mt-3" disabled={!key || !thread || saving}>{t('desktop.bind')}</Button>{error && <p className="text-red-500 mt-2">{error}</p>}
      <details className="text-xs text-gray-500 mt-3"><summary className="cursor-pointer">连接详情</summary><p className="break-all mt-1">{t('desktop.allowed')}：{state?.allowed_threads.join(', ')}</p></details>
      <p className="text-xs text-gray-500">{t('desktop.native')}：{t(state?.native_answers_enabled ? 'desktop.nativeEnabled' : 'desktop.nativeDisabled')}</p>
      {state && <p className="text-xs text-gray-500">Brodge 辅助功能权限：{state.accessibility_granted ? '已允许' : '尚未允许（自动新建会话需要）'}</p>}
      {newMessages.length > 0 && <details className="mt-3 text-gray-600 dark:text-gray-300"><summary className="text-sm cursor-pointer">新建会话状态（{newMessages.length}）</summary>{renderMessages(newMessages, true)}</details>}
    </Card>
    <Card>
      <h3 className="font-semibold mb-3">{t('desktop.bindings')}</h3>
      {state && boundChats.length === 0 && <p className="text-sm text-gray-500">尚未绑定会话</p>}
      {state && boundChats.map(b => {
        const conversations = state.conversations.filter(c => c.session_key === b.session_key);
        return <section key={b.session_key} className="py-4 border-b last:border-b-0 border-gray-200 dark:border-white/10">
          <h4 className="font-medium text-sm break-words">{b.display_name}</h4>
          {!b.thread && <p className="text-xs text-gray-500 mt-2">未绑定当前会话。可以先手动绑定；下一条正文消息会自动新建会话。</p>}
          <div className="space-y-3 mt-3">
            {conversations.map((c, index) => {
              const messages = state.messages.filter(m => m.session_key === b.session_key && m.thread === c.thread);
              return <div key={c.thread} className="rounded-lg bg-gray-50 dark:bg-white/5 p-3 text-sm">
                <div className="flex items-start justify-between gap-3 flex-wrap">
                  <p className="font-medium break-words min-w-0 flex-1">{conversations.length > 1 && `${index + 1}. `}{c.title || '未命名会话'}{c.thread === b.thread && <span className="text-xs text-emerald-600 dark:text-emerald-400 ml-2">当前</span>}</p>
                  <div className="flex gap-2 shrink-0">
                    {c.thread !== b.thread && <Button size="sm" variant="secondary" disabled={saving} onClick={() => bind(b.session_key, c.thread)}>切换到此会话</Button>}
                    <Button size="sm" variant="danger" disabled={saving} onClick={() => setRemoving(c)}>解除绑定</Button>
                  </div>
                </div>
                {conversations.length > 1 && <p className="text-sm text-gray-600 dark:text-gray-300 mt-2 break-words">最近提问：{c.latest_query || '暂未取得用户提问'}</p>}
                <details className="text-xs text-gray-500 mt-2"><summary className="cursor-pointer">会话详情</summary><p className="break-all mt-1">Codex：{c.thread}</p><p className="break-all">飞书对话：{b.chat}</p><p className="break-all">飞书用户：{b.actor}</p></details>
                <details className="mt-3 text-gray-600 dark:text-gray-300"><summary className="cursor-pointer">{t('desktop.messages')}（{messages.length}）</summary><div className="max-h-64 overflow-y-auto mt-1">{renderMessages(messages)}</div></details>
                {removing?.session_key === b.session_key && removing.thread === c.thread && <div className="mt-3 text-xs" role="alert">
                  <p>解除“{c.title}”的绑定？Codex 会话和聊天记录会保留。{c.thread === b.thread && '解除后没有当前绑定，下一条正文会自动新建会话。'}</p>
                  <div className="flex gap-2 mt-2"><Button size="sm" variant="danger" disabled={saving} onClick={() => unbind(c)}>{saving ? '正在解除…' : '确认解除'}</Button><Button size="sm" variant="ghost" disabled={saving} onClick={() => setRemoving(null)}>取消</Button></div>
                </div>}
              </div>;
            })}
          </div>
        </section>;
      })}
    </Card>
    <Card><h3 className="font-semibold mb-3">{t('desktop.outbox')}</h3>{state?.outbox.length === 0 && <p className="text-sm text-gray-500">没有待发送或异常回复</p>}{state?.outbox.map(o => <p className="text-sm py-1" key={o.id}>{t(`desktop.${o.state}`, o.state)}</p>)}</Card>
  </div>;
}
