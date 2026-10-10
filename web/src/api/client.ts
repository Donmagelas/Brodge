const API_BASE='/api/v1';
class ApiClient {
 private token='';
 private onUnauthorized?: () => void;
 setToken(token:string){this.token=token;}
 setOnUnauthorized(handler:()=>void){this.onUnauthorized=handler;}
 async request<T>(method:string,path:string,body?:unknown):Promise<T>{
  const res=await fetch(API_BASE+path,{method,headers:{'Content-Type':'application/json',Authorization:'Bearer '+this.token},body:body===undefined?undefined:JSON.stringify(body)});
  if(res.status===401){this.onUnauthorized?.();throw new Error('请使用本机管理 token 登录');}
  const json=await res.json();
  if(!json.ok)throw new Error(json.error || '请求失败');
  return json.data as T;
 }
 get<T>(path:string){return this.request<T>('GET',path);}
 post<T>(path:string,body?:unknown){return this.request<T>('POST',path,body);}
}
export const api=new ApiClient();
export default api;
