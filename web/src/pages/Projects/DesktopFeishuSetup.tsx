import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { QRCodeSVG } from 'qrcode.react';
import { CheckCircle2, ExternalLink, Link2, QrCode } from 'lucide-react';
import api from '@/api/client';
import { Card, Button } from '@/components/ui';

type Setup = {
  status: 'idle' | 'starting' | 'waiting' | 'checking' | 'created' | 'error' | 'expired' | 'denied' | 'cancelled' | 'pairing' | 'paired';
  qr_url?: string;
  expires_at?: number;
  app_id?: string;
  owner_open_id?: string;
  error?: string;
  can_retry: boolean;
  pairing_code?: string;
  pairing_expires_at?: number;
  chat_id?: string;
};

export type FeishuConnectionStatus = 'unknown' | 'not_enabled' | 'connected' | 'disconnected';

export default function DesktopFeishuSetup({ name, connectionStatus }: { name: string; connectionStatus: FeishuConnectionStatus }) {
  const { t } = useTranslation();
  const [state, setState] = useState<Setup | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const path = `/projects/${encodeURIComponent(name)}/conversation/feishu`;

  useEffect(() => {
    let active = true;
    let inFlight = false;
    const load = async () => {
      if (inFlight) return;
      inFlight = true;
      try {
        const value = await api.get<Setup>(path);
        if (active) { setState(value); setError(''); }
      } catch (e: any) {
        if (active) setError(e.message);
      } finally { inFlight = false; }
    };
    load();
    const timer = setInterval(load, 2000);
    return () => { active = false; clearInterval(timer); };
  }, [path]);

  const action = async (value: 'start' | 'retry' | 'cancel' | 'pair' | 'enable') => {
    setBusy(true);
    try { setState(await api.post<Setup>(path, { action: value })); setError(''); }
    catch (e: any) { setError(e.message); }
    finally { setBusy(false); }
  };
  const active = state && ['starting', 'waiting', 'checking', 'pairing'].includes(state.status);
  const created = state?.status === 'created' || state?.status === 'paired';
  const configured = state?.status === 'paired' || connectionStatus !== 'not_enabled';
  const canOpen = state?.qr_url?.startsWith('https://open.feishu.cn/page/cli?user_code=');

  return <Card className="text-gray-900 dark:text-gray-100">
    <div className="flex items-center gap-2 mb-3">
      {configured ? <Link2 size={18} className="text-accent" /> : <QrCode size={18} className="text-accent" />}
      <h3 className="font-semibold">{t(configured ? 'desktop.feishu.connectionTitle' : 'desktop.feishu.title')}</h3>
      {configured && <span className={`text-sm ${connectionStatus === 'connected' ? 'text-emerald-600 dark:text-emerald-400' : 'text-gray-600 dark:text-gray-300'}`} role="status" aria-live="polite">· {t(`desktop.feishu.connectionStatus.${connectionStatus}`)}</span>}
    </div>
    {!configured && <p className="text-sm text-gray-500 mb-4">{t('desktop.feishu.hint')}</p>}
    {state && !configured && <p className="text-sm mb-3" role="status" aria-live="polite">
      {created && <CheckCircle2 size={16} className="inline mr-2 text-green-500" />}
      {t(`desktop.feishu.status.${state.status}`)}
    </p>}
    {!configured && state?.status === 'waiting' && canOpen && <div className="flex flex-col sm:flex-row gap-5 items-start mb-4">
      <div className="p-3 bg-white rounded-xl border border-gray-200" aria-label={t('desktop.feishu.qr')}>
        <QRCodeSVG value={state.qr_url!} size={196} level="M" />
      </div>
      <div className="text-sm text-gray-500 space-y-3">
        <p>{t('desktop.feishu.scan')}</p>
        <a href={state.qr_url} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-accent">
          <ExternalLink size={14} />{t('desktop.feishu.open')}
        </a>
        {state.expires_at && <p className="text-xs">{t('desktop.feishu.expires')} {new Date(state.expires_at).toLocaleTimeString()}</p>}
      </div>
    </div>}
    {!configured && state?.status === 'pairing' && <div className="space-y-3 mb-4">
      <p className="text-sm text-gray-500">{t('desktop.feishu.pairHint')}</p>
      <code className="block p-3 rounded-lg bg-gray-100 dark:bg-gray-800 text-sm break-all select-all">{state.pairing_code}</code>
      <p className="text-xs text-gray-500">{t('desktop.feishu.expires')} {new Date(state.pairing_expires_at!).toLocaleTimeString()}</p>
    </div>}
    {!configured && state?.app_id && state.status !== 'pairing' && <p className="text-sm text-gray-500 mb-4">{t('desktop.feishu.next')}</p>}
    {(state?.app_id || state?.owner_open_id || state?.chat_id) && <details className="text-sm text-gray-600 dark:text-gray-300 mb-3">
      <summary className="cursor-pointer">{t('desktop.feishu.details')}</summary>
      {state.app_id && <p className="break-all mt-2">App ID：{state.app_id}</p>}
      {state.owner_open_id && <p className="break-all mt-1">{t('desktop.feishu.owner')}：{state.owner_open_id}</p>}
      {state.chat_id && <p className="break-all mt-1">{t('desktop.feishu.chat')}：{state.chat_id}</p>}
    </details>}
    {state?.error && <p role="alert" className="text-sm text-red-500 mb-3">{t(`desktop.feishu.errors.${state.error}`, t('desktop.feishu.errors.network'))}</p>}
    {error && <p role="alert" className="text-sm text-red-500 mb-3">{error}</p>}
    <div className="flex flex-wrap gap-2">
      {!configured && state && !active && !state.app_id && <Button loading={busy} onClick={() => action(state.can_retry ? 'retry' : 'start')}>
        {state.can_retry ? t('desktop.feishu.resume') : t('desktop.feishu.start')}
      </Button>}
      {!configured && state?.status === 'created' && !state.chat_id && <Button loading={busy} onClick={() => action('pair')}>{t('desktop.feishu.pair')}</Button>}
      {!configured && state?.app_id && !created && !active && <Button loading={busy} onClick={() => action('retry')}>{t('desktop.feishu.check')}</Button>}
      {state?.status === 'paired' && connectionStatus === 'not_enabled' && <Button loading={busy} onClick={() => action('enable')}>{t('desktop.feishu.enable')}</Button>}
      {!configured && active && <Button variant="secondary" loading={busy} onClick={() => action('cancel')}>{t('common.cancel')}</Button>}
    </div>
  </Card>;
}
