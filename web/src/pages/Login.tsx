import { useState } from 'react';
import api from '@/api/client';
import { useAuthStore } from '@/store/auth';
import { Card, Button, Input } from '@/components/ui';
export default function Login() {
 const [token,setToken]=useState('');
 const [error,setError]=useState('');
 const [busy,setBusy]=useState(false);
 const login=useAuthStore(s=>s.login);
 const submit=async(e:React.FormEvent)=>{
  e.preventDefault();setBusy(true);api.setToken(token);
  try{await api.get('/status');login(token);}catch(e:any){api.setToken('');setError(e.message);}finally{setBusy(false);}
 };
 return <main className="max-w-md mx-auto px-6 pt-24 text-gray-900 dark:text-gray-100"><Card>
  <img src="/assets/brodge-icon.png" alt="" width={64} height={64} className="rounded-xl mb-4"/>
  <h1 className="text-2xl font-semibold mb-2">Brodge</h1>
  <p className="text-sm text-gray-500 mb-6">连接本机桥接服务</p>
  <form onSubmit={submit} className="space-y-4">
   <Input type="password" label="管理 token" autoComplete="off" autoFocus value={token} onChange={e=>setToken(e.target.value)} />
   {error && <p role="alert" className="text-red-500 text-sm">{error}</p>}
   <Button type="submit" loading={busy} disabled={!token.trim()}>登录</Button>
  </form>
 </Card></main>;
}
