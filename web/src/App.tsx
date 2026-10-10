import { useEffect, useState } from 'react';
import api from '@/api/client';
import { Routes, Route, Navigate } from 'react-router-dom';
import { useAuthStore } from '@/store/auth';
import Login from '@/pages/Login';
import ProjectDetail from '@/pages/Projects/ProjectDetail';
export default function App(){
 const authenticated=useAuthStore(s=>s.isAuthenticated);
 const [launching,setLaunching]=useState(location.hash.startsWith('#launch='));
 useEffect(()=>{
  const code=new URLSearchParams(location.hash.slice(1)).get('launch');if(!code)return;
  history.replaceState(null,'',location.pathname);
  api.post<{token:string}>('/launch',{code}).then(result=>useAuthStore.getState().login(result.token)).catch(()=>{}).finally(()=>setLaunching(false));
 },[]);
 if(launching)return <p className="p-8">正在打开 Brodge…</p>;
 const page=authenticated?<ProjectDetail/>:<Navigate to="/login" replace/>;
 return <Routes>
  <Route path="/" element={page}/>
  <Route path="/projects/:name" element={page}/>
  <Route path="/login" element={authenticated?<Navigate to="/" replace/>:<Login/>}/>
  <Route path="*" element={<p className="p-8">页面不存在</p>}/>
 </Routes>;
}
