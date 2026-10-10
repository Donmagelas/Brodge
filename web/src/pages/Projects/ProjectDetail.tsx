import { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';
import api from '@/api/client';
import { useAuthStore } from '@/store/auth';
import { useThemeStore } from '@/store/theme';
import { Button } from '@/components/ui';
import DesktopConversation from './DesktopConversation';
type Status={connections:{name:string;platforms:{name:string;connected:boolean;degraded_reason?:string}[]}[]};
export default function ProjectDetail(){
 const {name}=useParams();
 const [status,setStatus]=useState<Status|null>(null);
 const [error,setError]=useState('');
 const [closed,setClosed]=useState(false);
 const shutdown=async()=>{try{await api.post('/shutdown');localStorage.setItem('brodge_shutdown',String(Date.now()));setClosed(true);window.close();}catch(e:any){setError(e.message);}};
 const logout=useAuthStore(s=>s.logout);
 const theme=useThemeStore();
 useEffect(()=>{
  const onStorage=(event:StorageEvent)=>{if(event.key==='brodge_shutdown')setClosed(true);};
  window.addEventListener('storage',onStorage);return()=>window.removeEventListener('storage',onStorage);
 },[]);
 useEffect(()=>{
  if(closed)return;
  let active=true,loading=false;
  const load=async()=>{if(loading)return;loading=true;try{const s=await api.get<Status>('/status');if(active){setStatus(s);setError('');}}catch(e:any){if(active)setError(e.message);}finally{loading=false;}};
  load();const timer=setInterval(load,3000);return()=>{active=false;clearInterval(timer);};
 },[closed]);
 const connection=status?.connections.find(c=>!name || c.name===name);
 const platform=connection?.platforms[0];
 if(closed)return <div className="p-8"><h1>Brodge 已退出</h1><p>后台已停止，可以关闭此页面。再次双击 Brodge 可重新启动。</p></div>;
 return <div className="max-w-5xl mx-auto px-5 py-8 text-gray-900 dark:text-gray-100">
  <header className="flex justify-between items-start gap-4 mb-8">
   <div className="flex items-center gap-3"><img src="/assets/brodge-icon.png" alt="" width={48} height={48} className="rounded-lg"/><div><h1 className="text-2xl font-semibold">Brodge</h1><p className="text-sm text-gray-500 mt-2">在飞书中继续本机 Codex 会话</p></div></div>
   <div className="flex gap-2"><Button variant="ghost" onClick={()=>theme.setTheme(theme.resolved==='dark'?'light':'dark')}>切换外观</Button><Button variant="ghost" onClick={logout}>退出登录</Button><Button variant="ghost" onClick={shutdown}>关闭 Brodge</Button></div>
  </header>
  <div className="rounded-xl border border-gray-200 dark:border-gray-700 p-4 mb-5 text-sm" role="status">
   <span className="mr-6">本机服务：{error?'连接失败':status?'已连接':'连接中'}</span>
   <span>飞书：{!platform?'尚未启用':platform.connected?'长连接已连接':'长连接未连接'}</span>
   {platform?.degraded_reason && <p className="text-amber-600 mt-2">连接诊断：{platform.degraded_reason}</p>}
   {error && <p role="alert" className="text-red-500 mt-2">{error}</p>}
  </div>
  {connection?<DesktopConversation name={connection.name}/>:status && <p>找不到该连接</p>}
 </div>;
}
